package operations

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotools"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
)

// ErrPITRNotRestorable is returned for a point in time that cannot be restored:
// outside every window, behind a break of the oplog chain or a chunk that failed
// verification. The message says which.
var ErrPITRNotRestorable = errors.New("the point in time cannot be restored")

// PITRRestorer runs point-in-time restores (implemented by *restore.Engine).
type PITRRestorer interface {
	// CanDecryptMode reports whether a key of an encryption mode is configured.
	CanDecryptMode(mode string) bool
	// PreparePITR validates req against run and returns the in-progress record.
	PreparePITR(req models.RestoreRequest, run restore.PITRRun) (*models.RestoreRecord, error)
	// ExecutePITR runs the restore described by req, run and record.
	ExecutePITR(ctx context.Context, req models.RestoreRequest, run restore.PITRRun, record *models.RestoreRecord) (*models.RestoreRecord, error)
	// DropPITRClones drops the clones of a point-in-time restore and returns a note
	// for its record.
	DropPITRClones(ctx context.Context, uri string, info *models.PITRRestore) string
}

// pitrPlan is a validated point-in-time request, resolved to its stream and target
// connection, and the plan that restores it.
type pitrPlan struct {
	req    models.RestoreRequest
	stream *pitr.Stream
	run    restore.PITRRun
	// planErr is why no plan was found (preflights only).
	planErr error
	// cloneErr is why the clone names do not fit (preflights only).
	cloneErr error
	// chainTest marks the restore of a chain test.
	chainTest bool
	// fallback explains why the restore reads a copy chain instead of the
	// primary (see pitrSource).
	fallback string
}

// resolveStream returns the PITR stream id, or the stream of connection id. A
// stream belongs to its connection: one the caller in ctx may not touch is not
// found, exactly like one that does not exist.
func (s *Service) resolveStream(ctx context.Context, id string) (*pitr.Stream, error) {
	st, err := s.cfg.PITR.GetStream(ctx, id)
	if errors.Is(err, pitr.ErrNotFound) {
		st, err = s.cfg.PITR.GetStreamByConnection(ctx, id)
	}
	switch {
	case errors.Is(err, pitr.ErrNotFound):
		return nil, public("PITR stream not found", ErrNotFound, err)
	case err != nil:
		return nil, fmt.Errorf("load PITR stream: %w", err)
	case !auth.ConnectionAllowed(ctx, st.ConnectionID):
		return nil, public("PITR stream not found", ErrNotFound)
	}
	return st, nil
}

// visibleStreamFirst answers a caller limited to some connections ErrNotFound for
// a stream outside them before any other check, so it learns nothing about the
// stream (not even that restoring it needs the admin role). Callers that may touch
// every connection get nil: their checks run in the usual order.
func (s *Service) visibleStreamFirst(ctx context.Context, id string) error {
	if !auth.ConnectionFilter(ctx).Limited() || s.cfg.PITR == nil {
		return nil
	}
	_, err := s.resolveStream(ctx, strings.TrimSpace(id))
	return err
}

// pitrBases returns the bases of records that can start a point-in-time restore
// (completed instance-scope backups with T_before and T_after, or missing ones
// with a usable copy, which a restore reads instead, see pitrSource and
// planWithUsableBase), and the records by ID.
func pitrBases(records []*models.BackupRecord) ([]pitr.Base, map[string]*models.BackupRecord) {
	var out []pitr.Base
	byID := map[string]*models.BackupRecord{}
	for _, r := range records {
		usable := r.Status == models.StatusCompleted || (r.Status == models.StatusMissing && hasUsableCopy(r))
		if !usable || !r.InstanceScope() || r.TBefore == nil || r.TAfter == nil {
			continue
		}
		byID[r.ID] = r
		out = append(out, pitr.Base{ID: r.ID, TBefore: *r.TBefore, TAfter: *r.TAfter, SizeBytes: r.SizeBytes,
			Encrypted: r.Encrypted, EncryptionMode: r.EncryptionMode, StartedAt: r.StartedAt})
	}
	return out, byID
}

