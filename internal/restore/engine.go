// Package restore coordinates streaming disaster recovery and database restoration
// from storage backends directly into MongoDB instances using safe clone namespaces.
package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotools"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// Sentinel errors returned by verify-before-restore.
var (
	// ErrChecksumMismatch indicates that the stored artifact's SHA-256 differs from the
	// checksum recorded at backup time; mongorestore is not started.
	ErrChecksumMismatch = errors.New("restore: backup checksum mismatch")

	// ErrChecksumUnavailable indicates that verification was requested but the backup
	// record carries no checksum to verify against; mongorestore is not started.
	ErrChecksumUnavailable = errors.New("restore: backup record has no checksum to verify")

	// ErrTimeout indicates that the restore exceeded its maximum run duration
	// (see WithTimeout) and was aborted.
	ErrTimeout = errors.New("restore: exceeded maximum run duration")
)

// ProcessRunner abstracts subprocess execution for restore commands.
type ProcessRunner func(ctx context.Context, name string, stdin io.Reader, args ...string) (stderr io.Reader, wait func() error, err error)

// Engine orchestrates MongoDB restore and disaster recovery operations.
type Engine struct {
	storage      storage.Storage
	logger       *slog.Logger
	defaultURI   string
	runner       ProcessRunner
	decryptor    *encryption.Decryptor
	verifyPolicy models.VerifyPolicy
	timeout      time.Duration

	// config and storageFor, when set, supply the settings and the storage driver of
	// each run instead of the static values above.
	config     func() RunConfig
	storageFor StorageFunc
}

// RunConfig holds the settings a restore uses, read when it starts.
type RunConfig struct {
	// Decryptor holds the keys for encrypted backups; nil means none.
	Decryptor *encryption.Decryptor
	// VerifyPolicy applies to requests that do not set Verify.
	VerifyPolicy models.VerifyPolicy
	// Timeout bounds the run (0 = unlimited).
	Timeout time.Duration
}

// StorageFunc returns the storage driver of a storage target.
type StorageFunc func(ctx context.Context, targetID string) (storage.Storage, error)

// WithRunConfig makes every restore read its keys, verify policy and timeout from fn,
// overriding WithDecryptor, WithVerifyPolicy and WithTimeout.
func WithRunConfig(fn func() RunConfig) Option {
	return func(e *Engine) {
		e.config = fn
	}
}

// WithStorageResolver makes every restore read from the storage target recorded on
// its backup (BackupRecord.StorageTargetID), resolved through fn.
func WithStorageResolver(fn StorageFunc) Option {
	return func(e *Engine) {
		e.storageFor = fn
	}
}

// runConfig returns the settings for a new run.
func (e *Engine) runConfig() RunConfig {
	if e.config != nil {
		cfg := e.config()
		if cfg.VerifyPolicy == "" {
			cfg.VerifyPolicy = models.VerifyAuto
		}
		return cfg
	}
	return RunConfig{Decryptor: e.decryptor, VerifyPolicy: e.verifyPolicy, Timeout: e.timeout}
}

