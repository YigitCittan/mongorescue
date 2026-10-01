// Package backup coordinates the execution of streaming MongoDB dumps and persists
// archives directly to configured storage drivers with constant O(1) memory footprint.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotools"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// Sentinel errors returned by the backup engine.
var (
	// ErrDumpFailed indicates that the mongodump process exited unsuccessfully.
	ErrDumpFailed = errors.New("backup: mongodump failed")

	// ErrTimeout indicates that the backup exceeded its maximum run duration
	// (see WithTimeout) and was aborted.
	ErrTimeout = errors.New("backup: exceeded maximum run duration")

	// ErrStalled indicates that mongodump produced no output for the stall timeout
	// (see WithStallTimeout), e.g. while hanging on an unreachable server, and was aborted.
	ErrStalled = errors.New("backup: mongodump stalled without producing output")

	// ErrCollectionFilter indicates that a filter naming several collections could not
	// be applied: mongodump dumps one collection or a whole database, so the engine
	// lists the database (see WithCollectionLister) and excludes the other collections.
	ErrCollectionFilter = errors.New("backup: collection filter cannot be applied")
)

// CollectionLister returns the names of the collections and views of database on the
// server at uri. Implementations must never include the URI's credentials in errors.
type CollectionLister func(ctx context.Context, uri, database string) ([]string, error)

// WithCollectionLister lets backups include several collections (BackupOptions
// Collections with more than one entry): the database is listed with fn and every
// other collection is excluded. Without a lister such backups fail with
// ErrCollectionFilter instead of silently dumping only one collection.
func WithCollectionLister(fn CollectionLister) Option {
	return func(e *Engine) {
		e.listCollections = fn
	}
}

// ProcessRunner abstracts subprocess execution for testing and process management.
type ProcessRunner func(ctx context.Context, name string, args ...string) (stdout io.ReadCloser, stderr io.Reader, wait func() error, err error)

// Engine orchestrates MongoDB backup operations.
type Engine struct {
	storage    storage.Storage
	logger     *slog.Logger
	defaultURI string
	runner     ProcessRunner
	toolsDir   string
	encryptor  *encryption.Encryptor

	timeout      time.Duration
	stallTimeout time.Duration

	listCollections CollectionLister

	// config and storageFor, when set, supply the settings and the storage driver of
	// each run instead of the static values above.
	config     func() RunConfig
	storageFor StorageFunc
}

// RunConfig holds the settings a run uses. They are read when a run is prepared and
// started, so changes apply to the next run without a restart.
type RunConfig struct {
	// Encryptor encrypts new backups; nil disables encryption.
	Encryptor *encryption.Encryptor
	// Timeout bounds the run (0 = unlimited).
	Timeout time.Duration
	// StallTimeout aborts a silent mongodump (0 = off).
	StallTimeout time.Duration
}

// StorageFunc returns the storage driver of a storage target (the default target for
// an empty ID).
type StorageFunc func(ctx context.Context, targetID string) (storage.Storage, error)

// WithRunConfig makes every run read its encryptor and limits from fn, overriding
// WithEncryptor, WithTimeout and WithStallTimeout.
func WithRunConfig(fn func() RunConfig) Option {
	return func(e *Engine) {
		e.config = fn
	}
}

// WithStorageResolver makes every run write to the storage target named by
// BackupOptions.StorageTargetID, resolved through fn, instead of the engine's fixed
// storage.
func WithStorageResolver(fn StorageFunc) Option {
	return func(e *Engine) {
		e.storageFor = fn
	}
}

// runConfig returns the settings for a new run.
func (e *Engine) runConfig() RunConfig {
	if e.config != nil {
		return e.config()
	}
	return RunConfig{Encryptor: e.encryptor, Timeout: e.timeout, StallTimeout: e.stallTimeout}
}

