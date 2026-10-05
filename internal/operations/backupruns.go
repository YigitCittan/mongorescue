package operations

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
)

// MaxBackupDatabases is the most databases one StartBackups request may name.
const MaxBackupDatabases = 200

// Validation errors of StartBackups requests (all also match ErrInvalid).
var (
	// ErrDatabasesConflict is returned when a request names both database and
	// databases.
	ErrDatabasesConflict = errors.New("database and databases cannot be combined: give one of them")
	// ErrDatabasesCount is returned when databases names no database or more than
	// MaxBackupDatabases.
	ErrDatabasesCount = fmt.Errorf("databases must name between 1 and %d databases", MaxBackupDatabases)
	// ErrDuplicateDatabase is returned when databases names a database twice.
	ErrDuplicateDatabase = errors.New("databases names a database more than once")
	// ErrCollectionsNeedOneDatabase is returned when collections or
	// exclude_collections are given with several databases.
	ErrCollectionsNeedOneDatabase = errors.New("collections and exclude_collections only apply to a backup of a single database; give each database its own filter in databases instead")
	// ErrCollectionFilterTwice is returned when a request of one database sets a
	// collection filter both at the top level and in its databases entry.
	ErrCollectionFilterTwice = errors.New("give the collection filter either in the databases entry or as collections and exclude_collections, not both")
)

// BusyDatabase is a database of a StartBackups request that was not backed up
// because another backup of it was already running.
type BusyDatabase struct {
	// Database is the database name.
	Database string `json:"database"`
	// Busy is always true.
	Busy bool `json:"busy"`
	// Error says why the database was skipped.
	Error string `json:"error"`
}

// BackupRun is what StartBackups started (the body of POST /api/v1/backups with
// databases): one in-progress backup per database, grouped under RunID.
type BackupRun struct {
	// RunID is the run's ID, stored on each backup as run_id (list them with
	// GET /api/v1/backups?run_id=...).
	RunID string `json:"run_id"`
	// Backups are snapshots of the in-progress records, in the order they run.
	Backups []*models.BackupRecord `json:"backups"`
	// Busy are the databases skipped because another backup of them was running.
	Busy []BusyDatabase `json:"busy"`
}

// StartBackups backs up several databases of one connection now, in the background:
// each into its own record and archive, grouped under one run ID and run exactly
// like the databases of a multi-database job run (up to req.Parallelism at once,
// cancelled together, one summary notification). req names its databases in
// Databases and must not set Database; each entry may carry its own collection
// filter (models.DatabaseFilter), applied to that database exactly like the
// collection filter of a backup of one database. The top-level collections and
// excluded collections apply only to a request of one database, and
// include_users_and_roles applies to every database it can (not admin). A database another backup is running is not backed
// up: it comes back in BackupRun.Busy while the others start; when every database is
// busy the request fails with ErrBusy. Expected failures: ErrInvalid (with
// ErrDatabasesConflict, ErrDatabasesCount, ErrDuplicateDatabase,
// ErrCollectionsNeedOneDatabase, ErrCollectionFilterTwice, ErrInvalidParallelism), ErrConnectionRequired,
// ErrUnknownConnection, ErrUnknownStorageTarget, ErrBusy, ErrShuttingDown and
// ErrSchedulerUnavailable.
func (s *Service) StartBackups(ctx context.Context, req BackupRequest) (*BackupRun, error) {
	filters, err := validateBackupDatabases(req)
	if err != nil {
		return nil, err
	}
	databases := models.FilterNames(filters)
	if s.cfg.Jobs == nil {
		return nil, ErrSchedulerUnavailable
	}
	opts, err := s.manualOptions(ctx, req)
	if err != nil {
		return nil, err
	}

	var locks []func()
	var started, busyNames []string
	busy := []BusyDatabase{}
	releaseAll := func() {
		for _, release := range locks {
			release()
		}
	}
	for _, db := range databases {
		release, lockErr := s.cfg.Runs.Acquire(runs.BackupKey(opts.ConnectionID, db))
		switch {
		case lockErr == nil:
			locks, started = append(locks, release), append(started, db)
		case errors.Is(lockErr, runs.ErrBusy):
			busy = append(busy, BusyDatabase{Database: db, Busy: true, Error: scheduler.BusyError(db)})
			busyNames = append(busyNames, db)
		default:
			releaseAll()
			return nil, runError(lockErr, "")
		}
	}
	if len(started) == 0 {
		return nil, public("a backup of every requested database is already running: "+strings.Join(busyNames, ", "), ErrBusy)
	}

	plan, err := s.cfg.Jobs.PrepareAdHocRun(scheduler.AdHocRun{
		Options: opts, Databases: started, Filters: filters, Locks: locks, Busy: busyNames, Parallelism: derefOr(req.Parallelism, 1),
	})
	if err != nil {
		releaseAll()
		return nil, invalid(err)
	}
	// From here the plan owns the locks: AbandonJobRun or the run releases them.
	if err = s.cfg.Jobs.BeginJobRun(ctx, plan); err != nil {
		s.cfg.Jobs.AbandonJobRun(ctx, plan, err)
		return nil, jobRunError(err)
	}
	out := &BackupRun{RunID: plan.Run.ID, Backups: make([]*models.BackupRecord, len(plan.Records)), Busy: busy}
	for i, rec := range plan.Records {
		snapshot := *rec
		out.Backups[i] = &snapshot
	}
	if err = s.cfg.Runs.Go("", func(runCtx context.Context) {
		// The scheduler persists the records and publishes the outcome events.
		_, _ = s.cfg.Jobs.ExecuteJobRun(runCtx, plan)
	}); err != nil {
		s.cfg.Jobs.AbandonJobRun(ctx, plan, err)
		return nil, runError(err, "")
	}
	return out, nil
}