// forRun returns a copy of the engine bound to the current settings and the storage
// driver of targetID.
func (e *Engine) forRun(ctx context.Context, targetID string) (*Engine, error) {
	run := *e
	cfg := e.runConfig()
	run.decryptor, run.verifyPolicy, run.timeout = cfg.Decryptor, cfg.VerifyPolicy, cfg.Timeout
	if e.storageFor != nil {
		driver, err := e.storageFor(ctx, targetID)
		if err != nil {
			return nil, fmt.Errorf("restore: storage target of the backup: %w", err)
		}
		run.storage = driver
	}
	if run.storage == nil {
		return nil, errors.New("restore: no storage configured")
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

// WithLogger specifies a custom structured logger.
func WithLogger(logger *slog.Logger) Option {
	return func(e *Engine) {
		e.logger = logger
	}
}

// WithDecryptor supplies the key material used to restore encrypted backups.
// Without it, restoring a backup whose record is Encrypted fails with
// encryption.ErrEncryptionKeyRequired; unencrypted backups are unaffected.
func WithDecryptor(dec *encryption.Decryptor) Option {
	return func(e *Engine) {
		e.decryptor = dec
	}
}

// WithVerifyPolicy sets the default verify-before-restore policy applied to requests
// that do not set RestoreRequest.Verify explicitly. The default is models.VerifyAuto.
func WithVerifyPolicy(policy models.VerifyPolicy) Option {
	return func(e *Engine) {
		e.verifyPolicy = policy
	}
}

// WithTimeout bounds the total duration of every restore (including verification);
// exceeding it aborts the run with ErrTimeout. Zero (the default) means unlimited.
func WithTimeout(d time.Duration) Option {
	return func(e *Engine) {
		e.timeout = d
	}
}

// CanDecrypt reports whether the engine holds key material for encrypted backups.
func (e *Engine) CanDecrypt() bool {
	return e.runConfig().Decryptor != nil
}

// NewEngine initializes a new RestoreEngine.
func NewEngine(store storage.Storage, defaultURI string, opts ...Option) *Engine {
	e := &Engine{
		storage:      store,
		logger:       slog.Default(),
		defaultURI:   defaultURI,
		runner:       defaultProcessRunner,
		verifyPolicy: models.VerifyAuto,
	}

	for _, opt := range opts {
		opt(e)
	}

	return e
}

// Run executes a streaming restore operation from storage into the target MongoDB
// instance. It is Prepare followed by Execute.
func (e *Engine) Run(ctx context.Context, req models.RestoreRequest, sourceRecord *models.BackupRecord) (*models.RestoreRecord, error) {
	record, err := e.Prepare(req, sourceRecord)
	if err != nil {
		return nil, err
	}
	return e.Execute(ctx, req, sourceRecord, record)
}

// Prepare validates req and returns the in-progress record (ID, resolved target
// namespace, start time) the restore will produce, without performing any I/O.
// Request errors (missing IDs or URI, unconfirmed in-place restore wrapping
// models.ErrInPlaceNotConfirmed) are returned without a record.
func (e *Engine) Prepare(req models.RestoreRequest, sourceRecord *models.BackupRecord) (*models.RestoreRecord, error) {
	if strings.TrimSpace(req.BackupID) == "" {
		return nil, errors.New("restore: backup id is required")
	}
	if sourceRecord == nil {
		return nil, errors.New("restore: source backup record must be provided")
	}
	if e.resolveURI(req) == "" {
		return nil, errors.New("restore: mongo connection uri is required")
	}

	// Safe clone is the default: overwriting a named namespace needs explicit consent.
	if err := req.ValidateTarget(); err != nil {
		return nil, fmt.Errorf("restore: %w", err)
	}

	startTime := time.Now().UTC()
	// Sanitize the database component so the derived ID satisfies models.ValidateID.
	// A random suffix keeps IDs unique for restores started within the same second.
	const restoreIDOverhead = len("rst___") + len("20060102_150405") + models.IDSuffixLength
	suffix, err := models.NewIDSuffix()
	if err != nil {
		return nil, fmt.Errorf("restore: %w", err)
	}
	idDB := models.SanitizeIDComponent(sourceRecord.Database, models.MaxIDLength-restoreIDOverhead)
	restoreID := fmt.Sprintf("rst_%s_%s_%s", idDB, startTime.Format("20060102_150405"), suffix)

	// Determine destination database name: a fresh clone unless in place was confirmed.
	targetDB := fmt.Sprintf("%s_rescue_%s", sourceRecord.Database, startTime.Format("20060102_150405"))
	if req.InPlace() {
		targetDB = strings.TrimSpace(req.TargetDatabase)
		if targetDB == "" {
			targetDB = sourceRecord.Database
		}
	}

	return &models.RestoreRecord{
		ID:                   restoreID,
		BackupID:             req.BackupID,
		SourceDatabase:       sourceRecord.Database,
		TargetDatabase:       targetDB,
		TargetConnectionID:   req.TargetConnectionID,
		TargetConnectionName: req.TargetConnectionName,
		Status:               models.RestoreStatusInProgress,
		StartedAt:            startTime,
		DryRun:               req.DryRun,
	}, nil
}

// resolveURI returns the connection string for req (falling back to the default).
func (e *Engine) resolveURI(req models.RestoreRequest) string {
	if req.MongoURI != "" {
		return req.MongoURI
	}
	return e.defaultURI
}

// Execute runs the restore described by record (as returned by Prepare for the same
// req and sourceRecord), updating and returning it.
//
// Encrypted backups are decrypted on the fly (storage -> age decrypt -> mongorestore
// stdin). Missing key material yields encryption.ErrEncryptionKeyRequired and a key
// that does not match, or a corrupted ciphertext, yields encryption.ErrDecryptionFailed;
// header-level failures are detected before mongorestore is started.
//
// When verification applies (see models.RestoreRequest.ShouldVerify), a first pass
// streams the artifact through SHA-256 (and full age authentication) into io.Discard;
// mongorestore only starts if it matches the recorded checksum, otherwise
// ErrChecksumMismatch is returned. Without verification, a stream that fails midway
// may leave partial data in the target namespace, which the record's ErrorMessage
// states. The whole run is bounded by WithTimeout.
func (e *Engine) Execute(ctx context.Context, req models.RestoreRequest, sourceRecord *models.BackupRecord, record *models.RestoreRecord) (*models.RestoreRecord, error) {
	if sourceRecord == nil {
		return failRestore(record, errors.New("restore: source backup record must be provided"), "source backup record missing")
	}
	run, err := e.forRun(ctx, sourceRecord.StorageTargetID)
	if err != nil {
		return failRestore(record, err, err.Error())
	}
	return run.execute(ctx, req, sourceRecord, record)
}

// execute runs the restore on an engine bound by forRun.
func (e *Engine) execute(ctx context.Context, req models.RestoreRequest, sourceRecord *models.BackupRecord, record *models.RestoreRecord) (*models.RestoreRecord, error) {
	restoreID, targetDB, startTime := record.ID, record.TargetDatabase, record.StartedAt
	mongoURI := e.resolveURI(req)

	if e.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, e.timeout, ErrTimeout)
		defer cancel()
	}

	e.logger.Info("initiating mongodb streaming restore",
		slog.String("restore_id", restoreID),
		slog.String("backup_id", req.BackupID),
		slog.String("source_db", sourceRecord.Database),
		slog.String("target_db", targetDB),
		slog.Bool("safe_clone", !req.InPlace()),
		slog.Bool("dry_run", req.DryRun),
		slog.String("mongo_uri", redact.URI(mongoURI)),
		slog.Bool("encrypted", sourceRecord.Encrypted),
	)

	if sourceRecord.Encrypted && e.decryptor == nil {
		return failRestore(record, fmt.Errorf("restore %s: %w", req.BackupID, encryption.ErrEncryptionKeyRequired),
			"backup is encrypted but no decryption identity or passphrase is configured")
	}

	if req.ShouldVerify(e.verifyPolicy) {
		if err := e.verifyArtifact(ctx, sourceRecord); err != nil {
			err = e.withCause(ctx, err)
			return failRestore(record, fmt.Errorf("verify backup %s: %w", req.BackupID, err),
				fmt.Sprintf("pre-restore verification failed (target untouched): %v", err))
		}
		record.Verified = true
		e.logger.Info("backup artifact verified before restore",
			slog.String("restore_id", restoreID),
			slog.String("backup_id", req.BackupID),
		)
	}

	// Open read stream from storage
	stream, err := e.storage.Retrieve(ctx, sourceRecord.StorageKey)
	if err != nil {
		return failRestore(record, fmt.Errorf("retrieve backup stream from storage: %w", err),
			fmt.Sprintf("retrieve backup stream: %v", err))
	}
	defer stream.Close()

	var archive io.Reader = stream
	if sourceRecord.Encrypted {
		decrypted, decErr := e.decryptor.Decrypt(stream)
		if decErr != nil {
			return failRestore(record, fmt.Errorf("decrypt backup stream: %w", decErr), decErr.Error())
		}
		archive = decrypted
	}
	input := &errTrackingReader{r: archive}

	// Pass the URI through a private config file so credentials never appear in argv.
	// Connection timeouts are defaulted there; the augmented URI is never logged.
	configArg, cleanupConfig, err := mongotools.WriteURIConfig("", mongotools.WithConnectionDefaults(mongoURI))
	if err != nil {
		return failRestore(record, fmt.Errorf("prepare mongorestore config: %w", err),
			fmt.Sprintf("prepare mongorestore config: %v", err))
	}
	defer cleanupConfig()

	// Build mongorestore arguments
	archiveKey := sourceRecord.StorageKey
	if sourceRecord.Encrypted {
		archiveKey = strings.TrimSuffix(archiveKey, encryption.FileExtension)
	}
	args := e.buildRestoreArgs(configArg, sourceRecord.Database, targetDB, req, strings.HasSuffix(archiveKey, ".gz"))

	stderr, wait, err := e.runner(ctx, "mongorestore", input, args...)
	if err != nil {
		return failRestore(record, fmt.Errorf("start mongorestore: %w", err), fmt.Sprintf("start mongorestore: %v", err))
	}

	// Capture stderr for status and error diagnostics
	stderrLogs, _ := io.ReadAll(stderr)
	waitErr := wait()

	finishTime := time.Now().UTC()
	record.CompletedAt = &finishTime
	record.DurationSeconds = finishTime.Sub(startTime).Seconds()

	partialNote := ""
	if !req.DryRun {
		partialNote = fmt.Sprintf("; partial data may have been applied to target namespace %s.*", targetDB)
	}

	// An aborted run (timeout, cancellation) may have stopped mongorestore midway.
	if ctx.Err() != nil {
		abortErr := e.withCause(ctx, ctx.Err())
		return failRestore(record, fmt.Errorf("restore aborted: %w", abortErr),
			fmt.Sprintf("restore aborted: %v%s", abortErr, partialNote))
	}

	// A mid-stream read or authentication failure means mongorestore saw truncated
	// input; report it as the root cause regardless of mongorestore's exit status.
	if streamErr := input.Err(); streamErr != nil {
		stage := "read backup stream"
		if errors.Is(streamErr, encryption.ErrDecryptionFailed) {
			stage = "decrypt backup stream"
		}
		return failRestore(record, fmt.Errorf("%s: %w", stage, streamErr),
			fmt.Sprintf("%s: %v%s", stage, streamErr, partialNote))
	}

	if waitErr != nil {
		return failRestore(record, fmt.Errorf("mongorestore failed: %w (stderr: %s)", waitErr, redact.Text(string(stderrLogs))),
			fmt.Sprintf("mongorestore failed: %v, stderr: %s", waitErr, stderrLogs))
	}

	record.Status = models.RestoreStatusCompleted
	e.logger.Info("mongodb streaming restore finished successfully",
		slog.String("restore_id", restoreID),
		slog.String("target_db", targetDB),
		slog.Float64("duration_sec", record.DurationSeconds),
		slog.Bool("dry_run", req.DryRun),
	)

	return record, nil
}