// forRun returns a copy of the engine bound to the current settings and the storage
// driver of targetID.
func (e *Engine) forRun(ctx context.Context, targetID string) (*Engine, error) {
	run := *e
	cfg := e.runConfig()
	run.encryptor, run.timeout, run.stallTimeout = cfg.Encryptor, cfg.Timeout, cfg.StallTimeout
	if e.storageFor != nil {
		driver, err := e.storageFor(ctx, targetID)
		if err != nil {
			return nil, fmt.Errorf("backup: storage target: %w", err)
		}
		run.storage = driver
	}
	if run.storage == nil {
		return nil, errors.New("backup: no storage configured")
	}
	return &run, nil
}

// Option configures optional parameters for Engine.
type Option func(*Engine)

// WithRunner overrides the default subprocess runner (primarily for unit testing).
func WithRunner(runner ProcessRunner) Option {
	return func(e *Engine) {
		e.runner = runner
	}
}

// WithToolsDir makes the default runner look for mongodump in dir before the bundled
// locations and PATH (see mongotools.Resolver). It has no effect with WithRunner.
func WithToolsDir(dir string) Option {
	return func(e *Engine) {
		e.toolsDir = dir
	}
}

// WithLogger specifies a custom structured logger.
func WithLogger(logger *slog.Logger) Option {
	return func(e *Engine) {
		e.logger = logger
	}
}

// WithEncryptor enables client-side age encryption of every backup the engine produces.
// Encrypted artifacts get the encryption.FileExtension suffix on their storage key and
// their records carry Encrypted/EncryptionMode. A nil encryptor disables encryption.
func WithEncryptor(enc *encryption.Encryptor) Option {
	return func(e *Engine) {
		e.encryptor = enc
	}
}

// WithTimeout bounds the total duration of every backup; exceeding it aborts the run
// with ErrTimeout. Zero (the default) means unlimited.
func WithTimeout(d time.Duration) Option {
	return func(e *Engine) {
		e.timeout = d
	}
}

// WithStallTimeout aborts a backup with ErrStalled when mongodump produces no output
// for d while the engine is waiting for it (time spent blocked on a slow storage
// upload does not count). Zero (the default) disables the watchdog.
func WithStallTimeout(d time.Duration) Option {
	return func(e *Engine) {
		e.stallTimeout = d
	}
}

// NewEngine initializes a new BackupEngine.
func NewEngine(store storage.Storage, defaultURI string, opts ...Option) *Engine {
	e := &Engine{
		storage:    store,
		logger:     slog.Default(),
		defaultURI: defaultURI,
	}

	for _, opt := range opts {
		opt(e)
	}
	if e.runner == nil {
		e.runner = newProcessRunner(mongotools.NewResolver(e.toolsDir), e.logger)
	}

	return e
}

// Run executes a streaming backup according to the provided options. It is Prepare
// followed by Execute.
func (e *Engine) Run(ctx context.Context, opts models.BackupOptions) (*models.BackupRecord, error) {
	record, err := e.Prepare(opts)
	if err != nil {
		return nil, err
	}
	return e.Execute(ctx, opts, record)
}

