package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/runs"
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

// Delays between attempts to load the jobs when Start could not list them: the first
// retry waits loadRetryFirst, every further one twice as long, up to loadRetryLimit.
const (
	loadRetryFirst = 5 * time.Second
	loadRetryLimit = 30 * time.Second
)

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
	registry      *runs.Registry

	// retentionLog, auditor and afterBackup are the integrity hooks (see
	// integrity.go).
	retentionLog RetentionLog
	auditor      Auditor
	afterBackup  AfterBackupFunc
	afterRun     AfterRunFunc
	// jobChanged is told about every job written through ApplyJobUpdate.
	jobChanged func(jobID string)

	// now is the time source of retention and the purge (WithClock), grace the
	// delete grace period in force (WithDeleteGrace) and maintenance the hooks
	// Maintain runs after each purge (WithMaintenance); see purge.go.
	now         func() time.Time
	grace       func() time.Duration
	maintenance []func(ctx context.Context)

	// databases lists a connection's databases for multi-database jobs.
	databases DatabaseLister
	// jobRunsMu guards jobRuns, the active multi-database run of each job.
	jobRunsMu sync.Mutex
	jobRuns   map[string]string
	// lockWaitFor and lockPollEvery bound the wait for a busy database (0 means
	// DefaultLockWait and lockPoll; lowered by tests).
	lockWaitFor, lockPollEvery time.Duration

	// mu guards entries, ctx, cancel, started, stopped and paused.
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	started bool
	stopped bool
	paused  bool

	// inflight tracks cron-triggered executions so Stop can wait for them to finish.
	inflight sync.WaitGroup
	// loader tracks the goroutine that retries loading the jobs after Start failed to.
	loader sync.WaitGroup
	// retryFirst and retryLimit bound the delay between job loading attempts.
	retryFirst, retryLimit time.Duration
	// changes counts job registrations and removals (guarded by mu), so a job listing
	// taken without the lock can tell whether it is still current.
	changes uint64

	// lastTick is the latest liveness tick (nil before Start), with its monotonic
	// clock reading, so a wall clock step (NTP, a resumed VM) never makes a fresh
	// tick look stale; see LastTick and Stale.
	lastTick atomic.Pointer[time.Time]
	// observer is told when job runs start and finish (see RunObserver).
	observer RunObserver
}

// TickInterval is how often a running scheduler records a liveness tick (see
// LastTick). The tick runs on the cron runner and takes the scheduler's lock, so a
// hung runner or a deadlocked scheduler stops it.
const TickInterval = 30 * time.Second

// StaleAfter is how old the last tick may be before the scheduler counts as stale
// (three missed ticks).
const StaleAfter = 3 * TickInterval

// tickSchedule is the cron schedule of the liveness tick (TickInterval).
const tickSchedule = "@every 30s"

// RunObserver is told when a job run starts and when it finishes (the run's status
// is final then: ok, partial, failed or cancelled). It sees the job as the run read
// it, secrets included. Its methods are called on the run's goroutine and must
// return at once.
type RunObserver interface {
	// JobRunStarted is called when the backups of a run start.
	JobRunStarted(job *models.Job, run *models.JobRun)
	// JobRunFinished is called once the run's outcome is recorded. interrupted
	// reports that the run's context ended before it finished (a shutdown, or the
	// caller of an on-demand run cancelling it); a run cancelled by a user has the
	// status cancelled.
	JobRunFinished(job *models.Job, run *models.JobRun, interrupted bool)
}

// WithRunObserver tells o about every job run, scheduled or on demand.
func WithRunObserver(o RunObserver) Option {
	return func(s *Scheduler) { s.observer = o }
}

// runStarted tells the observer that run of job started.
func (s *Scheduler) runStarted(job *models.Job, run *models.JobRun) {
	if s.observer != nil && job != nil && run != nil {
		s.observer.JobRunStarted(job, run)
	}
}

// runFinished tells the observer that run of job finished on ctx, the run's
// context (ended when a shutdown or its caller interrupted it).
func (s *Scheduler) runFinished(ctx context.Context, job *models.Job, run *models.JobRun) {
	if s.observer != nil && job != nil && run != nil {
		s.observer.JobRunFinished(job, run, ctx.Err() != nil)
	}
}

// LastTick returns when the scheduler last recorded a liveness tick, or the zero
// time before Start. A running scheduler ticks every TickInterval.
// The returned time keeps its monotonic clock reading.
func (s *Scheduler) LastTick() time.Time {
	if t := s.lastTick.Load(); t != nil {
		return *t
	}
	return time.Time{}
}

