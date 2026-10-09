package operations

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
)

// DefaultCronExpression is the schedule of a job saved without one.
const DefaultCronExpression = "@daily"

// JobDetailsNextRuns is how many upcoming activations GetJobDetails reports.
const JobDetailsNextRuns = 3

// Job validation errors. They are ErrInvalid errors: adapters answer them with 400.
var (
	// ErrDatabaseRequired is returned when a job names no database.
	ErrDatabaseRequired = errors.New("database is required")
	// ErrNegativeRetention is returned when retention_days or retention_count is
	// negative.
	ErrNegativeRetention = errors.New("retention_days and retention_count must not be negative")
	// ErrPausedUntilPast is returned when paused_until is not in the future.
	ErrPausedUntilPast = errors.New("paused_until must be in the future")
	// ErrCollectionsNeedSingle is returned when a job covering several databases
	// names collections: collection filters only apply to single-database jobs.
	ErrCollectionsNeedSingle = errors.New("collections and exclude_collections only apply to single-database jobs (database_selection mode single)")
	// ErrInvalidParallelism is returned when parallelism is outside 1 to
	// models.MaxJobParallelism.
	ErrInvalidParallelism = errors.New("parallelism must be between 1 and 4")
	// ErrUsersAndRolesAdmin is returned when a job or backup of the admin database
	// asks for include_users_and_roles: the admin database holds every user and role
	// as regular data, so its dumps already contain them.
	ErrUsersAndRolesAdmin = errors.New("include_users_and_roles does not apply to the admin database: its dumps already contain every user and role")
	// ErrInvalidRPO is returned when rpo_minutes is set outside
	// models.MinRPOMinutes to models.MaxRPOMinutes (15 minutes to 90 days).
	ErrInvalidRPO = fmt.Errorf("rpo_minutes must be 0 (the default) or between %d and %d (15 minutes to 90 days)",
		models.MinRPOMinutes, models.MaxRPOMinutes)
)

// ErrJobChanged is returned by UpdateJob when the request names the job's updated_at
// and the job was changed since (adapters answer 409 Conflict).
var ErrJobChanged = errors.New("the job was changed meanwhile; reload it and try again")

// JobScheduler (re)schedules jobs in the running scheduler (implemented by
// *scheduler.Scheduler).
type JobScheduler interface {
	// ApplyJobUpdate runs persist (which stores job) and then replaces the job's cron
	// entry, under one lock, so concurrent updates are scheduled in the order they
	// were stored. It returns persist's error unchanged.
	ApplyJobUpdate(job *models.Job, persist func() error) error
}