// Prepare validates opts and returns the in-progress record (ID, storage key, start
// time) the backup will produce, without performing any I/O. Callers that run backups
// asynchronously persist this record before handing it to Execute.
func (e *Engine) Prepare(opts models.BackupOptions) (*models.BackupRecord, error) {
	if strings.TrimSpace(opts.Database) == "" {
		return nil, errors.New("backup: target database name is required")
	}
	if e.resolveURI(opts) == "" {
		return nil, errors.New("backup: mongo connection uri is required")
	}

	startTime := time.Now().UTC()
	// The database name is sanitized so the derived ID always satisfies models.ValidateID
	// ("bkp_" + db + "_" + 15-char timestamp + "_" + random suffix must fit in
	// models.MaxIDLength). The random suffix keeps IDs unique when the same database
	// name is backed up from two connections within one second.
	const backupIDOverhead = len("bkp___") + len("20060102_150405") + models.IDSuffixLength
	suffix, err := models.NewIDSuffix()
	if err != nil {
		return nil, fmt.Errorf("backup: %w", err)
	}
	idDB := models.SanitizeIDComponent(opts.Database, models.MaxIDLength-backupIDOverhead)
	backupID := fmt.Sprintf("bkp_%s_%s_%s", idDB, startTime.Format("20060102_150405"), suffix)

	targetKey := opts.TargetKey
	if targetKey == "" {
		ext := "archive"
		if opts.Gzip {
			ext = "archive.gz"
		}
		targetKey = fmt.Sprintf("%s/%s/%s.%s", opts.Database, startTime.Format("2006/01"), backupID, ext)
	}
	enc := e.runConfig().Encryptor
	if enc != nil && !strings.HasSuffix(targetKey, encryption.FileExtension) {
		targetKey += encryption.FileExtension
	}

	record := &models.BackupRecord{
		ID:             backupID,
		JobID:          opts.JobID,
		Trigger:        opts.Trigger,
		Database:       opts.Database,
		ConnectionID:   opts.ConnectionID,
		ConnectionName: opts.ConnectionName,
		Status:         models.StatusInProgress,
		StorageType:    opts.StorageType,
		StorageKey:     targetKey,
		Collections:    opts.Collections,
		StartedAt:      startTime,
		Phases:         models.RunPhases{Queued: models.Stamp(startTime)},

		StorageTargetID:   opts.StorageTargetID,
		StorageTargetName: opts.StorageTargetName,
	}
	if enc != nil {
		record.Encrypted = true
		record.EncryptionMode = string(enc.Mode())
	}
	return record, nil
}

// alignEncryption updates record when the encryption settings changed between Prepare
// and Execute, so the record always describes the artifact actually written.
func (e *Engine) alignEncryption(record *models.BackupRecord) {
	switch {
	case e.encryptor != nil:
		record.Encrypted, record.EncryptionMode = true, string(e.encryptor.Mode())
		if !strings.HasSuffix(record.StorageKey, encryption.FileExtension) {
			record.StorageKey += encryption.FileExtension
		}
	case record.Encrypted:
		record.Encrypted, record.EncryptionMode = false, ""
		record.StorageKey = strings.TrimSuffix(record.StorageKey, encryption.FileExtension)
	}
}

// resolveURI returns the connection string for opts (falling back to the default).
func (e *Engine) resolveURI(opts models.BackupOptions) string {
	if opts.MongoURI != "" {
		return opts.MongoURI
	}
	return e.defaultURI
}

// Execute runs the backup described by record (as returned by Prepare for the same
// opts), updating and returning it.
//
// Data flows mongodump stdout -> [age encryption] -> SHA-256/byte counter -> storage
// without ever buffering the archive. When encryption is enabled, the recorded
// SizeBytes and SHA256 describe the stored ciphertext, not the plaintext archive;
// failed backups carry neither. The run is bounded by WithTimeout and WithStallTimeout.
//
// When ctx carries a runs.Run (see runs.Run.Bind), the run's log receives mongodump's
// output and the engine's phase lines, its progress is updated while the archive
// streams, and a cancellation through the run registry kills mongodump, removes the
// partial artifact and records the backup as models.StatusCancelled.
func (e *Engine) Execute(ctx context.Context, opts models.BackupOptions, record *models.BackupRecord) (*models.BackupRecord, error) {
	run, err := e.forRun(ctx, opts.StorageTargetID)
	if err != nil {
		return e.fail(ctx, record, err)
	}
	run.alignEncryption(record)
	return run.execute(ctx, opts, record)
}

