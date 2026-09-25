package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// Sentinel errors for scheduler lifecycle management. A Scheduler is single-use:
// it can be started successfully at most once and cannot be restarted after Stop.
var (
	// ErrAlreadyStarted is returned by Start when the scheduler is already running.
	ErrAlreadyStarted = errors.New("scheduler: already started")

	// ErrStopped is returned by Start when Stop has already been called.
	ErrStopped = errors.New("scheduler: stopped")

	// ErrNoConnection is returned when a job has no connection_id (for example a
	// legacy job that could not be migrated); edit the job to select a connection.
	ErrNoConnection = errors.New("scheduler: job has no connection; select one for the job")
)

// persistTimeout bounds post-run metadata writes, which run on a context detached from
// cancellation so that shutdown does not leave records stuck in an in-progress state.
const persistTimeout = 5 * time.Second

// Scheduler coordinates cron-based scheduled backups and automated retention pruning.
type Scheduler struct {
	cron          *cron.Cron
	metadataStore store.Store
	backupEngine  *backup.Engine
	storageDriver storage.Storage
	logger        *slog.Logger
	entries       map[string]cron.EntryID
	publisher     events.Publisher
	guard         BackupGuard
	connections   ConnectionResolver
	targets       StorageTargets

	// mu guards entries, ctx, cancel, started and stopped.
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	started bool
	stopped bool

	// inflight tracks cron-triggered executions so Stop can wait for them to finish.
	inflight sync.WaitGroup
}

// Option customises a Scheduler.
type Option func(*Scheduler)

// WithPublisher sets the port used to emit backup.succeeded / backup.failed events
// after every job run. Publishing never blocks or fails a backup.
func WithPublisher(p events.Publisher) Option {
	return func(s *Scheduler) { s.publisher = p }
}

// BackupGuard reserves the right to back up database on connection connectionID for
// the duration of a run. It returns a release function, or an error (e.g. runs.ErrBusy)
// when a backup of the same database is already running, in which case the scheduled
// run is skipped.
type BackupGuard func(connectionID, database string) (release func(), err error)

// ConnectionResolver returns a managed connection with its full URI.
type ConnectionResolver interface {
	// Resolve returns the connection or an error wrapping connections.ErrNotFound.
	Resolve(ctx context.Context, id string) (*models.Connection, error)
}

// WithConnectionResolver sets the port used to turn a job's connection_id into the
// connection string handed to the backup engine.
func WithConnectionResolver(r ConnectionResolver) Option {
	return func(s *Scheduler) { s.connections = r }
}

// StorageTargets resolves storage targets and their drivers.
type StorageTargets interface {
	// Resolve returns a target, or the default target for an empty id.
	Resolve(ctx context.Context, id string) (*models.StorageTarget, error)
	// Storage returns the driver of a target, or of the default target for an empty id.
	Storage(ctx context.Context, id string) (storage.Storage, error)
}

// WithStorageTargets makes job runs write to the job's storage target (the default
// target when the job names none) and retention delete from each backup's own target.
// Without it, the scheduler's fixed storage driver is used.
func WithStorageTargets(t StorageTargets) Option {
	return func(s *Scheduler) { s.targets = t }
}

// WithBackupGuard makes cron-triggered runs acquire guard first, so a scheduled run
// never overlaps a manual or previous run of the same database.
func WithBackupGuard(guard BackupGuard) Option {
	return func(s *Scheduler) { s.guard = guard }
}

