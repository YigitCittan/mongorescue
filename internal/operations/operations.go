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
	"slices"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/storage"
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
	// ErrNotRetryable is returned by RetryBackup for a backup that has not failed.
	ErrNotRetryable = errors.New("operations: only failed backups can be retried")
	// ErrRetryUnavailable is returned by RetryBackup when the failed backup cannot be
	// repeated as recorded: its connection or storage target no longer exists, or it
	// has no connection recorded.
	ErrRetryUnavailable = errors.New("operations: backup cannot be retried as recorded")
	// ErrDatabaseListing is returned when the databases of a job's connection cannot
	// be listed to resolve its selection (adapters answer 502 Bad Gateway). It aliases
	// scheduler.ErrDatabaseListing.
	ErrDatabaseListing = scheduler.ErrDatabaseListing
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
// *scheduler.Scheduler, which persists the final records and the run and publishes
// the outcome).
type JobRunner interface {
	// PrepareAdHocRun plans a run of several databases started without a job.
	PrepareAdHocRun(req scheduler.AdHocRun) (*scheduler.JobRunPlan, error)
	// PrepareJobRun loads the job and plans an on-demand run started by trigger
	// (models.TriggerOnDemand or models.TriggerMCP): one in-progress record per
	// database.
	PrepareJobRun(ctx context.Context, jobID string, trigger models.BackupTrigger) (*scheduler.JobRunPlan, error)
	// BeginJobRun stores, locks and tracks a multi-database or ad-hoc plan (a no-op
	// for a single-database one, whose caller does that).
	BeginJobRun(ctx context.Context, plan *scheduler.JobRunPlan) error
	// ExecuteJobRun runs the prepared plan.
	ExecuteJobRun(ctx context.Context, plan *scheduler.JobRunPlan) (*models.JobRun, error)
	// AbandonJobRun records a begun multi-database plan that could not start.
	AbandonJobRun(ctx context.Context, plan *scheduler.JobRunPlan, cause error)
	// ResolveJobDatabases resolves a job's database selection on its connection now,
	// exactly as a run does.
	ResolveJobDatabases(ctx context.Context, job *models.Job) (*models.DatabaseResolution, error)
	// StartJobRun starts a run of a multi-database job through spawn and returns its
	// running JobRun at once; the databases are resolved in the background.
	StartJobRun(ctx context.Context, jobID string, trigger models.BackupTrigger, spawn func(func(context.Context)) error) (*models.JobRun, error)
}

// JobStart is what RunJob started: the in-progress backup of a single-database job,
// or the run of a multi-database job, whose databases are resolved and backed up in
// the background.
type JobStart struct {
	// Backup is the backup of a single-database job.
	Backup *models.BackupRecord
	// Run is the run of a multi-database job (status running).
	Run *models.JobRun
}

// ID returns the backup's or the run's ID.
func (j *JobStart) ID() string {
	if j.Backup != nil {
		return j.Backup.ID
	}
	return j.Run.ID
}

