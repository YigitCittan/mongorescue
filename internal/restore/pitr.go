package restore

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotools"
	"github.com/yigitcittan/mongorescue/internal/oplog"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// Errors of point-in-time restores.
var (
	// ErrOpCountMismatch indicates that mongorestore reported another number of
	// applied oplog operations than the oplog filter wrote: the clones are kept for
	// inspection but must not be trusted.
	ErrOpCountMismatch = errors.New("restore: the oplog replay applied another number of operations than were written")

	// ErrNoServerVersion indicates that the base backup records no MongoDB version,
	// which the synthetic oplog archive needs.
	ErrNoServerVersion = errors.New("restore: the base backup records no MongoDB server version")
)

// PITRRun is a planned point-in-time restore: the plan of pitr.PlanRestore and the
// record of its base backup.
type PITRRun struct {
	// Plan is the base, the chunks and the limit to replay.
	Plan *pitr.RestorePlan
	// Base is the record of Plan.Base.
	Base *models.BackupRecord
	// Databases are the databases of the base (from its instance manifest), or nil
	// when it has no manifest. A whole-instance restore plans its clone names from
	// them and leaves MongoRescue's own clones among them out of pass 1.
	Databases []string
	// OnRecord, when set, is called with a copy of the record whenever the restore
	// records more clone databases while it runs (a database created in the window,
	// or one the base held beyond its manifest), so the caller can store them before
	// they are written to.
	OnRecord func(rec *models.RestoreRecord)
}

// ErrCloneNameTooLong is returned by PreparePITR when a database name plus the clone
// suffix exceeds MongoDB's limit of models.MaxDatabaseNameLength bytes.
var ErrCloneNameTooLong = errors.New("restore: a clone database name would be too long")

// PITRClones returns the clone names a point-in-time restore of run with
// databases (none: the whole instance) and suffix creates: each selected database,
// or each database of the base except admin, config, local and MongoRescue's own
// clones. known is false for a whole instance whose base has no manifest. It fails
// with ErrCloneNameTooLong naming every name that does not fit.
func PITRClones(run PITRRun, databases []string, suffix string) (clones []string, known bool, err error) {
	sources := databases
	if len(sources) == 0 {
		if run.Databases == nil {
			return nil, false, nil
		}
		for _, db := range run.Databases {
			if db != models.AdminDatabase && db != "config" && db != "local" && !models.IsRescueClone(db) {
				sources = append(sources, db)
			}
		}
	}
	var long []string
	for _, db := range sources {
		clone := db + suffix
		if len(clone) > models.MaxDatabaseNameLength {
			long = append(long, fmt.Sprintf("%s (%d bytes)", clone, len(clone)))
		}
		clones = append(clones, clone)
	}
	if len(long) > 0 {
		return nil, true, fmt.Errorf("%w: %s exceed the %d bytes MongoDB allows; restore those databases from a database backup instead",
			ErrCloneNameTooLong, strings.Join(long, ", "), models.MaxDatabaseNameLength)
	}
	return clones, true, nil
}

// DatabaseLister lists the database names on the server at uri. A whole-instance
// point-in-time restore uses it to refuse clone names that exist already and to
// record clones the base held beyond its manifest.
type DatabaseLister func(ctx context.Context, uri string) ([]string, error)

// WithDatabaseLister sets the lister of whole-instance point-in-time restores.
func WithDatabaseLister(fn DatabaseLister) Option {
	return func(e *Engine) {
		e.listDatabases = fn
	}
}

// CanDecryptMode reports whether a decryption key of encryption mode ("x25519" or
// "scrypt") is configured.
func (e *Engine) CanDecryptMode(mode string) bool {
	return e.runConfig().Decryptor.HasMode(encryption.Mode(mode))
}

// pitrIDPrefix starts the ID of a point-in-time restore.
const pitrIDPrefix = "rst_pitr_"

