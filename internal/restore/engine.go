// Package restore coordinates streaming disaster recovery and database restoration
// from storage backends directly into MongoDB instances using safe clone namespaces.
package restore

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"regexp"
	"strconv"
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

	// ErrDocumentsFailed indicates that mongorestore finished but reported documents
	// it could not insert (e.g. duplicate keys in a target that already held data), so
	// the target does not match the backup.
	ErrDocumentsFailed = errors.New("restore: documents failed to restore")

	// ErrCloneExists indicates that the safe-clone database of a restore already
	// exists (another restore of the same database started within the same second);
	// the restore is refused before anything is written.
	ErrCloneExists = errors.New("restore: safe clone target already exists")
)

// DatabaseAdmin checks and drops databases on the target server. The restore engine
// uses it for safe clones only: it refuses a clone name that already exists and drops
// a clone that turned out to hold data from a corrupted artifact.
type DatabaseAdmin interface {
	// DatabaseExists reports whether database exists on the server at uri.
	DatabaseExists(ctx context.Context, uri, database string) (bool, error)
	// DropDatabase drops database on the server at uri.
	DropDatabase(ctx context.Context, uri, database string) error
}

// WithDatabaseAdmin enables the safe-clone checks described at DatabaseAdmin.
func WithDatabaseAdmin(a DatabaseAdmin) Option {
	return func(e *Engine) {
		e.admin = a
	}
}

// cloneDropTimeout bounds dropping a corrupted safe clone after a failed restore.
const cloneDropTimeout = 2 * time.Minute

// ProcessRunner abstracts subprocess execution for restore commands.
type ProcessRunner func(ctx context.Context, name string, stdin io.Reader, args ...string) (stderr io.Reader, wait func() error, err error)

