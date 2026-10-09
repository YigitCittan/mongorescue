package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/diskguard"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// Errors of multi-database job runs.
var (
	// ErrDatabaseListing is returned when the databases of a job's connection cannot
	// be listed, which all and pattern selections need.
	ErrDatabaseListing = errors.New("scheduler: cannot list the databases of the job's connection")
	// ErrNoDatabases is returned when a job's selection matches no database that
	// exists on its connection.
	ErrNoDatabases = errors.New("scheduler: the job's selection matches no existing database")
	// ErrJobRunning is returned when a run of a multi-database job is started while
	// another run of it is still going. It also matches runs.ErrBusy.
	ErrJobRunning = errors.New("scheduler: a run of this job is already running")
	// ErrRunFailed is returned by a job run in which some or all databases failed.
	ErrRunFailed = errors.New("scheduler: job run failed")
	// ErrNotMulti is returned by StartJobRun for a single-database job, which is
	// started with PrepareJobRun and ExecuteJobRun.
	ErrNotMulti = errors.New("scheduler: the job backs up a single database")
	// ErrInvalidAdHocRun is returned by PrepareAdHocRun for a run without
	// databases or whose locks do not match its databases.
	ErrInvalidAdHocRun = errors.New("scheduler: invalid ad-hoc run")
)

// DatabaseLister lists the database names of a managed connection, including the
// system databases (admin, config, local), which selections exclude themselves.
type DatabaseLister func(ctx context.Context, connectionID string) ([]string, error)

// WithDatabaseLister sets the port used to list a connection's databases, which
// multi-database jobs need to resolve their selection.
func WithDatabaseLister(l DatabaseLister) Option {
	return func(s *Scheduler) { s.databases = l }
}

// AfterRunFunc runs after a scheduled multi-database job run with the backups it
// completed (the automated restore test hooks in here, testing one or all of them).
// It runs on the scheduler's run context, so Stop cancels it and waits for it.
type AfterRunFunc func(ctx context.Context, job *models.Job, records []*models.BackupRecord)

// WithAfterRun runs fn after every scheduled multi-database job run that completed
// at least one backup.
func WithAfterRun(fn AfterRunFunc) Option {
	return func(s *Scheduler) { s.afterRun = fn }
}

// JobRunPlan is a prepared run of a job, or of several databases started without a
// job (an ad-hoc run): one in-progress backup record per database it backs up, all
// sharing the run's ID. Get one from PrepareJobRun or PrepareAdHocRun and pass it to
// ExecuteJobRun.
type JobRunPlan struct {
	// Job is the job as it was loaded; nil for an ad-hoc run.
	Job *models.Job
	// Run is the run's summary, updated as its databases finish.
	Run *models.JobRun
	// Records are the backups of the run, in the order they run. A single-database
	// job has exactly one.
	Records []*models.BackupRecord
	// Resolution is how a multi-database job's selection resolved (nil for single).
	Resolution *models.DatabaseResolution

	options  []models.BackupOptions
	trackers []*runs.Run
	multi    bool
	begun    bool
	// adhoc marks a run without a job (PrepareAdHocRun): it has no job to claim,
	// update or store the run of; parallelism is its own.
	adhoc       bool
	parallelism int
	// held are the run locks the caller of PrepareAdHocRun took, by record (nil: the
	// database's lock is taken when its turn comes). Guarded by the run's mutex
	// while it runs.
	held []func()
}

// Multi reports whether the plan is a run of a multi-database job or an ad-hoc run,
// which the scheduler persists, locks and tracks itself (BeginJobRun).
func (p *JobRunPlan) Multi() bool { return p != nil && p.multi }

// AdHoc reports whether the plan is an ad-hoc run (PrepareAdHocRun), which has no job.
func (p *JobRunPlan) AdHoc() bool { return p != nil && p.adhoc }

// jobID returns the plan's job ID, "" for an ad-hoc run.
func (p *JobRunPlan) jobID() string {
	if p.Job == nil {
		return ""
	}
	return p.Job.ID
}

// effectiveParallelism returns how many databases of the plan run at once.
func (p *JobRunPlan) effectiveParallelism() int {
	if p.adhoc {
		return min(max(p.parallelism, 1), models.MaxJobParallelism)
	}
	return p.Job.EffectiveParallelism()
}

// takeHeld returns the run lock the caller took for record i and forgets it, or nil.
func (p *JobRunPlan) takeHeld(i int) func() {
	if i >= len(p.held) {
		return nil
	}
	release := p.held[i]
	p.held[i] = nil
	return release
}