// PreparePITR validates a point-in-time request (models.RestoreRequest.ValidatePITR)
// against run and returns the in-progress record, without any I/O. Every restored
// database goes into a clone named <db><suffix>, the suffix being
// models.RescueCloneSuffix of the start time and a random clone ID unless
// req.PITRCloneSuffix is set. The planned clone names are recorded (PITRRestore
// .Clones) before anything is written; a name that does not fit fails with
// ErrCloneNameTooLong.
func (e *Engine) PreparePITR(req models.RestoreRequest, run PITRRun) (*models.RestoreRecord, error) {
	if err := req.ValidatePITR(); err != nil {
		return nil, fmt.Errorf("restore: %w", err)
	}
	if req.PITR == nil {
		return nil, errors.New("restore: not a point-in-time request")
	}
	if run.Plan == nil || run.Base == nil || run.Base.ID != run.Plan.Base.ID {
		return nil, errors.New("restore: point-in-time restore without a plan and its base backup")
	}
	if e.resolveURI(req) == "" {
		return nil, errors.New("restore: mongo connection uri is required")
	}
	start := e.now().UTC()
	suffix := req.PITRCloneSuffix
	if suffix == "" {
		cloneID, err := e.newCloneID()
		if err != nil {
			return nil, fmt.Errorf("restore: %w", err)
		}
		if suffix, err = models.RescueCloneSuffix(start, cloneID); err != nil {
			return nil, fmt.Errorf("restore: %w", err)
		}
	}
	dbs := req.PITRDatabases()
	clones, _, err := PITRClones(run, dbs, suffix)
	if err != nil {
		return nil, err
	}
	idSuffix, err := models.NewIDSuffix()
	if err != nil {
		return nil, fmt.Errorf("restore: %w", err)
	}
	source, target := "*", "*"+suffix
	if len(dbs) > 0 {
		source, target = strings.Join(dbs, ","), strings.Join(clones, ",")
	}
	plan := run.Plan
	return &models.RestoreRecord{
		ID:                   fmt.Sprintf("%s%s_%s", pitrIDPrefix, start.Format("20060102_150405"), idSuffix),
		BackupID:             run.Base.ID,
		SourceDatabase:       source,
		TargetDatabase:       target,
		TargetConnectionID:   req.TargetConnectionID,
		TargetConnectionName: req.TargetConnectionName,
		Status:               models.RestoreStatusInProgress,
		StartedAt:            start,
		Phases:               models.RunPhases{Queued: models.Stamp(start)},
		PITR: &models.PITRRestore{
			StreamID: plan.StreamID, ChainID: plan.ChainID, BaseID: plan.Base.ID,
			TargetTime: plan.TargetTime, Limit: plan.Limit,
			Chunks: plan.ChunkCount, OplogBytes: plan.OplogBytes,
			Databases: dbs, CloneSuffix: suffix, Clones: clones,
		},
	}, nil
}

// ExecutePITR runs the point-in-time restore prepared by PreparePITR in two passes,
// both streamed from storage into mongorestore without temporary copies:
//
//  1. The base archive is restored without --oplogReplay into the clones: a whole
//     instance with --nsFrom '$db$.$coll$' --nsTo '$db$<suffix>.$coll$' and admin,
//     config and local excluded, or each selected database with --nsInclude and a
//     rename. The stored bytes are hashed while they stream.
//  2. The chunks are fetched in order, hashed while they stream (each against its
//     recorded SHA-256), decrypted, gunzipped and filtered by oplog.Filter with the
//     same renaming and selection and the plan's limit, into a synthetic oplog-only
//     archive fed to mongorestore --archive --oplogReplay --oplogLimit=<t>:<i>.
//
// Afterwards the operations the filter wrote are compared with mongorestore's
// "applied N oplog entries": a mismatch fails the restore with ErrOpCountMismatch
// and keeps the clones; a missing count adds a warning. A restore that fails or is
// cancelled after mongorestore started drops its clones (unless documents failed to
// insert or the counts differ, which keeps them for inspection). Clones that exist
// already are refused with ErrCloneExists before anything is written.
func (e *Engine) ExecutePITR(ctx context.Context, req models.RestoreRequest, run PITRRun, record *models.RestoreRecord) (*models.RestoreRecord, error) {
	if run.Plan == nil || run.Base == nil || record == nil || record.PITR == nil {
		return failRestore(record, errors.New("restore: point-in-time restore without a plan"), "point-in-time restore without a plan")
	}
	r, err := e.forRun(ctx, run.Base.StorageTargetID)
	if err != nil {
		return failRestore(record, err, err.Error())
	}
	return r.executePITR(ctx, req, run, record)
}