// withCause tags err with ErrTimeout when ctx ended because the run hit its limit.
func (e *Engine) withCause(ctx context.Context, err error) error {
	if errors.Is(context.Cause(ctx), ErrTimeout) && !errors.Is(err, ErrTimeout) {
		return fmt.Errorf("%w (limit %s): %w", ErrTimeout, e.timeout, err)
	}
	return err
}

// failRestore marks record failed with a redacted message and returns it with err.
func failRestore(record *models.RestoreRecord, err error, message string) (*models.RestoreRecord, error) {
	record.Status = models.RestoreStatusFailed
	record.ErrorMessage = redact.Text(message)
	return record, err
}

// buildRestoreArgs constructs safe CLI arguments for mongorestore. configArg is the
// "--config=<file>" argument carrying the connection URI (see mongotools.WriteURIConfig).
func (e *Engine) buildRestoreArgs(configArg, sourceDB, targetDB string, req models.RestoreRequest, isGzip bool) []string {
	args := []string{
		configArg,
		"--archive",
	}

	if isGzip {
		args = append(args, "--gzip")
	}

	if req.DryRun {
		args = append(args, "--dryRun")
	}

	if req.DropTarget {
		args = append(args, "--drop")
	}

	// Handle namespace renaming (e.g. restoring to safe clone or custom database)
	if targetDB != "" && targetDB != sourceDB {
		args = append(args,
			fmt.Sprintf("--nsFrom=%s.*", sourceDB),
			fmt.Sprintf("--nsTo=%s.*", targetDB),
		)
	}

	// Restrict to selected collections if requested
	if len(req.SelectedCollections) > 0 {
		for _, coll := range req.SelectedCollections {
			trimmed := strings.TrimSpace(coll)
			if trimmed != "" {
				args = append(args, fmt.Sprintf("--nsInclude=%s.%s", sourceDB, trimmed))
			}
		}
	} else if sourceDB != "" {
		args = append(args, fmt.Sprintf("--nsInclude=%s.*", sourceDB))
	}

	return args
}