// releaseHeld releases the run locks of plan's databases that never started.
func (p *JobRunPlan) releaseHeld() {
	for i := range p.held {
		if release := p.takeHeld(i); release != nil {
			release()
		}
	}
}

// First returns the first backup record of the plan, or nil.
func (p *JobRunPlan) First() *models.BackupRecord {
	if p == nil || len(p.Records) == 0 {
		return nil
	}
	return p.Records[0]
}

// newRun returns the summary of a run of job started now by trigger.
func newRun(job *models.Job, trigger models.BackupTrigger) *models.JobRun {
	return newRunOf(job.ID, trigger)
}

// newRunOf returns the summary of a run of job jobID ("" for an ad-hoc run) started
// now by trigger.
func newRunOf(jobID string, trigger models.BackupTrigger) *models.JobRun {
	now := time.Now().UTC()
	id, err := models.NewRunID(now)
	if err != nil {
		id = fmt.Sprintf("run_%s_%d", now.Format("20060102_150405"), now.UnixNano())
	}
	return &models.JobRun{
		ID: id, JobID: jobID, Trigger: trigger, Status: models.JobRunRunning,
		StartedAt: now, Databases: []models.JobRunDatabase{},
	}
}

// ResolveJobDatabases resolves job's database selection against its connection now:
// the databases a run would back up, the ones it skips and why, and the ones found
// since the job's known databases were recorded. A single selection names its
// database without asking the server. A list selection that cannot check which
// databases exist backs up all of them (with a warning); all and pattern selections
// fail with ErrDatabaseListing then.
func (s *Scheduler) ResolveJobDatabases(ctx context.Context, job *models.Job) (*models.DatabaseResolution, error) {
	sel := job.Selection()
	if !sel.Multi() {
		res := models.ResolveSelection(sel, nil, nil)
		return &res, nil
	}
	names, err := s.listDatabases(ctx, job.ConnectionID)
	if err != nil {
		if sel.Mode == models.SelectionList {
			res := models.ResolveSelection(sel, sel.Databases, nil)
			res.Warnings = append(res.Warnings, "could not check which databases exist: "+redact.Text(err.Error()))
			return &res, nil
		}
		return nil, err
	}
	res := models.ResolveSelection(sel, names, job.KnownDatabases)
	return &res, nil
}

// listDatabases lists the databases of connection connectionID.
func (s *Scheduler) listDatabases(ctx context.Context, connectionID string) ([]string, error) {
	if s.databases == nil {
		return nil, fmt.Errorf("%w: no database lister is configured", ErrDatabaseListing)
	}
	if connectionID == "" {
		return nil, ErrNoConnection
	}
	names, err := s.databases(ctx, connectionID)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrDatabaseListing, redact.Text(err.Error()))
	}
	return names, nil
}

// planRun prepares run of job: the backup options and in-progress records of every
// database it backs up. Databases named by the selection that do not exist are
// recorded in run as failed ("database not found").
func (s *Scheduler) planRun(ctx context.Context, job *models.Job, run *models.JobRun) (*JobRunPlan, error) {
	plan := &JobRunPlan{Job: job, Run: run, multi: job.MultiDatabase()}
	opts, err := s.jobOptions(ctx, job, run.Trigger)
	if err != nil {
		return plan, err
	}
	databases := []string{job.Database}
	if plan.multi {
		res, err := s.ResolveJobDatabases(ctx, job)
		if err != nil {
			return plan, err
		}
		plan.Resolution = res
		databases = res.Included
		run.Warnings = res.Warnings
		if job.Selection().AutoIncludeNew {
			run.AddedDatabases = res.New
		} else {
			run.NewDatabases = res.New
		}
	}
	if err := s.prepareDatabases(plan, opts, databases, plan.multi, job.Selection().FilterFor); err != nil {
		return plan, err
	}
	if plan.Resolution != nil {
		for _, db := range plan.Resolution.Missing {
			run.Databases = append(run.Databases, models.JobRunDatabase{Database: db, Status: models.StatusFailed, Error: models.ErrorDatabaseNotFound})
		}
		if len(plan.Records) == 0 && len(plan.Resolution.Missing) == 0 {
			run.Error = "the selection matches no database"
		}
	}
	return plan, nil
}