// executePITR runs ExecutePITR on an engine bound by forRun.
func (e *Engine) executePITR(ctx context.Context, req models.RestoreRequest, run PITRRun, record *models.RestoreRecord) (*models.RestoreRecord, error) {
	info, base, uri := record.PITR, run.Base, e.resolveURI(req)
	if e.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, e.timeout, ErrTimeout)
		defer cancel()
	}
	tracker := runs.FromContext(ctx)
	e.logger.Info("initiating point-in-time restore",
		slog.String("restore_id", record.ID),
		logsafe.Attr("stream_id", info.StreamID),
		logsafe.Attr("base_id", base.ID),
		slog.String("target_time", info.TargetTime.Format(time.RFC3339)),
		slog.Int("chunks", info.Chunks),
		logsafe.Attr("mongo_uri", redact.URI(uri)),
	)
	if c := runs.CancellationOf(ctx); c != nil {
		return e.cancelled(ctx, record, c, " before mongorestore started; the target is untouched")
	}
	record.Phases.Started = models.Stamp(time.Now())
	tracker.Printf("point-in-time restore %s to %s (limit %d:%d) from base %s and %d oplog chunk(s): %s -> %s (connection %s)",
		record.ID, info.TargetTime.Format(time.RFC3339), info.Limit.T, info.Limit.I, base.ID, info.Chunks,
		record.SourceDatabase, record.TargetDatabase, redact.URI(uri))

	if e.decryptor == nil && pitrEncrypted(run) {
		return e.failRun(ctx, record, fmt.Errorf("restore: %w: %s", encryption.ErrEncryptionKeyRequired, KeyRequiredHint),
			"the base backup or the oplog is encrypted but no decryption key is configured: "+KeyRequiredHint)
	}
	version, err := e.pitrServerVersion(ctx, uri, base)
	if err != nil {
		return e.failRun(ctx, record, err, err.Error()+"; the target is untouched")
	}
	if err = e.checkPITRClones(ctx, uri, info); err != nil {
		return e.failRun(ctx, record, err, fmt.Sprintf("%v; nothing was written, retry the restore", err))
	}
	// Every chunk is checked against its checksum before anything is written.
	tracker.Phase(models.PhaseVerifying, record.Phases)
	if err = e.verifyPITRChunks(ctx, run); err != nil {
		return e.failRun(ctx, record, err, err.Error()+"; the target is untouched")
	}
	record.Phases.VerifyDone = models.Stamp(time.Now())
	clones := &cloneRecorder{rec: record, on: run.OnRecord}

	// failAfterStart records a failure once mongorestore may have written, or the
	// cancellation that caused it; keep leaves the clones for inspection.
	failAfterStart := func(err error, keep bool) (*models.RestoreRecord, error) {
		finish := time.Now().UTC()
		record.CompletedAt = &finish
		record.DurationSeconds = finish.Sub(record.StartedAt).Seconds()
		if c := runs.CancellationOf(ctx); c != nil {
			return e.cancelled(ctx, record, c, e.dropPITRClones(ctx, uri, info))
		}
		err = e.withCause(ctx, err)
		note := fmt.Sprintf("; the clones %s were kept for inspection and must not be trusted", strings.Join(info.Clones, ", "))
		if !keep {
			note = e.dropPITRClones(ctx, uri, info)
		}
		return e.failDone(ctx, record, err, err.Error()+note)
	}

	// Pass 1: the base.
	tracker.Phase(models.PhaseRestoring, record.Phases)
	tracker.Printf("pass 1 of 2: restoring base backup %s", base.ID)
	started, err := e.restorePITRBase(ctx, uri, run, info)
	// A database the base held beyond its manifest has a clone too: record it (the
	// suffix carries this restore's random clone ID, so no other database matches).
	if started && e.listDatabases != nil {
		if names, listErr := e.listDatabases(context.WithoutCancel(ctx), uri); listErr == nil {
			var found []string
			for _, n := range names {
				if strings.HasSuffix(n, info.CloneSuffix) {
					found = append(found, n)
				}
			}
			clones.add(found...)
		}
	}
	if err != nil {
		if !started {
			return e.failRun(ctx, record, err, err.Error()+"; the target is untouched")
		}
		return failAfterStart(err, errors.Is(err, ErrDocumentsFailed))
	}
	if c := runs.CancellationOf(ctx); c != nil {
		return failAfterStart(c, false)
	}

	// Pass 2: the oplog.
	tracker.Printf("pass 2 of 2: replaying %d oplog chunk(s) up to %d:%d", info.Chunks, info.Limit.T, info.Limit.I)
	ops, applied, err := e.replayPITROplog(ctx, uri, version, run, info, clones)
	info.OpsReplayed, info.OpsApplied = ops, applied
	if err != nil {
		return failAfterStart(err, false)
	}
	if applied == nil {
		info.OpsUnverified = true
		addWarning(record, "mongorestore printed no count of applied oplog entries; the replay could not be cross-checked")
	} else if *applied != ops {
		tracker.Finishing()
		return failAfterStart(fmt.Errorf("%w: wrote %d, mongorestore applied %d", ErrOpCountMismatch, ops, *applied), true)
	}

	tracker.Finishing()
	// The connection's post-restore commands run against every clone before the
	// restore counts as complete; a failure keeps the clones.
	created := pitrCreated(info)
	if err = e.applyPostRestore(ctx, uri, req, record, created); err != nil {
		finish := time.Now().UTC()
		record.CompletedAt = &finish
		record.DurationSeconds = finish.Sub(record.StartedAt).Seconds()
		record.Phases.RestoreDone = models.Stamp(finish)
		record.Phases.Finished = models.Stamp(finish)
		return e.failDone(ctx, record, err, err.Error()+keptNote(sortedClones(created)))
	}
	finish := time.Now().UTC()
	record.CompletedAt = &finish
	record.DurationSeconds = finish.Sub(record.StartedAt).Seconds()
	record.Phases.RestoreDone = models.Stamp(finish)
	record.Phases.Finished = models.Stamp(finish)
	record.Status = models.RestoreStatusCompleted
	tracker.Phase(models.PhaseFinishing, record.Phases)
	tracker.Printf("point-in-time restore completed into %s in %.1fs (%d oplog operations replayed)", record.TargetDatabase, record.DurationSeconds, ops)
	e.logger.Info("point-in-time restore finished successfully",
		slog.String("restore_id", record.ID),
		logsafe.Attr("target_db", record.TargetDatabase),
		slog.Int64("ops", ops),
		slog.Float64("duration_sec", record.DurationSeconds),
	)
	return record, nil
}

