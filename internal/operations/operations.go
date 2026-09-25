// Package operations implements the backup and restore use cases shared by every
// delivery adapter: the REST API (internal/server) and the MCP server (internal/mcp)
// call the same methods, so validation, safety rules and bookkeeping cannot drift
// between them.
//
// The package owns no transport concerns. It starts long-running work through a
// runs.Manager (so it outlives the request that triggered it), persists records in
// the metadata store, publishes outcome events and reports expected failures as
// sentinel errors that adapters map to their own status codes.
package operations

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

// Sentinel errors. Adapters inspect them with errors.Is; the messages of the returned
// errors are safe to show to clients (they never contain credentials).
var (
	// ErrInvalid marks an invalid request (bad options, an unconfirmed in-place
	// restore, a missing backup_id).
	ErrInvalid = errors.New("operations: invalid request")
	// ErrNotFound marks a job, backup or restore that does not exist.
	ErrNotFound = errors.New("operations: not found")
	// ErrConnectionRequired is returned when a backup or restore names no connection.
	ErrConnectionRequired = errors.New("connection_id is required")
	// ErrUnknownConnection is returned when a request names a connection that does not
	// exist.
	ErrUnknownConnection = errors.New("unknown connection_id")
	// ErrUnknownStorageTarget is returned when a request names a storage target that
	// does not exist.
	ErrUnknownStorageTarget = errors.New("unknown storage_target_id")
	// ErrBusy is returned when the same backup or restore is already running. It
	// aliases runs.ErrBusy.
	ErrBusy = runs.ErrBusy
	// ErrShuttingDown is returned once the application stops accepting new runs. It
	// aliases runs.ErrShuttingDown.
	ErrShuttingDown = runs.ErrShuttingDown
	// ErrSchedulerUnavailable is returned by RunJob when no scheduler is configured.
	ErrSchedulerUnavailable = errors.New("scheduler not initialized")
	// ErrKeyRequired is returned when an encrypted backup is restored without a
	// decryption key. It aliases encryption.ErrEncryptionKeyRequired.
	ErrKeyRequired = encryption.ErrEncryptionKeyRequired
)

// persistTimeout bounds metadata writes of background runs after they finish.
const persistTimeout = 5 * time.Second

// BackupEngine prepares and executes backups (implemented by *backup.Engine).
type BackupEngine interface {
	// Prepare validates opts and returns the in-progress record.
	Prepare(opts models.BackupOptions) (*models.BackupRecord, error)
	// Execute runs the backup described by opts and record.
	Execute(ctx context.Context, opts models.BackupOptions, record *models.BackupRecord) (*models.BackupRecord, error)
}

// RestoreEngine prepares and executes restores (implemented by *restore.Engine).
type RestoreEngine interface {
	// CanDecrypt reports whether a decryption key is configured.
	CanDecrypt() bool
	// Prepare validates req and returns the in-progress record.
	Prepare(req models.RestoreRequest, source *models.BackupRecord) (*models.RestoreRecord, error)
	// Execute runs the restore described by req and record.
	Execute(ctx context.Context, req models.RestoreRequest, source *models.BackupRecord, record *models.RestoreRecord) (*models.RestoreRecord, error)
}

// JobRunner prepares and executes on-demand runs of scheduled jobs (implemented by
// *scheduler.Scheduler, which persists the final record and publishes the outcome).
type JobRunner interface {
	// PrepareJobRun loads the job and returns its in-progress record.
	PrepareJobRun(ctx context.Context, jobID string) (*models.Job, *models.BackupRecord, error)
	// ExecuteJobRun runs the prepared job.
	ExecuteJobRun(ctx context.Context, job *models.Job, record *models.BackupRecord) (*models.BackupRecord, error)
}

// Connections resolves managed MongoDB connections (implemented by
// *connections.Service).
type Connections interface {
	// Resolve returns the connection with its full URI (never shown to clients).
	Resolve(ctx context.Context, id string) (*models.Connection, error)
	// Get returns the connection with a redacted URI.
	Get(ctx context.Context, id string) (*models.Connection, error)
	// List returns every connection with redacted URIs.
	List(ctx context.Context) ([]*models.Connection, error)
}

// Targets resolves storage targets (implemented by *targets.Service).
type Targets interface {
	// Resolve returns target id, or the default target for "".
	Resolve(ctx context.Context, id string) (*models.StorageTarget, error)
	// List returns every target with masked secrets.
	List(ctx context.Context) ([]*models.StorageTarget, error)
}