// NewScheduler initializes a new Scheduler.
func NewScheduler(
	metaStore store.Store,
	engine *backup.Engine,
	storageDriver storage.Storage,
	logger *slog.Logger,
	opts ...Option,
) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}

	ctx, cancel := context.WithCancel(context.Background())

	s := &Scheduler{
		cron:          cron.New(cron.WithParser(cronParser)),
		metadataStore: metaStore,
		backupEngine:  engine,
		storageDriver: storageDriver,
		logger:        logger,
		entries:       make(map[string]cron.EntryID),
		ctx:           ctx,
		cancel:        cancel,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ActiveJobCount returns the number of jobs currently registered with cron.
func (s *Scheduler) ActiveJobCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// Start loads all active jobs from the metadata store, schedules them, and starts the cron worker.
// Cron-triggered backups run under a context derived from ctx and are cancelled by Stop.
// Start returns ErrStopped after Stop, and ErrAlreadyStarted once a previous call
// succeeded. If loading jobs fails, the scheduler stays unstarted and Start may be retried.
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stopped {
		return ErrStopped
	}
	if s.started {
		return ErrAlreadyStarted
	}

	parent := ctx
	if parent == nil {
		parent = context.Background()
	}
	runCtx, runCancel := context.WithCancel(parent)

	jobs, err := s.metadataStore.ListJobs(runCtx)
	if err != nil {
		runCancel()
		return fmt.Errorf("list scheduled jobs: %w", err)
	}

	// Commit the run context only once loading succeeded, releasing the placeholder.
	prevCancel := s.cancel
	s.ctx, s.cancel = runCtx, runCancel
	if prevCancel != nil {
		prevCancel()
	}
	s.started = true

	for _, job := range jobs {
		if job.Enabled {
			if err := s.registerJobLocked(job); err != nil {
				s.logger.Error("failed to register job schedule",
					slog.String("job_id", job.ID),
					slog.String("cron", job.CronExpression),
					slog.Any("error", err),
				)
			}
		}
	}

	s.cron.Start()
	s.logger.Info("backup scheduler started successfully",
		slog.Int("active_jobs", len(s.entries)),
	)

	return nil
}

// Stop stops the background cron runner, cancels any in-flight cron-triggered backup
// jobs, and blocks until they have finished persisting their results.
// It is safe to call Stop multiple times, and before Start.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	s.stopped = true
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()

	ctx := s.cron.Stop()
	<-ctx.Done()
	s.inflight.Wait()
	s.logger.Info("backup scheduler stopped")
}

// RegisterJob registers or updates a scheduled job.
func (s *Scheduler) RegisterJob(job *models.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.registerJobLocked(job)
}

// registerJobLocked registers a job with cron. Caller must hold s.mu.
func (s *Scheduler) registerJobLocked(job *models.Job) error {
	if job == nil || job.ID == "" {
		return errors.New("scheduler: invalid job")
	}

	// Remove existing schedule if already registered
	if entryID, exists := s.entries[job.ID]; exists {
		s.cron.Remove(entryID)
		delete(s.entries, job.ID)
	}

	if !job.Enabled {
		return nil
	}

	jobID := job.ID
	entryID, err := s.cron.AddFunc(job.CronExpression, func() {
		s.runScheduled(jobID)
	})
	if err != nil {
		return fmt.Errorf("invalid cron expression %q: %w", job.CronExpression, err)
	}

	s.entries[job.ID] = entryID

	// Update NextRun time in metadata store. The cron entry's Next field is
	// only populated once the runner has processed the entry, so compute it
	// from the schedule directly.
	job.NextRun = nextRun(job.CronExpression, time.Now())
	if err := s.metadataStore.UpdateJob(s.ctx, job); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.logger.Error("failed to update job next run metadata",
			slog.String("job_id", job.ID),
			slog.Any("error", err),
		)
	}

	s.logger.Info("scheduled backup job registered",
		slog.String("job_id", job.ID),
		slog.String("database", job.Database),
		slog.String("cron", job.CronExpression),
		slog.Any("next_run", job.NextRun),
	)

	return nil
}

// UnregisterJob removes a job from the active cron scheduler.
func (s *Scheduler) UnregisterJob(jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if entryID, exists := s.entries[jobID]; exists {
		s.cron.Remove(entryID)
		delete(s.entries, jobID)
		s.logger.Info("scheduled backup job unregistered", slog.String("job_id", jobID))
	}
}

// TriggerJob executes a job immediately on demand and waits for it to finish. Like
// every on-demand run it never applies retention (see ExecuteJobRun).
func (s *Scheduler) TriggerJob(ctx context.Context, jobID string) (*models.BackupRecord, error) {
	job, record, err := s.PrepareJobRun(ctx, jobID, models.TriggerOnDemand)
	if err != nil {
		return nil, err
	}
	return s.ExecuteJobRun(ctx, job, record)
}

// PrepareJobRun loads jobID and returns the job with the in-progress backup record of
// an on-demand run, without starting it. The record carries trigger (TriggerOnDemand
// or TriggerMCP; anything else, including TriggerScheduled, is recorded as
// TriggerOnDemand, so callers cannot make an on-demand run count for retention). It
// wraps store.ErrNotFound for unknown jobs. Pass both to ExecuteJobRun (typically in
// the background).
func (s *Scheduler) PrepareJobRun(ctx context.Context, jobID string, trigger models.BackupTrigger) (*models.Job, *models.BackupRecord, error) {
	job, err := s.metadataStore.GetJob(ctx, jobID)
	if err != nil {
		return nil, nil, fmt.Errorf("retrieve job %s: %w", jobID, err)
	}
	if trigger != models.TriggerMCP {
		trigger = models.TriggerOnDemand
	}
	opts, err := s.jobOptions(ctx, job, trigger)
	if err != nil {
		return nil, nil, fmt.Errorf("prepare job %s: %w", jobID, err)
	}
	record, err := s.backupEngine.Prepare(opts)
	if err != nil {
		return nil, nil, fmt.Errorf("prepare job %s: %w", jobID, err)
	}
	return job, record, nil
}