// Stale reports whether the scheduler was started and its last tick is older than
// StaleAfter: its cron runner is hung, it is deadlocked or it was stopped. The age
// is measured with the monotonic clock (time.Since), so changes of the wall clock
// do not count.
func (s *Scheduler) Stale() bool {
	last := s.LastTick()
	return !last.IsZero() && time.Since(last) > StaleAfter
}

// recordTick stores the current time as the last liveness tick.
func (s *Scheduler) recordTick() {
	now := time.Now()
	s.lastTick.Store(&now)
}

// tick records a liveness tick. It takes s.mu, so a scheduler whose lock is held
// forever stops ticking; a stopped scheduler does not tick.
func (s *Scheduler) tick() {
	s.mu.Lock()
	stopped := s.stopped
	s.mu.Unlock()
	if !stopped {
		s.recordTick()
	}
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

// WithRunRegistry tracks cron-triggered runs in reg, so they can be cancelled, report
// live progress and write a run log; retention removes the logs of pruned backups.
func WithRunRegistry(reg *runs.Registry) Option {
	return func(s *Scheduler) { s.registry = reg }
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
		retryFirst:    loadRetryFirst,
		retryLimit:    loadRetryLimit,
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
// succeeded. If the jobs cannot be listed, Start logs the error, starts anyway and
// keeps retrying in the background (with a growing delay up to 30 s) until the jobs
// load or the scheduler stops; jobs registered meanwhile are kept. Rows the store
// cannot decode are skipped by the store, so one damaged job never blocks the others.
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

	// Release the placeholder context of NewScheduler.
	prevCancel := s.cancel
	s.ctx, s.cancel = runCtx, runCancel
	if prevCancel != nil {
		prevCancel()
	}
	s.started = true

	if err := s.loadJobsLocked(); err != nil {
		s.logger.Error("failed to load scheduled jobs; scheduled backups do not run until they load, retrying",
			slog.Duration("retry_in", s.retryFirst),
			slog.Any("error", err),
		)
		s.loader.Add(1)
		go s.retryLoad(runCtx)
	}

	// Jobs paused until a given time resume on their own (checked every minute). The
	// check lists the jobs itself, so it also covers jobs a late retry loads.
	if _, err := s.cron.AddFunc(resumeCheckSchedule, s.resumeDueJobs); err != nil {
		s.logger.Error("failed to schedule the resumption of paused jobs", slog.Any("error", err))
	}
	// The liveness tick read by the health check and the heartbeat.
	s.recordTick()
	if _, err := s.cron.AddFunc(tickSchedule, s.tick); err != nil {
		s.logger.Error("failed to schedule the scheduler liveness tick", slog.Any("error", err))
	}
	// Deleted backups are purged once their grace period ends, and pending changes
	// that lower a protection are applied once they are due.
	if _, err := s.cron.AddFunc(maintenanceSchedule, s.runMaintenance); err != nil {
		s.logger.Error("failed to schedule the purge of deleted backups", slog.Any("error", err))
	}

	s.cron.Start()
	s.logger.Info("backup scheduler started successfully",
		slog.Int("active_jobs", len(s.entries)),
	)

	return nil
}

// loadJobsLocked registers every enabled job of the store. Caller must hold s.mu.
func (s *Scheduler) loadJobsLocked() error {
	jobs, err := s.metadataStore.ListJobs(s.ctx)
	if err != nil {
		return fmt.Errorf("list scheduled jobs: %w", err)
	}
	s.registerJobsLocked(jobs)
	return nil
}

// registerJobsLocked registers the enabled jobs of a store listing. Caller must hold s.mu.
func (s *Scheduler) registerJobsLocked(jobs []*models.Job) {
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
}

// retryLoad retries loading the jobs with a growing delay until it succeeds or ctx
// (the scheduler's run context, cancelled by Stop) ends. The store is listed without
// holding s.mu, so a slow database never blocks job updates, Pause or Stop. A listing
// is only registered when no job was registered or removed while it ran (it could be
// stale then); otherwise the jobs are listed again right away.
func (s *Scheduler) retryLoad(ctx context.Context) {
	defer s.loader.Done()
	delay := s.retryFirst
	wait := delay
	for attempt := 2; ; attempt++ {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		s.mu.Lock()
		if s.stopped {
			s.mu.Unlock()
			return
		}
		seen := s.changes
		s.mu.Unlock()

		jobs, err := s.metadataStore.ListJobs(ctx)
		if err != nil {
			delay = min(2*delay, s.retryLimit)
			wait = delay
			s.logger.Error("failed to load scheduled jobs; retrying",
				slog.Int("attempt", attempt),
				slog.Duration("retry_in", delay),
				slog.Any("error", err),
			)
			continue
		}

		s.mu.Lock()
		if s.stopped {
			s.mu.Unlock()
			return
		}
		if s.changes != seen {
			s.mu.Unlock()
			wait = 0
			continue
		}
		s.registerJobsLocked(jobs)
		active := len(s.entries)
		s.mu.Unlock()
		s.logger.Info("scheduled jobs loaded after a failed attempt",
			slog.Int("attempt", attempt), slog.Int("active_jobs", active))
		return
	}
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
	s.loader.Wait()
	s.inflight.Wait()
	s.logger.Info("backup scheduler stopped")
}

// Pause keeps cron triggers from starting new runs until Resume; runs in progress
// and on-demand runs (TriggerJob, ExecuteJobRun) are not affected. Jobs stay
// registered, so triggers missed while paused are skipped, not queued. The desktop
// app pauses scheduling while it waits for runs to finish before it quits.
func (s *Scheduler) Pause() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.paused {
		s.paused = true
		s.logger.Info("backup scheduler paused: scheduled runs will not start")
	}
}