// Config holds the dependencies of a Service. Store, Backup, Restore and Runs are
// required; the others are optional (nil disables the features that need them).
type Config struct {
	// Store persists jobs and backup and restore records.
	Store store.Store
	// Backup runs backups.
	Backup BackupEngine
	// Restore runs restores.
	Restore RestoreEngine
	// Jobs runs scheduled jobs on demand; nil makes RunJob fail with
	// ErrSchedulerUnavailable.
	Jobs JobRunner
	// Runs owns the background runs and their per-database concurrency keys.
	Runs *runs.Manager
	// Connections resolves connections; nil means none are configured.
	Connections Connections
	// Targets resolves storage targets; nil means an anonymous local target served
	// by the engines' fixed storage driver.
	Targets Targets
	// Settings returns the live settings; nil means the defaults.
	Settings func() settings.Settings
	// Publisher receives backup and restore outcome events; nil disables them.
	Publisher events.Publisher
	// Logger receives operational logs; nil means slog.Default().
	Logger *slog.Logger
	// Version is the build version reported by Status.
	Version string
}

// Service implements the backup and restore use cases. It is safe for concurrent use.
type Service struct {
	cfg    Config
	logger *slog.Logger
	now    func() time.Time
}

// New returns a Service. It panics when a required dependency is missing, which is a
// wiring bug.
func New(cfg Config) *Service {
	if cfg.Store == nil || cfg.Backup == nil || cfg.Restore == nil || cfg.Runs == nil {
		panic("operations: Store, Backup, Restore and Runs are required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{cfg: cfg, logger: logger, now: time.Now}
}

// settings returns the live settings or the defaults.
func (s *Service) settings() settings.Settings {
	if s.cfg.Settings == nil {
		return settings.Defaults()
	}
	return s.cfg.Settings()
}

// publicError is an error whose message is shown to clients as-is while errors.Is
// still finds the sentinels it wraps.
type publicError struct {
	msg  string
	errs []error
}

// Error implements error.
func (e *publicError) Error() string { return e.msg }

// Unwrap exposes the wrapped sentinels and causes.
func (e *publicError) Unwrap() []error { return e.errs }

// public returns an error with message msg that matches every one of errs.
func public(msg string, errs ...error) error {
	return &publicError{msg: msg, errs: errs}
}

// invalid marks err as an invalid request, keeping its message.
func invalid(err error) error {
	return public(err.Error(), ErrInvalid, err)
}

// BackupRequest starts an on-demand backup. An omitted Gzip takes the
// general.default_gzip setting.
type BackupRequest struct {
	models.BackupOptions
	// Gzip overrides the default compression when set.
	Gzip *bool `json:"gzip"`
}

// StartBackup validates req, persists the in-progress record and runs the backup in
// the background. It returns a snapshot of the in-progress record; poll GetBackup for
// the outcome. Expected failures: ErrConnectionRequired, ErrUnknownConnection,
// ErrUnknownStorageTarget, ErrInvalid, ErrBusy and ErrShuttingDown.
func (s *Service) StartBackup(ctx context.Context, req BackupRequest) (*models.BackupRecord, error) {
	opts := req.BackupOptions
	conn, err := s.ResolveConnection(ctx, opts.ConnectionID)
	if err != nil {
		return nil, err
	}
	opts.MongoURI, opts.ConnectionName = conn.URI, conn.Name
	target, err := s.ResolveTarget(ctx, opts.StorageTargetID)
	if err != nil {
		return nil, err
	}
	opts.StorageTargetID, opts.StorageTargetName, opts.StorageType = target.ID, target.Name, target.Type
	opts.Gzip = derefOr(req.Gzip, s.settings().General.DefaultGzip)

	record, err := s.cfg.Backup.Prepare(opts)
	if err != nil {
		return nil, invalid(err)
	}
	jobID := s.knownJobID(ctx, opts.JobID)
	return s.startBackup(ctx, record, func(runCtx context.Context) {
		final, runErr := s.cfg.Backup.Execute(runCtx, opts, record)
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(runCtx), persistTimeout)
		defer cancel()
		if saveErr := s.cfg.Store.SaveBackupRecord(persistCtx, final); saveErr != nil {
			s.logger.Error("failed to persist backup metadata record",
				slog.String("backup_id", final.ID),
				slog.Any("error", saveErr),
			)
		}
		s.publish(persistCtx, events.BackupEvent(final, runErr, jobID, opts.Database))
	})
}

// RunJob runs the stored job jobID now, in the background, and returns a snapshot of
// the in-progress record. Expected failures: ErrSchedulerUnavailable, ErrNotFound,
// ErrInvalid, ErrBusy and ErrShuttingDown.
func (s *Service) RunJob(ctx context.Context, jobID string) (*models.BackupRecord, error) {
	if s.cfg.Jobs == nil {
		return nil, ErrSchedulerUnavailable
	}
	// Lookup-based: any stored job (including legacy-format IDs) may be triggered.
	job, record, err := s.cfg.Jobs.PrepareJobRun(ctx, jobID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, public("job not found", ErrNotFound, err)
		}
		return nil, invalid(err)
	}
	// The scheduler persists the final record and publishes the outcome event.
	return s.startBackup(ctx, record, func(runCtx context.Context) {
		_, _ = s.cfg.Jobs.ExecuteJobRun(runCtx, job, record)
	})
}

