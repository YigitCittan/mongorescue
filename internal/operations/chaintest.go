package operations

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
)

// ErrNoChainTest is returned by StartChainTest for a stream without two eligible
// base backups in one window, the newer with a manifest.
var ErrNoChainTest = errors.New("no PITR chain test is possible yet")

// StartChainTest starts a chain test of PITR stream streamID (by stream or
// connection ID) in the background: the newest eligible base with a manifest (B2)
// and the eligible base before it (B1) are taken, B1 is restored to B2's consistent
// point (every write up to and including T_after) into temporary
// <db>_rescue_cv<base36 time><id> clones on the stream's chain_test_connection_id
// (default: its own connection), the clones are compared with B2's
// manifest (RestoreRecord.Verification) and then dropped. The restore record is
// marked as a chain test (PITRRestore.ChainTest); readiness reports the newest
// failed one, and its duration feeds the RTO estimate of point-in-time restores.
// The preflight applies like to any restore (no force). It needs the admin scope.
// Expected failures: those of StartRestore with PITR (a *PreflightError too), and
// ErrNoChainTest.
func (s *Service) StartChainTest(ctx context.Context, streamID string) (*models.RestoreRecord, error) {
	if err := s.visibleStreamFirst(ctx, streamID); err != nil {
		return nil, err
	}
	if err := auth.RequireScope(ctx, auth.ScopeAdmin); err != nil {
		return nil, fmt.Errorf("PITR chain tests need the admin role or an admin API key: %w", err)
	}
	if s.cfg.PITR == nil || s.cfg.PITRRestore == nil || s.cfg.PITRBases == nil {
		return nil, ErrPITRUnavailable
	}
	stream, err := s.resolveStream(ctx, strings.TrimSpace(streamID))
	if err != nil {
		return nil, err
	}
	records, err := s.cfg.PITRBases(ctx, stream.ID)
	if err != nil {
		return nil, fmt.Errorf("list base backups: %w", err)
	}
	bases, byID := pitrBases(records)
	sort.SliceStable(bases, func(i, j int) bool { return bases[i].TAfter.TS.Compare(bases[j].TAfter.TS) > 0 })
	var plan *pitr.RestorePlan
	var b2 pitr.Base
	var lastErr error
	for i := 0; i+1 < len(bases) && plan == nil; i++ {
		if !byID[bases[i].ID].HasManifest && byID[bases[i].ID].Manifest == nil {
			continue
		}
		b2 = bases[i]
		limit := pitr.Timestamp{T: b2.TAfter.TS.T, I: b2.TAfter.TS.I + 1}
		plan, lastErr = pitr.PlanRestore(ctx, stream, pitr.PlanSource{Repo: s.cfg.PITR, Bases: []pitr.Base{bases[i+1]},
			HasKey: s.cfg.PITRRestore.CanDecryptMode}, pitr.Target{TS: &limit})
	}
	if plan == nil {
		msg := "a chain test needs two eligible base backups in one window, the newer one with a manifest"
		if lastErr != nil {
			msg += ": " + strings.TrimPrefix(lastErr.Error(), "pitr: ")
		}
		return nil, public(msg, ErrNoChainTest, lastErr)
	}
	cloneID, err := models.NewCloneID()
	if err != nil {
		return nil, err
	}
	suffix, err := models.ChainTestCloneSuffix(s.now(), cloneID)
	if err != nil {
		return nil, err
	}
	// The test may run on another server (chain_test_connection_id), so production
	// is spared its load; it defaults to the stream's own connection.
	targetID := stream.ChainTestConnectionID
	if targetID == "" {
		targetID = stream.ConnectionID
	}
	conn, err := s.ResolveConnection(ctx, targetID)
	if err != nil {
		return nil, err
	}
	limit := plan.Limit
	// No force: the preflight (privileges, disk space) can refuse a chain test.
	req := models.RestoreRequest{
		PITR: &models.PITRTarget{StreamID: stream.ID, TS: &limit}, PITRCloneSuffix: suffix,
		TargetConnectionID: conn.ID, TargetConnectionName: conn.Name, MongoURI: conn.URI,
	}
	base := byID[plan.Base.ID]
	pp := &pitrPlan{req: req, stream: stream, chainTest: true,
		run: restore.PITRRun{Plan: plan, Base: base, Databases: base.InstanceDatabases}}
	expected := s.manifest(ctx, byID[b2.ID])
	return s.runPITRRestore(ctx, pp, func(runCtx context.Context, final *models.RestoreRecord) {
		s.finishChainTest(runCtx, final, b2.ID, expected, conn.URI)
	})
}

