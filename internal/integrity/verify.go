package integrity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/verify"
)

// Run keys of integrity work in the runs.Manager.
const (
	keyPrefixVerify      = "verify:"
	keyPrefixRestoreTest = "restore-test:"
	// keyPrefixRestoreTestDB locks a restore test's temporary database name.
	keyPrefixRestoreTestDB = "restore-test-db:"
	keyPrefixScan          = "scan:"
	keyPrefixImport        = "import:"
	keySweep               = "sweep"
)

// isIntegrityKey reports whether a runs.Manager key belongs to integrity work.
func isIntegrityKey(k string) bool {
	return k == keySweep || strings.HasPrefix(k, keyPrefixVerify) || strings.HasPrefix(k, keyPrefixRestoreTest) ||
		strings.HasPrefix(k, keyPrefixRestoreTestDB) || strings.HasPrefix(k, keyPrefixScan) || strings.HasPrefix(k, keyPrefixImport)
}

// stateSweep is the integrity state document of the sweep.
const stateSweep = "sweep"

// verifiable reports whether rec can be verified: a completed backup with a
// checksum and a storage key.
func verifiable(rec *models.BackupRecord) bool {
	return rec.Status == models.StatusCompleted && strings.TrimSpace(rec.SHA256) != "" && rec.StorageKey != ""
}

// notFound maps store.ErrNotFound to ErrNotFound with msg.
func notFound(err error, msg string) error {
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: %s", ErrNotFound, msg)
	}
	return err
}

// StartVerify checks that backup id can be verified and verifies it in the
// background (poll the backup record for verified_at and verification). It returns
// the record as it is now. Expected failures: ErrNotFound, ErrNotVerifiable, ErrBusy
// and runs.ErrShuttingDown.
func (s *Service) StartVerify(ctx context.Context, id string) (*models.BackupRecord, error) {
	rec, err := s.cfg.Store.GetBackupRecord(ctx, id)
	if err != nil {
		return nil, notFound(err, "backup not found")
	}
	if !verifiable(rec) {
		return nil, fmt.Errorf("%w; backup %s is %s", ErrNotVerifiable, rec.ID, rec.Status)
	}
	if err := s.cfg.Runs.Go(keyPrefixVerify+id, func(runCtx context.Context) {
		if _, err := s.Verify(runCtx, id, events.VerificationOnDemand, 0); err != nil {
			s.logger.Warn("on-demand verification did not run", slog.String("backup_id", id), slog.Any("error", err))
		}
	}); err != nil {
		return nil, err
	}
	return rec, nil
}

// Verify re-reads the archive of backup id now (at most bytesPerSecond, 0 =
// unlimited), records the outcome on the record and publishes a verification event.
// source is events.VerificationSweep or events.VerificationOnDemand. It returns the
// updated record; a mismatch is an outcome, not an error. Expected failures:
// ErrNotFound and ErrNotVerifiable.
func (s *Service) Verify(ctx context.Context, id, source string, bytesPerSecond int64) (*models.BackupRecord, error) {
	rec, err := s.cfg.Store.GetBackupRecord(ctx, id)
	if err != nil {
		return nil, notFound(err, "backup not found")
	}
	if !verifiable(rec) {
		return nil, fmt.Errorf("%w; backup %s is %s", ErrNotVerifiable, rec.ID, rec.Status)
	}
	var res verify.Result
	driver, err := s.cfg.Targets.Storage(ctx, rec.StorageTargetID)
	if err != nil {
		res = verify.Result{Status: models.VerificationError, At: s.now(), Err: fmt.Errorf("storage target %s: %w", rec.StorageTargetID, err)}
	} else {
		res = verify.Archive(ctx, driver, rec, verify.Options{Decryptor: s.verifyDecryptor(), BytesPerSecond: bytesPerSecond})
	}
	if ctx.Err() != nil && res.Status != models.VerificationOK {
		// A cancelled verification (shutdown) says nothing about the archive.
		return rec, fmt.Errorf("verification of %s cancelled: %w", id, ctx.Err())
	}
	// The outcome is written onto the stored record, so a pin or another change
	// made meanwhile is kept; a record deleted or pruned meanwhile is left alone.
	updated, err := s.cfg.Store.UpdateBackupRecord(context.WithoutCancel(ctx), id, func(r *models.BackupRecord) error {
		if r.Status != models.StatusCompleted || r.StorageKey != rec.StorageKey {
			return errRecordChanged
		}
		res.Apply(r)
		return nil
	})
	switch {
	case errors.Is(err, errRecordChanged), errors.Is(err, store.ErrNotFound):
		return rec, nil
	case err != nil:
		return nil, fmt.Errorf("record verification of %s: %w", id, err)
	}
	if e, ok := events.VerificationEvent(updated, source); ok {
		s.publish(ctx, e)
	}
	attrs := []any{slog.String("backup_id", id), slog.String("source", source), slog.String("result", string(res.Status)), slog.Int64("bytes", res.Bytes)}
	if res.Err != nil {
		attrs = append(attrs, slog.String("error", redact.Text(res.Err.Error())))
	}
	if res.Status == models.VerificationOK {
		s.logger.Info("backup archive verified", attrs...)
	} else {
		s.logger.Warn("backup archive verification failed", attrs...)
	}
	return updated, nil
}