// prepareDatabases adds to plan the in-progress record and the options of every one
// of databases, backed up with opts; perDatabase sets each database's name in the
// options and replaces opts' collection filters, which only a single database has,
// by the database's own filter from filterFor (none: the whole database).
func (s *Scheduler) prepareDatabases(plan *JobRunPlan, opts models.BackupOptions, databases []string, perDatabase bool, filterFor func(string) (models.DatabaseFilter, bool)) error {
	for _, db := range databases {
		o := opts
		if perDatabase {
			o.Database, o.Collections, o.ExcludeCollections = db, nil, nil
			if f, ok := filterFor(db); ok {
				o.Collections, o.ExcludeCollections = f.Collections, f.ExcludeCollections
			}
		}
		rec, err := s.backupEngine.Prepare(o)
		if err != nil {
			return fmt.Errorf("prepare the backup of %s: %w", db, err)
		}
		rec.RunID = plan.Run.ID
		plan.Records = append(plan.Records, rec)
		plan.options = append(plan.options, o)
		plan.Run.Databases = append(plan.Run.Databases, models.JobRunDatabase{Database: rec.Database, BackupID: rec.ID, Status: rec.Status})
	}
	return nil
}

// AdHocRun is a run of several databases of one connection started without a job,
// such as the dashboard's "Backup now" of more than one database. Its databases are
// backed up like those of a multi-database job run: each into its own record and
// archive, sharing the run's ID, up to Parallelism at once, and cancelled together.
// The run itself is not stored (it belongs to no job); its records are, and one
// summary event is published when it ends.
type AdHocRun struct {
	// Options are the backup options every database shares: connection and URI,
	// storage target, compression, trigger and users and roles. Database is set per
	// database; the collection filters are kept only for a run of one database
	// without an entry in Filters.
	Options models.BackupOptions
	// Databases are backed up in this order (at least one).
	Databases []string
	// Filters are the collection filters of some of Databases, at most one per
	// database (see models.DatabaseFilter); the others are backed up whole.
	Filters []models.DatabaseFilter
	// Locks are, by database, the releases of the run locks the caller took (a nil
	// slice or entry: the run takes the lock when the database's turn comes,
	// waiting for it as a job run does). Once PrepareAdHocRun succeeds the plan owns
	// them and releases each when its database ends or the run is abandoned; when
	// it fails the caller still owns them.
	Locks []func()
	// Busy are databases the run does not back up because another backup of them is
	// running; they are recorded in the run as skipped (models.StatusSkipped) with
	// BusyError, which changes neither the run's outcome nor its failure counts.
	Busy []string
	// Parallelism is how many databases run at once (1 to models.MaxJobParallelism;
	// 0 means 1, as for jobs).
	Parallelism int
}

// BusyError is the error an ad-hoc run records for database db, which another
// backup was running when the run started.
func BusyError(db string) string {
	return fmt.Sprintf("another backup of database %s is already running", db)
}

// PrepareAdHocRun plans an ad-hoc run: one in-progress backup record per database,
// all sharing the run's ID. Start it with BeginJobRun and ExecuteJobRun (or give it
// up with AbandonJobRun), exactly like the plan of a multi-database job. Expected
// failures: ErrInvalidAdHocRun and the engine's validation errors.
func (s *Scheduler) PrepareAdHocRun(req AdHocRun) (*JobRunPlan, error) {
	if len(req.Databases) == 0 {
		return nil, fmt.Errorf("%w: no database to back up", ErrInvalidAdHocRun)
	}
	if req.Locks != nil && len(req.Locks) != len(req.Databases) {
		return nil, fmt.Errorf("%w: %d locks for %d databases", ErrInvalidAdHocRun, len(req.Locks), len(req.Databases))
	}
	filterFor := func(db string) (models.DatabaseFilter, bool) {
		i := slices.IndexFunc(req.Filters, func(f models.DatabaseFilter) bool { return f.Name == db && f.Filtered() })
		if i < 0 {
			return models.DatabaseFilter{}, false
		}
		return req.Filters[i].Clone(), true
	}
	opts := req.Options
	opts.JobID = ""
	if len(req.Databases) == 1 {
		opts.Database = req.Databases[0]
		if f, ok := filterFor(opts.Database); ok {
			opts.Collections, opts.ExcludeCollections = f.Collections, f.ExcludeCollections
		}
	}
	plan := &JobRunPlan{Run: newRunOf("", opts.Trigger), multi: true, adhoc: true, parallelism: req.Parallelism}
	if err := s.prepareDatabases(plan, opts, req.Databases, len(req.Databases) > 1, filterFor); err != nil {
		return nil, err
	}
	for _, db := range req.Busy {
		plan.Run.Databases = append(plan.Run.Databases, models.JobRunDatabase{Database: db, Status: models.StatusSkipped, Error: BusyError(db)})
	}
	plan.held = slices.Clone(req.Locks)
	return plan, nil
}

