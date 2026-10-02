package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// Health is the answer of GET /api/v1/health.
type Health struct {
	// Status is "healthy" when the server is up.
	Status string `json:"status"`
	// Version is the server's version.
	Version string `json:"version"`
	// Time is the server's clock.
	Time time.Time `json:"time"`
}

// MeUser is the user behind a session or an API key.
type MeUser struct {
	// ID and Username identify the user.
	ID       string `json:"id"`
	Username string `json:"username"`
}

// Me is the answer of GET /api/v1/auth/me.
type Me struct {
	// User is the acting user (the creator of an API key); nil when signed out.
	User *MeUser `json:"user"`
	// Auth is how the request authenticated: "api_key", "session" or "" (signed out).
	Auth string `json:"auth"`
	// Scope is the effective scope (read, operator or admin). Servers before v0.17
	// do not report it; it is empty then.
	Scope string `json:"scope,omitempty"`
}

// BackupBrief summarises one backup in Stats.
type BackupBrief struct {
	// ID, Database, JobID and Status identify the backup.
	ID       string `json:"id"`
	Database string `json:"database"`
	JobID    string `json:"job_id,omitempty"`
	Status   string `json:"status"`
	// StartedAt is when it started.
	StartedAt time.Time `json:"started_at"`
	// ErrorMessage explains a failure.
	ErrorMessage string `json:"error_message,omitempty"`
}

// Stats is the answer of GET /api/v1/stats (the fields the CLI shows).
type Stats struct {
	// Backup counts.
	TotalBackups     int `json:"total_backups"`
	CompletedBackups int `json:"completed_backups"`
	FailedBackups    int `json:"failed_backups"`
	FailedBackups24h int `json:"failed_backups_24h"`
	ActiveBackups    int `json:"active_backups"`
	// TotalBytes is the stored size of every backup.
	TotalBytes int64 `json:"total_bytes"`
	// Restore counts.
	TotalRestores  int `json:"total_restores"`
	ActiveRestores int `json:"active_restores"`
	// ActiveJobs counts the enabled jobs.
	ActiveJobs int `json:"active_jobs"`
	// LastBackup is the newest backup.
	LastBackup *BackupBrief `json:"last_backup,omitempty"`
	// Degraded reports that some figures could not be read.
	Degraded       bool   `json:"degraded,omitempty"`
	DegradedReason string `json:"degraded_reason,omitempty"`
}