// validateBackupDatabases checks the databases of a StartBackups request and
// returns them normalized (see models.DatabaseFilter.Normalize). A request of one
// database takes its collection filter from its entry or from the top-level
// collections and exclude_collections (not both); the result then carries it.
func validateBackupDatabases(req BackupRequest) ([]models.DatabaseFilter, error) {
	if strings.TrimSpace(req.Database) != "" {
		return nil, invalid(ErrDatabasesConflict)
	}
	if len(req.Databases) == 0 || len(req.Databases) > MaxBackupDatabases {
		return nil, invalid(ErrDatabasesCount)
	}
	if err := validateParallelism(req.Parallelism); err != nil {
		return nil, err
	}
	filters := make([]models.DatabaseFilter, 0, len(req.Databases))
	seen := make(map[string]bool, len(req.Databases))
	for _, entry := range req.Databases {
		f := entry.Clone()
		if err := f.Normalize(); err != nil {
			return nil, invalid(fmt.Errorf("databases: %w", err))
		}
		if seen[f.Name] {
			return nil, public(fmt.Sprintf("%s: %s", ErrDuplicateDatabase.Error(), f.Name), ErrInvalid, ErrDuplicateDatabase)
		}
		seen[f.Name] = true
		filters = append(filters, f)
	}
	topLevel := len(req.Collections) > 0 || len(req.ExcludeCollections) > 0
	if len(filters) > 1 {
		if topLevel {
			return nil, invalid(ErrCollectionsNeedOneDatabase)
		}
		return filters, nil
	}
	only := &filters[0]
	if topLevel {
		if only.Filtered() {
			return nil, invalid(ErrCollectionFilterTwice)
		}
		if err := validateNamespaces(only.Name, req.Collections, req.ExcludeCollections); err != nil {
			return nil, err
		}
		only.Collections, only.ExcludeCollections = slices.Clone(req.Collections), slices.Clone(req.ExcludeCollections)
	}
	if req.IncludeUsersAndRoles && only.Name == models.AdminDatabase {
		return nil, invalid(ErrUsersAndRolesAdmin)
	}
	return filters, nil
}

// validateParallelism checks an optional parallelism of a backup request: 0 to
// models.MaxJobParallelism, as for jobs (0 means 1).
func validateParallelism(p *int) error {
	if p != nil && (*p < 0 || *p > models.MaxJobParallelism) {
		return invalid(ErrInvalidParallelism)
	}
	return nil
}