// saveRunOf stores plan's run; an ad-hoc run has no job to store it with.
func (s *Scheduler) saveRunOf(ctx context.Context, plan *JobRunPlan) error {
	if plan.adhoc {
		return nil
	}
	return s.metadataStore.SaveJobRun(ctx, plan.Run)
}

// claimJobRun marks a run of job jobID as active, or fails with ErrJobRunning.
func (s *Scheduler) claimJobRun(jobID, runID string) error {
	s.jobRunsMu.Lock()
	defer s.jobRunsMu.Unlock()
	if current, busy := s.jobRuns[jobID]; busy && current != runID {
		return fmt.Errorf("%w (run %s): %w", ErrJobRunning, current, runs.ErrBusy)
	}
	if s.jobRuns == nil {
		s.jobRuns = map[string]string{}
	}
	s.jobRuns[jobID] = runID
	return nil
}

// releaseJobRun forgets the active run runID of job jobID.
func (s *Scheduler) releaseJobRun(jobID, runID string) {
	s.jobRunsMu.Lock()
	defer s.jobRunsMu.Unlock()
	if s.jobRuns[jobID] == runID {
		delete(s.jobRuns, jobID)
	}
}

// ActiveJobRun returns the ID of the multi-database run of job jobID in progress, or "".
func (s *Scheduler) ActiveJobRun(jobID string) string {
	s.jobRunsMu.Lock()
	defer s.jobRunsMu.Unlock()
	return s.jobRuns[jobID]
}

// BeginJobRun starts a prepared multi-database or ad-hoc run: it marks the job's
// run as active (ErrJobRunning when another one is; an ad-hoc run has no job to
// claim), stores the run and every database's backup record as queued, and
// registers them with the run registry, so the dashboard shows them and cancelling
// the run reaches the databases still waiting. It does nothing for single-database
// plans, whose caller locks, stores and tracks the one record. Call ExecuteJobRun
// next, or AbandonJobRun when the run cannot start.
func (s *Scheduler) BeginJobRun(ctx context.Context, plan *JobRunPlan) error {
	if !plan.Multi() || plan.begun {
		return nil
	}
	jobID := plan.jobID()
	if !plan.adhoc {
		if err := s.claimJobRun(jobID, plan.Run.ID); err != nil {
			return err
		}
	}
	plan.begun = true
	if err := s.saveRunOf(ctx, plan); err != nil {
		s.logger.Warn("failed to record the job run as running",
			slog.String("job_id", jobID), slog.String("run_id", plan.Run.ID), slog.Any("error", err))
	}
	s.runStarted(plan.Job, plan.Run)
	plan.trackers = make([]*runs.Run, len(plan.Records))
	for i, rec := range plan.Records {
		if err := s.metadataStore.SaveBackupRecord(ctx, rec); err != nil {
			s.logger.Warn("failed to record a queued backup of the job run",
				logsafe.Attr("job_id", jobID), logsafe.Attr("backup_id", rec.ID), logsafe.Error(err))
		}
		tracked, err := s.registry.Register(runs.Meta{
			Kind: models.RunBackup, ID: rec.ID, JobID: jobID, Database: rec.Database, Group: plan.Run.ID,
		})
		if err != nil {
			s.logger.Warn("backup of the job run is not tracked", logsafe.Attr("backup_id", rec.ID), logsafe.Error(err))
		}
		plan.trackers[i] = tracked
	}
	return nil
}

// AbandonJobRun records a begun multi-database or ad-hoc run that could not start as
// failed with cause: every queued record, the run itself and the job's active-run
// mark. The run locks the caller of an ad-hoc run took are released.
func (s *Scheduler) AbandonJobRun(ctx context.Context, plan *JobRunPlan, cause error) {
	if !plan.Multi() {
		return
	}
	defer plan.releaseHeld()
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	msg := "backup not started: " + redact.Text(cause.Error())
	for i, rec := range plan.Records {
		rec.Status, rec.ErrorMessage = models.StatusFailed, msg
		if err := s.metadataStore.SaveBackupRecord(persistCtx, rec); err != nil {
			s.logger.Error("failed to persist an abandoned backup record", logsafe.Attr("backup_id", rec.ID), logsafe.Error(err))
		}
		if i < len(plan.trackers) {
			plan.trackers[i].End()
		}
		plan.setDatabase(rec)
	}
	plan.Run.Error = msg
	plan.Run.Finish(time.Now())
	if err := s.saveRunOf(persistCtx, plan); err != nil {
		s.logger.Error("failed to persist an abandoned job run", slog.String("run_id", plan.Run.ID), slog.Any("error", err))
	}
	if !plan.adhoc {
		s.releaseJobRun(plan.Job.ID, plan.Run.ID)
	}
	s.runFinished(ctx, plan.Job, plan.Run)
}