// ServerVersionFunc returns the MongoDB version of the server at uri.
type ServerVersionFunc func(ctx context.Context, uri string) (string, error)

// WithServerVersion sets how a point-in-time restore learns the MongoDB version its
// synthetic oplog archive records when the base backup records none (base backups
// of a whole instance have no manifest): the target server's version.
func WithServerVersion(fn ServerVersionFunc) Option {
	return func(e *Engine) {
		e.serverVersion = fn
	}
}

// pitrServerVersion returns the version the synthetic oplog archive records: the
// base's, or else the target's. It fails with ErrNoServerVersion without either.
func (e *Engine) pitrServerVersion(ctx context.Context, uri string, base *models.BackupRecord) (string, error) {
	if v := strings.TrimSpace(base.ServerVersion); v != "" {
		return v, nil
	}
	if e.serverVersion == nil {
		return "", fmt.Errorf("%w (base %s)", ErrNoServerVersion, base.ID)
	}
	v, err := e.serverVersion(ctx, uri)
	if err != nil || strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("%w (base %s) and the target's version cannot be read: %v", ErrNoServerVersion, base.ID, redact.Text(fmt.Sprint(err)))
	}
	return strings.TrimSpace(v), nil
}

// pitrEncrypted reports whether the base or a chunk of run is encrypted.
func pitrEncrypted(run PITRRun) bool {
	if run.Base.Encrypted || keyEncrypted(run.Base.StorageKey) {
		return true
	}
	for _, c := range run.Plan.Chunks {
		if c.Encrypted || keyEncrypted(c.StorageKey) {
			return true
		}
	}
	return false
}

// pitrCreated maps the source database of every clone a point-in-time restore
// recorded (info.Clones) to the clone.
func pitrCreated(info *models.PITRRestore) map[string]string {
	out := make(map[string]string, len(info.Clones))
	for _, clone := range info.Clones {
		if src, ok := strings.CutSuffix(clone, info.CloneSuffix); ok && src != "" && info.CloneSuffix != "" {
			out[src] = clone
		}
	}
	return out
}

// pitrClones returns the clone names of a database selection (nil for a whole
// instance, whose clones are only known on the server).
func pitrClones(info *models.PITRRestore) []string {
	out := make([]string, 0, len(info.Databases))
	for _, db := range info.Databases {
		out = append(out, db+info.CloneSuffix)
	}
	return out
}

// checkPITRClones refuses recorded clone names that exist already (ErrCloneExists).
func (e *Engine) checkPITRClones(ctx context.Context, uri string, info *models.PITRRestore) error {
	if len(info.Clones) == 0 {
		return nil
	}
	if e.listDatabases != nil {
		names, err := e.listDatabases(ctx, uri)
		if err != nil {
			return fmt.Errorf("list the databases of the target: %w", err)
		}
		for _, clone := range info.Clones {
			if slices.Contains(names, clone) {
				return fmt.Errorf("%w: %s", ErrCloneExists, clone)
			}
		}
		return nil
	}
	if e.admin == nil {
		return nil
	}
	for _, clone := range info.Clones {
		exists, err := e.admin.DatabaseExists(ctx, uri, clone)
		if err != nil {
			return fmt.Errorf("check safe clone target %s: %w", clone, err)
		}
		if exists {
			return fmt.Errorf("%w: %s", ErrCloneExists, clone)
		}
	}
	return nil
}