// execute runs the backup on an engine bound by forRun.
func (e *Engine) execute(ctx context.Context, opts models.BackupOptions, record *models.BackupRecord) (*models.BackupRecord, error) {
	backupID, targetKey, startTime := record.ID, record.StorageKey, record.StartedAt
	mongoURI := e.resolveURI(opts)

	tracker := runs.FromContext(ctx)
	record.Phases.Started = models.Stamp(time.Now())
	if record.Phases.Queued == nil {
		record.Phases.Queued = models.Stamp(startTime)
	}
	tracker.Printf("backup %s of database %s started (connection %s, storage key %s, encrypted %v)",
		backupID, opts.Database, redact.URI(mongoURI), targetKey, record.Encrypted)
	tracker.Phase(models.PhaseDumping, record.Phases)
	tracker.StartTransfer(0)

	// runCtx carries the run's own limits; its cause tells a timeout, a stall and an
	// external cancellation apart.
	runCtx, cancelRun := context.WithCancelCause(ctx)
	defer cancelRun(nil)
	if e.timeout > 0 {
		var cancelTimeout context.CancelFunc
		runCtx, cancelTimeout = context.WithTimeoutCause(runCtx, e.timeout, ErrTimeout)
		defer cancelTimeout()
	}

	e.logger.Info("initiating mongodb streaming backup",
		slog.String("backup_id", backupID),
		slog.String("database", opts.Database),
		slog.String("target_key", targetKey),
		slog.String("mongo_uri", redact.URI(mongoURI)),
		slog.Bool("encrypted", record.Encrypted),
	)

	// Pass the URI through a private config file so credentials never appear in argv.
	// Connection timeouts are defaulted there so an unreachable host fails fast; the
	// augmented URI is never logged or stored.
	configArg, cleanupConfig, err := mongotools.WriteURIConfig("", mongotools.WithConnectionDefaults(mongoURI))
	if err != nil {
		return e.fail(runCtx, record, fmt.Errorf("prepare mongodump config: %w", err))
	}
	defer cleanupConfig()

	dumpOpts, err := e.expandCollections(runCtx, mongoURI, opts)
	if err != nil {
		return e.fail(runCtx, record, err)
	}

	// Build mongodump arguments
	args := e.buildDumpArgs(configArg, dumpOpts)

	// procCtx lets failure paths kill mongodump even when the caller's ctx is still live,
	// so a blocked writer can never keep Wait from returning.
	procCtx, cancelProc := context.WithCancel(runCtx)
	defer cancelProc()

	stdout, stderr, wait, err := e.runner(procCtx, "mongodump", args...)
	if err != nil {
		return e.fail(runCtx, record, fmt.Errorf("start mongodump: %w", err))
	}
	defer stdout.Close()
	tracker.Printf("mongodump started: %s", strings.Join(args[1:], " "))

	// Capture the end of stderr concurrently for diagnostics (bounded: a long dump
	// logs a line per collection and progress lines); the full output streams to the
	// run's log and progress parser. reap waits for this goroutine.
	stderrChan := make(chan string, 1)
	go func() {
		tail := &mongotools.TailBuffer{}
		out := tracker.ToolOutput()
		_, _ = io.Copy(io.MultiWriter(tail, out), stderr)
		_ = out.Close()
		stderrChan <- tail.String()
	}()

	proc := &dumpProcess{
		stdout:     stdout,
		cancel:     cancelProc,
		wait:       wait,
		stderrChan: stderrChan,
	}

	// Once the run is cancelled (caller, timeout or stall), release every pending read
	// immediately instead of waiting for mongodump to react to its termination signal.
	stopAbortOnCancel := context.AfterFunc(runCtx, proc.abort)
	defer stopAbortOnCancel()

	// Stall watchdog: abort when a read from mongodump has been pending for too long.
	watched := &activityReader{r: stdout}
	stopWatchdog := e.startStallWatchdog(watched, cancelRun)
	defer stopWatchdog()

	// Optional encryption stage between mongodump and storage.
	var source io.Reader = watched
	var encStage *encryptStage
	if e.encryptor != nil {
		encStage = startEncryptStage(e.encryptor, watched, proc.reap)
		source = encStage
	}

	// In-flight hashing and byte counting of the stored (possibly encrypted) bytes
	h := sha256.New()
	var byteCount atomic.Int64
	countedReader := &countingReader{
		r:      tracker.CountingReader(source),
		hasher: h,
		total:  &byteCount,
	}

	// Stream directly to storage (Local or S3)
	savedObj, saveErr := e.storage.Save(runCtx, targetKey, countedReader)
	if saveErr != nil {
		proc.abort()
	} else {
		record.Phases.UploadDone = models.Stamp(time.Now())
	}

	var stageErr error
	if encStage != nil {
		// A legitimate EOF from the stage implies it already reaped mongodump, so aborting
		// here is harmless on success and releases a producer blocked on any other path.
		proc.abort()
		stageErr = encStage.stop(saveErr)
	}

	// Reap mongodump exactly once (stderr is drained before Wait, as os/exec requires).
	waitErr := proc.reap()
	stopWatchdog()
	if waitErr == nil {
		dumpDone := watched.eofTime()
		if dumpDone.IsZero() {
			dumpDone = time.Now()
		}
		record.Phases.DumpDone = models.Stamp(dumpDone)
	}

	finishTime := time.Now().UTC()
	record.CompletedAt = &finishTime
	record.DurationSeconds = finishTime.Sub(startTime).Seconds()

	if failErr := e.classifyFailure(runCtx, saveErr, waitErr, stageErr, proc.stderrLogs); failErr != nil {
		e.deleteArtifact(runCtx, targetKey)
		record.Phases.UploadDone = nil
		if runs.CancellationOf(runCtx) != nil {
			tracker.Printf("mongodump stopped and the partial artifact %s removed", targetKey)
		}
		return e.fail(runCtx, record, failErr)
	}

	// The artifact is complete: the run is past the point of no return and later
	// cancellations are refused (a late cancel or timeout never discards it).
	tracker.Finishing()

	// Size and checksum describe a complete artifact, so they are only recorded on success.
	record.SizeBytes = byteCount.Load()
	record.SHA256 = hex.EncodeToString(h.Sum(nil))
	if savedObj != nil && savedObj.SizeBytes > 0 && record.SizeBytes == 0 {
		record.SizeBytes = savedObj.SizeBytes
	}

	record.Status = models.StatusCompleted
	record.Phases.Finished = models.Stamp(finishTime)
	tracker.Phase(models.PhaseFinishing, record.Phases)
	tracker.Printf("backup completed: %d bytes, sha256 %s, %.1fs", record.SizeBytes, record.SHA256, record.DurationSeconds)
	e.logger.Info("mongodb streaming backup completed successfully",
		slog.String("backup_id", backupID),
		slog.Int64("size_bytes", record.SizeBytes),
		slog.String("sha256", record.SHA256),
		slog.Float64("duration_sec", record.DurationSeconds),
	)

	return record, nil
}