// JobUpdate is the body of PUT /api/v1/jobs/{id}. The schedule, database, collection
// filters, connection and storage target are replaced; omitted retention, gzip and
// enabled fields keep the job's current values. The id, creation time and run history
// never change.
type JobUpdate struct {
	// Name is the human-readable label.
	Name string `json:"name"`
	// CronExpression is the schedule; "" means DefaultCronExpression.
	CronExpression string `json:"cron_expression"`
	// Database is the database to back up. Without DatabaseSelection it makes the job
	// a single-database job (as before selections existed); with neither, a
	// multi-database job keeps its selection.
	Database string `json:"database"`
	// DatabaseSelection, when set, replaces the job's database selection.
	DatabaseSelection *models.DatabaseSelection `json:"database_selection"`
	// Parallelism, when set, replaces how many databases a run backs up at once.
	Parallelism *int `json:"parallelism"`
	// Collections restricts the dump to these collections.
	Collections []string `json:"collections"`
	// ExcludeCollections lists collections skipped by the dump.
	ExcludeCollections []string `json:"exclude_collections"`
	// ConnectionID names the managed connection to back up from.
	ConnectionID string `json:"connection_id"`
	// StorageTargetID names the storage target; "" means the default target.
	StorageTargetID string `json:"storage_target_id"`
	// RetentionDays keeps backups for this many days (0 = forever) when set.
	RetentionDays *int `json:"retention_days"`
	// RetentionCount keeps at most this many backups (0 = unlimited) when set.
	RetentionCount *int `json:"retention_count"`
	// Gzip compresses the dumps when set.
	Gzip *bool `json:"gzip"`
	// IncludeUsersAndRoles, when set, replaces whether the dumps include the users and
	// roles of each database.
	IncludeUsersAndRoles *bool `json:"include_users_and_roles"`
	// Enabled schedules (true) or pauses (false) the job when set.
	Enabled *bool `json:"enabled"`
	// PausedUntil, for a paused job, resumes it automatically at that time. Pausing
	// without it pauses until resumed; a job left paused keeps its current value.
	PausedUntil *time.Time `json:"paused_until"`
	// VerifyAfterBackup, when set, replaces the job's post-backup verification
	// override ("" follows the setting, "on", "off").
	VerifyAfterBackup *models.VerifyOverride `json:"verify_after_backup"`
	// RestoreTest, when set, replaces the job's restore test policy.
	RestoreTest *models.RestoreTestPolicy `json:"restore_test"`
	// RPOMinutes, when set, replaces the job's recovery point objective in minutes
	// (0 restores the default from the schedule).
	RPOMinutes *int `json:"rpo_minutes"`
	// HeartbeatURL, when set, replaces the job's heartbeat URL: the masked value (as
	// responses show it) or models.SecretMask keeps the stored URL, "" removes it.
	HeartbeatURL *string `json:"heartbeat_url"`
	// ReadPreference, when set, replaces the job's read preference ("" uses the
	// connection's) and its tag sets with ReadPreferenceTags.
	ReadPreference     *string             `json:"read_preference"`
	ReadPreferenceTags []map[string]string `json:"read_preference_tags"`
	// MaxUploadMbps, when set, replaces the job's upload cap in megabits per
	// second (0 uses the general.max_upload_mbps setting).
	MaxUploadMbps *float64 `json:"max_upload_mbps"`
	// NumParallelCollections, when set, replaces mongodump's
	// --numParallelCollections (0 = mongodump's default).
	NumParallelCollections *int `json:"num_parallel_collections"`
	// BackupWindow, when set, replaces the job's backup window; an empty object
	// ({}) removes it.
	BackupWindow *models.BackupWindow `json:"backup_window"`
	// CopyTargets, when set, replaces the storage targets every backup of the job
	// is copied to (an empty list removes them).
	CopyTargets *[]string `json:"copy_targets"`
	// CopyMode, when set, replaces the job's copy mode ("async" or "sync").
	CopyMode *models.CopyMode `json:"copy_mode"`
	// RequireLockedCopies, when set, replaces the job's locked copies policy.
	RequireLockedCopies *bool `json:"require_locked_copies"`
	// UpdatedAt, when set, is the job's updated_at the client edited: the update is
	// refused with ErrJobChanged if the job was changed since.
	UpdatedAt *time.Time `json:"updated_at"`
}

// JobDetails is a job with its upcoming activations.
type JobDetails struct {
	*models.Job
	// NextRuns lists the next JobDetailsNextRuns activations in UTC; it is empty for
	// a disabled job or an unparsable schedule.
	NextRuns []time.Time `json:"next_runs"`
	// EffectiveRPOMinutes is the recovery point objective that applies to the job:
	// RPOMinutes when set, else the default from its schedule (two intervals plus an
	// hour, at least six hours). RPODefault reports that the default applies.
	EffectiveRPOMinutes int  `json:"effective_rpo_minutes"`
	RPODefault          bool `json:"rpo_default"`
	// WindowOpen reports, for a job with a backup window, whether a scheduled run
	// could start now. NextRuns then lists only the runs the window allows.
	WindowOpen *bool `json:"window_open,omitempty"`
	// PendingRetention is a shortening of the job's retention that takes effect
	// later (after the delete grace period), if any.
	PendingRetention *models.PendingChange `json:"pending_retention,omitempty"`
}

// JobSaveResult is a stored job and what its edit deferred: a shortened retention
// waits for the delete grace period (and, with the two-person rule, for a second
// administrator first). Warnings are notes on the saved job that did not refuse it
// (see JobWarnings).
type JobSaveResult struct {
	*models.Job
	JobProtection
	Warnings []string `json:"warnings,omitempty"`
}

