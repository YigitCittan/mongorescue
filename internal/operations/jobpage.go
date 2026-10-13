package operations

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// Page sizes of ListJobsPage.
const (
	// DefaultJobPageSize is the page size of ListJobsPage without a limit.
	DefaultJobPageSize = 100
	// MaxJobPageSize is the largest page size of ListJobsPage.
	MaxJobPageSize = 500
	// MaxJobCursor bounds the length of a jobs cursor (the after of ListJobsPage).
	MaxJobCursor = 2048
)

// Orders of ListJobsPage (JobListRequest.Sort); ties are broken by job ID.
const (
	// JobSortName orders by name (the default).
	JobSortName = "name"
	// JobSortNextRun orders by the next scheduled run; paused jobs and jobs without
	// one come last in both directions.
	JobSortNextRun = "next_run"
	// JobSortLastStatus orders by the outcome of the last run (failed, partial,
	// cancelled, in_progress, completed; then the newest run first), jobs that
	// never ran last in both directions.
	JobSortLastStatus = "last_status"
	// JobSortCreatedAt orders by creation time.
	JobSortCreatedAt = "created_at"
)

// JobSorts are the accepted values of JobListRequest.Sort.
var JobSorts = []string{JobSortName, JobSortNextRun, JobSortLastStatus, JobSortCreatedAt}

// jobCursorPrefix versions the jobs cursor format.
const jobCursorPrefix = "j1:"

// JobListRequest selects a page of ListJobsPage.
type JobListRequest struct {
	// Filter keeps the matching jobs; only the job fields of BulkFilter apply
	// (database, connection_id, q, enabled, schedule, last_status), as in
	// FilterJobs.
	Filter BulkFilter
	// Sort is the order, one of JobSorts ("" is JobSortName); Desc reverses it,
	// except that jobs without a value stay last.
	Sort string
	Desc bool
	// Limit is the page size, 1 to MaxJobPageSize; 0 means DefaultJobPageSize.
	Limit int
	// After is the Next cursor of the previous page in the same order; empty
	// starts at the first job.
	After string
}

// JobPage is one page of ListJobsPage.
type JobPage struct {
	// Items are the jobs of the page in the requested order, heartbeat URLs masked.
	Items []*models.Job
	// Total counts every job matching the filter.
	Total int
	// Limit is the page size used.
	Limit int
	// Next is the cursor of the next page; empty on the last page.
	Next string
}

// jobCursor is the position of the last job of a page in its order: the next page
// starts after it.
type jobCursor struct {
	Sort string       `json:"s"`
	Desc bool         `json:"d,omitempty"`
	Key  store.JobKey `json:"k"`
}

// encodeJobCursor returns the opaque cursor of the page after key in an order.
func encodeJobCursor(sortBy string, desc bool, key store.JobKey) string {
	raw, _ := json.Marshal(jobCursor{Sort: sortBy, Desc: desc, Key: key})
	return base64.RawURLEncoding.EncodeToString(append([]byte(jobCursorPrefix), raw...))
}

// decodeJobCursor reads a cursor of encodeJobCursor for the order sortBy, desc;
// "" is the first page (nil).
func decodeJobCursor(v, sortBy string, desc bool) (*store.JobKey, error) {
	if v == "" {
		return nil, nil
	}
	bad := public("after must be the next cursor of a previous page of GET /api/v1/jobs", ErrInvalid)
	if len(v) > MaxJobCursor {
		return nil, bad
	}
	raw, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil || len(raw) <= len(jobCursorPrefix) || string(raw[:len(jobCursorPrefix)]) != jobCursorPrefix {
		return nil, bad
	}
	var c jobCursor
	if err := json.Unmarshal(raw[len(jobCursorPrefix):], &c); err != nil || c.Key.ID == "" {
		return nil, bad
	}
	if c.Sort != sortBy || c.Desc != desc {
		return nil, public("after is the cursor of another sort or order: start again without after", ErrInvalid)
	}
	return &c.Key, nil
}

