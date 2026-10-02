package operations

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// MaxJobRunList is the most job runs ListJobRuns returns.
const MaxJobRunList = store.MaxJobRunList

// DatabasePreviewRequest changes what PreviewJobDatabases resolves: the selection
// and connection of a job being edited (or created, without a job). Nil fields keep
// the job's values.
type DatabasePreviewRequest struct {
	// ConnectionID replaces the job's connection when set.
	ConnectionID string
	// Selection replaces the job's database selection when set.
	Selection *models.DatabaseSelection
}

// DatabasePreview is what a job's database selection backs up on its connection now
// (GET /api/v1/jobs/{id}/databases/preview).
type DatabasePreview struct {
	// JobID is the job, empty for a preview of an unsaved job.
	JobID string `json:"job_id,omitempty"`
	// Selection is the selection resolved.
	Selection models.DatabaseSelection `json:"selection"`
	// The resolution: included, excluded (with reasons), missing,
	// new_since_last_run and warnings.
	models.DatabaseResolution
}

// PreviewJobDatabases resolves job jobID's database selection against the live
// server exactly as its next run would (the resolution is the scheduler's), without
// changing anything. req (optional) previews another selection or connection, as
// while the job is edited; a changed selection counts every database it matches as
// known, as saving it would. An empty jobID previews an unsaved job, which needs
// req.ConnectionID and req.Selection. Expected failures: ErrNotFound, ErrInvalid,
// ErrConnectionRequired, ErrUnknownConnection, ErrDatabaseListing and
// ErrSchedulerUnavailable.
func (s *Service) PreviewJobDatabases(ctx context.Context, jobID string, req DatabasePreviewRequest) (*DatabasePreview, error) {
	if s.cfg.Jobs == nil {
		return nil, ErrSchedulerUnavailable
	}
	job := &models.Job{}
	var existing *models.Job
	if jobID != "" {
		stored, err := s.GetJob(ctx, jobID)
		if err != nil {
			return nil, err
		}
		existing, job = stored, stored.Clone()
	}
	if req.ConnectionID != "" {
		job.ConnectionID = req.ConnectionID
	}
	if req.Selection != nil {
		job.DatabaseSelection = req.Selection.Clone()
		job.Database = ""
	} else if existing == nil {
		return nil, public("database_selection is required to preview an unsaved job", ErrInvalid)
	}
	if _, err := s.ResolveConnection(ctx, job.ConnectionID); err != nil {
		return nil, err
	}
	if err := normalizeSelection(job); err != nil {
		if !errors.Is(err, ErrCollectionsNeedSingle) {
			return nil, err
		}
		// The collections of the job being edited do not change what is backed up.
		job.Collections, job.ExcludeCollections = nil, nil
		if err = normalizeSelection(job); err != nil {
			return nil, err
		}
	}
	CarryKnownDatabases(job, existing)
	res, err := s.cfg.Jobs.ResolveJobDatabases(ctx, job)
	if err != nil {
		return nil, jobRunError(err)
	}
	return &DatabasePreview{JobID: jobID, Selection: job.DatabaseSelection, DatabaseResolution: *res}, nil
}

// ListJobRuns returns up to limit (at most MaxJobRunList) runs of job jobID, newest
// first, each with the outcome of every database. Expected failures: ErrNotFound.
func (s *Service) ListJobRuns(ctx context.Context, jobID string, limit int) ([]*models.JobRun, error) {
	if _, err := s.GetJob(ctx, jobID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > MaxJobRunList {
		limit = MaxJobRunList
	}
	list, err := s.cfg.Store.ListJobRuns(ctx, jobID, limit)
	if err != nil {
		return nil, fmt.Errorf("list job runs: %w", err)
	}
	if list == nil {
		list = []*models.JobRun{}
	}
	return list, nil
}

// JobRunCancellation is the outcome of CancelJobRun.
type JobRunCancellation struct {
	// JobID is the job.
	JobID string `json:"job_id"`
	// RunIDs lists the job runs that are stopping.
	RunIDs []string `json:"run_ids"`
	// Cancelled lists the backups that were cancelled: the running database and the
	// ones still waiting.
	Cancelled []string `json:"cancelled"`
	// Backups are the cancelled backups' records as stored now (final once they
	// stopped, in progress with Cancelling while they still stop).
	Backups []*models.BackupRecord `json:"backups"`
}

// CancelJobRun stops the current run of job jobID: its running database and every
// database still waiting (via names the adapter for the record, as for
// CancelBackup). It waits up to CancelSettleTimeout for the backups to stop.
// Expected failures: ErrNotFound and ErrNotRunning (no run of the job is active in
// this process).
func (s *Service) CancelJobRun(ctx context.Context, jobID, via string) (*JobRunCancellation, error) {
	if _, err := s.GetJob(ctx, jobID); err != nil {
		return nil, err
	}
	active := s.cfg.Registry.JobRuns(jobID)
	if len(active) == 0 {
		return nil, public("no run of job "+jobID+" is active in this MongoRescue process", ErrNotRunning)
	}
	c := canceller(ctx, via, s.now().UTC())
	ids := s.cfg.Registry.CancelJob(jobID, c)
	out := &JobRunCancellation{JobID: jobID, RunIDs: []string{}, Cancelled: ids, Backups: []*models.BackupRecord{}}
	if out.Cancelled == nil {
		out.Cancelled = []string{}
	}
	timer := time.NewTimer(CancelSettleTimeout)
	defer timer.Stop()
	for _, run := range active {
		select {
		case <-run.Done():
		case <-timer.C:
		case <-ctx.Done():
		}
	}
	for _, run := range active {
		rec, err := s.cfg.Store.GetBackupRecord(context.WithoutCancel(ctx), run.ID())
		if err != nil {
			continue
		}
		if rec.RunID != "" && !slices.Contains(out.RunIDs, rec.RunID) {
			out.RunIDs = append(out.RunIDs, rec.RunID)
		}
		out.Backups = append(out.Backups, rec)
	}
	s.withBackupProgress(out.Backups)
	slices.Sort(out.RunIDs)
	s.logger.Warn("job run cancellation requested",
		append([]any{logsafe.Attr("job_id", jobID), slog.Int("cancelled", len(ids))}, c.LogAttrs()...)...)
	return out, nil
}