// cloneRecorder records the clones of a running restore on its record and reports
// each new one through PITRRun.OnRecord before it is written to.
type cloneRecorder struct {
	mu  sync.Mutex
	rec *models.RestoreRecord
	on  func(*models.RestoreRecord)
}

// add records the names that are not recorded yet.
func (c *cloneRecorder) add(names ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	added := false
	for _, n := range names {
		if !slices.Contains(c.rec.PITR.Clones, n) {
			c.rec.PITR.Clones = append(c.rec.PITR.Clones, n)
			added = true
		}
	}
	if !added || c.on == nil {
		return
	}
	cp := *c.rec
	info := *c.rec.PITR
	info.Clones = slices.Clone(info.Clones)
	cp.PITR = &info
	c.on(&cp)
}

// DropPITRClones drops the clones recorded on a point-in-time restore (info.Clones)
// on the server at uri, and nothing else: chain tests drop theirs once compared, and
// the startup sweep drops those of a restore a stop interrupted. It returns a note
// for the record: what was dropped, or what to drop manually.
func (e *Engine) DropPITRClones(ctx context.Context, uri string, info *models.PITRRestore) string {
	if info == nil || len(info.Clones) == 0 {
		return ""
	}
	return e.dropPITRClones(ctx, uri, info)
}

// dropPITRClones drops the clones recorded on info and returns a note for the
// record's message.
func (e *Engine) dropPITRClones(ctx context.Context, uri string, info *models.PITRRestore) string {
	clones := slices.Clone(info.Clones)
	if len(clones) == 0 {
		return ""
	}
	if e.admin == nil {
		return fmt.Sprintf("; drop the clones %s manually", strings.Join(clones, ", "))
	}
	dropCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cloneDropTimeout)
	defer cancel()
	var failed []string
	for _, clone := range clones {
		if err := e.admin.DropDatabase(dropCtx, uri, clone); err != nil {
			e.logger.Warn("failed to drop a clone of a failed point-in-time restore",
				logsafe.Attr("target_db", clone), slog.String("error", redact.Text(err.Error())))
			failed = append(failed, clone)
		}
	}
	if len(failed) > 0 {
		return fmt.Sprintf("; dropping the partially restored clones %s failed, drop them manually", strings.Join(failed, ", "))
	}
	return fmt.Sprintf("; the clones %s were dropped", strings.Join(clones, ", "))
}

// restorePITRBase runs pass 1: the base archive into the clones, without
// --oplogReplay. started reports whether mongorestore was started (the target may
// have been written to).
func (e *Engine) restorePITRBase(ctx context.Context, uri string, run PITRRun, info *models.PITRRestore) (started bool, err error) {
	base := run.Base
	tracker := runs.FromContext(ctx)
	tracker.StartTransfer(base.SizeBytes)
	stream, err := storage.RetrieveVersion(ctx, e.storage, base.StorageKey, base.StorageVersionID)
	if err != nil {
		return false, fmt.Errorf("retrieve base backup %s: %w", base.ID, err)
	}
	defer stream.Close()
	hashed := &hashingReader{r: tracker.CountingReader(stream), h: sha256.New()}
	stored := bufio.NewReader(hashed)
	encrypted := base.Encrypted || keyEncrypted(base.StorageKey)
	if !encrypted {
		if encrypted, err = e.detectUnrecordedEncryption(stored, base); err != nil {
			return false, fmt.Errorf("base backup %s: %w", base.ID, err)
		}
	}
	var archive io.Reader = stored
	if encrypted {
		if archive, err = e.decryptor.Decrypt(stored); err != nil {
			return false, fmt.Errorf("decrypt base backup %s: %w", base.ID, err)
		}
	}
	plain := bufio.NewReader(archive)
	isGzip, err := sniffGzip(plain, base.StorageKey)
	if err != nil {
		return false, fmt.Errorf("read base backup %s: %w", base.ID, err)
	}
	input := &errTrackingReader{r: plain}

	configArg, cleanup, err := mongotools.WriteURIConfig("", mongotools.WithConnectionDefaults(uri))
	if err != nil {
		return false, fmt.Errorf("prepare mongorestore config: %w", err)
	}
	defer cleanup()
	args := pitrBaseArgs(configArg, info, run.Databases, isGzip)
	if e.pitrBypass(ctx, uri, info) {
		args = append(args, "--bypassDocumentValidation")
	}
	if c := runs.CancellationOf(ctx); c != nil {
		return false, c
	}
	stderr, waitErr, startErr := e.runRestoreTool(ctx, input, args)
	if startErr != nil {
		return false, startErr
	}
	streamErr := input.Err()
	switch {
	case ctx.Err() != nil && (waitErr != nil || streamErr != nil):
		return true, fmt.Errorf("restore aborted: %w", ctx.Err())
	case streamErr != nil:
		return true, fmt.Errorf("read base backup %s: %w", base.ID, streamErr)
	case waitErr != nil:
		return true, fmt.Errorf("mongorestore failed on base backup %s: %w (stderr: %s)", base.ID, waitErr, stderrTail(stderr))
	}
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cloneDropTimeout)
	defer cancel()
	if err := verifyStreamed(checkCtx, hashed, base.SHA256); err != nil {
		return true, fmt.Errorf("base backup %s: %w", base.ID, err)
	}
	if failed, _ := failedDocuments(stderr); failed > 0 {
		return true, fmt.Errorf("%w: %d document(s) of base backup %s failed to restore; mongorestore output: %s",
			ErrDocumentsFailed, failed, base.ID, stderrTail(stderr))
	}
	return true, nil
}