// fail marks record as failed with a redacted message and returns it with err. When
// ctx was cancelled through the run registry (see runs.CancellationOf), the record is
// marked cancelled instead, with who cancelled it and when.
func (e *Engine) fail(ctx context.Context, record *models.BackupRecord, err error) (*models.BackupRecord, error) {
	record.Status = models.StatusFailed
	record.SizeBytes = 0
	record.SHA256 = ""
	record.Phases.Finished = models.Stamp(time.Now())
	if c := runs.CancellationOf(ctx); c != nil {
		if !errors.Is(err, runs.ErrCancelled) {
			err = fmt.Errorf("backup %w", c)
		}
		record.Status = models.StatusCancelled
		record.CancelledBy, record.CancelledAt = c.By, models.Stamp(c.At)
	}
	record.ErrorMessage = redact.Text(err.Error())
	tracker := runs.FromContext(ctx)
	tracker.Phase(models.PhaseFinishing, record.Phases)
	tracker.Printf("backup %s: %s", record.Status, record.ErrorMessage)
	return record, err
}

// activityReader records when a Read from the wrapped reader started and has not yet
// returned, so the stall watchdog can measure how long mongodump kept us waiting. It
// also notes when the stream ended.
type activityReader struct {
	r            io.Reader
	waitingSince atomic.Int64 // UnixNano of the pending Read's start, 0 when idle
	eofAt        atomic.Int64 // UnixNano of the first io.EOF, 0 before
}

func (a *activityReader) Read(p []byte) (int, error) {
	a.waitingSince.Store(time.Now().UnixNano())
	n, err := a.r.Read(p)
	a.waitingSince.Store(0)
	if errors.Is(err, io.EOF) {
		a.eofAt.CompareAndSwap(0, time.Now().UnixNano())
	}
	return n, err
}