// pitrPlanError maps a refusal of pitr.PlanRestore to an operations error.
func pitrPlanError(err error) error {
	msg := strings.TrimPrefix(err.Error(), "pitr: ")
	switch {
	case errors.Is(err, pitr.ErrInvalidTarget):
		return public(msg, ErrInvalid, err)
	case errors.Is(err, pitr.ErrKeyMissing):
		return fmt.Errorf("%w: %s; %s", ErrKeyRequired, msg, restore.KeyRequiredHint)
	case errors.Is(err, pitr.ErrOutsideWindow), errors.Is(err, pitr.ErrChainBreak), errors.Is(err, pitr.ErrChunkFailed):
		return public(msg, ErrPITRNotRestorable, err)
	}
	return fmt.Errorf("plan the point-in-time restore: %w", err)
}

// planPITR validates a point-in-time request, which needs the admin scope, loads
// its stream (by stream or connection ID), plans the restore and resolves the
// target connection (the stream's unless req names another). A preflight
// (forPreflight) keeps a plan refusal in planErr instead of failing.
func (s *Service) planPITR(ctx context.Context, req models.RestoreRequest, forPreflight bool) (*pitrPlan, error) {
	if req.PITR != nil {
		if err := s.visibleStreamFirst(ctx, req.PITR.StreamID); err != nil {
			return nil, err
		}
	}
	if err := auth.RequireScope(ctx, auth.ScopeAdmin); err != nil {
		return nil, fmt.Errorf("point-in-time restores need the admin role or an admin API key: %w", err)
	}
	if err := req.ValidatePITR(); err != nil {
		return nil, invalid(err)
	}
	if s.cfg.PITR == nil || s.cfg.PITRRestore == nil || s.cfg.PITRBases == nil {
		return nil, ErrPITRUnavailable
	}
	stream, err := s.resolveStream(ctx, strings.TrimSpace(req.PITR.StreamID))
	if err != nil {
		return nil, err
	}
	target := *req.PITR
	target.StreamID = stream.ID
	req.PITR = &target
	records, err := s.cfg.PITRBases(ctx, stream.ID)
	if err != nil {
		return nil, fmt.Errorf("list base backups: %w", err)
	}
	bases, byID := pitrBases(records)
	out := &pitrPlan{stream: stream}
	plan, err := s.planWithUsableBase(ctx, stream, bases, byID, target.Target(), req.SourceTargetID)
	switch {
	case err != nil && (!forPreflight || errors.Is(err, pitr.ErrInvalidTarget)):
		return nil, pitrPlanError(err)
	case err != nil:
		out.planErr = pitrPlanError(err)
	default:
		base := byID[plan.Base.ID]
		out.run = restore.PITRRun{Plan: plan, Base: base, Databases: base.InstanceDatabases}
		if missing := missingDatabases(base.InstanceDatabases, req.PITRDatabases()); len(missing) > 0 {
			return nil, public(fmt.Sprintf("databases: %s not in base backup %s; restore the whole instance to get databases created after the base",
				strings.Join(missing, ", "), base.ID), ErrInvalid)
		}
		fallback, srcErr := s.pitrSource(ctx, out, req.SourceTargetID)
		switch {
		case srcErr != nil && !forPreflight:
			return nil, srcErr
		case srcErr != nil:
			out.planErr = srcErr
		}
		out.fallback = fallback
	}

	targetID := req.TargetConnectionID
	if targetID == "" {
		targetID = stream.ConnectionID
	}
	conn, err := s.ResolveConnection(ctx, targetID)
	if err != nil {
		return nil, err
	}
	req.TargetConnectionID, req.TargetConnectionName, req.MongoURI, req.MongoTLS = conn.ID, conn.Name, conn.URI, conn.TLS()
	req.PostRestoreCommands = models.ClonePostRestoreCommands(conn.PostRestoreCommands)
	out.req = req
	return out, nil
}