// pitrBaseArgs returns the mongorestore arguments of pass 1. baseDatabases are the
// databases of the base: for a whole instance, MongoRescue's own clones among them
// are excluded, so a restore never clones a clone.
func pitrBaseArgs(configArg string, info *models.PITRRestore, baseDatabases []string, isGzip bool) []string {
	args := []string{configArg, "--archive"}
	if isGzip {
		args = append(args, "--gzip")
	}
	if len(info.Databases) == 0 {
		args = append(args, "--nsExclude=admin.*", "--nsExclude=config.*", "--nsExclude=local.*")
		for _, db := range baseDatabases {
			if models.IsRescueClone(db) {
				args = append(args, "--nsExclude="+escapeNamespace(db)+".*")
			}
		}
		return append(args, "--nsFrom=$db$.$coll$", "--nsTo=$db$"+escapeNamespace(info.CloneSuffix)+".$coll$")
	}
	for _, db := range info.Databases {
		src := escapeNamespace(db)
		args = append(args, "--nsInclude="+src+".*")
	}
	for _, db := range info.Databases {
		args = append(args, "--nsFrom="+escapeNamespace(db)+".*", "--nsTo="+escapeNamespace(db+info.CloneSuffix)+".*")
	}
	return args
}

// pitrBypass reports whether both passes may bypass document validation: the user
// holds the privilege on the first clone (or, for a whole instance, on admin, which
// only a grant on any resource such as the restore role's gives).
func (e *Engine) pitrBypass(ctx context.Context, uri string, info *models.PITRRestore) bool {
	db := models.AdminDatabase
	if clones := pitrClones(info); len(clones) > 0 {
		db = clones[0]
	}
	return e.bypassValidation(ctx, uri, db)
}

// pitrFilter returns the oplog filter of pass 2: the same selection and renaming as
// pass 1 (MongoRescue's own clones left out), and the plan's limit. Every clone it
// renames into is recorded through clones before its first entry is written.
func pitrFilter(info *models.PITRRestore, clones *cloneRecorder) *oplog.Filter {
	f := &oplog.Filter{
		Rename: func(db string) string {
			clone := db + info.CloneSuffix
			if clones != nil {
				clones.add(clone)
			}
			return clone
		},
		Exclude: models.IsRescueClone,
		Limit:   oplog.LimitAt(info.Limit.T, info.Limit.I),
	}
	if len(info.Databases) > 0 {
		f.Select = make(map[string]bool, len(info.Databases))
		for _, db := range info.Databases {
			f.Select[db] = true
		}
	}
	return f
}

// errReplayExited closes the archive pipe once mongorestore has exited, so a
// writer still blocked on it stops.
var errReplayExited = errors.New("restore: mongorestore stopped reading the oplog archive")