// eofTime returns when the stream ended, or the zero time.
func (a *activityReader) eofTime() time.Time {
	if ns := a.eofAt.Load(); ns != 0 {
		return time.Unix(0, ns)
	}
	return time.Time{}
}

// startStallWatchdog cancels the run with ErrStalled once a Read on r has been pending
// for longer than the stall timeout. The returned stop function is idempotent and
// waits for the watchdog goroutine to exit.
func (e *Engine) startStallWatchdog(r *activityReader, cancel context.CancelCauseFunc) (stop func()) {
	if e.stallTimeout <= 0 {
		return func() {}
	}
	interval := min(max(e.stallTimeout/10, 10*time.Millisecond), 5*time.Second)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-ticker.C:
				if since := r.waitingSince.Load(); since != 0 && now.Sub(time.Unix(0, since)) > e.stallTimeout {
					cancel(ErrStalled)
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			wg.Wait()
		})
	}
}

// cleanupTimeout bounds deletion of a failed artifact after the run context is gone.
const cleanupTimeout = 30 * time.Second

// deleteArtifact removes a partial or truncated artifact. It runs detached from ctx
// cancellation (a cancelled backup must still be cleaned up) but with a bounded timeout.
func (e *Engine) deleteArtifact(ctx context.Context, key string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := e.storage.Delete(cleanupCtx, key); err != nil && !errors.Is(err, storage.ErrNotFound) {
		e.logger.Warn("failed to delete partial backup artifact",
			slog.String("storage_key", key),
			slog.Any("error", err),
		)
	}
}

// classifyFailure picks the root cause of a failed backup, or returns nil on success.
// Errors induced by our own abort (e.g. mongodump killed after a storage failure, a
// timeout or a stall) are reported as the failure that triggered the abort.
func (e *Engine) classifyFailure(ctx context.Context, saveErr, waitErr, stageErr error, stderrLogs string) error {
	if saveErr == nil && waitErr == nil && stageErr == nil {
		return nil
	}
	stderrNote := ""
	if logs := strings.TrimSpace(stderrLogs); logs != "" {
		stderrNote = " (stderr: " + mongotools.QuoteTail(logs) + ")"
	}
	dumpErr := func() error {
		return fmt.Errorf("%w: %w%s", ErrDumpFailed, waitErr, stderrNote)
	}
	switch {
	case ctx.Err() != nil:
		switch cause := context.Cause(ctx); {
		case errors.Is(cause, ErrTimeout):
			return fmt.Errorf("%w (limit %s)%s: %w", ErrTimeout, e.timeout, stderrNote, context.DeadlineExceeded)
		case errors.Is(cause, ErrStalled):
			return fmt.Errorf("%w for %s%s", ErrStalled, e.stallTimeout, stderrNote)
		case runs.CancellationOf(ctx) != nil:
			return fmt.Errorf("backup %w", runs.CancellationOf(ctx))
		default:
			return fmt.Errorf("backup cancelled: %w", ctx.Err())
		}
	case errors.Is(stageErr, ErrDumpFailed) && waitErr != nil:
		return dumpErr()
	case saveErr != nil:
		return fmt.Errorf("stream to storage: %w", saveErr)
	case waitErr != nil:
		return dumpErr()
	default:
		return fmt.Errorf("encrypt backup stream: %w", stageErr)
	}
}

// dumpProcess owns the mongodump subprocess handles and guarantees that it is
// aborted and reaped at most once, from whichever goroutine gets there first.
type dumpProcess struct {
	stdout     io.Closer
	cancel     context.CancelFunc
	wait       func() error
	stderrChan <-chan string

	abortOnce  sync.Once
	reapOnce   sync.Once
	waitErr    error
	stderrLogs string // valid after reap returns
}

// abort kills mongodump and closes its stdout so that neither the process (blocked on
// a full pipe) nor a goroutine reading the pipe can block Wait. It is idempotent.
func (p *dumpProcess) abort() {
	p.abortOnce.Do(func() {
		p.cancel()
		_ = p.stdout.Close()
	})
}