// errRecordChanged aborts a record update whose record changed meanwhile.
var errRecordChanged = errors.New("integrity: the backup record changed meanwhile")

// SweepStatus is the state of the integrity sweep.
type SweepStatus struct {
	// Schedule is the integrity.sweep_schedule setting.
	Schedule string `json:"schedule"`
	// Running reports a sweep in progress.
	Running bool `json:"running"`
	// Trigger is "scheduled" or "manual".
	Trigger string `json:"trigger,omitempty"`
	// StartedAt and FinishedAt bound the latest sweep.
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// Total is the number of backups the sweep verifies; Done how many it has.
	Total int `json:"total"`
	Done  int `json:"done"`
	// OK, Mismatch and Errors count the outcomes.
	OK       int `json:"ok"`
	Mismatch int `json:"mismatch"`
	Errors   int `json:"errors"`
	// Current is the backup being verified.
	Current string `json:"current,omitempty"`
	// Interrupted is set when the sweep was stopped before it finished.
	Interrupted string `json:"interrupted,omitempty"`
	// NextRunAt is when the next scheduled sweep starts (nil when off).
	NextRunAt *time.Time `json:"next_run_at,omitempty"`
	// Chunks is the outcome of the PITR oplog chunk item (nil without streams or
	// when the sweep stopped before it).
	Chunks *ChunkSweep `json:"chunks,omitempty"`
}

// ChunkSweep is the outcome of the PITR chunk item of a sweep: the hash, the
// decrypted and gunzipped entries (count, first and last positions and terms) and
// the continuity of every chain are checked.
type ChunkSweep struct {
	// Verified counts the chunks checked and Failed those that did not match.
	Verified int `json:"verified"`
	Failed   int `json:"failed"`
	// Breaks counts the chunks that do not start where the previous one ends.
	Breaks int `json:"breaks"`
	// Error is why the chunk item stopped, if it did.
	Error string `json:"error,omitempty"`
}

// SweepOrder returns the backups a sweep verifies, in the order it verifies them:
// completed backups with a checksum, never verified ones first (oldest backup
// first), then by the time of their last verification (oldest first).
func SweepOrder(records []*models.BackupRecord) []*models.BackupRecord {
	out := make([]*models.BackupRecord, 0, len(records))
	for _, r := range records {
		if verifiable(r) {
			out = append(out, r)
		}
	}
	slices.SortStableFunc(out, func(a, b *models.BackupRecord) int {
		switch {
		case a.VerifiedAt == nil && b.VerifiedAt != nil:
			return -1
		case a.VerifiedAt != nil && b.VerifiedAt == nil:
			return 1
		case a.VerifiedAt != nil && !a.VerifiedAt.Equal(*b.VerifiedAt):
			return a.VerifiedAt.Compare(*b.VerifiedAt)
		default:
			if c := a.StartedAt.Compare(b.StartedAt); c != 0 {
				return c
			}
			return strings.Compare(a.ID, b.ID)
		}
	})
	return out
}

// StartSweep starts a sweep in the background. Expected failures: ErrBusy and
// runs.ErrShuttingDown.
func (s *Service) StartSweep(ctx context.Context) (SweepStatus, error) {
	if !s.sweepMu.TryLock() {
		return s.SweepStatus(ctx), fmt.Errorf("%w: an integrity sweep is already running", ErrBusy)
	}
	s.sweepMu.Unlock()
	err := s.cfg.Runs.Go(keySweep, func(runCtx context.Context) {
		if _, err := s.Sweep(runCtx, TriggerManual); err != nil && !errors.Is(err, ErrBusy) && runCtx.Err() == nil {
			s.logger.Warn("integrity sweep failed", slog.Any("error", err))
		}
	})
	return s.SweepStatus(ctx), err
}