// ListJobsPage returns one page of the jobs matching req.Filter in the order of
// req.Sort and req.Desc (stable: ties are broken by ID), with the total number of
// matches and the cursor of the next page. Paging is by key: a job created or
// deleted between two pages moves no other job across a page boundary. Pages by
// name, creation time, next run and last status, with every filter, are answered in
// SQL from the jobs indexes and the jobs' search, schedule, database and last
// status columns (see indexedJobFilter); otherwise the jobs of the connection and
// state filters are read once and matched as FilterJobs does. Invalid filters,
// orders, limits or cursors return ErrInvalid errors.
func (s *Service) ListJobsPage(ctx context.Context, req JobListRequest) (*JobPage, error) {
	limit := req.Limit
	if limit == 0 {
		limit = DefaultJobPageSize
	}
	if limit < 1 || limit > MaxJobPageSize {
		return nil, public(fmt.Sprintf("limit must be an integer between 1 and %d", MaxJobPageSize), ErrInvalid)
	}
	sortBy := req.Sort
	if sortBy == "" {
		sortBy = JobSortName
	}
	if !slices.Contains(JobSorts, sortBy) {
		return nil, public("sort must be one of "+strings.Join(JobSorts, ", "), ErrInvalid)
	}
	after, err := decodeJobCursor(req.After, sortBy, req.Desc)
	if err != nil {
		return nil, err
	}
	f := req.Filter
	if err = checkBulkFilter(BulkJobs, &f); err != nil {
		return nil, err
	}
	if err = checkJobFilter(&f); err != nil {
		return nil, err
	}
	q := store.JobQuery{ConnectionID: f.ConnectionID, Enabled: f.Enabled}
	page := &JobPage{Limit: limit}
	indexed, err := s.indexedJobFilter(ctx, &f, sortBy)
	if err != nil {
		return nil, err
	}
	if indexed {
		q.Search, q.Database, q.Schedule, q.LastStatus = f.Q, f.Database, f.Schedule, f.LastStatus
		if page.Total, err = s.store.CountJobs(ctx, q); err != nil {
			return nil, fmt.Errorf("count jobs: %w", err)
		}
		q.Sort, q.Desc, q.After, q.Limit = store.JobSort(sortBy), req.Desc, after, limit
		w, qerr := s.store.QueryJobs(ctx, q)
		if qerr != nil {
			return nil, fmt.Errorf("list jobs: %w", qerr)
		}
		page.Items = models.RedactJobs(w.Jobs)
		if w.More {
			page.Next = encodeJobCursor(sortBy, req.Desc, w.Last)
		}
		return page, nil
	}
	w, err := s.store.QueryJobs(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	matched, err := s.keepMatchingJobs(ctx, w.Jobs, &f)
	if err != nil {
		return nil, err
	}
	keys, err := s.jobKeys(ctx, matched, sortBy)
	if err != nil {
		return nil, err
	}
	order := make([]int, len(matched))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int { return store.CompareJobKeys(keys[a], keys[b], req.Desc) })
	page.Total = len(matched)
	start := 0
	if after != nil {
		start = sort.Search(len(order), func(i int) bool { return store.CompareJobKeys(keys[order[i]], *after, req.Desc) > 0 })
	}
	end := min(start+limit, len(order))
	items := make([]*models.Job, 0, end-start)
	for _, i := range order[start:end] {
		items = append(items, matched[i])
	}
	page.Items = models.RedactJobs(items)
	if end < len(order) {
		page.Next = encodeJobCursor(sortBy, req.Desc, keys[order[end-1]])
	}
	return page, nil
}

// foreignLastBackups is implemented by stores that keep the jobs' last status
// (*store.SQLiteStore, see store.SQLiteStore.HasForeignLastBackups).
type foreignLastBackups interface {
	HasForeignLastBackups(ctx context.Context, set auth.ConnectionSet) (bool, error)
}

// indexedJobFilter reports whether the store answers the jobs list for filter f in
// order sortBy with the same result as FilterJobs: always, except for a search or
// database containing store.JobSearchSeparator, and for a last-status filter or
// order of a caller limited to some connections while one of its jobs has its
// newest backup on another connection (the stored last status is then not the one
// of the newest backup the caller may see), or without a store that keeps it.
func (s *Service) indexedJobFilter(ctx context.Context, f *BulkFilter, sortBy string) (bool, error) {
	if strings.Contains(f.Q, store.JobSearchSeparator) || strings.Contains(f.Database, store.JobSearchSeparator) {
		return false, nil
	}
	set := auth.ConnectionFilter(ctx)
	if (f.LastStatus == "" && sortBy != JobSortLastStatus) || !set.Limited() {
		return true, nil
	}
	fs, ok := s.cfg.Store.(foreignLastBackups)
	if !ok {
		return false, nil
	}
	foreign, err := fs.HasForeignLastBackups(ctx, set)
	if err != nil {
		return false, fmt.Errorf("list jobs: %w", err)
	}
	return !foreign, nil
}

// jobKeys returns the key of each job of jobs in order sortBy (as the store orders
// them; for JobSortLastStatus the rank of the last run's outcome, then the newest
// backup or run first, as the store keeps them on the jobs).
func (s *Service) jobKeys(ctx context.Context, jobs []*models.Job, sortBy string) ([]store.JobKey, error) {
	keys := make([]store.JobKey, len(jobs))
	if sortBy != JobSortLastStatus {
		for i, j := range jobs {
			keys[i] = store.JobKeyOf(j, store.JobSort(sortBy))
		}
		return keys, nil
	}
	if len(jobs) == 0 {
		return keys, nil
	}
	lastBackups, lastRuns, err := s.lastOutcomes(ctx, jobs)
	if err != nil {
		return nil, err
	}
	for i, j := range jobs {
		k := store.JobKey{ID: j.ID}
		// The ranks the store keeps on the jobs (the LastRun* values are its
		// JobLast* values), so both answers page with the same cursors.
		rank, ran := store.JobLastStatusRank(jobLastStatus(j, lastBackups, lastRuns))
		switch {
		case !ran:
			k.Null = true
		default:
			k.Int = rank
			if r := lastRuns[j.ID]; j.MultiDatabase() && r != nil {
				k.Time = store.JobStatusTime(r.StartedAt)
			} else if b := lastBackups[j.ID]; !j.MultiDatabase() && b != nil {
				k.Time = store.JobStatusTime(b.StartedAt)
			}
		}
		keys[i] = k
	}
	return keys, nil
}