// ExecuteJobRun runs a job prepared by PrepareJobRun: it executes the backup, persists
// the record and the job's run timestamps and publishes the outcome event.
//
// On-demand runs never apply the job's retention policy: only cron-triggered runs
// prune, so a caller that may run jobs (an operator API key, an assistant) cannot
// delete good backups by running a job repeatedly.
func (s *Scheduler) ExecuteJobRun(ctx context.Context, job *models.Job, record *models.BackupRecord) (*models.BackupRecord, error) {
	s.logger.Info("running backup job",
		slog.String("job_id", job.ID),
		slog.String("database", job.Database),
	)
	opts, err := s.jobOptions(ctx, job, record.Trigger)
	if err != nil {
		record.Status, record.ErrorMessage = models.StatusFailed, err.Error()
		return s.finishJobRun(ctx, job, record, err, false)
	}
	record, err = s.backupEngine.Execute(ctx, opts, record)
	return s.finishJobRun(ctx, job, record, err, false)
}

// runScheduled is the cron callback. It reads the scheduler context under s.mu and
// registers the execution with the in-flight WaitGroup, refusing to start once Stop
// has been called so that Stop's Wait never races with a new Add.
func (s *Scheduler) runScheduled(jobID string) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	ctx := s.ctx
	s.inflight.Add(1)
	s.mu.Unlock()

	defer s.inflight.Done()
	s.executeJob(ctx, jobID)
}

// executeJob is the internal callback invoked when a cron trigger fires.
func (s *Scheduler) executeJob(ctx context.Context, jobID string) {
	job, err := s.metadataStore.GetJob(ctx, jobID)
	if err != nil {
		s.logger.Error("cron triggered for missing job", slog.String("job_id", jobID), slog.Any("error", err))
		return
	}

	if s.guard != nil {
		release, err := s.guard(job.ConnectionID, job.Database)
		if err != nil {
			s.logger.Warn("skipping scheduled backup: another backup of this database is running",
				slog.String("job_id", job.ID),
				slog.String("database", job.Database),
				slog.Any("error", err),
			)
			return
		}
		defer release()
	}

	_, _ = s.runBackupForJob(ctx, job)
}

// jobOptions derives the backup options of a job run started by trigger, resolving its
// connection. Without a ConnectionResolver the engine's default URI applies.
func (s *Scheduler) jobOptions(ctx context.Context, job *models.Job, trigger models.BackupTrigger) (models.BackupOptions, error) {
	opts := models.BackupOptions{
		JobID:              job.ID,
		Trigger:            trigger,
		Database:           job.Database,
		Collections:        job.Collections,
		ExcludeCollections: job.ExcludeCollections,
		StorageType:        job.StorageType,
		Gzip:               job.Gzip,
		ConnectionID:       job.ConnectionID,
	}
	if s.targets != nil {
		target, err := s.targets.Resolve(ctx, job.StorageTargetID)
		if err != nil {
			return opts, fmt.Errorf("resolve storage target %s: %w", job.StorageTargetID, err)
		}
		opts.StorageTargetID, opts.StorageTargetName, opts.StorageType = target.ID, target.Name, target.Type
	}
	if s.connections == nil {
		return opts, nil
	}
	if job.ConnectionID == "" {
		return opts, ErrNoConnection
	}
	conn, err := s.connections.Resolve(ctx, job.ConnectionID)
	if err != nil {
		return opts, fmt.Errorf("resolve connection %s: %w", job.ConnectionID, err)
	}
	opts.MongoURI, opts.ConnectionName = conn.URI, conn.Name
	return opts, nil
}

// runBackupForJob executes a scheduled (cron-triggered) run: the backup, the job's
// timestamps and retention pruning.
func (s *Scheduler) runBackupForJob(ctx context.Context, job *models.Job) (*models.BackupRecord, error) {
	s.logger.Info("running backup job",
		slog.String("job_id", job.ID),
		slog.String("database", job.Database),
	)
	opts, err := s.jobOptions(ctx, job, models.TriggerScheduled)
	if err != nil {
		return s.finishJobRun(ctx, job, nil, err, true)
	}
	record, err := s.backupEngine.Run(ctx, opts)
	return s.finishJobRun(ctx, job, record, err, true)
}