// ReadinessSummary counts the readiness rows per status.
type ReadinessSummary struct {
	// OK, Warn and Fail count the rows of each status.
	OK   int `json:"ok"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
}

// ReadinessRPO is the recovery point objective of a readiness row.
type ReadinessRPO struct {
	// TargetSeconds is the strictest objective; AgeSeconds the age of the newest
	// good backup.
	TargetSeconds float64 `json:"target_seconds"`
	AgeSeconds    float64 `json:"age_seconds"`
	// Met is absent when every job is paused.
	Met *bool `json:"met,omitempty"`
}

// ReadinessBackup names a backup of a readiness row.
type ReadinessBackup struct {
	// ID and At identify the backup.
	ID string    `json:"id"`
	At time.Time `json:"at"`
}

// ReadinessRow is one database of the readiness report.
type ReadinessRow struct {
	// ConnectionID, ConnectionName and Database name the database.
	ConnectionID   string `json:"connection_id"`
	ConnectionName string `json:"connection_name,omitempty"`
	Database       string `json:"database"`
	// LastGoodBackup is the newest completed backup.
	LastGoodBackup *ReadinessBackup `json:"last_good_backup,omitempty"`
	// RPO is the objective and whether it is met.
	RPO ReadinessRPO `json:"rpo"`
	// Status is ok, warn or fail; Reasons explain it.
	Status  string   `json:"status"`
	Reasons []string `json:"reasons"`
}

// Readiness is the answer of GET /api/v1/readiness (the fields the CLI shows).
type Readiness struct {
	// GeneratedAt is when the report was computed.
	GeneratedAt time.Time `json:"generated_at"`
	// Summary counts the rows per status.
	Summary ReadinessSummary `json:"summary"`
	// Rows has one row per database a job backs up, failing rows first.
	Rows []ReadinessRow `json:"rows"`
}

// BackupRequest is the body of POST /api/v1/backups.
type BackupRequest struct {
	// ConnectionID and Database name what to back up.
	ConnectionID string `json:"connection_id"`
	Database     string `json:"database"`
	// Collections or ExcludeCollections narrow the backup.
	Collections        []string `json:"collections,omitempty"`
	ExcludeCollections []string `json:"exclude_collections,omitempty"`
	// StorageTargetID selects the target; empty is the default target.
	StorageTargetID string `json:"storage_target_id,omitempty"`
	// Gzip overrides the default compression when set.
	Gzip *bool `json:"gzip,omitempty"`
	// IncludeUsersAndRoles dumps the database's users and roles too.
	IncludeUsersAndRoles bool `json:"include_users_and_roles,omitempty"`
}

// JobStart is what POST /api/v1/jobs/{id}/run started: the backup of a
// single-database job or the run of a multi-database job.
type JobStart struct {
	// Backup is set for a single-database job.
	Backup *models.BackupRecord
	// Run is set for a multi-database job.
	Run *models.JobRun
}

// Preflight returns the preflight result carried by a refused restore (409 with the
// checks in data), if e is one.
func (e *APIError) Preflight() (*models.PreflightResult, bool) {
	if e == nil || e.StatusCode != http.StatusConflict || len(e.Data) == 0 {
		return nil, false
	}
	var p models.PreflightResult
	if err := json.Unmarshal(e.Data, &p); err != nil || len(p.Checks) == 0 {
		return nil, false
	}
	return &p, true
}

// Health calls GET /api/v1/health.
func (c *Client) Health(ctx context.Context) (*Result[Health], error) {
	return call[Health](ctx, c, http.MethodGet, "/health", nil, nil)
}

// Me calls GET /api/v1/auth/me.
func (c *Client) Me(ctx context.Context) (*Result[Me], error) {
	return call[Me](ctx, c, http.MethodGet, "/auth/me", nil, nil)
}

// Stats calls GET /api/v1/stats.
func (c *Client) Stats(ctx context.Context) (*Result[Stats], error) {
	return call[Stats](ctx, c, http.MethodGet, "/stats", nil, nil)
}

// ActiveRuns calls GET /api/v1/runs/active.
func (c *Client) ActiveRuns(ctx context.Context) (*Result[[]models.RunProgress], error) {
	return call[[]models.RunProgress](ctx, c, http.MethodGet, "/runs/active", nil, nil)
}

// Readiness calls GET /api/v1/readiness.
func (c *Client) Readiness(ctx context.Context) (*Result[Readiness], error) {
	return call[Readiness](ctx, c, http.MethodGet, "/readiness", nil, nil)
}

// ListBackups calls GET /api/v1/backups with the filters in query (see docs/api.md).
func (c *Client) ListBackups(ctx context.Context, query url.Values) (*Result[[]models.BackupRecord], error) {
	return call[[]models.BackupRecord](ctx, c, http.MethodGet, "/backups", query, nil)
}

// ListRestores calls GET /api/v1/restores with the filters in query.
func (c *Client) ListRestores(ctx context.Context, query url.Values) (*Result[[]models.RestoreRecord], error) {
	return call[[]models.RestoreRecord](ctx, c, http.MethodGet, "/restores", query, nil)
}

// ListJobs calls GET /api/v1/jobs with the filters in query.
func (c *Client) ListJobs(ctx context.Context, query url.Values) (*Result[[]models.Job], error) {
	return call[[]models.Job](ctx, c, http.MethodGet, "/jobs", query, nil)
}

// ListConnections calls GET /api/v1/connections.
func (c *Client) ListConnections(ctx context.Context) (*Result[[]models.Connection], error) {
	return call[[]models.Connection](ctx, c, http.MethodGet, "/connections", nil, nil)
}

// ListStorageTargets calls GET /api/v1/storage-targets.
func (c *Client) ListStorageTargets(ctx context.Context) (*Result[[]models.StorageTarget], error) {
	return call[[]models.StorageTarget](ctx, c, http.MethodGet, "/storage-targets", nil, nil)
}

// GetBackup returns backup id (GET /api/v1/backups?id=), or an ErrNotFound error.
func (c *Client) GetBackup(ctx context.Context, id string) (*Result[models.BackupRecord], error) {
	list, err := c.ListBackups(ctx, url.Values{"id": {id}})
	if err != nil {
		return nil, err
	}
	return single(list, id, "backup", func(b models.BackupRecord) string { return b.ID })
}

// GetRestore returns restore id (GET /api/v1/restores?id=), or an ErrNotFound error.
func (c *Client) GetRestore(ctx context.Context, id string) (*Result[models.RestoreRecord], error) {
	list, err := c.ListRestores(ctx, url.Values{"id": {id}})
	if err != nil {
		return nil, err
	}
	return single(list, id, "restore", func(r models.RestoreRecord) string { return r.ID })
}

// single picks the item with id from a list result filtered by it.
func single[T any](list *Result[[]T], id, kind string, idOf func(T) string) (*Result[T], error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(list.Raw, &raws); err != nil {
		return nil, fmt.Errorf("%w: decode %ss: %w", ErrUnexpectedResponse, kind, err)
	}
	for i, it := range list.Value {
		if idOf(it) == id && i < len(raws) {
			return &Result[T]{Value: it, Raw: raws[i]}, nil
		}
	}
	return nil, fmt.Errorf("%w: %s %s", ErrNotFound, kind, id)
}

// JobRuns calls GET /api/v1/jobs/{id}/runs (newest first, at most limit; 0 is the
// server's default).
func (c *Client) JobRuns(ctx context.Context, jobID string, limit int) (*Result[[]models.JobRun], error) {
	var q url.Values
	if limit > 0 {
		q = url.Values{"limit": {strconv.Itoa(limit)}}
	}
	return call[[]models.JobRun](ctx, c, http.MethodGet, "/jobs/"+url.PathEscape(jobID)+"/runs", q, nil)
}

// GetJobRun returns run runID of job jobID, or an ErrNotFound error. It looks at the
// job's 50 newest runs.
func (c *Client) GetJobRun(ctx context.Context, jobID, runID string) (*Result[models.JobRun], error) {
	list, err := c.JobRuns(ctx, jobID, 50)
	if err != nil {
		return nil, err
	}
	return single(list, runID, "job run", func(r models.JobRun) string { return r.ID })
}

// StartBackup calls POST /api/v1/backups and returns the in-progress record.
func (c *Client) StartBackup(ctx context.Context, req BackupRequest) (*Result[models.BackupRecord], error) {
	return call[models.BackupRecord](ctx, c, http.MethodPost, "/backups", nil, req)
}

// RunJob calls POST /api/v1/jobs/{id}/run.
func (c *Client) RunJob(ctx context.Context, jobID string) (*Result[JobStart], error) {
	res, err := call[json.RawMessage](ctx, c, http.MethodPost, "/jobs/"+url.PathEscape(jobID)+"/run", nil, nil)
	if err != nil {
		return nil, err
	}
	var probe map[string]json.RawMessage
	if err = json.Unmarshal(res.Raw, &probe); err != nil {
		return nil, fmt.Errorf("%w: decode job start: %w", ErrUnexpectedResponse, err)
	}
	out := &Result[JobStart]{Raw: res.Raw}
	if _, isRun := probe["databases"]; isRun {
		out.Value.Run = &models.JobRun{}
		err = json.Unmarshal(res.Raw, out.Value.Run)
	} else {
		out.Value.Backup = &models.BackupRecord{}
		err = json.Unmarshal(res.Raw, out.Value.Backup)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: decode job start: %w", ErrUnexpectedResponse, err)
	}
	return out, nil
}

// Preflight calls POST /api/v1/restores/preflight.
func (c *Client) Preflight(ctx context.Context, req models.RestoreRequest) (*Result[models.PreflightResult], error) {
	return call[models.PreflightResult](ctx, c, http.MethodPost, "/restores/preflight", nil, req)
}

// StartRestore calls POST /api/v1/restore and returns the in-progress record. A
// failed preflight is an *APIError (ErrConflict) whose Preflight method returns the
// checks.
func (c *Client) StartRestore(ctx context.Context, req models.RestoreRequest) (*Result[models.RestoreRecord], error) {
	return call[models.RestoreRecord](ctx, c, http.MethodPost, "/restore", nil, req)
}

// VerifyBackup calls POST /api/v1/backups/{id}/verify, which re-reads the archive in
// the background, and returns the record as it was before (poll GetBackup until its
// VerifiedAt changes).
func (c *Client) VerifyBackup(ctx context.Context, id string) (*Result[models.BackupRecord], error) {
	return call[models.BackupRecord](ctx, c, http.MethodPost, "/backups/"+url.PathEscape(id)+"/verify", nil, nil)
}

// AsAPIError returns err as an *APIError, if it is one.
func AsAPIError(err error) (*APIError, bool) {
	var e *APIError
	ok := errors.As(err, &e)
	return e, ok
}