// Body returns what the REST API answers: the backup, or the run.
func (j *JobStart) Body() any {
	if j.Backup != nil {
		return j.Backup
	}
	return j.Run
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

// Auditor records audit entries (implemented by *audit.Service).
type Auditor interface {
	// Record stores e; it never fails the caller.
	Record(ctx context.Context, e audit.Entry)
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
	// Scheduler reschedules jobs after UpdateJob; nil means an updated job is only
	// stored (the schedule takes effect when the scheduler next loads it).
	Scheduler JobScheduler
	// Runs owns the background runs and their per-database concurrency keys.
	Runs *runs.Manager
	// Registry tracks the active runs: cancellation, live progress and run logs; nil
	// disables those features (cancelling then answers ErrNotRunning).
	Registry *runs.Registry
	// Connections resolves connections; nil means none are configured.
	Connections Connections
	// Targets resolves storage targets; nil means an anonymous local target served
	// by the engines' fixed storage driver.
	Targets Targets
	// Storage returns the storage driver of a storage target (the default target for
	// ""), used to delete archives. nil means archives cannot be deleted: deleting a
	// backup then removes only its record and reports why the archive stayed.
	Storage func(ctx context.Context, targetID string) (storage.Storage, error)
	// Audit receives one entry per bulk operation (real runs); nil disables them.
	Audit Auditor
	// OnJobDeleted is called after a job has been deleted (for example to drop its
	// metric series); nil disables it.
	OnJobDeleted func(jobID string)
	// Settings returns the live settings; nil means the defaults.
	Settings func() settings.Settings
	// SettingsUpdater applies settings changes for UpdateSettings and for the
	// delayed or approved changes of the delete protection; nil makes UpdateSettings
	// fail with ErrUnavailable.
	SettingsUpdater SettingsUpdater
	// SecondApproverCheck returns an error (auth.ErrTooFewAdmins) unless the
	// two-person rule can be turned on (implemented by
	// auth.Service.CheckSecondApproverPossible); nil refuses turning it on.
	SecondApproverCheck func(ctx context.Context) error
	// Users grants the admin rights that the two-person rule held back once they are
	// approved (implemented by *auth.Service, which calls HoldsAdminGrants and
	// RequestAdminGrant through auth.Service.SetAdminGrantGate); nil makes such
	// approvals fail.
	Users UserAdmin
	// Publisher receives backup and restore outcome events; nil disables them.
	Publisher events.Publisher
	// Verifier verifies archives on demand; nil makes VerifyBackup fail with
	// ErrUnavailable.
	Verifier Verifier
	// Inspector inspects the target server of restores: StartRestore runs a preflight
	// with it and verifies restored databases. nil skips both (PreflightRestore then
	// reports the server checks as not checked).
	Inspector RestoreInspector
	// Logger receives operational logs; nil means slog.Default().
	Logger *slog.Logger
	// Version is the build version reported by Status.
	Version string
	// PreviewTimeout bounds reading the collection list of an archive in
	// ListBackupCollections; 0 means ArchivePreviewTimeout.
	PreviewTimeout time.Duration
}

// Service implements the backup and restore use cases. It is safe for concurrent use.
type Service struct {
	cfg    Config
	logger *slog.Logger
	now    func() time.Time

	// archiveCache holds the archive collection lists of ListBackupCollections.
	archiveCache *collectionCache
	// previewSlots bounds the archive previews read concurrently.
	previewSlots chan struct{}

	// bulk is the registry of bulk actions, keyed by resource and action name.
	bulk map[BulkResource]map[string]BulkAction
	// bulkCap is the most items of one bulk operation (MaxBulkItems; lowered by tests).
	bulkCap int
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
	s := &Service{
		cfg: cfg, logger: logger, now: time.Now,
		archiveCache: newCollectionCache(archiveCacheSize), previewSlots: make(chan struct{}, maxConcurrentPreviews),
		bulkCap: MaxBulkItems,
	}
	s.registerBulkActions()
	return s
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

// BackupRequest starts an on-demand backup of Database (StartBackup), or of every
// one of Databases in one run (StartBackups). An omitted Gzip takes the
// general.default_gzip setting.
type BackupRequest struct {
	models.BackupOptions
	// Gzip overrides the default compression when set.
	Gzip *bool `json:"gzip"`
	// Databases names the databases of a run of several (StartBackups); it cannot be
	// combined with Database. An entry is a database name or an object with its own
	// collection filter (see models.DatabaseFilter), so ["a", "b"] stays valid.
	Databases []models.DatabaseFilter `json:"databases,omitempty"`
	// Parallelism is how many of Databases are backed up at once (0 or omitted
	// means 1, as for jobs; at most models.MaxJobParallelism). A single backup
	// (Database) ignores it.
	Parallelism *int `json:"parallelism,omitempty"`
	// Trigger is set by the adapter: models.TriggerMCP for MCP, anything else is
	// recorded as models.TriggerManual. It is never read from clients.
	Trigger models.BackupTrigger `json:"-"`
}

// StartBackup validates req, persists the in-progress record and runs the backup in
// the background. It returns a snapshot of the in-progress record; poll GetBackup for
// the outcome. Expected failures: ErrConnectionRequired, ErrUnknownConnection,
// ErrUnknownStorageTarget, ErrInvalid, ErrBusy and ErrShuttingDown.
func (s *Service) StartBackup(ctx context.Context, req BackupRequest) (*models.BackupRecord, error) {
	if req.Databases != nil {
		return nil, invalid(fmt.Errorf("databases: %w", ErrDatabasesConflict))
	}
	// Parallelism only applies to several databases: a single backup ignores it, as
	// it always did.
	if strings.TrimSpace(req.Database) != "" {
		if err := validateNamespaces(req.Database, req.Collections, req.ExcludeCollections); err != nil {
			return nil, err
		}
	}
	if req.IncludeUsersAndRoles && strings.TrimSpace(req.Database) == models.AdminDatabase {
		return nil, invalid(ErrUsersAndRolesAdmin)
	}
	return s.startManualBackup(ctx, req, "")
}

// validateNamespaces checks client-supplied database and collection names (see
// models.ErrInvalidNamespace), as an ErrInvalid error.
func validateNamespaces(database string, collections, excluded []string) error {
	if err := models.ValidateDatabaseName(database); err != nil {
		return invalid(err)
	}
	if err := models.ValidateCollectionNames(collections); err != nil {
		return invalid(fmt.Errorf("collections: %w", err))
	}
	if err := models.ValidateCollectionNames(excluded); err != nil {
		return invalid(fmt.Errorf("exclude_collections: %w", err))
	}
	return nil
}

// RetryBackup starts a new backup with the parameters of the failed backup id: the
// same connection, database, collections and storage target, the job's excluded
// collections and compression when the backup belongs to a job that still exists, and
// otherwise the compression recorded in its storage key. It runs exactly like
// StartBackup (same concurrency keys, events and bookkeeping) and returns a snapshot
// of the new in-progress record, whose RetryOf is id. The failed record is never
// modified. trigger is models.TriggerMCP for MCP and otherwise recorded as
// models.TriggerManual. Expected failures: ErrNotFound, ErrNotRetryable,
// ErrRetryUnavailable, ErrInvalid, ErrBusy and ErrShuttingDown.
func (s *Service) RetryBackup(ctx context.Context, id string, trigger models.BackupTrigger) (*models.BackupRecord, error) {
	original, err := s.cfg.Store.GetBackupRecord(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, public("backup not found", ErrNotFound, err)
		}
		return nil, fmt.Errorf("load backup: %w", err)
	}
	if original.Status != models.StatusFailed {
		return nil, public(fmt.Sprintf("only failed backups can be retried; backup %s is %s", original.ID, original.Status), ErrNotRetryable)
	}
	if original.ConnectionID == "" {
		return nil, public("this backup has no source connection recorded; start a new backup instead", ErrRetryUnavailable, ErrConnectionRequired)
	}

	req := BackupRequest{
		BackupOptions: models.BackupOptions{
			Database:        original.Database,
			Collections:     slices.Clone(original.Collections),
			StorageTargetID: original.StorageTargetID,
			ConnectionID:    original.ConnectionID,
			// A failed backup records whether it was meant to include users and roles.
			IncludeUsersAndRoles: original.UsersAndRoles,
		},
		Trigger: trigger,
	}
	if gzip, ok := gzipFromKey(original.StorageKey); ok {
		req.Gzip = &gzip
	}
	if original.JobID != "" {
		if job, jobErr := s.cfg.Store.GetJob(ctx, original.JobID); jobErr == nil {
			req.JobID = job.ID
			req.ExcludeCollections = slices.Clone(job.ExcludeCollections)
			req.IncludeUsersAndRoles = job.IncludeUsersAndRoles
			gzip := job.Gzip
			req.Gzip = &gzip
		}
	}

	record, err := s.startManualBackup(ctx, req, original.ID)
	switch {
	case errors.Is(err, ErrUnknownConnection):
		return nil, public("the connection of backup "+original.ID+" no longer exists", ErrRetryUnavailable, err)
	case errors.Is(err, ErrUnknownStorageTarget):
		return nil, public("the storage target of backup "+original.ID+" no longer exists", ErrRetryUnavailable, err)
	}
	return record, err
}

// gzipFromKey reports whether the archive at a default storage key was compressed
// (".archive.gz", optionally encrypted). ok is false for custom keys.
func gzipFromKey(key string) (gzip, ok bool) {
	key = strings.TrimSuffix(key, encryption.FileExtension)
	switch {
	case strings.HasSuffix(key, ".archive.gz"):
		return true, true
	case strings.HasSuffix(key, ".archive"):
		return false, true
	default:
		return false, false
	}
}

// manualOptions returns the backup options of an on-demand backup request: its
// connection and storage target resolved, its compression and its trigger.
func (s *Service) manualOptions(ctx context.Context, req BackupRequest) (models.BackupOptions, error) {
	opts := req.BackupOptions
	conn, err := s.ResolveConnection(ctx, opts.ConnectionID)
	if err != nil {
		return opts, err
	}
	opts.MongoURI, opts.ConnectionName = conn.URI, conn.Name
	target, err := s.ResolveTarget(ctx, opts.StorageTargetID)
	if err != nil {
		return opts, err
	}
	opts.StorageTargetID, opts.StorageTargetName, opts.StorageType = target.ID, target.Name, target.Type
	opts.Gzip = derefOr(req.Gzip, s.settings().General.DefaultGzip)
	opts.Trigger = models.TriggerManual
	if req.Trigger == models.TriggerMCP {
		opts.Trigger = models.TriggerMCP
	}
	return opts, nil
}

// startManualBackup implements StartBackup; a non-empty retryOf is recorded as the
// new record's RetryOf.
func (s *Service) startManualBackup(ctx context.Context, req BackupRequest, retryOf string) (*models.BackupRecord, error) {
	opts, err := s.manualOptions(ctx, req)
	if err != nil {
		return nil, err
	}

	record, err := s.cfg.Backup.Prepare(opts)
	if err != nil {
		return nil, invalid(err)
	}
	record.RetryOf = retryOf
	jobID := s.knownJobID(ctx, opts.JobID)
	return s.startBackup(ctx, record, func(runCtx context.Context) {
		final, runErr := s.cfg.Backup.Execute(runCtx, opts, record)
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(runCtx), persistTimeout)
		defer cancel()
		if saveErr := s.cfg.Store.SaveBackupRecord(persistCtx, final); saveErr != nil {
			s.logger.Error("failed to persist backup metadata record",
				logsafe.Attr("backup_id", final.ID),
				logsafe.Error(saveErr),
			)
		}
		s.publish(persistCtx, events.BackupEvent(final, runErr, jobID, opts.Database))
		if ve, ok := events.VerificationEvent(final, events.VerificationAfterUpload); ok {
			s.publish(persistCtx, ve)
		}
	})
}