// cancellation returns the Cancellation of a database of the run, or nil when none
// of them was cancelled.
func (p *JobRunPlan) cancellation() *runs.Cancellation {
	for _, tracked := range p.trackers {
		if c := tracked.Cancellation(); c != nil {
			return c
		}
	}
	return nil
}

// setDatabase copies rec's outcome into the run's database entry.
func (p *JobRunPlan) setDatabase(rec *models.BackupRecord) {
	for i := range p.Run.Databases {
		if d := &p.Run.Databases[i]; d.BackupID == rec.ID {
			d.Status = rec.Status
			d.Error = ""
			if rec.Status != models.StatusCompleted {
				d.Error = redact.Text(rec.ErrorMessage)
			}
			return
		}
	}
}

// runMulti executes a begun multi-database run: every database in turn (or up to
// the job's parallelism at once), each under its own run lock and with its own
// record, archive, verification, manifest and log, then the run's summary, the
// job's known databases and, for scheduled runs, retention per database and the
// restore tests.
func (s *Scheduler) runMulti(ctx context.Context, plan *JobRunPlan, scheduled bool) (*models.JobRun, error) {
	if err := s.BeginJobRun(ctx, plan); err != nil {
		return plan.Run, err
	}
	// An ad-hoc run claimed no job run.
	released := plan.adhoc
	release := func() {
		if !released {
			released = true
			s.releaseJobRun(plan.Job.ID, plan.Run.ID)
		}
	}
	defer release()
	s.logger.Info("running multi-database backup job",
		slog.String("job_id", plan.jobID()), slog.String("run_id", plan.Run.ID),
		slog.Int("databases", len(plan.Records)), slog.Int("parallelism", plan.effectiveParallelism()))

	var mu sync.Mutex
	work := make(chan int)
	var wg sync.WaitGroup
	for range min(plan.effectiveParallelism(), len(plan.Records)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				s.runDatabase(ctx, plan, i, &mu)
			}
		}()
	}
	for i := range plan.Records {
		work <- i
	}
	close(work)
	wg.Wait()
	run, err := s.finishMulti(ctx, plan)
	// The run is over: the next one may start while retention and the restore tests
	// (which take their own locks) still run.
	release()
	if scheduled {
		s.afterMulti(ctx, plan)
	}
	return run, err
}

// runDatabase backs up database i of plan; mu guards the plan's records and run.
func (s *Scheduler) runDatabase(ctx context.Context, plan *JobRunPlan, i int, mu *sync.Mutex) {
	var tracked *runs.Run
	if i < len(plan.trackers) {
		tracked = plan.trackers[i]
	}
	defer tracked.End()
	mu.Lock()
	rec, opts := plan.Records[i], plan.options[i]
	release := plan.takeHeld(i)
	mu.Unlock()

	// Bound now, so a cancellation of the run also ends the wait for the lock below.
	dbCtx := tracked.Bind(ctx)
	var err error
	if release == nil {
		// No lock was taken for this database when the run started: wait for it.
		release, err = s.waitForDatabase(dbCtx, rec)
	}
	if err != nil {
		now := time.Now().UTC()
		rec.Status = models.StatusFailed
		switch {
		case errors.Is(err, runs.ErrBusy):
			rec.ErrorMessage = fmt.Sprintf("another backup of database %s was still running after %s; skipped by this job run", rec.Database, s.lockWait())
		case errors.Is(err, diskguard.ErrLowSpace):
			rec.ErrorMessage = "backup not started: " + redact.Text(err.Error())
		default:
			rec.ErrorMessage = fmt.Sprintf("the run lock of database %s could not be taken: %s", rec.Database, redact.Text(err.Error()))
		}
		rec.CompletedAt, rec.Phases.Finished = &now, models.Stamp(now)
	}
	if err == nil {
		// A cancellation of the run reaches its databases one by one (runs.Registry
		// cancels the requested database before the others), so this database may not
		// be cancelled yet although another one is: adopt that cancellation, and the
		// engine records this database as cancelled without starting mongodump.
		if c := plan.cancellation(); c != nil {
			_ = tracked.Cancel(*c)
		}
		// The backup starts now; it was queued since the run began (Phases.Queued).
		rec.StartedAt = time.Now().UTC()
		rec, err = s.backupEngine.Execute(dbCtx, opts, rec)
		if rec != nil && ctx.Err() != nil && rec.Status == models.StatusInProgress {
			rec.Status = models.StatusFailed
			if rec.ErrorMessage == "" {
				rec.ErrorMessage = fmt.Sprintf("backup cancelled: %v", ctx.Err())
			}
		}
	}
	release()

	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	// A record that cannot be saved fails the database (and so the run).
	saveErr := s.diskGuard.FinishBackup(persistCtx, rec, s.metadataStore.SaveBackupRecord, s.logger, tracked)
	if err == nil {
		err = saveErr
	}
	if s.publisher != nil {
		e := events.BackupEvent(rec, err, plan.jobID(), rec.Database)
		e.RunID, e.InRun = plan.Run.ID, true
		s.publisher.Publish(persistCtx, e)
		if ve, ok := events.VerificationEvent(rec, events.VerificationAfterUpload); ok && saveErr == nil {
			s.publisher.Publish(persistCtx, ve)
		}
	}
	if err != nil {
		s.logger.Warn("database of the job run failed",
			logsafe.Attr("job_id", plan.jobID()), logsafe.Attr("run_id", plan.Run.ID),
			logsafe.Attr("database", rec.Database), slog.String("status", string(rec.Status)), logsafe.Error(err))
	}
	// The run is stored after every database, so a crash leaves the outcome of the
	// databases that finished (saved under mu, so an older state never overwrites a
	// newer one).
	mu.Lock()
	plan.Records[i] = rec
	plan.setDatabase(rec)
	if saveErr := s.saveRunOf(persistCtx, plan); saveErr != nil {
		s.logger.Warn("failed to record the progress of the job run",
			slog.String("job_id", plan.jobID()), slog.String("run_id", plan.Run.ID), slog.Any("error", saveErr))
	}
	mu.Unlock()
}