// Engine orchestrates MongoDB restore and disaster recovery operations.
type Engine struct {
	storage      storage.Storage
	logger       *slog.Logger
	defaultURI   string
	runner       ProcessRunner
	toolsDir     string
	decryptor    *encryption.Decryptor
	verifyPolicy models.VerifyPolicy
	timeout      time.Duration
	canBypass    BypassCheck
	admin        DatabaseAdmin

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

// WithToolsDir makes the default runner look for mongorestore in dir before the
// bundled locations and PATH (see mongotools.Resolver). It has no effect with
// WithRunner.
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

// BypassCheck reports whether the user of the connection string uri may bypass
// document validation on every collection of database.
type BypassCheck func(ctx context.Context, uri, database string) (bool, error)

// WithValidationBypassCheck makes restores pass --bypassDocumentValidation when fn
// reports that the user holds the privilege (e.g. the built-in restore role), so
// documents that predate a collection's validator are restored too. Without it, or
// when the user lacks the privilege, the server validates restored documents and a
// restore with rejected documents fails with ErrDocumentsFailed.
func WithValidationBypassCheck(fn BypassCheck) Option {
	return func(e *Engine) {
		e.canBypass = fn
	}
}

// bypassValidation reports whether the restore into database may bypass document
// validation (see WithValidationBypassCheck).
func (e *Engine) bypassValidation(ctx context.Context, uri, database string) bool {
	if e.canBypass == nil {
		return false
	}
	ok, err := e.canBypass(ctx, uri, database)
	if err != nil {
		e.logger.Warn("could not check the bypassDocumentValidation privilege; documents are validated",
			slog.String("target_db", database), slog.String("error", redact.Text(err.Error())))
		return false
	}
	return ok
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
		verifyPolicy: models.VerifyAuto,
	}

	for _, opt := range opts {
		opt(e)
	}
	if e.runner == nil {
		e.runner = newProcessRunner(mongotools.NewResolver(e.toolsDir), e.logger)
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
	targetDB := models.RescueDatabaseName(sourceRecord.Database, startTime)
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
// stdin). A backup is treated as encrypted when its record says so, its storage key
// ends in encryption.FileExtension or its content starts with the age header; gzip
// compression is taken from the archive signature, falling back to the ".gz" key
// suffix. Missing key material yields encryption.ErrEncryptionKeyRequired (with
// KeyRequiredHint) and a key that does not match, or a corrupted ciphertext, yields
// encryption.ErrDecryptionFailed; failures in the header or the first chunk are
// detected before mongorestore is started.
//
// In-place restores are always verified first; clones follow
// models.RestoreRequest.ShouldVerify (see verifyFirst). With WithDatabaseAdmin, a
// clone whose database already exists is refused (ErrCloneExists) and a clone
// restore that fails after mongorestore started drops the partial clone, unless
// documents failed to insert (ErrDocumentsFailed), which keeps it for inspection.
//
// When verification applies, a first pass
// streams the artifact through SHA-256 (and full age authentication) into io.Discard;
// mongorestore only starts if it matches the recorded checksum, otherwise
// ErrChecksumMismatch is returned. Without verification, a stream that fails midway
// may leave partial data in the target namespace, which the record's ErrorMessage
// states. The stored bytes are hashed while they stream as well: a restore whose
// artifact does not match the recorded SHA-256 fails with ErrChecksumMismatch once
// mongorestore has exited (the data it applied must not be trusted). A mongorestore
// run that reports documents it could not insert fails with ErrDocumentsFailed.
// The whole run is bounded by WithTimeout.
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

	// A record written without its encryption flag, but with an .age key, is treated as
	// encrypted as well: ciphertext is never handed to mongorestore as plaintext.
	encrypted := sourceRecord.Encrypted || keyEncrypted(sourceRecord.StorageKey)
	if encrypted && e.decryptor == nil {
		return failRestore(record, fmt.Errorf("restore %s: %w: %s", req.BackupID, encryption.ErrEncryptionKeyRequired, KeyRequiredHint),
			"backup is encrypted but no decryption key is configured: "+KeyRequiredHint)
	}

	clone := !req.InPlace() && !req.DryRun
	if clone && e.admin != nil {
		exists, err := e.admin.DatabaseExists(ctx, mongoURI, targetDB)
		if err != nil {
			return failRestore(record, fmt.Errorf("check safe clone target %s: %w", targetDB, err),
				fmt.Sprintf("could not check the safe clone target %s: %v", targetDB, err))
		}
		if exists {
			err := fmt.Errorf("%w: %s", ErrCloneExists, targetDB)
			return failRestore(record, err,
				fmt.Sprintf("%v (another restore of %s started within the same second); nothing was written, retry the restore",
					err, sourceRecord.Database))
		}
	}

	if e.verifyFirst(req, sourceRecord, record) {
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

	// The stored bytes are hashed while they stream, so an artifact that changed at
	// rest fails the restore even without a verification pass. The buffered reader
	// sits on top of the hash so every stored byte is hashed exactly once, including
	// the header bytes peeked to detect encryption.
	hashed := &hashingReader{r: stream, h: sha256.New()}
	stored := bufio.NewReader(hashed)
	if !encrypted {
		if encrypted, err = e.detectUnrecordedEncryption(stored, sourceRecord); err != nil {
			return failRestore(record, fmt.Errorf("restore %s: %w", req.BackupID, err), err.Error())
		}
	}

	var archive io.Reader = stored
	if encrypted {
		decrypted, decErr := e.decryptor.Decrypt(stored)
		if decErr != nil {
			return failRestore(record, fmt.Errorf("decrypt backup stream: %w", decErr), decErr.Error())
		}
		archive = decrypted
	}
	// Peeking decrypts and authenticates the first chunk, so a corrupted start fails
	// here, before mongorestore could apply anything.
	plain := bufio.NewReader(archive)
	isGzip, err := sniffGzip(plain, sourceRecord.StorageKey)
	if err != nil {
		stage := "read backup stream"
		if errors.Is(err, encryption.ErrDecryptionFailed) {
			stage = "decrypt backup stream"
		}
		return failRestore(record, fmt.Errorf("%s: %w", stage, err),
			fmt.Sprintf("%s (target untouched): %v", stage, err))
	}
	if isGzip != keyGzip(sourceRecord.StorageKey) {
		e.logger.Warn("backup compression differs from its storage key; using the archive content",
			slog.String("backup_id", req.BackupID), slog.Bool("gzip", isGzip))
	}
	input := &errTrackingReader{r: plain}

	// Pass the URI through a private config file so credentials never appear in argv.
	// Connection timeouts are defaulted there; the augmented URI is never logged.
	configArg, cleanupConfig, err := mongotools.WriteURIConfig("", mongotools.WithConnectionDefaults(mongoURI))
	if err != nil {
		return failRestore(record, fmt.Errorf("prepare mongorestore config: %w", err),
			fmt.Sprintf("prepare mongorestore config: %v", err))
	}
	defer cleanupConfig()

	args := e.buildRestoreArgs(configArg, sourceRecord.Database, targetDB, req, isGzip)
	if e.bypassValidation(ctx, mongoURI, targetDB) {
		// A backup is restored as it was taken: documents that predate a collection's
		// validator (or were written with validationAction "warn") would otherwise be
		// rejected by it and missing from the target.
		args = append(args, "--bypassDocumentValidation")
	}

	stderr, wait, err := e.runner(ctx, "mongorestore", input, args...)
	if err != nil {
		return failRestore(record, fmt.Errorf("start mongorestore: %w", err), fmt.Sprintf("start mongorestore: %v", err))
	}

	// Capture the end of stderr (bounded) for status and error diagnostics.
	stderrBuf := &mongotools.TailBuffer{}
	_, _ = io.Copy(stderrBuf, stderr)
	stderrLogs := stderrBuf.String()
	waitErr := wait()

	finishTime := time.Now().UTC()
	record.CompletedAt = &finishTime
	record.DurationSeconds = finishTime.Sub(startTime).Seconds()

	partialNote := ""
	if !req.DryRun {
		partialNote = fmt.Sprintf("; partial data may have been applied to target namespace %s.*", targetDB)
	}
	// dropClone removes the partial safe clone of a failed restore and says so: it was
	// created by this restore (see ErrCloneExists) and cannot be trusted. In-place
	// targets are never dropped, nor clones with failed documents (kept for inspection).
	dropClone := func() string {
		if !clone || e.admin == nil {
			return partialNote
		}
		dropCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cloneDropTimeout)
		defer cancel()
		if err := e.admin.DropDatabase(dropCtx, mongoURI, targetDB); err != nil {
			e.logger.Warn("failed to drop the safe clone of a failed restore",
				slog.String("restore_id", restoreID), slog.String("target_db", targetDB),
				slog.String("error", redact.Text(err.Error())))
			return fmt.Sprintf("; dropping the partially restored clone %s failed (%v), drop it manually", targetDB, err)
		}
		return fmt.Sprintf("; the partially restored clone %s was dropped", targetDB)
	}

	// An aborted run (timeout, cancellation) may have stopped mongorestore midway.
	if ctx.Err() != nil {
		abortErr := e.withCause(ctx, ctx.Err())
		return failRestore(record, fmt.Errorf("restore aborted: %w", abortErr),
			fmt.Sprintf("restore aborted: %v%s", abortErr, dropClone()))
	}

	// A mid-stream read or authentication failure means mongorestore saw truncated
	// input; report it as the root cause regardless of mongorestore's exit status.
	if streamErr := input.Err(); streamErr != nil {
		stage := "read backup stream"
		if errors.Is(streamErr, encryption.ErrDecryptionFailed) {
			stage = "decrypt backup stream"
		}
		return failRestore(record, fmt.Errorf("%s: %w", stage, streamErr),
			fmt.Sprintf("%s: %v%s", stage, streamErr, dropClone()))
	}

	if waitErr != nil {
		tail := stderrTail(stderrLogs)
		return failRestore(record, fmt.Errorf("mongorestore failed: %w (stderr: %s)", waitErr, tail),
			fmt.Sprintf("mongorestore failed: %v%s, stderr: %s", waitErr, dropClone(), tail))
	}

	// mongorestore has exited and os/exec has finished copying its stdin, so the
	// stream is no longer read concurrently: hash what it did not consume and compare.
	if err := verifyStreamed(ctx, hashed, sourceRecord.SHA256); err != nil {
		err = e.withCause(ctx, err)
		note := fmt.Sprintf("; the data restored into %s.* does not match the backup and must not be trusted", targetDB)
		if clone && e.admin != nil {
			note = dropClone()
		}
		return failRestore(record, fmt.Errorf("restore %s: %w", req.BackupID, err), err.Error()+note)
	}

	failed, ok := failedDocuments(stderrLogs)
	if failed > 0 {
		// The target is kept (a clone too) so the documents that did arrive can be
		// inspected; the record is failed.
		err := fmt.Errorf("%w: %d document(s) failed to restore", ErrDocumentsFailed, failed)
		return failRestore(record, err,
			fmt.Sprintf("%v (duplicate keys in a target that already held data, or documents rejected by a validator when the user lacks the bypassDocumentValidation privilege of the restore role)%s; mongorestore output: %s",
				err, partialNote, stderrTail(stderrLogs)))
	}
	if !ok && !req.DryRun {
		addWarning(record, "document counts unavailable: mongorestore printed no restored/failed summary")
		e.logger.Warn("mongorestore printed no document summary; document counts unavailable",
			slog.String("restore_id", restoreID), slog.String("target_db", targetDB))
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

// verifyFirst decides whether the artifact is verified in a first pass before
// mongorestore starts. In-place restores are always verified, whatever the policy or
// the request says, so a damaged artifact or a wrong key never reaches an existing
// database; clones follow req.ShouldVerify. A record without a checksum (from an
// older release) cannot be verified: an explicit request (Verify or the "always"
// policy) then fails with ErrChecksumUnavailable, while the implicit in-place
// verification is skipped with a warning on the record.
func (e *Engine) verifyFirst(req models.RestoreRequest, src *models.BackupRecord, record *models.RestoreRecord) bool {
	explicit := (req.Verify != nil && *req.Verify) || (req.Verify == nil && e.verifyPolicy == models.VerifyAlways)
	verify := req.ShouldVerify(e.verifyPolicy) || (req.InPlace() && !req.DryRun)
	if verify && !explicit && strings.TrimSpace(src.SHA256) == "" {
		addWarning(record, "backup record has no checksum (older release): the artifact was not verified before the in-place restore")
		e.logger.Warn("in-place restore of a backup without a recorded checksum; skipping verification",
			slog.String("restore_id", record.ID), slog.String("backup_id", src.ID))
		return false
	}
	return verify
}

// withCause tags err with ErrTimeout when ctx ended because the run hit its limit.
func (e *Engine) withCause(ctx context.Context, err error) error {
	if errors.Is(context.Cause(ctx), ErrTimeout) && !errors.Is(err, ErrTimeout) {
		return fmt.Errorf("%w (limit %s): %w", ErrTimeout, e.timeout, err)
	}
	return err
}

// addWarning appends w to the record's warnings.
func addWarning(record *models.RestoreRecord, w string) {
	if record.Warning != "" {
		record.Warning += "; "
	}
	record.Warning += w
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
	// Every stored byte passes the hash exactly once; the buffer only allows peeking.
	stored := bufio.NewReader(io.TeeReader(&ctxReader{ctx: ctx, r: stream}, h))

	encrypted := rec.Encrypted || keyEncrypted(rec.StorageKey)
	if !encrypted {
		if encrypted, err = e.detectUnrecordedEncryption(stored, rec); err != nil {
			return err
		}
	}
	if encrypted {
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

// KeyRequiredHint tells the operator how to restore an encrypted backup when no key
// material is configured. Errors wrapping encryption.ErrEncryptionKeyRequired from
// this package include it.
const KeyRequiredHint = "add the age identity (AGE-SECRET-KEY-1...) or the passphrase this backup was encrypted with under Settings → Encryption; without that key the backup cannot be restored"

// detectUnrecordedEncryption reports whether the artifact of a backup recorded as
// plaintext is in fact age-encrypted (a record that lost its encryption flag). It
// fails with encryption.ErrEncryptionKeyRequired when it is and no key is configured,
// so ciphertext never reaches mongorestore as if it were an archive.
func (e *Engine) detectUnrecordedEncryption(stored *bufio.Reader, rec *models.BackupRecord) (bool, error) {
	isAge, err := sniffEncrypted(stored)
	if err != nil {
		return false, fmt.Errorf("read backup stream: %w", err)
	}
	if !isAge {
		return false, nil
	}
	e.logger.Warn("backup recorded as unencrypted holds an age-encrypted artifact; decrypting it",
		slog.String("backup_id", rec.ID))
	if e.decryptor == nil {
		return false, fmt.Errorf("%w: %s", encryption.ErrEncryptionKeyRequired, KeyRequiredHint)
	}
	return true, nil
}

// hashingReader hashes the bytes read through it. It is not safe for concurrent use.
type hashingReader struct {
	r io.Reader
	h hash.Hash
}

func (hr *hashingReader) Read(p []byte) (int, error) {
	n, err := hr.r.Read(p)
	if n > 0 {
		hr.h.Write(p[:n])
	}
	return n, err
}

// verifyStreamed reads the part of the stored artifact that mongorestore did not
// consume (normally nothing) through stored and compares the SHA-256 of all stored
// bytes with expected. Records without a checksum (from older releases) are not
// checked. A mismatch wraps ErrChecksumMismatch.
func verifyStreamed(ctx context.Context, stored *hashingReader, expected string) error {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return nil
	}
	if _, err := io.Copy(io.Discard, &ctxReader{ctx: ctx, r: stored}); err != nil {
		return fmt.Errorf("read backup stream for checksum: %w", err)
	}
	if actual := hex.EncodeToString(stored.h.Sum(nil)); !strings.EqualFold(actual, expected) {
		return fmt.Errorf("%w: recorded %s, streamed artifact %s", ErrChecksumMismatch, expected, actual)
	}
	return nil
}

// failedDocumentsPattern matches mongorestore's summary line, e.g.
// "10 document(s) restored successfully. 2 document(s) failed to restore."
var failedDocumentsPattern = regexp.MustCompile(`(\d+) document\(s\) failed to restore`)

// failedDocuments returns the number of documents mongorestore reported as failed; ok
// is false when its output has no summary line.
func failedDocuments(stderr string) (failed int64, ok bool) {
	m := failedDocumentsPattern.FindAllStringSubmatch(stderr, -1)
	if len(m) == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(m[len(m)-1][1], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// stderrTail returns the last lines of mongorestore's output, redacted and bounded
// (each line to mongotools.QuotedLineMax, so document key values are not stored at
// length).
func stderrTail(stderr string) string {
	return mongotools.QuoteTail(stderr)
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

// newProcessRunner returns the default runner: it resolves the tool with tools
// (failing with an error wrapping mongotools.ErrToolNotFound; the searched locations
// are logged, not returned) and starts it piping
// stdin and capturing stderr. The process runs in its own process group and is
// terminated (SIGTERM, then SIGKILL after mongotools.KillGracePeriod) together with
// any children when ctx is done.
func newProcessRunner(tools *mongotools.Resolver, logger *slog.Logger) ProcessRunner {
	return func(ctx context.Context, name string, stdin io.Reader, args ...string) (io.Reader, func() error, error) {
		path, err := tools.Resolve(name)
		if err != nil {
			mongotools.LogNotFound(ctx, logger, err)
			return nil, nil, err
		}
		return startProcess(ctx, path, stdin, args...)
	}
}

// startProcess starts the executable at path piping stdin and capturing stderr.
func startProcess(ctx context.Context, path string, stdin io.Reader, args ...string) (io.Reader, func() error, error) {
	cmd := mongotools.Command(ctx, path, args...)
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
