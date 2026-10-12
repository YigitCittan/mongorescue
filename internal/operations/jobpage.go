package operations

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// Page sizes of ListJobsPage.
const (
	// DefaultJobPageSize is the page size of ListJobsPage without a limit.
	DefaultJobPageSize = 100
	// MaxJobPageSize is the largest page size of ListJobsPage.
	MaxJobPageSize = 500
	// maxJobCursor bounds the length of a jobs cursor.
	maxJobCursor = 2048
)

// jobCursorPrefix versions the jobs cursor format.
const jobCursorPrefix = "j1:"

// JobListRequest selects a page of ListJobsPage.
type JobListRequest struct {
	// Filter keeps the matching jobs; only the job fields of BulkFilter apply
	// (database, connection_id, q, enabled, schedule, last_status), as in
	// FilterJobs.
	Filter BulkFilter
	// Limit is the page size, 1 to MaxJobPageSize; 0 means DefaultJobPageSize.
	Limit int
	// After is the Next cursor of the previous page; empty starts at the first job.
	After string
}

// JobPage is one page of ListJobsPage.
type JobPage struct {
	// Items are the jobs of the page in name and ID order, heartbeat URLs masked.
	Items []*models.Job
	// Total counts every job matching the filter.
	Total int
	// Limit is the page size used.
	Limit int
	// Next is the cursor of the next page; empty on the last page.
	Next string
}

// jobCursor is the key of the last job of a page: the next page starts after it.
type jobCursor struct {
	Name string `json:"n"`
	ID   string `json:"i"`
}

// encodeJobCursor returns the opaque cursor of the page after the job name, id.
func encodeJobCursor(name, id string) string {
	raw, _ := json.Marshal(jobCursor{Name: name, ID: id})
	return base64.RawURLEncoding.EncodeToString(append([]byte(jobCursorPrefix), raw...))
}

// decodeJobCursor reads a cursor of encodeJobCursor; "" is the first page.
func decodeJobCursor(v string) (jobCursor, error) {
	var c jobCursor
	if v == "" {
		return c, nil
	}
	bad := public("after must be the next cursor of a previous page of GET /api/v1/jobs", ErrInvalid)
	if len(v) > maxJobCursor {
		return c, bad
	}
	raw, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil || len(raw) <= len(jobCursorPrefix) || string(raw[:len(jobCursorPrefix)]) != jobCursorPrefix {
		return c, bad
	}
	if err := json.Unmarshal(raw[len(jobCursorPrefix):], &c); err != nil || c.ID == "" {
		return jobCursor{}, bad
	}
	return c, nil
}

// jobKeyAfter reports whether job j sorts after cursor c in name and ID order (the
// order of the jobs_by_name index: bytewise, like Go strings).
func jobKeyAfter(j *models.Job, c jobCursor) bool {
	return j.Name > c.Name || (j.Name == c.Name && j.ID > c.ID)
}

// ListJobsPage returns one page of the jobs matching req.Filter, sorted by name and
// ID (stable: a job's position depends only on its name and ID), with the total
// number of matches and the cursor of the next page. Paging is by key: a job
// created or deleted between two pages moves no other job across a page boundary.
// The connection and state filters and the page itself are answered from the jobs
// indexes; the filters evaluated on the jobs (q, database, schedule, last_status)
// read the jobs of the indexed filters once. Invalid filters, limits or cursors
// return ErrInvalid errors.
func (s *Service) ListJobsPage(ctx context.Context, req JobListRequest) (*JobPage, error) {
	limit := req.Limit
	if limit == 0 {
		limit = DefaultJobPageSize
	}
	if limit < 1 || limit > MaxJobPageSize {
		return nil, public(fmt.Sprintf("limit must be an integer between 1 and %d", MaxJobPageSize), ErrInvalid)
	}
	after, err := decodeJobCursor(req.After)
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
	if f.Q == "" && f.Database == "" && f.Schedule == "" && f.LastStatus == "" {
		if page.Total, err = s.store.CountJobs(ctx, q); err != nil {
			return nil, fmt.Errorf("count jobs: %w", err)
		}
		q.AfterName, q.AfterID, q.Limit = after.Name, after.ID, limit
		w, qerr := s.store.QueryJobs(ctx, q)
		if qerr != nil {
			return nil, fmt.Errorf("list jobs: %w", qerr)
		}
		page.Items = models.RedactJobs(w.Jobs)
		if w.More {
			page.Next = encodeJobCursor(w.LastName, w.LastID)
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
	page.Total = len(matched)
	start := 0
	if after.ID != "" {
		start = sort.Search(len(matched), func(i int) bool { return jobKeyAfter(matched[i], after) })
	}
	end := min(start+limit, len(matched))
	page.Items = models.RedactJobs(matched[start:end])
	if end < len(matched) {
		last := matched[end-1]
		page.Next = encodeJobCursor(last.Name, last.ID)
	}
	return page, nil
}