// replayPITROplog runs pass 2 and returns the operations the filter wrote and the
// count mongorestore reported applying (nil when it printed none).
func (e *Engine) replayPITROplog(ctx context.Context, uri, version string, run PITRRun, info *models.PITRRestore, clones *cloneRecorder) (int64, *int64, error) {
	tracker := runs.FromContext(ctx)
	tracker.StartTransfer(info.OplogBytes)
	configArg, cleanup, err := mongotools.WriteURIConfig("", mongotools.WithConnectionDefaults(uri))
	if err != nil {
		return 0, nil, fmt.Errorf("prepare mongorestore config: %w", err)
	}
	defer cleanup()
	args := []string{configArg, "--archive", "--oplogReplay", fmt.Sprintf("--oplogLimit=%d:%d", info.Limit.T, info.Limit.I)}
	if e.pitrBypass(ctx, uri, info) {
		args = append(args, "--bypassDocumentValidation")
	}

	filter := pitrFilter(info, clones)
	pr, pw := io.Pipe()
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		werr := e.writeOplogArchive(gctx, pw, version, run, filter)
		_ = pw.CloseWithError(werr)
		return werr
	})
	stderr, waitErr, startErr := e.runRestoreTool(ctx, pr, args)
	_ = pr.CloseWithError(errReplayExited)
	writeErr := g.Wait()
	if errors.Is(writeErr, errReplayExited) {
		writeErr = nil
	}
	switch {
	case startErr != nil:
		return 0, nil, startErr
	case ctx.Err() != nil && (waitErr != nil || writeErr != nil):
		return filter.Ops(), nil, fmt.Errorf("restore aborted: %w", ctx.Err())
	case writeErr != nil:
		return filter.Ops(), nil, fmt.Errorf("oplog replay: %w", writeErr)
	case waitErr != nil:
		return filter.Ops(), nil, fmt.Errorf("mongorestore failed replaying the oplog: %w (stderr: %s)", waitErr, stderrTail(stderr))
	}
	if n, ok := appliedOps(stderr); ok {
		return filter.Ops(), &n, nil
	}
	return filter.Ops(), nil, nil
}

// writeOplogArchive writes the synthetic oplog archive of run to w: every chunk in
// order through the filter, until the filter reaches its limit.
func (e *Engine) writeOplogArchive(ctx context.Context, w io.Writer, version string, run PITRRun, filter *oplog.Filter) error {
	aw, err := oplog.NewArchiveWriter(w, oplog.ArchiveOptions{ServerVersion: version})
	if err != nil {
		return err
	}
	stores := map[string]storage.Storage{run.Base.StorageTargetID: e.storage}
	for _, c := range run.Plan.Chunks {
		st, ok := stores[c.TargetID]
		if !ok {
			if st, err = e.chunkStorage(ctx, c.TargetID); err != nil {
				return err
			}
			stores[c.TargetID] = st
		}
		if err := e.copyChunk(ctx, st, c, filter, aw); err != nil {
			return fmt.Errorf("oplog chunk %s (%s-%s): %w", c.ID, c.From, c.To, err)
		}
		if filter.LimitReached() {
			break
		}
	}
	return aw.Close()
}

// ChunkVerifiedFresh is how recently the integrity sweep must have verified a chunk
// for a point-in-time restore to skip its checksum pre-pass.
const ChunkVerifiedFresh = 24 * time.Hour

// ErrChunkChecksum indicates that an oplog chunk has no recorded checksum or does
// not match it; the point-in-time restore is refused before anything is written.
var ErrChunkChecksum = errors.New("restore: oplog chunk checksum")

// verifyPITRChunks is the pre-pass of a point-in-time restore: every chunk of run
// not verified by the integrity sweep within ChunkVerifiedFresh is downloaded once,
// hashed and discarded (it is not decrypted), and must match its recorded SHA-256.
// A chunk without a checksum is refused. Chunks are hashed again while they replay.
func (e *Engine) verifyPITRChunks(ctx context.Context, run PITRRun) error {
	tracker := runs.FromContext(ctx)
	var todo []*pitr.Chunk
	var bytes int64
	now := time.Now()
	for _, c := range run.Plan.Chunks {
		if strings.TrimSpace(c.SHA256) == "" {
			return fmt.Errorf("%w: chunk %s (%s-%s) has no recorded checksum", ErrChunkChecksum, c.ID, c.From, c.To)
		}
		if c.VerifiedAt != nil && c.VerifyError == "" && now.Sub(*c.VerifiedAt) < ChunkVerifiedFresh {
			continue
		}
		todo = append(todo, c)
		bytes += c.SizeBytes
	}
	if len(todo) == 0 {
		return nil
	}
	tracker.Printf("checking %d oplog chunk(s) not verified in the last %s against their checksums", len(todo), ChunkVerifiedFresh)
	tracker.StartTransfer(bytes)
	stores := map[string]storage.Storage{run.Base.StorageTargetID: e.storage}
	for _, c := range todo {
		st, ok := stores[c.TargetID]
		if !ok {
			var err error
			if st, err = e.chunkStorage(ctx, c.TargetID); err != nil {
				return err
			}
			stores[c.TargetID] = st
		}
		if err := hashChunk(ctx, st, c, tracker.CountingReader); err != nil {
			return fmt.Errorf("oplog chunk %s (%s-%s): %w", c.ID, c.From, c.To, err)
		}
	}
	return nil
}