// startBackup reserves the database, persists the in-progress record and runs execute
// in the background under the application lifecycle (never the request context).
func (s *Service) startBackup(ctx context.Context, record *models.BackupRecord, execute func(ctx context.Context)) (*models.BackupRecord, error) {
	release, err := s.cfg.Runs.Acquire(runs.BackupKey(record.ConnectionID, record.Database))
	if err != nil {
		return nil, runError(err, "a backup of database "+record.Database+" is already running")
	}
	snapshot := *record
	if err := s.cfg.Store.SaveBackupRecord(ctx, &snapshot); err != nil {
		release()
		return nil, fmt.Errorf("save backup record: %w", err)
	}
	if err := s.cfg.Runs.Go("", func(runCtx context.Context) {
		defer release()
		execute(runCtx)
	}); err != nil {
		release()
		s.abandonBackup(ctx, &snapshot, err)
		return nil, runError(err, "")
	}
	return &snapshot, nil
}

// abandonBackup marks a persisted in-progress record failed when its run never started.
func (s *Service) abandonBackup(ctx context.Context, record *models.BackupRecord, cause error) {
	record.Status = models.StatusFailed
	record.ErrorMessage = "backup not started: " + cause.Error()
	if err := s.cfg.Store.SaveBackupRecord(context.WithoutCancel(ctx), record); err != nil {
		s.logger.Error("failed to persist abandoned backup record", slog.String("backup_id", record.ID), slog.Any("error", err))
	}
}

// runError turns a runs.Manager error into a client-facing error.
func runError(err error, busyMessage string) error {
	switch {
	case errors.Is(err, runs.ErrBusy):
		return public(busyMessage, err)
	case errors.Is(err, runs.ErrShuttingDown):
		return public("server is shutting down", err)
	default:
		return fmt.Errorf("start background run: %w", err)
	}
}