// reap drains stderr, waits for mongodump to exit and returns its exit error.
// Concurrent and repeated calls return the same result.
func (p *dumpProcess) reap() error {
	p.reapOnce.Do(func() {
		p.stderrLogs = <-p.stderrChan
		p.waitErr = p.wait()
	})
	return p.waitErr
}

// trimmedNames returns the non-empty, trimmed, de-duplicated names in order.
func trimmedNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// expandCollections checks an include filter against the database and rewrites a
// filter that includes several collections into the equivalent exclusion list, since
// mongodump honours only the last --collection flag.
//
// It fails closed: a requested collection that does not exist fails the backup
// (mongodump would silently dump nothing for it). Every collection that was not
// requested is excluded, system collections such as system.js included, except
// system.views and the buckets of requested time-series collections, which mongodump
// dumps together with the views and collections they belong to.
//
// A collection created between the listing and the dump is not excluded and ends up
// in the backup as well; that race is accepted, since it can only add data and never
// drop a requested collection. Without a lister, a single collection is passed to
// mongodump unchecked and several collections fail with ErrCollectionFilter.
func (e *Engine) expandCollections(ctx context.Context, uri string, opts models.BackupOptions) (models.BackupOptions, error) {
	include := trimmedNames(opts.Collections)
	if len(include) == 0 {
		return opts, nil
	}
	if e.listCollections == nil {
		if len(include) == 1 {
			return opts, nil
		}
		return opts, fmt.Errorf("%w: %d collections requested but no collection lister is configured", ErrCollectionFilter, len(include))
	}
	names, err := e.listCollections(ctx, uri, opts.Database)
	if err != nil {
		return opts, fmt.Errorf("%w: list collections of %s: %w", ErrCollectionFilter, opts.Database, err)
	}
	for _, want := range include {
		if !slices.Contains(names, want) {
			return opts, fmt.Errorf("%w: collection %s not found in database %s", ErrCollectionFilter, want, opts.Database)
		}
	}
	if len(include) == 1 {
		return opts, nil
	}
	keep := func(name string) bool {
		if slices.Contains(include, name) || name == "system.views" {
			return true
		}
		ts, ok := strings.CutPrefix(name, "system.buckets.")
		return ok && slices.Contains(include, ts)
	}
	exclude := trimmedNames(opts.ExcludeCollections)
	for _, name := range names {
		if !keep(name) && !slices.Contains(exclude, name) {
			exclude = append(exclude, name)
		}
	}
	opts.Collections = nil
	opts.ExcludeCollections = exclude
	if len(exclude) == 0 {
		// Every collection is included: dump the whole database.
		opts.ExcludeCollections = nil
	}
	return opts, nil
}

// buildDumpArgs constructs safe CLI arguments for mongodump. configArg is the
// "--config=<file>" argument carrying the connection URI (see mongotools.WriteURIConfig).
// Names are passed as they are: mongodump's --db, --collection and --excludeCollection
// match literally, so '*' and '\' need no escaping (unlike mongorestore's --ns* patterns).
func (e *Engine) buildDumpArgs(configArg string, opts models.BackupOptions) []string {
	args := []string{
		configArg,
		"--archive",
	}

	if opts.Gzip {
		args = append(args, "--gzip")
	}

	if opts.Database != "" {
		args = append(args, fmt.Sprintf("--db=%s", opts.Database))
	}

	for _, coll := range opts.Collections {
		trimmed := strings.TrimSpace(coll)
		if trimmed != "" {
			args = append(args, fmt.Sprintf("--collection=%s", trimmed))
		}
	}

	for _, excl := range opts.ExcludeCollections {
		trimmed := strings.TrimSpace(excl)
		if trimmed != "" {
			args = append(args, fmt.Sprintf("--excludeCollection=%s", trimmed))
		}
	}

	return args
}