// Sweep verifies every completed backup in SweepOrder, one at a time, at the rate
// the integrity.sweep_bandwidth_limit setting allows, and returns the final status.
// It returns ErrBusy while another sweep runs. Cancelling ctx stops it between (or
// during) archives; the status then says it was interrupted.
func (s *Service) Sweep(ctx context.Context, trigger string) (SweepStatus, error) {
	if !s.sweepMu.TryLock() {
		return s.SweepStatus(ctx), fmt.Errorf("%w: an integrity sweep is already running", ErrBusy)
	}
	defer s.sweepMu.Unlock()

	records, err := s.cfg.Store.ListBackupRecords(ctx, "")
	if err != nil {
		return s.SweepStatus(ctx), fmt.Errorf("list backups: %w", err)
	}
	order := SweepOrder(records)
	started := s.now()
	st := &SweepStatus{Running: true, Trigger: trigger, StartedAt: &started, Total: len(order)}
	s.setSweep(ctx, st)
	s.logger.Info("integrity sweep started", slog.String("trigger", trigger), slog.Int("backups", len(order)))

	rate := s.settings().Integrity.SweepBytesPerSecond()
	for _, rec := range order {
		if ctx.Err() != nil {
			break
		}
		s.mu.Lock()
		st.Current = rec.ID
		s.mu.Unlock()
		updated, err := s.Verify(ctx, rec.ID, events.VerificationSweep, rate)
		s.mu.Lock()
		switch {
		case ctx.Err() != nil:
		case err != nil:
			// The backup changed (deleted, pruned) since the sweep listed it.
		case updated.Verification == models.VerificationOK:
			st.OK++
		case updated.Verification == models.VerificationMismatch:
			st.Mismatch++
		default:
			st.Errors++
		}
		if ctx.Err() == nil {
			st.Done++
		}
		s.mu.Unlock()
		s.saveSweep(ctx, st)
	}

	if s.cfg.VerifyChunks != nil && ctx.Err() == nil {
		s.mu.Lock()
		st.Current = "oplog chunks"
		s.mu.Unlock()
		chunks, err := s.cfg.VerifyChunks(ctx)
		if err != nil && ctx.Err() == nil {
			chunks.Error = redact.Text(err.Error())
			s.logger.Warn("the oplog chunk item of the integrity sweep failed", slog.String("error", chunks.Error))
		}
		if ctx.Err() == nil {
			s.mu.Lock()
			st.Chunks = &chunks
			s.mu.Unlock()
		}
	}

	finished := s.now()
	s.mu.Lock()
	st.Running, st.Current, st.FinishedAt = false, "", &finished
	if ctx.Err() != nil {
		st.Interrupted = "stopped before it finished: " + ctx.Err().Error()
	}
	final := *st
	s.sweep = nil
	s.mu.Unlock()
	s.saveSweep(ctx, &final)
	s.logger.Info("integrity sweep finished", slog.Int("verified", final.Done), slog.Int("ok", final.OK),
		slog.Int("mismatch", final.Mismatch), slog.Int("errors", final.Errors), slog.String("interrupted", final.Interrupted))
	final.Schedule, final.NextRunAt = s.sweepSchedule(&final)
	return final, ctx.Err()
}

// setSweep publishes st as the live status and persists it.
func (s *Service) setSweep(ctx context.Context, st *SweepStatus) {
	s.mu.Lock()
	s.sweep = st
	s.mu.Unlock()
	s.saveSweep(ctx, st)
}

// saveSweep persists a copy of st, detached from ctx's cancellation.
func (s *Service) saveSweep(ctx context.Context, st *SweepStatus) {
	s.mu.Lock()
	snapshot := *st
	s.mu.Unlock()
	snapshot.Schedule, snapshot.NextRunAt = "", nil
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.cfg.Store.SaveIntegrityState(writeCtx, stateSweep, snapshot); err != nil {
		s.logger.Warn("failed to save the integrity sweep status", slog.Any("error", err))
	}
}

// SweepStatus returns the live status of a running sweep, or the stored status of
// the latest one, with the schedule and the next run.
func (s *Service) SweepStatus(ctx context.Context) SweepStatus {
	s.mu.Lock()
	live := s.sweep
	var st SweepStatus
	if live != nil {
		st = *live
	}
	s.mu.Unlock()
	if live == nil {
		if _, err := s.cfg.Store.LoadIntegrityState(ctx, stateSweep, &st); err != nil {
			s.logger.Warn("failed to load the integrity sweep status", slog.Any("error", err))
		}
		st.Running = false
	}
	st.Schedule, st.NextRunAt = s.sweepSchedule(&st)
	return st
}

// sweepSchedule returns the schedule setting and the next scheduled sweep after st.
func (s *Service) sweepSchedule(st *SweepStatus) (string, *time.Time) {
	schedule := s.settings().Integrity.SweepSchedule
	interval := schedule.Interval()
	if interval <= 0 {
		return string(schedule), nil
	}
	next := s.now()
	if st.StartedAt != nil {
		if due := st.StartedAt.Add(interval); due.After(next) {
			next = due
		}
	}
	return string(schedule), &next
}

// nextSweep returns when the next scheduled sweep is due, or nil when it is off.
func (s *Service) nextSweep(ctx context.Context) *time.Time {
	st := s.SweepStatus(ctx)
	return st.NextRunAt
}

// recoverInterrupted marks a sweep that was running when the process stopped as
// interrupted.
func (s *Service) recoverInterrupted(ctx context.Context) {
	var st SweepStatus
	ok, err := s.cfg.Store.LoadIntegrityState(ctx, stateSweep, &st)
	if err != nil || !ok || !st.Running {
		return
	}
	st.Running, st.Current, st.Interrupted = false, "", "interrupted: the server stopped before the sweep finished"
	s.saveSweep(ctx, &st)
}