// Waiting for a database another run is backing up (see waitForDatabase).
const (
	// DefaultLockWait is how long a job run waits for a database another backup
	// holds before it records that database as failed.
	DefaultLockWait = 30 * time.Minute
	// lockPoll is how often the lock is tried while waiting.
	lockPoll = time.Second
)

// lockWait returns how long runs wait for a busy database.
func (s *Scheduler) lockWait() time.Duration {
	if s.lockWaitFor > 0 {
		return s.lockWaitFor
	}
	return DefaultLockWait
}

// waitForDatabase takes the run lock of rec's database, waiting while another backup
// of it runs (a manual backup, another job), up to lockWait. It returns at once,
// without the lock, when ctx ends (the run was cancelled): the engine then records
// the cancellation. Without a guard there is nothing to take.
func (s *Scheduler) waitForDatabase(ctx context.Context, rec *models.BackupRecord) (func(), error) {
	none := func() {}
	if s.guard == nil {
		return none, nil
	}
	deadline := time.Now().Add(s.lockWait())
	poll := lockPoll
	if s.lockPollEvery > 0 {
		poll = s.lockPollEvery
	}
	logged := false
	for {
		if ctx.Err() != nil {
			return none, nil
		}
		release, err := s.guard(rec.ConnectionID, rec.Database)
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, runs.ErrBusy) || !time.Now().Before(deadline) {
			return none, err
		}
		if !logged {
			logged = true
			s.logger.Info("job run waits for another backup of the database to finish",
				logsafe.Attr("backup_id", rec.ID), logsafe.Attr("database", rec.Database))
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return none, nil
		case <-timer.C:
		}
	}
}

// finishMulti records the outcome of a multi-database or ad-hoc run (see runMulti).
// An ad-hoc run has no job whose run times and known databases to update.
func (s *Scheduler) finishMulti(ctx context.Context, plan *JobRunPlan) (*models.JobRun, error) {
	run := plan.Run
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()

	run.Finish(time.Now())
	if !plan.adhoc {
		s.recordRunTimes(persistCtx, plan.Job)
		run.AddedDatabases = s.storeKnownDatabases(persistCtx, plan)
	}
	if err := s.saveRunOf(persistCtx, plan); err != nil {
		s.logger.Error("failed to persist the job run", slog.String("job_id", plan.jobID()), slog.String("run_id", run.ID), slog.Any("error", err))
	}
	s.runFinished(ctx, plan.Job, run)
	if s.publisher != nil {
		if len(run.AddedDatabases) > 0 {
			s.publisher.Publish(persistCtx, events.DatabasesAddedEvent(plan.jobID(), run.ID, run.AddedDatabases))
		}
		s.publisher.Publish(persistCtx, events.JobRunEvent(run))
	}
	ok, failed, cancelled, _ := run.Counts()
	s.logger.Info("multi-database backup job finished",
		slog.String("job_id", plan.jobID()), slog.String("run_id", run.ID), slog.String("status", string(run.Status)),
		slog.Int("succeeded", ok), slog.Int("failed", failed), slog.Int("cancelled", cancelled),
		slog.Int("new_databases", len(run.NewDatabases)))

	switch run.Status {
	case models.JobRunFailed, models.JobRunPartial:
		reason := strings.Join(run.FailedDatabases(), ", ")
		if run.Error != "" {
			reason = run.Error
		}
		return run, fmt.Errorf("%w (%s): %s", ErrRunFailed, run.Status, reason)
	}
	return run, nil
}