// encryptStage runs age encryption of a plaintext stream in its own goroutine and
// exposes the ciphertext through an io.Pipe. Errors in either direction propagate:
// an encryption or mongodump failure surfaces as a read error to the consumer, and
// stop() with a consumer error unblocks the producer. The goroutine never outlives stop().
type encryptStage struct {
	pr   *io.PipeReader
	done chan struct{}
	err  error // written by the goroutine before done is closed
}

// errConsumerStopped is delivered to the encryption goroutine when the consumer stops
// reading without an error of its own (e.g. a storage driver that returned early).
var errConsumerStopped = errors.New("backup: ciphertext consumer stopped reading")

// startEncryptStage launches the encryption goroutine reading from plaintext. reap is
// called once plaintext hits EOF; the stream is sealed only if it reports success.
func startEncryptStage(enc *encryption.Encryptor, plaintext io.Reader, reap func() error) *encryptStage {
	pr, pw := io.Pipe()
	s := &encryptStage{pr: pr, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		s.err = encryptStream(enc, pw, plaintext, reap)
		// With a nil error this is a plain Close: the consumer sees io.EOF only after
		// the age writer has flushed its final authenticated chunk.
		_ = pw.CloseWithError(s.err)
	}()
	return s
}

// encryptStream copies plaintext into an age stream written to dst. The final chunk
// is sealed only after the producing process exited successfully: a killed mongodump
// yields a clean EOF on its pipe, and sealing then would turn a partial dump into a
// well-formed (authenticated) but truncated ciphertext.
func encryptStream(enc *encryption.Encryptor, dst io.Writer, plaintext io.Reader, reap func() error) error {
	w, err := enc.Encrypt(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, plaintext); err != nil {
		return fmt.Errorf("encrypt dump stream: %w", err)
	}
	if err := reap(); err != nil {
		return fmt.Errorf("%w: %w", ErrDumpFailed, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("finalize encrypted stream: %w", err)
	}
	return nil
}

// Read implements io.Reader over the ciphertext pipe.
func (s *encryptStage) Read(p []byte) (int, error) {
	return s.pr.Read(p)
}

// stop closes the pipe (with consumerErr, if any) to unblock a pending write and waits
// for the goroutine to exit, returning its own error. Callers must abort the dump
// process first so that a pending plaintext read or reap is released as well.
func (s *encryptStage) stop(consumerErr error) error {
	if consumerErr == nil {
		consumerErr = errConsumerStopped
	}
	_ = s.pr.CloseWithError(consumerErr)
	<-s.done
	return s.err
}

// countingReader computes running SHA-256 and byte count while streaming.
type countingReader struct {
	r      io.Reader
	hasher hash.Hash
	total  *atomic.Int64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	if n > 0 {
		cr.hasher.Write(p[:n])
		cr.total.Add(int64(n))
	}
	return n, err
}

// newProcessRunner returns the default runner: it resolves the tool with tools
// (failing with an error wrapping mongotools.ErrToolNotFound; the searched locations
// are logged, not returned) and starts it with piped
// stdout/stderr. The process runs in its own process group and is terminated (SIGTERM,
// then SIGKILL after mongotools.KillGracePeriod) together with any children when ctx
// is done.
func newProcessRunner(tools *mongotools.Resolver, logger *slog.Logger) ProcessRunner {
	return func(ctx context.Context, name string, args ...string) (io.ReadCloser, io.Reader, func() error, error) {
		path, err := tools.Resolve(name)
		if err != nil {
			mongotools.LogNotFound(ctx, logger, err)
			return nil, nil, nil, err
		}
		return startProcess(ctx, path, args...)
	}
}

// startProcess starts the executable at path with piped stdout/stderr.
func startProcess(ctx context.Context, path string, args ...string) (io.ReadCloser, io.Reader, func() error, error) {
	cmd := mongotools.Command(ctx, path, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, nil, nil, err
	}

	if err := mongotools.Start(cmd); err != nil {
		_ = stdout.Close()
		return nil, nil, nil, err
	}

	wait := func() error {
		return mongotools.Wait(ctx, cmd)
	}

	return stdout, stderr, wait, nil
}