// missingDatabases returns the entries of selected that the base's database list
// does not hold; nothing when the base has no list (an earlier release).
func missingDatabases(base, selected []string) []string {
	if base == nil {
		return nil
	}
	var out []string
	for _, db := range selected {
		if !slices.Contains(base, db) {
			out = append(out, db)
		}
	}
	return out
}

// startPITRRestore is StartRestore for a point-in-time request (req.PITR): an
// admin-only restore into safe clones, refused in place (models.ErrPITRInPlace).
// Since it never overwrites data, the two-person rule does not hold it back.
func (s *Service) startPITRRestore(ctx context.Context, req models.RestoreRequest) (*models.RestoreRecord, error) {
	pp, err := s.planPITR(ctx, req, false)
	if err != nil {
		return nil, err
	}
	return s.runPITRRestore(ctx, pp, func(context.Context, *models.RestoreRecord) {})
}

// runPITRRestore prepares, preflights, stores and starts the restore of pp in the
// background; done is called with the final record before it is stored.
func (s *Service) runPITRRestore(ctx context.Context, pp *pitrPlan, done func(context.Context, *models.RestoreRecord)) (*models.RestoreRecord, error) {
	req := pp.req
	record, err := s.cfg.PITRRestore.PreparePITR(req, pp.run)
	if err != nil {
		return nil, public(redact.Text(err.Error()), ErrInvalid, err)
	}
	record.PITR.BaseBytes, record.PITR.ChainTest = pp.run.Base.SizeBytes, pp.chainTest
	record.SourceTargetID, record.SourceTargetName, record.SourceFallback = pp.run.Base.StorageTargetID, pp.run.Base.StorageTargetName, pp.fallback
	if s.cfg.Inspector != nil {
		pre := s.preflightPITR(ctx, pp, record)
		if !pre.OK && !req.Force {
			return nil, &PreflightError{Result: pre}
		}
		record.Preflight, record.Forced = pre, !pre.OK && req.Force
	}
	record.SourceConnectionID = pp.stream.ConnectionID
	if s.cfg.Connections != nil {
		if src, getErr := s.cfg.Connections.Get(ctx, pp.stream.ConnectionID); getErr == nil {
			record.SourceConnectionName = src.Name
		}
	}
	release, err := s.cfg.Runs.Acquire(runs.RestoreKey(record.TargetConnectionID, record.TargetDatabase))
	if err != nil {
		return nil, runError(err, "a restore into "+record.TargetDatabase+" is already running")
	}
	snapshot := *record
	if err := s.cfg.Store.SaveRestoreRecord(ctx, &snapshot); err != nil {
		release()
		return nil, s.startSaveError(ctx, "restore", err)
	}
	tracked := s.track(models.RunRestore, record.ID, "", record.TargetDatabase)
	// The clones a restore discovers as it runs are stored before they are written
	// to, so a restart can always drop exactly what an interrupted restore created.
	run := pp.run
	run.OnRecord = func(r *models.RestoreRecord) {
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
		defer cancel()
		if saveErr := s.cfg.Store.SaveRestoreRecord(saveCtx, r); saveErr != nil {
			s.cfg.DiskGuard.Observe(saveCtx, saveErr)
			s.logger.Error("failed to record the clones of a point-in-time restore",
				logsafe.Attr("restore_id", r.ID), logsafe.Error(saveErr))
		}
	}
	if err := s.cfg.Runs.Go("", func(runCtx context.Context) {
		defer release()
		defer tracked.End()
		runCtx = tracked.Bind(runCtx)
		final, runErr := s.cfg.PITRRestore.ExecutePITR(runCtx, req, run, record)
		done(runCtx, final)
		// Stored first, then published (see finishRestore).
		s.finishRestore(runCtx, final, runErr, final.BackupID, false)
	}); err != nil {
		tracked.End()
		release()
		snapshot.Status = models.RestoreStatusFailed
		snapshot.ErrorMessage = "restore not started: " + err.Error()
		if saveErr := s.cfg.Store.SaveRestoreRecord(context.WithoutCancel(ctx), &snapshot); saveErr != nil {
			s.logger.Error("failed to persist abandoned restore record", logsafe.Attr("restore_id", snapshot.ID), logsafe.Error(saveErr))
		}
		return nil, runError(err, "")
	}
	return &snapshot, nil
}