// Resume lets cron triggers start runs again after Pause.
func (s *Scheduler) Resume() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.paused {
		s.paused = false
		s.logger.Info("backup scheduler resumed")
	}
}

// Paused reports whether Pause is in effect.
func (s *Scheduler) Paused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused
}

// RegisterJob registers or updates a scheduled job.
func (s *Scheduler) RegisterJob(job *models.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.registerJobLocked(job)
}

// ApplyJobUpdate runs persist, which stores job, and then replaces the job's cron
// entry, holding the scheduler's lock for both. Concurrent updates of a job are so
// registered in the order they were stored, and a finishing run (which writes its run
// timestamps under the same lock) never interleaves with them. It returns persist's
// error unchanged; a registration failure after a successful persist is logged (the
// job was validated before, so it means a bug, not a client error).
func (s *Scheduler) ApplyJobUpdate(job *models.Job, persist func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := persist(); err != nil {
		return err
	}
	if err := s.registerJobLocked(job); err != nil {
		s.logger.Error("failed to register job with scheduler",
			logsafe.Attr("job_id", job.ID),
			logsafe.Error(err),
		)
	}
	if s.jobChanged != nil {
		s.jobChanged(job.ID)
	}
	return nil
}

// registerJobLocked registers a job with cron. Caller must hold s.mu.
func (s *Scheduler) registerJobLocked(job *models.Job) error {
	if job == nil || job.ID == "" {
		return errors.New("scheduler: invalid job")
	}
	s.changes++

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
	// from the schedule directly. Only the run timestamps are written: the rest of
	// job may be older than the stored job.
	job.NextRun = nextRun(job.CronExpression, time.Now())
	if err := s.metadataStore.UpdateJobRunTimes(s.ctx, job.ID, nil, job.NextRun); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.logger.Error("failed to update job next run metadata",
			logsafe.Attr("job_id", job.ID),
			slog.Any("error", err),
		)
	}

	s.logger.Info("scheduled backup job registered",
		logsafe.Attr("job_id", job.ID),
		logsafe.Attr("database", job.Database),
		logsafe.Attr("cron", job.CronExpression),
		logsafe.Time("next_run", job.NextRun),
	)

	return nil
}

// UnregisterJob removes a job from the active cron scheduler.
func (s *Scheduler) UnregisterJob(jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.changes++
	if entryID, exists := s.entries[jobID]; exists {
		s.cron.Remove(entryID)
		delete(s.entries, jobID)
		s.logger.Info("scheduled backup job unregistered", slog.String("job_id", jobID))
	}
}

// TriggerJob executes a job immediately on demand and waits for it to finish. Like
// every on-demand run it never applies retention (see ExecuteJobRun). It returns the
// backup record of a single-database job, or the first record of a multi-database
// run.
func (s *Scheduler) TriggerJob(ctx context.Context, jobID string) (*models.BackupRecord, error) {
	plan, err := s.PrepareJobRun(ctx, jobID, models.TriggerOnDemand)
	if err != nil {
		return nil, err
	}
	if err = s.BeginJobRun(ctx, plan); err != nil {
		return nil, err
	}
	_, err = s.ExecuteJobRun(ctx, plan)
	return plan.First(), err
}