// RunJob runs the stored job jobID now, in the background. For a single-database job
// it returns a snapshot of the in-progress backup record. A multi-database job
// returns at once with its run (status running): its databases are resolved and
// backed up in the background, each into its own record sharing the run's ID, and
// planning failures (databases that cannot be listed, none that exist) are recorded
// in the run. trigger is models.TriggerMCP for MCP and otherwise recorded as
// models.TriggerOnDemand; on-demand runs never apply or count towards retention.
// Expected failures: ErrSchedulerUnavailable, ErrNotFound, ErrInvalid, ErrBusy (also
// while a run of a multi-database job is going) and ErrShuttingDown.
func (s *Service) RunJob(ctx context.Context, jobID string, trigger models.BackupTrigger) (*JobStart, error) {
	if s.cfg.Jobs == nil {
		return nil, ErrSchedulerUnavailable
	}
	// Lookup-based: any stored job (including legacy-format IDs) may be triggered.
	job, err := s.cfg.Store.GetJob(ctx, jobID)
	if err != nil {
		return nil, jobRunError(err)
	}
	if job.MultiDatabase() {
		run, startErr := s.cfg.Jobs.StartJobRun(ctx, jobID, trigger, func(fn func(context.Context)) error {
			return s.cfg.Runs.Go("", fn)
		})
		if startErr != nil {
			if errors.Is(startErr, runs.ErrShuttingDown) {
				return nil, runError(startErr, "")
			}
			return nil, jobRunError(startErr)
		}
		return &JobStart{Run: run}, nil
	}
	plan, err := s.cfg.Jobs.PrepareJobRun(ctx, jobID, trigger)
	if err != nil {
		return nil, jobRunError(err)
	}
	// The scheduler persists the final record and publishes the outcome event.
	record, err := s.startBackup(ctx, plan.First(), func(runCtx context.Context) {
		_, _ = s.cfg.Jobs.ExecuteJobRun(runCtx, plan)
	})
	if err != nil {
		return nil, err
	}
	return &JobStart{Backup: record}, nil
}