// finishChainTest compares the clones of a completed chain test with the manifest
// of base b2 (expected) and drops them.
func (s *Service) finishChainTest(ctx context.Context, rec *models.RestoreRecord, b2 string, expected *models.Manifest, uri string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), verifyTimeout)
	defer cancel()
	tracker := runs.FromContext(ctx)
	defer func() {
		note := s.cfg.PITRRestore.DropPITRClones(ctx, uri, rec.PITR)
		tracker.Printf("chain test clean-up%s", note)
		if strings.Contains(note, "failed") || strings.Contains(note, "manually") {
			addRecordWarning(rec, strings.TrimPrefix(note, "; "))
		}
	}()
	if rec.Status != models.RestoreStatusCompleted {
		return
	}
	v := &models.RestoreVerification{CheckedAt: s.now().UTC()}
	rec.Verification = v
	skip := func(note string) {
		v.Status, v.Notes = models.RestoreVerificationSkipped, append(v.Notes, note)
		addRecordWarning(rec, "chain test verification skipped: "+note)
	}
	switch {
	case expected == nil:
		skip("base " + b2 + " has no manifest")
		return
	case s.cfg.Inspector == nil:
		skip("the clones cannot be inspected in this setup")
		return
	}
	actual := &models.Manifest{CapturedAt: s.now().UTC()}
	seen := map[string]bool{}
	for _, c := range expected.Collections {
		db, _, ok := strings.Cut(c.Name, ".")
		if !ok || seen[db] {
			continue
		}
		seen[db] = true
		m, err := s.cfg.Inspector.Manifest(ctx, uri, db+rec.PITR.CloneSuffix)
		if err != nil {
			skip("could not inspect the clone of " + db + ": " + redact.Text(err.Error()))
			return
		}
		for _, mc := range m.Collections {
			mc.Name = db + "." + mc.Name
			actual.Collections = append(actual.Collections, mc)
		}
	}
	actual.Normalize()
	v.Mismatches, v.Notes, v.Collections = compareRestored(expected, actual, nil, false, false)
	v.CheckedAt = s.now().UTC()
	if len(v.Mismatches) == 0 {
		v.Status = models.RestoreVerificationPassed
		tracker.Printf("chain test passed: %d collection(s) match the manifest of base %s", v.Collections, b2)
		return
	}
	v.Status = models.RestoreVerificationFailed
	addRecordWarning(rec, fmt.Sprintf("chain test failed: %d mismatch(es) with the manifest of base %s: %s", len(v.Mismatches), b2, v.Mismatches[0]))
	s.logger.Warn("PITR chain test does not match the manifest of the newer base",
		logsafe.Attr("restore_id", rec.ID), logsafe.Attr("base_id", b2), slog.Int("mismatches", len(v.Mismatches)))
}

// ChainTestResult is the outcome of the newest chain test of a stream.
type ChainTestResult struct {
	// RestoreID is the chain test's restore.
	RestoreID string `json:"restore_id"`
	// StartedAt is when it started and DurationSeconds how long it took.
	StartedAt       time.Time `json:"started_at"`
	DurationSeconds float64   `json:"duration_seconds"`
	// Status is the restore's status and Verification the comparison's status
	// (empty when it did not run).
	Status       models.RestoreStatus             `json:"status"`
	Verification models.RestoreVerificationStatus `json:"verification,omitempty"`
	// Failed reports a chain test that failed or whose comparison failed.
	Failed bool `json:"failed"`
}

// LastChainTest returns the outcome of the newest finished chain test of stream
// streamID, or nil without one: while a new test runs, the previous result stays.
func (s *Service) LastChainTest(ctx context.Context, streamID string) *ChainTestResult {
	r := s.lastChainTest(ctx, streamID, func(r *models.RestoreRecord) bool {
		return r.Status != models.RestoreStatusInProgress && r.Status != models.RestoreStatusPending
	})
	if r == nil {
		return nil
	}
	out := &ChainTestResult{RestoreID: r.ID, StartedAt: r.StartedAt, DurationSeconds: r.DurationSeconds, Status: r.Status}
	if r.Verification != nil {
		out.Verification = r.Verification.Status
	}
	out.Failed = r.Status == models.RestoreStatusFailed || out.Verification == models.RestoreVerificationFailed
	return out
}

// LastChainTestStart returns when the newest chain test of streamID started (zero
// without one), for the chain test schedule.
func (s *Service) LastChainTestStart(ctx context.Context, streamID string) time.Time {
	if r := s.lastChainTest(ctx, streamID, nil); r != nil {
		return r.StartedAt
	}
	return time.Time{}
}

// CleanupInterruptedPITR drops the clones recorded on rec, a point-in-time restore
// or chain test that a stop interrupted (the startup sweep calls it before it marks
// the record failed), and returns a note for its message. It drops exactly the
// names on the record (PITRRestore.Clones), never a pattern: a chain test's clones
// are temporary, and a restore's are partial and cannot be trusted. It returns ""
// for any other record.
func (s *Service) CleanupInterruptedPITR(ctx context.Context, rec *models.RestoreRecord) string {
	if rec == nil || rec.PITR == nil || len(rec.PITR.Clones) == 0 || s.cfg.PITRRestore == nil {
		return ""
	}
	what := "partially restored clones"
	if rec.PITR.ChainTest {
		what = "chain test clones"
	}
	conn, err := s.ResolveConnection(ctx, rec.TargetConnectionID)
	if err != nil {
		return fmt.Sprintf("; the %s %s could not be dropped (%s), drop them manually",
			what, strings.Join(rec.PITR.Clones, ", "), redact.Text(err.Error()))
	}
	return s.cfg.PITRRestore.DropPITRClones(ctx, conn.URI, rec.PITR)
}