// JobWarnings returns the notes on job worth showing after a save: a retention that
// deletes backups before the S3 Object Lock of its storage target ends (allowed, but
// storage keeps them, and their cost, until it does).
func (s *Service) JobWarnings(ctx context.Context, job *models.Job) []string {
	if s.cfg.Targets == nil || job == nil {
		return nil
	}
	t, err := s.cfg.Targets.Resolve(ctx, job.StorageTargetID)
	if err != nil {
		return nil
	}
	if w := t.RetentionLockWarning(job.RetentionDays, job.RetentionCount); w != "" {
		return []string{w}
	}
	return nil
}

// ValidateJob checks and normalises a job before it is created or updated: an empty
// schedule becomes DefaultCronExpression, the schedule must parse, the database must be
// named, retention must not be negative, the connection must exist and the storage
// target (the default one for "") is resolved into StorageTargetID and StorageType.
// Expected failures: ErrInvalid (wrapping scheduler.ErrInvalidCron,
// ErrDatabaseRequired or ErrNegativeRetention), ErrConnectionRequired,
// ErrUnknownConnection and ErrUnknownStorageTarget. An enabled job's PausedUntil is
// cleared; a stored PausedUntil that has passed is kept (the scheduler resumes the
// job within a minute), so it never blocks an edit. A PausedUntil sent by a client is
// checked with ValidatePausedUntil.
func (s *Service) ValidateJob(ctx context.Context, job *models.Job) error {
	job.CronExpression = strings.TrimSpace(job.CronExpression)
	if job.CronExpression == "" {
		job.CronExpression = DefaultCronExpression
	}
	if err := scheduler.ValidateCron(job.CronExpression); err != nil {
		return invalid(err)
	}
	if err := normalizeSelection(job); err != nil {
		return err
	}
	if err := s.checkCollectionFilters(job); err != nil {
		return err
	}
	if job.RetentionDays < 0 || job.RetentionCount < 0 {
		return invalid(ErrNegativeRetention)
	}
	if err := ValidateRPO(job.RPOMinutes); err != nil {
		return err
	}
	if job.IncludeUsersAndRoles && !job.MultiDatabase() && job.Database == models.AdminDatabase {
		return invalid(ErrUsersAndRolesAdmin)
	}
	job.HeartbeatURL = strings.TrimSpace(job.HeartbeatURL)
	if err := models.ValidateHeartbeatURL(job.HeartbeatURL); err != nil {
		return invalid(err)
	}
	if err := validateJobThrottling(job); err != nil {
		return err
	}
	if job.Enabled {
		job.PausedUntil = nil
	}
	if job.PausedUntil != nil {
		until := job.PausedUntil.UTC()
		job.PausedUntil = &until
	}
	if err := s.validateTrust(ctx, job); err != nil {
		return err
	}
	if _, err := s.ResolveConnection(ctx, job.ConnectionID); err != nil {
		return err
	}
	target, err := s.ResolveTarget(ctx, job.StorageTargetID)
	if err != nil {
		return err
	}
	job.StorageTargetID, job.StorageType = target.ID, target.Type
	resolved, err := s.resolveCopyTargets(ctx, job.CopyTargets, target.ID, job.CopyMode)
	if err != nil {
		return err
	}
	if job.RequireLockedCopies {
		if err = s.checkLockedCopies(ctx, resolved); err != nil {
			return err
		}
	}
	job.CopyTargets = nil
	for _, c := range resolved {
		job.CopyTargets = append(job.CopyTargets, c.ID)
	}
	if len(job.CopyTargets) == 0 {
		job.CopyMode = ""
	}
	s.snapshotKnownDatabases(ctx, job)
	return nil
}

// ValidateRPO checks a job's rpo_minutes: 0 (the default from the schedule) or
// between models.MinRPOMinutes and models.MaxRPOMinutes. Anything else is an
// ErrInvalidRPO (ErrInvalid) error.
func ValidateRPO(minutes int) error {
	if minutes != 0 && (minutes < models.MinRPOMinutes || minutes > models.MaxRPOMinutes) {
		return invalid(ErrInvalidRPO)
	}
	return nil
}