// verifyArtifact streams the stored artifact once into io.Discard, hashing the stored
// bytes and, for encrypted backups, fully decrypting and authenticating the age
// stream (including the final chunk). It returns nil only if the SHA-256 matches.
func (e *Engine) verifyArtifact(ctx context.Context, rec *models.BackupRecord) error {
	expected := strings.TrimSpace(rec.SHA256)
	if expected == "" {
		return ErrChecksumUnavailable
	}

	stream, err := e.storage.Retrieve(ctx, rec.StorageKey)
	if err != nil {
		return fmt.Errorf("retrieve backup stream for verification: %w", err)
	}
	defer stream.Close()

	h := sha256.New()
	stored := io.TeeReader(&ctxReader{ctx: ctx, r: stream}, h)

	if rec.Encrypted {
		plain, err := e.decryptor.Decrypt(stored)
		if err != nil {
			return fmt.Errorf("verify decryption: %w", err)
		}
		// age only reports io.EOF after authenticating the final chunk.
		if _, err := io.Copy(io.Discard, plain); err != nil {
			return fmt.Errorf("verify decryption: %w", err)
		}
	}
	// Hash any remaining stored bytes (the whole object for plaintext backups).
	if _, err := io.Copy(io.Discard, stored); err != nil {
		return fmt.Errorf("read backup stream for verification: %w", err)
	}

	if actual := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(actual, expected) {
		return fmt.Errorf("%w: recorded %s, stored artifact %s", ErrChecksumMismatch, expected, actual)
	}
	return nil
}

// ctxReader aborts a long verification pass promptly once ctx is cancelled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// errTrackingReader remembers the first non-EOF read error of the wrapped reader.
// It may be read from the subprocess stdin-copy goroutine while Err is called later.
type errTrackingReader struct {
	r   io.Reader
	mu  sync.Mutex
	err error
}

func (t *errTrackingReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		t.mu.Lock()
		if t.err == nil {
			t.err = err
		}
		t.mu.Unlock()
	}
	return n, err
}

// Err returns the first recorded read error, if any.
func (t *errTrackingReader) Err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// defaultProcessRunner starts an OS subprocess piping stdin and capturing stderr. The
// process runs in its own process group and is terminated (SIGTERM, then SIGKILL after
// mongotools.KillGracePeriod) together with any children when ctx is done.
func defaultProcessRunner(ctx context.Context, name string, stdin io.Reader, args ...string) (io.Reader, func() error, error) {
	cmd := mongotools.Command(ctx, name, args...)
	cmd.Stdin = stdin

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, err
	}

	if err := mongotools.Start(cmd); err != nil {
		return nil, nil, err
	}

	wait := func() error {
		return mongotools.Wait(ctx, cmd)
	}

	return stderr, wait, nil
}