// preflightPITREndpoint is PreflightRestore for a point-in-time request: a plan
// refusal fails the pitr_chain check instead of the call.
func (s *Service) preflightPITREndpoint(ctx context.Context, req models.RestoreRequest) (*models.PreflightResult, error) {
	pp, err := s.planPITR(ctx, req, true)
	if err != nil {
		return nil, err
	}
	var record *models.RestoreRecord
	if pp.planErr == nil {
		record, err = s.cfg.PITRRestore.PreparePITR(pp.req, pp.run)
		switch {
		case errors.Is(err, restore.ErrCloneNameTooLong):
			// Reported by the target_database check.
			pp.cloneErr, record = err, nil
		case err != nil:
			return nil, public(redact.Text(err.Error()), ErrInvalid, err)
		default:
			record.PITR.BaseBytes = pp.run.Base.SizeBytes
		}
	}
	return s.preflightPITR(ctx, pp, record), nil
}

// Default rates of the RTO estimate without a chain test: restoring a base archive
// and replaying the oplog, which mongorestore does on one thread.
const (
	defaultBaseRate   = 50 << 20 // bytes per second
	defaultReplayRate = 4 << 20  // bytes per second
)

// estimatePITR returns the estimated duration of restoring baseBytes and replaying
// oplogBytes, from the rate of the stream's newest completed chain test, or from
// the default rates.
func (s *Service) estimatePITR(ctx context.Context, streamID string, baseBytes, oplogBytes int64) (float64, string) {
	completed := func(r *models.RestoreRecord) bool { return r.Status == models.RestoreStatusCompleted }
	if test := s.lastChainTest(ctx, streamID, completed); test != nil && test.DurationSeconds > 0 {
		if bytes := test.PITR.BaseBytes + test.PITR.OplogBytes; bytes > 0 {
			rate := float64(bytes) / test.DurationSeconds
			return float64(baseBytes+oplogBytes) / rate, "chain_test"
		}
	}
	return float64(baseBytes)/defaultBaseRate + float64(oplogBytes)/defaultReplayRate, "default"
}

// lastChainTest returns the newest chain test restore of streamID that accept takes
// (every one when accept is nil), or nil.
func (s *Service) lastChainTest(ctx context.Context, streamID string, accept func(*models.RestoreRecord) bool) *models.RestoreRecord {
	recs, err := s.store.ListRestoreRecords(ctx)
	if err != nil {
		return nil
	}
	for _, r := range recs {
		if r.PITR == nil || !r.PITR.ChainTest || r.PITR.StreamID != streamID {
			continue
		}
		if accept != nil && !accept(r) {
			continue
		}
		return r
	}
	return nil
}