// normalizeSelection checks and normalises job's database selection: a job without
// one (a client that predates selections) becomes a single selection of Database; a
// single selection names Database (its one database wins over Database), and a
// multi-database selection clears Database and refuses collection filters.
// CollectionFilterChecker is implemented by backup engines that can tell up front
// whether a collection filter can be applied (*backup.Engine: wildcard patterns
// need its collection lister). A BackupEngine that implements it lets ValidateJob
// refuse such a job instead of letting every run fail.
type CollectionFilterChecker interface {
	// CheckCollectionFilter fails (wrapping backup.ErrCollectionFilter) for a filter
	// the engine cannot apply.
	CheckCollectionFilter(include, exclude []string) error
}

// checkCollectionFilters checks the job's collection filters and its per-database
// ones with the backup engine (see CollectionFilterChecker), as ErrInvalid errors.
func (s *Service) checkCollectionFilters(job *models.Job) error {
	checker, ok := s.cfg.Backup.(CollectionFilterChecker)
	if !ok {
		return nil
	}
	if err := checker.CheckCollectionFilter(job.Collections, job.ExcludeCollections); err != nil {
		return invalid(err)
	}
	for _, f := range job.DatabaseSelection.CollectionFilters {
		if err := checker.CheckCollectionFilter(f.Collections, f.ExcludeCollections); err != nil {
			return invalid(fmt.Errorf("%s: %w", f.Name, err))
		}
	}
	return nil
}

func normalizeSelection(job *models.Job) error {
	sel := job.DatabaseSelection.Clone()
	if sel.Mode == "" {
		sel = models.DatabaseSelection{Mode: models.SelectionSingle}
	}
	if sel.Mode == models.SelectionSingle && len(sel.Databases) == 0 && strings.TrimSpace(job.Database) != "" {
		sel.Databases = []string{strings.TrimSpace(job.Database)}
	}
	if sel.Mode == models.SelectionSingle && len(sel.Databases) == 0 {
		return invalid(ErrDatabaseRequired)
	}
	if err := sel.Normalize(); err != nil {
		return invalid(err)
	}
	job.DatabaseSelection = sel
	if job.Parallelism < 0 || job.Parallelism > models.MaxJobParallelism {
		return invalid(ErrInvalidParallelism)
	}
	job.Parallelism = max(job.Parallelism, 1)
	if !sel.Multi() {
		job.Database = sel.Databases[0]
		job.KnownDatabases = nil
		return validateNamespaces(job.Database, job.Collections, job.ExcludeCollections)
	}
	job.Database = ""
	if len(job.Collections) > 0 || len(job.ExcludeCollections) > 0 {
		return invalid(ErrCollectionsNeedSingle)
	}
	job.Collections, job.ExcludeCollections = nil, nil
	if !sel.Discovers() {
		job.KnownDatabases = nil
	}
	return nil
}

// snapshotKnownDatabases records, for an all or pattern job without known databases
// (a new job, or one whose selection or connection changed), the databases its
// selection matches now: without auto_include_new its runs back up only those. When
// the server cannot be asked, the first run records them instead.
func (s *Service) snapshotKnownDatabases(ctx context.Context, job *models.Job) {
	if !job.DatabaseSelection.Discovers() || job.KnownDatabases != nil || s.cfg.Jobs == nil {
		return
	}
	probe := job.Clone()
	probe.KnownDatabases = nil
	res, err := s.cfg.Jobs.ResolveJobDatabases(ctx, probe)
	if err != nil {
		s.logger.Info("the job's databases could not be listed now; its first run records them",
			logsafe.Attr("job_id", job.ID), logsafe.Error(err))
		return
	}
	job.KnownDatabases = res.Known
}

// RefreshKnownDatabases is called right before job replaces current, under the
// scheduler's lock: when both back up the same databases from the same connection,
// job takes current's known databases, so ones a run recorded since job was read are
// never dropped (and never announced as added again).
func RefreshKnownDatabases(job, current *models.Job) {
	if current != nil && current.KnownDatabases != nil && job.ConnectionID == current.ConnectionID &&
		job.Selection().SameMatch(current.Selection()) {
		job.KnownDatabases = slices.Clone(current.KnownDatabases)
	}
}