// completedRecords returns the backups of plan that completed.
func (p *JobRunPlan) completedRecords() []*models.BackupRecord {
	var out []*models.BackupRecord
	for _, rec := range p.Records {
		if rec.Status == models.StatusCompleted {
			out = append(out, rec)
		}
	}
	return out
}

// afterMulti is the work after a scheduled multi-database run, once its slot is
// released: retention per database (a database that failed today keeps every
// backup, including its last good one) and the restore tests.
func (s *Scheduler) afterMulti(ctx context.Context, plan *JobRunPlan) {
	completed := plan.completedRecords()
	for _, rec := range completed {
		s.applyRetention(ctx, plan.Job, rec)
	}
	if len(completed) > 0 && s.afterRun != nil {
		s.afterRun(ctx, plan.Job, completed)
	}
}

// storeKnownDatabases records the databases the run resolved as the job's known
// databases, against the job as it is stored now (read and written in one
// transaction, under the lock of job updates). When the job's connection and
// selection are those the run was planned with, the resolved known databases are
// merged into the stored ones (a newer entry is never dropped); when they changed
// meanwhile, only the databases the run backed up that the current selection still
// matches are added, and a job without known databases is left for its next save
// or run to record them. It returns the databases added for the first time that the
// run included automatically (AddedDatabases), for job.databases_added.
func (s *Scheduler) storeKnownDatabases(ctx context.Context, plan *JobRunPlan) []string {
	res := plan.Resolution
	if res == nil || res.Known == nil {
		return plan.Run.AddedDatabases
	}
	var added []string
	update := func(current *models.Job) ([]string, bool) {
		sel := current.Selection()
		if !sel.Discovers() {
			return nil, false
		}
		var candidates []string
		switch {
		case current.SameSource(plan.Job):
			if current.KnownDatabases == nil {
				candidates = res.Known
			} else {
				candidates = append(slices.Clone(current.KnownDatabases), res.Known...)
			}
		case current.KnownDatabases == nil:
			return nil, false
		default:
			candidates = slices.Clone(current.KnownDatabases)
			for _, rec := range plan.completedRecords() {
				if sel.Matches(rec.Database) {
					candidates = append(candidates, rec.Database)
				}
			}
		}
		known := slices.Clone(candidates)
		slices.Sort(known)
		known = slices.Compact(known)
		for _, db := range plan.Run.AddedDatabases {
			if slices.Contains(known, db) && !slices.Contains(current.KnownDatabases, db) {
				added = append(added, db)
			}
		}
		was := slices.Clone(current.KnownDatabases)
		slices.Sort(was)
		return known, current.KnownDatabases == nil || !slices.Equal(known, was)
	}
	s.mu.Lock()
	err := s.metadataStore.UpdateJobKnownDatabases(ctx, plan.Job.ID, update)
	s.mu.Unlock()
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.logger.Error("failed to store the job's known databases", slog.String("job_id", plan.Job.ID), slog.Any("error", err))
		}
		return nil
	}
	return added
}