// preflightPITR runs the checks of a point-in-time restore: the plan (pitr_chain),
// the connection, the server versions, the clone names, the privileges replay
// needs, the disk space for the base and the oplog, and the tools version. record
// is nil when no plan was found.
func (s *Service) preflightPITR(ctx context.Context, pp *pitrPlan, record *models.RestoreRecord) *models.PreflightResult {
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()
	targetDB := models.AdminDatabase
	if record != nil {
		if dbs := record.PITR.Databases; len(dbs) > 0 {
			targetDB = dbs[0] + record.PITR.CloneSuffix
		}
	}
	p := s.newPreflightRun(ctx, pp.req, pp.run.Base, targetDB)
	defer p.close()
	p.pitrChain(pp, record)
	p.connection()
	if p.source != nil {
		p.serverVersion()
	}
	switch {
	case pp.cloneErr != nil:
		p.res.Add(models.PreflightCheckTargetDatabase, models.PreflightFail, strings.TrimPrefix(pp.cloneErr.Error(), "restore: "))
	case record != nil:
		p.pitrClones(record.PITR)
	}
	if record != nil {
		p.postRestore(pitrPreflightClones(record.PITR))
	}
	p.pitrPrivileges()
	if p.source != nil {
		p.extraBytes = pp.run.Plan.OplogBytes
		p.diskSpace()
	}
	p.toolsVersion()
	return p.res
}

func (p *preflightRun) pitrChain(pp *pitrPlan, record *models.RestoreRecord) {
	const id = models.PreflightCheckPITRChain
	if pp.planErr != nil {
		p.res.Add(id, models.PreflightFail, pp.planErr.Error())
		return
	}
	plan := pp.run.Plan
	msg := fmt.Sprintf("base backup %s (consistent at %s) and %d oplog chunk(s) (%s) reach %s without a break",
		plan.Base.ID, plan.Base.TAfter.TS.Time().Format(time.RFC3339), plan.ChunkCount, formatBytes(plan.OplogBytes),
		plan.TargetTime.Format(time.RFC3339))
	if plan.UnverifiedChunks > 0 {
		msg += fmt.Sprintf("; %d chunk(s) not verified yet are checked while they stream", plan.UnverifiedChunks)
	}
	p.res.Add(id, models.PreflightPass, msg)
	if record == nil {
		return
	}
	secs, from := p.svc.estimatePITR(p.ctx, pp.stream.ID, plan.Base.SizeBytes, plan.OplogBytes)
	p.res.PITR = &models.PITRPreflight{
		BaseID: plan.Base.ID, BaseStartedAt: plan.Base.StartedAt, BaseConsistentAt: plan.Base.TAfter.TS.Time(),
		BaseBytes: plan.Base.SizeBytes, OplogBytes: plan.OplogBytes,
		Chunks: plan.ChunkCount, UnverifiedChunks: plan.UnverifiedChunks,
		TargetTime: plan.TargetTime, Limit: fmt.Sprintf("%d:%d", plan.Limit.T, plan.Limit.I),
		CloneSuffix: record.PITR.CloneSuffix, EstimatedSeconds: secs, EstimateFrom: from,
	}
}

func (p *preflightRun) pitrClones(info *models.PITRRestore) {
	const id = models.PreflightCheckTargetDatabase
	if len(info.Clones) == 0 {
		p.res.Add(id, models.PreflightWarn,
			"the base backup lists no databases (taken by an earlier release): the clone names cannot be checked in advance")
		return
	}
	if p.skipServer(id) {
		return
	}
	var clones []string
	for _, clone := range info.Clones {
		exists, err := p.target.DatabaseExists(p.ctx, clone)
		if err != nil {
			p.res.Add(id, models.PreflightWarn, fmt.Sprintf("could not check whether database %s exists: %s", clone, errText(err)))
			return
		}
		if exists {
			p.res.Add(id, models.PreflightFail, fmt.Sprintf("the safe clone database %s already exists; retry in a moment", clone))
			return
		}
		clones = append(clones, clone)
	}
	p.res.Add(id, models.PreflightPass, fmt.Sprintf("restores into the new database(s) %s; existing data is untouched", listNames(clones)))
}

// pitrRoles are the roles oplog replay needs together (in admin), unless the user
// holds anyAction.
var pitrRoles = []string{"restore", "readWriteAnyDatabase", "dbAdminAnyDatabase"}