// finishJobRun persists a finished run, updates the job, publishes the outcome and,
// for scheduled runs only, applies retention.
func (s *Scheduler) finishJobRun(ctx context.Context, job *models.Job, record *models.BackupRecord, err error, scheduled bool) (*models.BackupRecord, error) {
	// A cancelled run must never be persisted as in-progress.
	if record != nil && ctx.Err() != nil && record.Status == models.StatusInProgress {
		record.Status = models.StatusFailed
		if record.ErrorMessage == "" {
			record.ErrorMessage = fmt.Sprintf("backup cancelled: %v", ctx.Err())
		}
	}

	// Post-run persistence uses a context detached from cancellation (bounded by a timeout)
	// so shutdown still records the final outcome.
	persistCtx, cancelPersist := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancelPersist()

	// Update job last run time and next run time
	now := time.Now().UTC()
	job.LastRun = &now
	s.mu.Lock()
	if _, exists := s.entries[job.ID]; exists {
		job.NextRun = nextRun(job.CronExpression, now)
	}
	s.mu.Unlock()
	// UpdateJob: a job deleted while it ran stays deleted.
	if saveErr := s.metadataStore.UpdateJob(persistCtx, job); saveErr != nil && !errors.Is(saveErr, store.ErrNotFound) {
		s.logger.Error("failed to persist job run metadata",
			slog.String("job_id", job.ID),
			slog.Any("error", saveErr),
		)
	}

	// Persist the backup record last: once a client sees the final status, the
	// job's run timestamps are already stored.
	if record != nil {
		if saveErr := s.metadataStore.SaveBackupRecord(persistCtx, record); saveErr != nil {
			s.logger.Error("failed to persist backup record",
				slog.String("job_id", job.ID),
				slog.String("backup_id", record.ID),
				slog.Any("error", saveErr),
			)
		}
	}

	// Emit the outcome once it is persisted; publishing is non-blocking by contract.
	if s.publisher != nil {
		s.publisher.Publish(persistCtx, events.BackupEvent(record, err, job.ID, job.Database))
	}

	if err != nil {
		s.logger.Error("backup job execution failed",
			slog.String("job_id", job.ID),
			slog.Any("error", err),
		)
		return record, err
	}

	// Retention runs after a successful scheduled run only; on-demand runs never prune.
	if scheduled && record.Status == models.StatusCompleted && (job.RetentionDays > 0 || job.RetentionCount > 0) {
		history, listErr := s.metadataStore.ListBackupRecords(ctx, job.Database)
		if listErr == nil {
			// Retention only ever sees this job's own scheduled backups: on-demand,
			// manual and MCP backups neither count towards the kept ones nor get
			// pruned. The same database name on another server is a different
			// dataset, and retention counts per storage target: backups kept on
			// another target (e.g. before the job was moved) are not pruned by this one.
			history = slices.DeleteFunc(history, func(r *models.BackupRecord) bool {
				return r.JobID != job.ID || r.EffectiveTrigger() != models.TriggerScheduled ||
					r.ConnectionID != job.ConnectionID || r.StorageTargetID != record.StorageTargetID
			})
			_, _ = PruneBackupsOn(
				ctx,
				job.RetentionDays,
				job.RetentionCount,
				history,
				s.metadataStore,
				s.storageFor,
				s.logger,
			)
		}
	}

	return record, nil
}

// storageFor returns the driver of a storage target: through WithStorageTargets when
// configured, else the scheduler's fixed driver.
func (s *Scheduler) storageFor(ctx context.Context, targetID string) (storage.Storage, error) {
	if s.targets != nil {
		return s.targets.Storage(ctx, targetID)
	}
	if s.storageDriver == nil {
		return nil, errors.New("scheduler: no storage configured")
	}
	return s.storageDriver, nil
}

// cronParser accepts standard five-field cron expressions and descriptors
// such as "@daily".
var cronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// nextRun returns the first activation of expr strictly after from, or nil
// when expr cannot be parsed.
func nextRun(expr string, from time.Time) *time.Time {
	schedule, err := cronParser.Parse(expr)
	if err != nil {
		return nil
	}
	next := schedule.Next(from).UTC()
	return &next
}
