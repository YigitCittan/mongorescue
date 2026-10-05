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
	"strconv"
	"strings"
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
}

// DatabaseLister lists the database names on the server at uri. A whole-instance
// point-in-time restore uses it to refuse clone names that exist already and to
// find the clones to drop after a failure.
type DatabaseLister func(ctx context.Context, uri string) ([]string, error)

// WithDatabaseLister sets the lister of whole-instance point-in-time restores.
// Without it, a whole-instance restore cannot check or drop its clones.
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
// models.RescueCloneSuffix of the start time unless req.PITRCloneSuffix is set.
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
	start := time.Now().UTC()
	suffix := req.PITRCloneSuffix
	if suffix == "" {
		suffix = models.RescueCloneSuffix(start)
	}
	dbs := req.PITRDatabases()
	clones := make([]string, 0, len(dbs))
	for _, db := range dbs {
		clone := db + suffix
		if len(clone) > models.MaxDatabaseNameLength {
			return nil, fmt.Errorf("restore: the clone of database %s would be named %s, longer than %d bytes", db, clone, models.MaxDatabaseNameLength)
		}
		clones = append(clones, clone)
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
			Databases: dbs, CloneSuffix: suffix,
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
	if err := e.checkPITRClones(ctx, uri, info); err != nil {
		return e.failRun(ctx, record, err, fmt.Sprintf("%v; nothing was written, retry the restore", err))
	}

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
		note := fmt.Sprintf("; the clones (databases ending in %s) were kept for inspection and must not be trusted", info.CloneSuffix)
		if !keep {
			note = e.dropPITRClones(ctx, uri, info)
		}
		return e.failDone(ctx, record, err, err.Error()+note)
	}

	// Pass 1: the base.
	tracker.Phase(models.PhaseRestoring, record.Phases)
	tracker.Printf("pass 1 of 2: restoring base backup %s", base.ID)
	started, err := e.restorePITRBase(ctx, uri, base, info)
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
	ops, applied, err := e.replayPITROplog(ctx, uri, run, info)
	info.OpsReplayed, info.OpsApplied = ops, applied
	if err != nil {
		return failAfterStart(err, false)
	}
	if applied == nil {
		addWarning(record, "mongorestore printed no count of applied oplog entries; the replay could not be cross-checked")
	} else if *applied != ops {
		tracker.Finishing()
		return failAfterStart(fmt.Errorf("%w: wrote %d, mongorestore applied %d", ErrOpCountMismatch, ops, *applied), true)
	}

	tracker.Finishing()
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

// pitrClones returns the clone names of a database selection (nil for a whole
// instance, whose clones are only known on the server).
func pitrClones(info *models.PITRRestore) []string {
	out := make([]string, 0, len(info.Databases))
	for _, db := range info.Databases {
		out = append(out, db+info.CloneSuffix)
	}
	return out
}

// checkPITRClones refuses clone names that exist already (ErrCloneExists).
func (e *Engine) checkPITRClones(ctx context.Context, uri string, info *models.PITRRestore) error {
	if len(info.Databases) > 0 {
		if e.admin == nil {
			return nil
		}
		for _, clone := range pitrClones(info) {
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
	if e.listDatabases == nil {
		return nil
	}
	names, err := e.listDatabases(ctx, uri)
	if err != nil {
		return fmt.Errorf("list the databases of the target: %w", err)
	}
	for _, n := range names {
		if strings.HasSuffix(n, info.CloneSuffix) {
			return fmt.Errorf("%w: %s", ErrCloneExists, n)
		}
	}
	return nil
}

// dropPITRClones drops the clones of a failed restore and returns a note for the
// record's message.
func (e *Engine) dropPITRClones(ctx context.Context, uri string, info *models.PITRRestore) string {
	manual := fmt.Sprintf("; drop the databases ending in %s manually", info.CloneSuffix)
	if e.admin == nil {
		return manual
	}
	dropCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cloneDropTimeout)
	defer cancel()
	clones := pitrClones(info)
	if len(info.Databases) == 0 {
		if e.listDatabases == nil {
			return manual
		}
		names, err := e.listDatabases(dropCtx, uri)
		if err != nil {
			return fmt.Sprintf("; listing the partially restored clones failed (%v)%s", err, manual)
		}
		for _, n := range names {
			if strings.HasSuffix(n, info.CloneSuffix) {
				clones = append(clones, n)
			}
		}
	}
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
	return fmt.Sprintf("; the partially restored clones (databases ending in %s) were dropped", info.CloneSuffix)
}

// restorePITRBase runs pass 1: the base archive into the clones, without
// --oplogReplay. started reports whether mongorestore was started (the target may
// have been written to).
func (e *Engine) restorePITRBase(ctx context.Context, uri string, base *models.BackupRecord, info *models.PITRRestore) (started bool, err error) {
	tracker := runs.FromContext(ctx)
	tracker.StartTransfer(base.SizeBytes)
	stream, err := e.storage.Retrieve(ctx, base.StorageKey)
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
	args := pitrBaseArgs(configArg, info, isGzip)
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

// pitrBaseArgs returns the mongorestore arguments of pass 1.
func pitrBaseArgs(configArg string, info *models.PITRRestore, isGzip bool) []string {
	args := []string{configArg, "--archive"}
	if isGzip {
		args = append(args, "--gzip")
	}
	if len(info.Databases) == 0 {
		return append(args,
			"--nsExclude=admin.*", "--nsExclude=config.*", "--nsExclude=local.*",
			"--nsFrom=$db$.$coll$", "--nsTo=$db$"+escapeNamespace(info.CloneSuffix)+".$coll$")
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
// pass 1, and the plan's limit.
func pitrFilter(info *models.PITRRestore) *oplog.Filter {
	f := &oplog.Filter{
		Rename: func(db string) string { return db + info.CloneSuffix },
		Limit:  oplog.LimitAt(info.Limit.T, info.Limit.I),
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
func (e *Engine) replayPITROplog(ctx context.Context, uri string, run PITRRun, info *models.PITRRestore) (int64, *int64, error) {
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

	filter := pitrFilter(info)
	pr, pw := io.Pipe()
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		werr := e.writeOplogArchive(gctx, pw, run, filter)
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
func (e *Engine) writeOplogArchive(ctx context.Context, w io.Writer, run PITRRun, filter *oplog.Filter) error {
	version := strings.TrimSpace(run.Base.ServerVersion)
	if version == "" {
		return fmt.Errorf("%w (base %s)", ErrNoServerVersion, run.Base.ID)
	}
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
	rc, err := st.Retrieve(ctx, c.StorageKey)
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
	// Drain what the filter did not read: the rest of the entries, the end of the
	// age stream (its last authentication) and any trailing stored bytes.
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return fmt.Errorf("read: %w", err)
	}
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