// StartRestore validates req, persists the in-progress record and runs the restore in
// the background. It returns a snapshot of the in-progress record; poll GetRestore for
// the outcome.
//
// Restores go into a safe clone unless req explicitly asks for an in-place restore,
// which must be confirmed (models.ErrInPlaceNotConfirmed, an ErrInvalid) and needs a
// principal with the admin scope in ctx (auth.ErrForbidden). Other expected failures:
// ErrNotFound, ErrConnectionRequired, ErrUnknownConnection, ErrKeyRequired, ErrBusy
// and ErrShuttingDown.
func (s *Service) StartRestore(ctx context.Context, req models.RestoreRequest) (*models.RestoreRecord, error) {
	if req.BackupID == "" {
		return nil, public("backup_id required", ErrInvalid)
	}
	if err := req.ValidateTarget(); err != nil {
		return nil, invalid(err)
	}
	// Restores need operator (checked by the adapters); overwriting existing data in
	// place needs admin.
	if req.InPlace() {
		if err := auth.RequireScope(ctx, auth.ScopeAdmin); err != nil {
			return nil, fmt.Errorf("in-place restores need an admin API key or a session: %w", err)
		}
	}

	// Lookup-based: any stored backup (including legacy-format IDs) may be restored.
	source, err := s.cfg.Store.GetBackupRecord(ctx, req.BackupID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, public("source backup not found", ErrNotFound, err)
		}
		return nil, fmt.Errorf("load source backup: %w", err)
	}

	// The target defaults to the server the backup was taken from; another connection
	// restores across servers.
	targetID := req.TargetConnectionID
	if targetID == "" {
		targetID = source.ConnectionID
	}
	target, err := s.ResolveConnection(ctx, targetID)
	if err != nil {
		if errors.Is(err, ErrConnectionRequired) {
			err = fmt.Errorf("%w: the backup has no connection, choose a target_connection_id", ErrConnectionRequired)
		}
		return nil, err
	}
	req.TargetConnectionID, req.TargetConnectionName, req.MongoURI = target.ID, target.Name, target.URI

	// Key material is checked synchronously so the client learns about it immediately.
	if source.Encrypted && !s.cfg.Restore.CanDecrypt() {
		return nil, ErrKeyRequired
	}

	record, err := s.cfg.Restore.Prepare(req, source)
	if err != nil {
		return nil, public(redact.Text(err.Error()), ErrInvalid, err)
	}
	record.SourceConnectionID, record.SourceConnectionName = source.ConnectionID, source.ConnectionName
	if source.ConnectionID != "" && s.cfg.Connections != nil {
		if src, getErr := s.cfg.Connections.Get(ctx, source.ConnectionID); getErr == nil {
			record.SourceConnectionName = src.Name
		}
	}

	release, err := s.cfg.Runs.Acquire(runs.RestoreKey(record.TargetConnectionID, record.TargetDatabase))
	if err != nil {
		return nil, runError(err, "a restore into database "+record.TargetDatabase+" is already running")
	}
	snapshot := *record
	if err := s.cfg.Store.SaveRestoreRecord(ctx, &snapshot); err != nil {
		release()
		return nil, fmt.Errorf("save restore record: %w", err)
	}

	if err := s.cfg.Runs.Go("", func(runCtx context.Context) {
		defer release()
		final, runErr := s.cfg.Restore.Execute(runCtx, req, source, record)
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(runCtx), persistTimeout)
		defer cancel()
		if saveErr := s.cfg.Store.SaveRestoreRecord(persistCtx, final); saveErr != nil {
			s.logger.Error("failed to persist restore metadata record",
				slog.String("restore_id", final.ID),
				slog.Any("error", saveErr),
			)
		}
		s.publish(persistCtx, events.RestoreEvent(final, runErr, req.BackupID))
	}); err != nil {
		release()
		snapshot.Status = models.RestoreStatusFailed
		snapshot.ErrorMessage = "restore not started: " + err.Error()
		if saveErr := s.cfg.Store.SaveRestoreRecord(context.WithoutCancel(ctx), &snapshot); saveErr != nil {
			s.logger.Error("failed to persist abandoned restore record", slog.String("restore_id", snapshot.ID), slog.Any("error", saveErr))
		}
		return nil, runError(err, "")
	}
	return &snapshot, nil
}

// ResolveConnection returns connection id with its full URI. It returns
// ErrConnectionRequired for an empty id and ErrUnknownConnection for a missing one.
func (s *Service) ResolveConnection(ctx context.Context, id string) (*models.Connection, error) {
	if id == "" {
		return nil, ErrConnectionRequired
	}
	if s.cfg.Connections == nil {
		return nil, fmt.Errorf("%w: connections are not configured", ErrUnknownConnection)
	}
	c, err := s.cfg.Connections.Resolve(ctx, id)
	if errors.Is(err, connections.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrUnknownConnection, id)
	}
	return c, err
}

// ResolveTarget returns target id (the default target for ""). Without a targets
// service it returns an anonymous local target served by the fixed storage driver.
func (s *Service) ResolveTarget(ctx context.Context, id string) (*models.StorageTarget, error) {
	if s.cfg.Targets == nil {
		return &models.StorageTarget{Type: models.StorageLocal}, nil
	}
	t, err := s.cfg.Targets.Resolve(ctx, id)
	if errors.Is(err, targets.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrUnknownStorageTarget, id)
	}
	return t, err
}

// knownJobID returns id when it names a stored job and "" otherwise, so arbitrary
// client-supplied job IDs on manual backups cannot inflate metric label cardinality
// or spoof rule matching.
func (s *Service) knownJobID(ctx context.Context, id string) string {
	if id == "" {
		return ""
	}
	if _, err := s.cfg.Store.GetJob(ctx, id); err != nil {
		return ""
	}
	return id
}

// publish emits e if a publisher is configured. It never blocks.
func (s *Service) publish(ctx context.Context, e events.Event) {
	if s.cfg.Publisher != nil {
		s.cfg.Publisher.Publish(ctx, e)
	}
}

// derefOr returns *p, or def when p is nil.
func derefOr[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}