// knownBuiltinRoles are the built-in roles; any other role may be a custom one
// that grants what replay needs.
var knownBuiltinRoles = []string{
	"read", "readWrite", "dbAdmin", "dbOwner", "userAdmin",
	"readAnyDatabase", "readWriteAnyDatabase", "userAdminAnyDatabase", "dbAdminAnyDatabase",
	"clusterAdmin", "clusterManager", "clusterMonitor", "hostManager", "backup", "restore",
	"root", "__system", "enableSharding", "directShardOperations", "__queryableBackup", "searchCoordinator",
}

// pitrRoleCheck evaluates the roles of the target's user for oplog replay: the
// missing roles, and whether a custom role might still grant them.
func pitrRoleCheck(u connections.UserRoles) (missing []string, custom bool) {
	if !u.AuthEnabled || u.AnyAction {
		return nil, false
	}
	held := map[string]bool{}
	for _, r := range u.Roles {
		if r.DB == models.AdminDatabase {
			held[r.Role] = true
		}
		if !slices.Contains(knownBuiltinRoles, r.Role) {
			custom = true
		}
	}
	if held["root"] || held["__system"] {
		return nil, false
	}
	for _, r := range pitrRoles {
		if !held[r] {
			missing = append(missing, r)
		}
	}
	return missing, custom
}

func (p *preflightRun) pitrPrivileges() {
	const id = models.PreflightCheckPrivileges
	if p.skipServer(id) {
		return
	}
	reporter, ok := p.target.(connections.RoleReporter)
	if !ok {
		p.res.Add(id, models.PreflightWarn, "not checked: the roles of the target connection's user cannot be read in this setup")
		return
	}
	u, err := reporter.UserRoles(p.ctx)
	if err != nil {
		p.res.Add(id, models.PreflightWarn, "could not read the roles of the target connection's user: "+errText(err))
		return
	}
	missing, custom := pitrRoleCheck(u)
	need := "restore, readWriteAnyDatabase and dbAdminAnyDatabase on admin (or anyAction)"
	switch {
	case len(missing) == 0:
		p.res.Add(id, models.PreflightPass, fmt.Sprintf("the user of connection %s may replay the oplog (%s)", p.connectionName(), need))
	case custom:
		p.res.Add(id, models.PreflightWarn, fmt.Sprintf(
			"the user of connection %s lacks the built-in role(s) %s; replaying the oplog needs %s, which its custom roles may grant",
			p.connectionName(), strings.Join(missing, ", "), need))
	default:
		p.res.Add(id, models.PreflightFail, fmt.Sprintf(
			"the user of connection %s lacks %s; replaying the oplog checks privileges per operation and needs %s",
			p.connectionName(), strings.Join(missing, ", "), need))
	}
}

func (p *preflightRun) toolsVersion() {
	const id = models.PreflightCheckToolsVersion
	fn := p.svc.cfg.ToolsVersion
	if fn == nil {
		p.res.Add(id, models.PreflightWarn, "not checked: the mongorestore version cannot be read in this setup")
		return
	}
	v, err := fn(p.ctx)
	switch {
	case errors.Is(err, mongotools.ErrToolNotFound):
		p.res.Add(id, models.PreflightFail, "mongorestore was not found; install MongoDB Database Tools "+mongotools.MinPITRToolsVersion+" or newer")
	case err != nil:
		p.res.Add(id, models.PreflightWarn, "could not read the mongorestore version: "+errText(err))
	case !mongotools.VersionAtLeast(v, mongotools.MinPITRToolsVersion):
		p.res.Add(id, models.PreflightFail, fmt.Sprintf("mongorestore %s is older than %s, the oldest version point-in-time restores are tested with", v, mongotools.MinPITRToolsVersion))
	default:
		p.res.Add(id, models.PreflightPass, fmt.Sprintf("mongorestore %s (%s or newer is needed)", v, mongotools.MinPITRToolsVersion))
	}
}