// StartJobRun starts an on-demand run of multi-database job jobID in the
// background and returns at once with the run (status running): the selection is
// resolved and the databases are backed up by the function spawn starts (typically
// runs.Manager.Go); planning errors, such as databases that cannot be listed, are
// recorded in the run. trigger is models.TriggerMCP or models.TriggerOnDemand.
// Expected failures: store.ErrNotFound, ErrNotMulti, ErrJobRunning (also matching
// runs.ErrBusy) and the error of spawn.
func (s *Scheduler) StartJobRun(ctx context.Context, jobID string, trigger models.BackupTrigger, spawn func(func(context.Context)) error) (*models.JobRun, error) {
	job, err := s.metadataStore.GetJob(ctx, jobID)
	if err != nil {
		return nil, fmt.Errorf("retrieve job %s: %w", jobID, err)
	}
	if !job.MultiDatabase() {
		return nil, fmt.Errorf("start job %s: %w", jobID, ErrNotMulti)
	}
	if trigger != models.TriggerMCP {
		trigger = models.TriggerOnDemand
	}
	run := newRun(job, trigger)
	if err = s.claimJobRun(job.ID, run.ID); err != nil {
		return nil, fmt.Errorf("start job %s: %w", jobID, err)
	}
	s.saveRun(ctx, run)
	snapshot := run.Clone()
	err = spawn(func(runCtx context.Context) {
		plan, planErr := s.planRun(runCtx, job, run)
		if planErr != nil {
			s.releaseJobRun(job.ID, run.ID)
			_, _ = s.failRun(runCtx, job, run, planErr)
			return
		}
		_, _ = s.runMulti(runCtx, plan, false)
	})
	if err != nil {
		s.releaseJobRun(job.ID, run.ID)
		run.Error = "run not started: " + redact.Text(err.Error())
		run.Finish(time.Now())
		s.saveRun(context.WithoutCancel(ctx), run)
		s.runFinished(ctx, job, run)
		return nil, err
	}
	return snapshot, nil
}

// failRun records a multi-database run that could not be planned (its databases
// cannot be listed, its connection is gone): the run as failed with cause, the job's
// run times and the summary event, which notifications deliver.
func (s *Scheduler) failRun(ctx context.Context, job *models.Job, run *models.JobRun, cause error) (*models.JobRun, error) {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	run.Error = redact.Text(cause.Error())
	run.Finish(time.Now())
	s.recordRunTimes(persistCtx, job)
	if err := s.metadataStore.SaveJobRun(persistCtx, run); err != nil {
		s.logger.Error("failed to persist the job run", slog.String("job_id", job.ID), slog.String("run_id", run.ID), slog.Any("error", err))
	}
	s.runFinished(ctx, job, run)
	if s.publisher != nil {
		s.publisher.Publish(persistCtx, events.JobRunEvent(run))
	}
	s.logger.Error("backup job execution failed", slog.String("job_id", job.ID), slog.String("run_id", run.ID), slog.Any("error", cause))
	return run, cause
}

// recordRunTimes stores job's last run (now) and next run. Only the run timestamps
// are written, under the lock that also covers job updates (ApplyJobUpdate), so an
// edit saved while the backup ran is never reverted. The next run follows the
// registered cron entry, which is the job's current schedule even when it was edited
// meanwhile.
func (s *Scheduler) recordRunTimes(ctx context.Context, job *models.Job) {
	now := time.Now().UTC()
	s.mu.Lock()
	var next *time.Time
	if entryID, exists := s.entries[job.ID]; exists {
		if sched := s.cron.Entry(entryID).Schedule; sched != nil {
			n := sched.Next(now).UTC()
			next = &n
		}
	}
	// A job deleted while it ran stays deleted (ErrNotFound is ignored).
	saveErr := s.metadataStore.UpdateJobRunTimes(ctx, job.ID, &now, next)
	s.mu.Unlock()
	job.LastRun = &now
	if next != nil {
		job.NextRun = next
	}
	if saveErr != nil && !errors.Is(saveErr, store.ErrNotFound) {
		s.logger.Error("failed to persist job run metadata",
			slog.String("job_id", job.ID),
			slog.Any("error", saveErr),
		)
	}
}

// applyRetention prunes the job's scheduled backups of record's database after the
// successful scheduled backup record.
func (s *Scheduler) applyRetention(ctx context.Context, job *models.Job, record *models.BackupRecord) {
	if record == nil || record.Status != models.StatusCompleted || (job.RetentionDays <= 0 && job.RetentionCount <= 0) {
		return
	}
	history, listErr := s.metadataStore.ListBackupRecords(ctx, record.Database)
	if listErr != nil {
		s.logger.Warn("retention skipped: the backups cannot be listed",
			slog.String("job_id", job.ID), slog.String("database", record.Database), slog.Any("error", listErr))
		return
	}
	// Retention only ever sees this job's own scheduled backups (see
	// JobRetentionHistory).
	history = JobRetentionHistory(job, record.StorageTargetID, history)
	// Retention deletes softly: the archives stay for the grace period, the purge
	// removes them (and the run logs) afterwards.
	_, _ = prune(ctx, s.clock().UTC(), s.deleteGrace(), job.RetentionDays, job.RetentionCount, history,
		s.metadataStore, s.logger, s.retentionDeleted)
}