// CarryKnownDatabases sets job's known databases (server-managed, never taken from
// clients) before it is validated: existing's when job replaces existing with the
// same selection and connection, else none, so ValidateJob records them afresh.
// existing is nil for a new job.
func CarryKnownDatabases(job, existing *models.Job) {
	job.KnownDatabases = nil
	if existing != nil && job.ConnectionID == existing.ConnectionID && job.Selection().SameMatch(existing.Selection()) {
		job.KnownDatabases = slices.Clone(existing.KnownDatabases)
	}
}

// ValidatePausedUntil checks a paused_until sent by a client: it must be in the
// future (ErrPausedUntilPast, an ErrInvalid error). Nil is valid.
func (s *Service) ValidatePausedUntil(until *time.Time) error {
	if until != nil && !until.After(s.now()) {
		return invalid(ErrPausedUntilPast)
	}
	return nil
}

// UpdateJob replaces the editable fields of job id with u, validates the result like
// a new job (see ValidateJob), stores it and reschedules it at once: the new schedule,
// or a pause, takes effect without a restart. The job keeps its id, creation time,
// last run and backups. A shorter retention is not stored now: it takes effect after
// the delete grace period (see HoldRetention), reported in the result. Expected
// failures: ErrNotFound, ErrJobChanged, ErrPausedUntilPast (for a paused_until sent
// in the past) and those of ValidateJob.
func (s *Service) UpdateJob(ctx context.Context, id string, u JobUpdate) (*JobSaveResult, error) {
	existing, err := s.store.GetJob(ctx, id)
	if err != nil {
		return nil, notFound(err, "job not found")
	}
	job := existing.Clone()
	job.Name = u.Name
	job.CronExpression = u.CronExpression
	switch {
	case u.DatabaseSelection != nil:
		job.DatabaseSelection = u.DatabaseSelection.Clone()
		job.Database = u.Database
	case strings.TrimSpace(u.Database) != "":
		job.DatabaseSelection = models.DatabaseSelection{Mode: models.SelectionSingle, Databases: []string{strings.TrimSpace(u.Database)}}
		job.Database = u.Database
	case existing.MultiDatabase():
		// A client that only knows single-database jobs keeps the selection.
	default:
		job.DatabaseSelection, job.Database = models.DatabaseSelection{}, ""
	}
	job.Parallelism = derefOr(u.Parallelism, existing.Parallelism)
	job.Collections = u.Collections
	job.ExcludeCollections = u.ExcludeCollections
	job.ConnectionID = u.ConnectionID
	job.StorageTargetID = u.StorageTargetID
	job.RetentionDays = derefOr(u.RetentionDays, existing.RetentionDays)
	job.RetentionCount = derefOr(u.RetentionCount, existing.RetentionCount)
	job.Gzip = derefOr(u.Gzip, existing.Gzip)
	job.IncludeUsersAndRoles = derefOr(u.IncludeUsersAndRoles, existing.IncludeUsersAndRoles)
	if u.CopyTargets != nil {
		job.CopyTargets = slices.Clone(*u.CopyTargets)
	}
	job.CopyMode = derefOr(u.CopyMode, existing.CopyMode)
	job.RequireLockedCopies = derefOr(u.RequireLockedCopies, existing.RequireLockedCopies)
	CarryKnownDatabases(job, existing)
	job.Enabled = derefOr(u.Enabled, existing.Enabled)
	switch {
	case job.Enabled:
		job.PausedUntil = nil
	case u.PausedUntil != nil:
		if err = s.ValidatePausedUntil(u.PausedUntil); err != nil {
			return nil, err
		}
		job.PausedUntil = u.PausedUntil
	case existing.Enabled:
		// Paused now without a time: until resumed.
		job.PausedUntil = nil
	}
	job.VerifyAfterBackup = derefOr(u.VerifyAfterBackup, existing.VerifyAfterBackup)
	job.RPOMinutes = derefOr(u.RPOMinutes, existing.RPOMinutes)
	if u.HeartbeatURL != nil {
		if job.HeartbeatURL, err = models.ResolveHeartbeatURL(*u.HeartbeatURL, existing.HeartbeatURL); err != nil {
			return nil, invalid(err)
		}
	}
	if u.RestoreTest != nil {
		rt := *u.RestoreTest
		job.RestoreTest = &rt
	}
	if err = applyThrottlingUpdate(job, u); err != nil {
		return nil, err
	}
	if err = s.ValidateJob(ctx, job); err != nil {
		return nil, err
	}
	hold, err := s.HoldRetention(ctx, existing, job)
	if err != nil {
		return nil, err
	}
	// The next run belongs to the old schedule until the scheduler computes the new one.
	job.NextRun = nil
	if job.Enabled && s.cfg.Scheduler == nil {
		job.NextRun = nextRunOf(job.CronExpression, s.now())
	}
	// persist re-reads the job under the scheduler's lock: the precondition is checked
	// against, and the run history taken from, what is stored right now, so a run that
	// finished since the first read is not reverted.
	persist := func() error {
		current, getErr := s.store.GetJob(ctx, id)
		if getErr != nil {
			return notFound(getErr, "job not found")
		}
		if u.UpdatedAt != nil && !current.UpdatedAt.Equal(*u.UpdatedAt) {
			return ErrJobChanged
		}
		job.LastRun, job.CreatedAt, job.LastRestoreTest = current.LastRun, current.CreatedAt, current.LastRestoreTest
		RefreshKnownDatabases(job, current)
		// UpdateJob never recreates a job deleted meanwhile.
		return notFound(s.store.UpdateJob(ctx, job), "job not found")
	}
	if s.cfg.Scheduler != nil {
		err = s.cfg.Scheduler.ApplyJobUpdate(job, persist)
	} else {
		err = persist()
	}
	if err != nil {
		return nil, err
	}
	changed := job.RetentionDays != existing.RetentionDays || job.RetentionCount != existing.RetentionCount
	prot, err := s.ApplyRetentionHold(ctx, job.ID, hold, changed)
	if err != nil {
		return nil, err
	}
	return &JobSaveResult{Job: job, JobProtection: *prot, Warnings: s.JobWarnings(ctx, job)}, nil
}