// PrepareJobRun loads jobID and plans an on-demand run of it, without starting it:
// the in-progress backup record of every database it backs up (one for a
// single-database job), sharing a new run ID. The records carry trigger
// (TriggerOnDemand or TriggerMCP; anything else, including TriggerScheduled, is
// recorded as TriggerOnDemand, so callers cannot make an on-demand run count for
// retention). It wraps store.ErrNotFound for unknown jobs, ErrJobRunning while a
// multi-database run of the job is still going, ErrDatabaseListing when the
// selection cannot be resolved and ErrNoDatabases when it matches no existing
// database. A multi-database plan is started with BeginJobRun and ExecuteJobRun (or
// given up with AbandonJobRun); the caller of a single-database plan locks, stores
// and tracks its record, then calls ExecuteJobRun (typically in the background).
func (s *Scheduler) PrepareJobRun(ctx context.Context, jobID string, trigger models.BackupTrigger) (*JobRunPlan, error) {
	job, err := s.metadataStore.GetJob(ctx, jobID)
	if err != nil {
		return nil, fmt.Errorf("retrieve job %s: %w", jobID, err)
	}
	if trigger != models.TriggerMCP {
		trigger = models.TriggerOnDemand
	}
	if job.MultiDatabase() {
		if current := s.ActiveJobRun(job.ID); current != "" {
			return nil, fmt.Errorf("prepare job %s: %w (run %s): %w", jobID, ErrJobRunning, current, runs.ErrBusy)
		}
	}
	plan, err := s.planRun(ctx, job, newRun(job, trigger))
	if err != nil {
		return nil, fmt.Errorf("prepare job %s: %w", jobID, err)
	}
	if len(plan.Records) == 0 {
		detail := "nothing to back up"
		if res := plan.Resolution; res != nil && len(res.Missing) > 0 {
			detail = "not found: " + strings.Join(res.Missing, ", ")
		}
		return nil, fmt.Errorf("prepare job %s: %w (%s)", jobID, ErrNoDatabases, detail)
	}
	return plan, nil
}

// ExecuteJobRun runs a job prepared by PrepareJobRun: it executes the backups,
// persists the records, the run and the job's run timestamps and publishes the
// outcome events. A multi-database run backs up its databases in turn (or up to the
// job's parallelism at once), each under its own run lock, and publishes one summary
// event for the run; cancelling any of its backups cancels the whole run.
//
// On-demand runs never apply the job's retention policy: only cron-triggered runs
// prune, so a caller that may run jobs (an operator API key, an assistant) cannot
// delete good backups by running a job repeatedly.
func (s *Scheduler) ExecuteJobRun(ctx context.Context, plan *JobRunPlan) (*models.JobRun, error) {
	if plan.Multi() {
		return s.runMulti(ctx, plan, false)
	}
	job, record := plan.Job, plan.First()
	s.logger.Info("running backup job",
		slog.String("job_id", job.ID),
		slog.String("database", job.Database),
	)
	s.saveRun(ctx, plan.Run)
	s.runStarted(job, plan.Run)
	opts, err := s.jobOptions(ctx, job, record.Trigger)
	if err != nil {
		record.Status, record.ErrorMessage = models.StatusFailed, err.Error()
		_, err = s.finishJobRun(ctx, job, plan.Run, record, err, false)
		return plan.Run, err
	}
	record, err = s.backupEngine.Execute(ctx, opts, record)
	plan.Records[0] = record
	_, err = s.finishJobRun(ctx, job, plan.Run, record, err, false)
	return plan.Run, err
}

// saveRun stores run, logging a failure.
func (s *Scheduler) saveRun(ctx context.Context, run *models.JobRun) {
	if run == nil {
		return
	}
	if err := s.metadataStore.SaveJobRun(ctx, run); err != nil {
		s.logger.Warn("failed to record the job run", slog.String("job_id", run.JobID), slog.String("run_id", run.ID), slog.Any("error", err))
	}
}