// hashChunk streams the stored object of c into a SHA-256 and compares it.
func hashChunk(ctx context.Context, st storage.Storage, c *pitr.Chunk, count func(io.Reader) io.Reader) error {
	rc, err := storage.RetrieveVersion(ctx, st, c.StorageKey, c.VersionID)
	if err != nil {
		return fmt.Errorf("retrieve: %w", err)
	}
	defer rc.Close()
	h := sha256.New()
	if _, err = io.Copy(h, &ctxReader{ctx: ctx, r: count(rc)}); err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, strings.TrimSpace(c.SHA256)) {
		return fmt.Errorf("%w mismatch: recorded %s, stored object %s", ErrChunkChecksum, c.SHA256, got)
	}
	return nil
}

// chunkStorage returns the storage driver of a chunk's target.
func (e *Engine) chunkStorage(ctx context.Context, targetID string) (storage.Storage, error) {
	if e.storageFor == nil {
		return e.storage, nil
	}
	st, err := e.storageFor(ctx, targetID)
	if err != nil {
		return nil, fmt.Errorf("storage target of the oplog: %w", err)
	}
	return st, nil
}

// copyChunk streams one chunk through its checks and the filter into aw. The
// whole stored object is read, decrypted and hashed even when the filter stops
// early, and its SHA-256 must match the chunk's.
func (e *Engine) copyChunk(ctx context.Context, st storage.Storage, c *pitr.Chunk, filter *oplog.Filter, aw *oplog.ArchiveWriter) error {
	tracker := runs.FromContext(ctx)
	rc, err := storage.RetrieveVersion(ctx, st, c.StorageKey, c.VersionID)
	if err != nil {
		return fmt.Errorf("retrieve: %w", err)
	}
	defer rc.Close()
	hashed := &hashingReader{r: &ctxReader{ctx: ctx, r: tracker.CountingReader(rc)}, h: sha256.New()}
	var plain io.Reader = hashed
	if c.Encrypted || keyEncrypted(c.StorageKey) {
		if e.decryptor == nil {
			return fmt.Errorf("%w: %s", encryption.ErrEncryptionKeyRequired, KeyRequiredHint)
		}
		if plain, err = e.decryptor.Decrypt(hashed); err != nil {
			return err
		}
	}
	gz, err := gzip.NewReader(plain)
	if err != nil {
		return fmt.Errorf("gunzip: %w", err)
	}
	defer gz.Close()
	if err := filter.Copy(ctx, aw, gz); err != nil {
		return err
	}
	// Drain what the filter did not read, still compressed (it is never inflated):
	// the end of the age stream (its last authentication) and any trailing stored
	// bytes, so the whole object is hashed.
	if _, err := io.Copy(io.Discard, plain); err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if _, err := io.Copy(io.Discard, hashed); err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if want := strings.TrimSpace(c.SHA256); want != "" {
		if got := hex.EncodeToString(hashed.h.Sum(nil)); !strings.EqualFold(got, want) {
			return fmt.Errorf("%w: recorded %s, streamed chunk %s", ErrChecksumMismatch, want, got)
		}
	}
	return nil
}

// runRestoreTool runs mongorestore with stdin and returns the end of its output and
// its exit error; startErr is set when it could not be started.
func (e *Engine) runRestoreTool(ctx context.Context, stdin io.Reader, args []string) (stderr string, waitErr, startErr error) {
	tracker := runs.FromContext(ctx)
	out, wait, err := e.runner(ctx, "mongorestore", stdin, args...)
	if err != nil {
		return "", nil, fmt.Errorf("start mongorestore: %w", err)
	}
	tracker.Printf("mongorestore started: %s", strings.Join(args[1:], " "))
	buf := &mongotools.TailBuffer{}
	toolOut := tracker.ToolOutput()
	_, _ = io.Copy(io.MultiWriter(buf, toolOut), out)
	_ = toolOut.Close()
	return buf.String(), wait(), nil
}

// appliedPattern matches mongorestore's count of replayed oplog operations.
var appliedPattern = regexp.MustCompile(`applied (\d+) oplog entries`)

// appliedOps returns the count of mongorestore's last "applied N oplog entries".
func appliedOps(stderr string) (int64, bool) {
	m := appliedPattern.FindAllStringSubmatch(stderr, -1)
	if len(m) == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(m[len(m)-1][1], 10, 64)
	return n, err == nil
}