// GetJobDetails returns job id with its next JobDetailsNextRuns activations, or an
// ErrNotFound error.
func (s *Service) GetJobDetails(ctx context.Context, id string) (*JobDetails, error) {
	job, err := s.GetJob(ctx, id)
	if err != nil {
		return nil, err
	}
	details := &JobDetails{Job: job, NextRuns: []time.Time{}, PendingRetention: s.pendingRetention(ctx, job.ID)}
	rpo, isDefault := scheduler.EffectiveRPO(job, s.now())
	details.EffectiveRPOMinutes, details.RPODefault = int(rpo/time.Minute), isDefault
	if job.Enabled {
		details.NextRuns = allowedRuns(job, s.now())
	}
	if w := job.BackupWindow; w != nil {
		open := w.Contains(s.now())
		details.WindowOpen = &open
	}
	return details, nil
}

// windowLookahead bounds the activations allowedRuns reads to find the next
// JobDetailsNextRuns runs a backup window allows.
const windowLookahead = 2000

// allowedRuns returns the next JobDetailsNextRuns activations of job after now
// that its backup window lets start (all of them without a window).
func allowedRuns(job *models.Job, now time.Time) []time.Time {
	n := JobDetailsNextRuns
	if job.BackupWindow != nil {
		n = windowLookahead
	}
	out := []time.Time{}
	for _, t := range scheduler.NextRuns(job.CronExpression, now, n) {
		if job.BackupWindow.Contains(t) {
			out = append(out, t)
			if len(out) == JobDetailsNextRuns {
				break
			}
		}
	}
	return out
}

// nextRunOf returns the first activation of expr after from, or nil.
func nextRunOf(expr string, from time.Time) *time.Time {
	runs := scheduler.NextRuns(expr, from, 1)
	if len(runs) == 0 {
		return nil
	}
	return &runs[0]
}