// runScheduled is the cron callback. It reads the scheduler context under s.mu and
// registers the execution with the in-flight WaitGroup, refusing to start once Stop
// has been called so that Stop's Wait never races with a new Add, and while paused.
func (s *Scheduler) runScheduled(jobID string) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	if s.paused {
		s.mu.Unlock()
		s.logger.Info("skipping scheduled backup: scheduling is paused", logsafe.Attr("job_id", jobID))
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
		s.logger.Error("cron triggered for missing job", logsafe.Attr("job_id", jobID), slog.Any("error", err))
		return
	}
	// Only scheduled runs honour the backup window: outside it the run is recorded
	// as skipped; inside it, cancel_at_window_end stops the run when it closes.
	cancelAt, start := s.checkWindow(ctx, job)
	if !start {
		return
	}

	// A multi-database run takes the run lock of each database itself.
	if s.guard != nil && !job.MultiDatabase() {
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

	_, _ = s.runScheduledBackup(ctx, job, cancelAt)
}

// jobOptions derives the backup options of a job run started by trigger, resolving its
// connection. Without a ConnectionResolver the engine's default URI applies.
func (s *Scheduler) jobOptions(ctx context.Context, job *models.Job, trigger models.BackupTrigger) (models.BackupOptions, error) {
	opts := models.BackupOptions{
		JobID:                job.ID,
		Trigger:              trigger,
		Database:             job.Database,
		Collections:          job.Collections,
		ExcludeCollections:   job.ExcludeCollections,
		StorageType:          job.StorageType,
		Gzip:                 job.Gzip,
		ConnectionID:         job.ConnectionID,
		IncludeUsersAndRoles: job.IncludeUsersAndRoles,
		Verify:               job.VerifyAfterBackup,

		ReadPreference:         job.ReadPref(),
		MaxUploadMbps:          job.MaxUploadMbps,
		NumParallelCollections: job.NumParallelCollections,
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
	// The job's read preference overrides its connection's; the connection's
	// limit of concurrent backups applies to every job that reads from it.
	opts.ReadPreference = opts.ReadPreference.Or(conn.ReadPref())
	opts.MaxConcurrentBackups = conn.MaxConcurrentBackups
	return opts, nil
}

// runBackupForJob executes a scheduled (cron-triggered) run: the backups, the job's
// timestamps and retention pruning. The in-progress records are stored first, so the
// run shows up (and can be cancelled through the run registry) while it runs. It
// returns the record of a single-database job, or the first record of a
// multi-database run.
func (s *Scheduler) runBackupForJob(ctx context.Context, job *models.Job) (*models.BackupRecord, error) {
	return s.runScheduledBackup(ctx, job, time.Time{})
}

// runScheduledBackup is runBackupForJob; a non-zero cancelAt cancels the run (and
// only it, by its run ID) when the job's backup window closes.
func (s *Scheduler) runScheduledBackup(ctx context.Context, job *models.Job, cancelAt time.Time) (*models.BackupRecord, error) {
	s.logger.Info("running backup job",
		slog.String("job_id", job.ID),
		slog.String("database", job.Database),
	)
	run := s.newScheduledRun(job)
	defer s.cancelRunAt(job, run.ID, cancelAt)()
	if job.MultiDatabase() {
		if current := s.ActiveJobRun(job.ID); current != "" {
			s.logger.Warn("skipping scheduled backup: the previous run of this job is still running",
				slog.String("job_id", job.ID), slog.String("run_id", current))
			return nil, fmt.Errorf("%w (run %s): %w", ErrJobRunning, current, runs.ErrBusy)
		}
		plan, err := s.planRun(ctx, job, run)
		if err != nil {
			_, err = s.failRun(ctx, job, run, err)
			return nil, err
		}
		_, err = s.runMulti(ctx, plan, true)
		return plan.First(), err
	}
	plan, err := s.planRun(ctx, job, run)
	if err != nil {
		return s.finishJobRun(ctx, job, run, nil, err, true)
	}
	record := plan.First()
	opts := plan.options[0]
	if saveErr := s.metadataStore.SaveBackupRecord(ctx, record); saveErr != nil {
		s.logger.Warn("failed to record the scheduled backup as in progress",
			slog.String("job_id", job.ID), slog.String("backup_id", record.ID), slog.Any("error", saveErr))
	}
	s.saveRun(ctx, run)
	s.runStarted(job, run)
	tracked, regErr := s.registry.Register(runs.Meta{Kind: models.RunBackup, ID: record.ID, JobID: job.ID, Database: job.Database, Group: run.ID})
	if regErr != nil {
		s.logger.Warn("scheduled backup is not tracked", slog.String("backup_id", record.ID), slog.Any("error", regErr))
	}
	defer tracked.End()
	record, err = s.backupEngine.Execute(tracked.Bind(ctx), opts, record)
	return s.finishJobRun(ctx, job, run, record, err, true)
}

// finishJobRun persists a finished single-database run, updates the job, publishes
// the outcome and, for scheduled runs only, applies retention. run (nil for none) is
// the run's summary, completed with the record's outcome.
func (s *Scheduler) finishJobRun(ctx context.Context, job *models.Job, run *models.JobRun, record *models.BackupRecord, err error, scheduled bool) (*models.BackupRecord, error) {
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

	s.recordRunTimes(persistCtx, job)

	if run != nil {
		switch {
		case record != nil:
			record.RunID = run.ID
			if len(run.Databases) == 0 {
				run.Databases = []models.JobRunDatabase{{Database: record.Database, BackupID: record.ID}}
			}
			run.Databases[0].Status = record.Status
			if record.Status != models.StatusCompleted {
				run.Databases[0].Error = redact.Text(record.ErrorMessage)
			}
		case err != nil:
			run.Error = redact.Text(err.Error())
		}
		run.Finish(time.Now())
		s.saveRun(persistCtx, run)
		s.runFinished(ctx, job, run)
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
		e := events.BackupEvent(record, err, job.ID, job.Database)
		if run != nil {
			e.RunID, e.Run = run.ID, events.RunSummaryOf(run, false)
		}
		s.publisher.Publish(persistCtx, e)
		if ve, ok := events.VerificationEvent(record, events.VerificationAfterUpload); ok {
			s.publisher.Publish(persistCtx, ve)
		}
	}

	if err != nil {
		s.logger.Error("backup job execution failed",
			slog.String("job_id", job.ID),
			slog.Any("error", err),
		)
		return record, err
	}

	// Retention runs after a successful scheduled run only; on-demand runs never prune.
	if scheduled {
		s.applyRetention(ctx, job, record)
	}

	// Post-backup work (restore tests) runs last, after retention, on the run's
	// context: a shutdown cancels it like the backup itself.
	if scheduled && record.Status == models.StatusCompleted && s.afterBackup != nil {
		s.afterBackup(ctx, job, record)
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
var cronParser = safeParser{cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)}

// errCronPanic reports an expression on which the cron parser panicked.
var errCronPanic = errors.New("unparsable schedule")

// safeParser wraps the cron parser, which panics (slice bounds out of range) on a
// time zone prefix without a schedule such as "TZ=UTC", turning that into an error.
type safeParser struct {
	parser cron.Parser
}

// Parse implements cron.ScheduleParser.
func (p safeParser) Parse(spec string) (schedule cron.Schedule, err error) {
	if (strings.HasPrefix(spec, "TZ=") || strings.HasPrefix(spec, "CRON_TZ=")) && !strings.Contains(spec, " ") {
		return nil, fmt.Errorf("%w: time zone without a schedule", errCronPanic)
	}
	defer func() {
		if r := recover(); r != nil {
			schedule, err = nil, fmt.Errorf("%w: %v", errCronPanic, r)
		}
	}()
	return p.parser.Parse(spec)
}

// ErrInvalidCron is returned by ValidateCron for an expression the scheduler cannot
// parse.
var ErrInvalidCron = errors.New("invalid cron expression")

// ValidateCron reports whether expr is a schedule the scheduler accepts: five
// standard cron fields or a descriptor such as "@daily" or "@every 6h". The returned
// error wraps ErrInvalidCron.
func ValidateCron(expr string) error {
	if _, err := cronParser.Parse(expr); err != nil {
		return fmt.Errorf("%w %q: %w", ErrInvalidCron, expr, err)
	}
	return nil
}

// NextRuns returns the next n activations of expr strictly after from, in UTC. It
// returns nil when expr cannot be parsed or n is not positive.
func NextRuns(expr string, from time.Time, n int) []time.Time {
	if n <= 0 {
		return nil
	}
	schedule, err := cronParser.Parse(expr)
	if err != nil {
		return nil
	}
	runs := make([]time.Time, 0, n)
	for next := from; len(runs) < n; {
		next = schedule.Next(next)
		if next.IsZero() {
			break
		}
		runs = append(runs, next.UTC())
	}
	return runs
}

// nextRun returns the first activation of expr strictly after from, or nil
// when expr cannot be parsed.
func nextRun(expr string, from time.Time) *time.Time {
	runs := NextRuns(expr, from, 1)
	if len(runs) == 0 {
		return nil
	}
	return &runs[0]
}