// jobRunError maps an error of planning or beginning a job run to a client error.
func jobRunError(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return public("job not found", ErrNotFound, err)
	case errors.Is(err, runs.ErrBusy):
		return public(redact.Text(err.Error()), ErrBusy, err)
	case errors.Is(err, scheduler.ErrDatabaseListing):
		return public(redact.Text(err.Error()), ErrDatabaseListing, err)
	default:
		return invalid(err)
	}
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
	tracked := s.track(models.RunBackup, record.ID, record.JobID, record.Database)
	if err := s.cfg.Runs.Go("", func(runCtx context.Context) {
		defer release()
		defer tracked.End()
		execute(tracked.Bind(runCtx))
	}); err != nil {
		tracked.End()
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
		s.logger.Error("failed to persist abandoned backup record", logsafe.Attr("backup_id", record.ID), logsafe.Error(err))
	}
}

// runError turns a runs.Manager error into a client-facing error.
func runError(err error, busyMessage string) error {
	switch {
	case errors.Is(err, runs.ErrBusy):
		return public(busyMessage, err)
	case errors.Is(err, runs.ErrShuttingDown):
		return public("MongoRescue is shutting down", err)
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
// principal with the admin scope in ctx (auth.ErrForbidden). Restoring into another
// connection than the backup's (req.TargetConnectionID) needs admin too: an operator
// may only safe-clone into the server the backup was taken from.
//
// With an inspector configured, the checks of PreflightRestore run first and are
// recorded on the restore (RestoreRecord.Preflight): a failed check refuses the
// restore with a *PreflightError (ErrPreflightFailed) unless req.Force is set; dry
// runs and warnings are never refused. With req.VerifyRestore, a completed restore is
// compared with the backup's manifest (RestoreRecord.Verification); a failed
// verification adds a warning and emits restore.verification_failed. A dry run
// takes no lock on the target database (it writes nothing), so it never gets
// ErrBusy. Other expected
// failures: ErrNotFound, ErrConnectionRequired, ErrUnknownConnection, ErrKeyRequired,
// ErrBusy and ErrShuttingDown.
func (s *Service) StartRestore(ctx context.Context, req models.RestoreRequest) (*models.RestoreRecord, error) {
	asked := req
	plan, err := s.planRestore(ctx, req, false)
	if err != nil {
		return nil, err
	}
	req, source := plan.req, plan.source
	// An in-place restore that drops the target first overwrites live data: with the
	// two-person rule it waits for a second administrator.
	if req.InPlace() && req.DropTarget && !req.DryRun && s.needsApproval(ctx) {
		held := asked
		target := strings.TrimSpace(req.TargetDatabase)
		if target == "" {
			target = source.Database
		}
		return nil, s.requestApproval(ctx, &models.Approval{Action: models.ApprovalRestoreDropTarget, Subject: source.ID, Restore: &held,
			Summary: fmt.Sprintf("restore backup %s in place into database %s, dropping it first", source.ID, target)})
	}

	record, err := s.cfg.Restore.Prepare(req, source)
	if err != nil {
		return nil, public(redact.Text(err.Error()), ErrInvalid, err)
	}
	if s.cfg.Inspector != nil {
		pre := s.preflight(ctx, req, source, record.TargetDatabase)
		if !pre.OK && !req.Force && !req.DryRun {
			return nil, &PreflightError{Result: pre}
		}
		record.Preflight, record.Forced = pre, !pre.OK && req.Force
	}
	record.SourceConnectionID, record.SourceConnectionName = source.ConnectionID, source.ConnectionName
	if source.ConnectionID != "" && s.cfg.Connections != nil {
		if src, getErr := s.cfg.Connections.Get(ctx, source.ConnectionID); getErr == nil {
			record.SourceConnectionName = src.Name
		}
	}

	// A dry run never writes to the target, so it takes no lock on it: it neither
	// waits for nor blocks a restore into the same database.
	release := func() {}
	if !req.DryRun {
		release, err = s.cfg.Runs.Acquire(runs.RestoreKey(record.TargetConnectionID, record.TargetDatabase))
		if err != nil {
			return nil, runError(err, "a restore into database "+record.TargetDatabase+" is already running")
		}
	}
	snapshot := *record
	if err := s.cfg.Store.SaveRestoreRecord(ctx, &snapshot); err != nil {
		release()
		return nil, fmt.Errorf("save restore record: %w", err)
	}

	tracked := s.track(models.RunRestore, record.ID, "", record.TargetDatabase)
	if err := s.cfg.Runs.Go("", func(runCtx context.Context) {
		defer release()
		defer tracked.End()
		runCtx = tracked.Bind(runCtx)
		final, runErr := s.cfg.Restore.Execute(runCtx, req, source, record)
		if runErr == nil && req.VerifyRestore {
			s.verifyRestore(runCtx, req, source, final)
		}
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(runCtx), persistTimeout)
		defer cancel()
		// The outcome events are published before the final record is stored, so a
		// client that sees the restore finished (GET /api/v1/restores) can rely on its
		// events having been published. Publishing never blocks.
		s.publish(persistCtx, events.RestoreEvent(final, runErr, req.BackupID))
		if ve, ok := events.RestoreVerificationEvent(final); ok {
			s.publish(persistCtx, ve)
		}
		if saveErr := s.cfg.Store.SaveRestoreRecord(persistCtx, final); saveErr != nil {
			s.logger.Error("failed to persist restore metadata record",
				logsafe.Attr("restore_id", final.ID),
				logsafe.Error(saveErr),
			)
		}
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

// restorePlan is a validated restore request, resolved to its target connection, and
// the backup it restores.
type restorePlan struct {
	req    models.RestoreRequest
	source *models.BackupRecord
}

// planRestore validates req, applies the scope rules of StartRestore, loads the source
// backup and resolves the target connection into req. A preflight (forPreflight)
// leaves users-and-roles and decryption problems to its checks instead of refusing
// the request.
func (s *Service) planRestore(ctx context.Context, req models.RestoreRequest, forPreflight bool) (*restorePlan, error) {
	if req.BackupID == "" {
		return nil, public("backup_id required", ErrInvalid)
	}
	if err := req.ValidateTarget(); err != nil {
		return nil, invalid(err)
	}
	// Users and roles are restored only in place; the backup is checked below.
	if err := req.ValidateUsersAndRoles(nil); !forPreflight && errors.Is(err, models.ErrUsersAndRolesNotAllowed) {
		return nil, invalid(err)
	}
	// Client-supplied names are checked; the backup's own database name is not, so
	// every existing backup stays restorable.
	if target := strings.TrimSpace(req.TargetDatabase); req.InPlace() && target != "" {
		if err := models.ValidateDatabaseName(target); err != nil {
			return nil, invalid(fmt.Errorf("target_database: %w", err))
		}
	}
	if err := models.ValidateCollectionNames(req.SelectedCollections); err != nil {
		return nil, invalid(fmt.Errorf("selected_collections: %w", err))
	}
	// Restores need operator (checked by the adapters); overwriting existing data in
	// place needs admin.
	if req.InPlace() {
		if err := auth.RequireScope(ctx, auth.ScopeAdmin); err != nil {
			return nil, fmt.Errorf("in-place restores need the admin role or an admin API key: %w", err)
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
	// A deleted backup is not restorable during its grace period: undelete it first
	// (its archive is still there). A purged one has no archive any more.
	if source.Status.Deleted() {
		return nil, public(fmt.Sprintf("backup %s is %s; undelete it to restore it", source.ID, source.Status), ErrBackupDeleted)
	}
	if err = req.ValidateUsersAndRoles(source); err != nil && !forPreflight {
		return nil, invalid(err)
	}

	// The target defaults to the server the backup was taken from; another connection
	// restores across servers, which writes to a server the backup did not come from
	// and so needs admin.
	if req.TargetConnectionID != "" && req.TargetConnectionID != source.ConnectionID {
		if scopeErr := auth.RequireScope(ctx, auth.ScopeAdmin); scopeErr != nil {
			return nil, fmt.Errorf("restores into another connection than the backup's need the admin role or an admin API key: %w", scopeErr)
		}
	}
	targetID := req.TargetConnectionID
	if targetID == "" {
		targetID = source.ConnectionID
	}
	target, err := s.ResolveConnection(ctx, targetID)
	if err != nil {
		if errors.Is(err, ErrConnectionRequired) {
			// A legacy backup without a source connection can only be restored into a
			// connection named explicitly, which needs admin (see above).
			if auth.RequireScope(ctx, auth.ScopeAdmin) != nil {
				return nil, public("this backup has no source connection recorded; an admin must choose the target connection", ErrConnectionRequired, err)
			}
			return nil, public("this backup has no source connection recorded; choose the target connection (target_connection_id)", ErrConnectionRequired, err)
		}
		return nil, err
	}
	req.TargetConnectionID, req.TargetConnectionName, req.MongoURI = target.ID, target.Name, target.URI

	// Key material is checked synchronously so the client learns about it immediately.
	if !forPreflight && (source.Encrypted || strings.HasSuffix(source.StorageKey, encryption.FileExtension)) && !s.cfg.Restore.CanDecrypt() {
		return nil, fmt.Errorf("%w: backup %s is encrypted; %s", ErrKeyRequired, source.ID, restore.KeyRequiredHint)
	}
	return &restorePlan{req: req, source: source}, nil
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
