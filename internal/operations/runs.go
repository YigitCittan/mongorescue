package operations

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runlog"
	"github.com/yigitcittan/mongorescue/internal/runs"
)

// ErrNotRunning is returned when a backup or restore to cancel is not running
// (adapters answer 409 Conflict).
var ErrNotRunning = errors.New("operations: the run is not running")

// track registers a run with the registry. Registration failures are logged and the
// run proceeds untracked (a nil Run), so a log directory problem never stops a backup.
func (s *Service) track(kind models.RunKind, id, jobID, database string) *runs.Run {
	run, err := s.cfg.Registry.Register(runs.Meta{Kind: kind, ID: id, JobID: jobID, Database: database})
	if err != nil {
		s.logger.Warn("run is not tracked: cancellation and live progress are unavailable for it",
			logsafe.Attr("run_id", id), logsafe.Error(err))
		return nil
	}
	return run
}

// canceller describes the principal in ctx as a cancellation: By (for the record) is
// the username of a session, "API key <name>" or runs.SystemActor, plus " via <via>"
// for an adapter such as MCP; Kind and UserID are what logs record.
func canceller(ctx context.Context, via string, at time.Time) runs.Cancellation {
	p := auth.PrincipalFrom(ctx)
	c := runs.Cancellation{By: runs.SystemActor, Kind: runs.ActorSystem, At: at, UserID: p.UserID()}
	switch {
	case p == nil:
	case p.Method == auth.MethodAPIKey:
		label := p.APIKeyName
		if label == "" {
			label = p.APIKeyID
		}
		c.By, c.Kind = "API key "+label, runs.ActorAPIKey
	case p.User != nil && p.User.Username != "":
		c.By, c.Kind = p.User.Username, runs.ActorUser
	}
	if via != "" {
		c.By += " via " + via
	}
	return c
}

// CancelBackup cancels the running backup id: mongodump is killed, the partial
// artifact removed and the record ends as models.StatusCancelled with who cancelled
// it. It waits up to CancelSettleTimeout for the run to stop and returns its record:
// the final cancelled record, or, if the run is still stopping, the in-progress
// record with its progress (Cancelling); poll GetBackup then. via names the adapter for the record ("MCP", or "" for the REST API).
// Expected failures: ErrNotFound and ErrNotRunning.
func (s *Service) CancelBackup(ctx context.Context, id, via string) (*models.BackupRecord, error) {
	rec, err := s.cfg.Store.GetBackupRecord(ctx, id)
	if err != nil {
		return nil, notFound(err, "backup not found")
	}
	if rec.Status != models.StatusInProgress {
		return nil, public(fmt.Sprintf("backup %s is %s, not running", rec.ID, rec.Status), ErrNotRunning)
	}
	if err := s.cancel(ctx, id, via); err != nil {
		return nil, err
	}
	if final, getErr := s.cfg.Store.GetBackupRecord(context.WithoutCancel(ctx), id); getErr == nil {
		rec = final
	}
	s.withBackupProgress([]*models.BackupRecord{rec})
	return rec, nil
}

// CancelRestore cancels the running restore id. A cancelled safe-clone restore drops
// the partial clone; a cancelled in-place restore may leave the target partially
// restored, so cancelling one needs the admin scope (like starting it). Like
// CancelBackup it waits briefly and returns the final record, or the in-progress
// one while the run is still stopping. Expected failures:
// ErrNotFound, ErrNotRunning and auth.ErrForbidden.
func (s *Service) CancelRestore(ctx context.Context, id, via string) (*models.RestoreRecord, error) {
	rec, err := s.cfg.Store.GetRestoreRecord(ctx, id)
	if err != nil {
		return nil, notFound(err, "restore not found")
	}
	if rec.Status != models.RestoreStatusInProgress {
		return nil, public(fmt.Sprintf("restore %s is %s, not running", rec.ID, rec.Status), ErrNotRunning)
	}
	if rec.InPlace && !rec.DryRun {
		if err := auth.RequireScope(ctx, auth.ScopeAdmin); err != nil {
			return nil, fmt.Errorf("cancelling an in-place restore leaves the target partially restored and needs an admin API key or a session: %w", err)
		}
	}
	if err := s.cancel(ctx, id, via); err != nil {
		return nil, err
	}
	if final, getErr := s.cfg.Store.GetRestoreRecord(context.WithoutCancel(ctx), id); getErr == nil {
		rec = final
	}
	s.withRestoreProgress([]*models.RestoreRecord{rec})
	return rec, nil
}

// CancelledRun is the outcome of CancelRun: the backup or the restore that is
// stopping.
type CancelledRun struct {
	// Kind is models.RunBackup or models.RunRestore.
	Kind models.RunKind `json:"kind"`
	// Backup is the cancelled backup.
	Backup *models.BackupRecord `json:"backup,omitempty"`
	// Restore is the cancelled restore.
	Restore *models.RestoreRecord `json:"restore,omitempty"`
}

// CancelRun cancels the running backup or restore id (see CancelBackup and
// CancelRestore).
func (s *Service) CancelRun(ctx context.Context, id, via string) (*CancelledRun, error) {
	b, err := s.CancelBackup(ctx, id, via)
	if err == nil {
		return &CancelledRun{Kind: models.RunBackup, Backup: b}, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	r, err := s.CancelRestore(ctx, id, via)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, public("no backup or restore with this id", ErrNotFound, err)
		}
		return nil, err
	}
	return &CancelledRun{Kind: models.RunRestore, Restore: r}, nil
}

// CancelSettleTimeout bounds how long a cancel request waits for the run to stop and
// record its outcome, so the response usually carries the final cancelled record.
const CancelSettleTimeout = 5 * time.Second

// cancel asks the registry to stop run id on behalf of the principal in ctx, and
// waits up to CancelSettleTimeout (or until ctx ends) for the run to end.
func (s *Service) cancel(ctx context.Context, id, via string) error {
	c := canceller(ctx, via, s.now().UTC())
	run := s.cfg.Registry.Get(id)
	err := s.cfg.Registry.Cancel(id, c)
	if err == nil && run != nil {
		timer := time.NewTimer(CancelSettleTimeout)
		defer timer.Stop()
		select {
		case <-run.Done():
		case <-timer.C:
		case <-ctx.Done():
		}
	}
	if errors.Is(err, runs.ErrFinishing) {
		return public("the run "+id+" is already finishing (its tool completed) and can no longer be cancelled", ErrNotRunning, err)
	}
	if errors.Is(err, runs.ErrNotRunning) {
		return public("the run "+id+" is not active in this MongoRescue process (it may have just finished)", ErrNotRunning)
	}
	if err != nil {
		return fmt.Errorf("cancel run: %w", err)
	}
	s.logger.Warn("run cancellation requested", append([]any{logsafe.Attr("run_id", id)}, c.LogAttrs()...)...)
	return nil
}

// ActiveRuns returns the live progress of every backup and restore running in this
// process, oldest first.
func (s *Service) ActiveRuns() []models.RunProgress {
	return s.cfg.Registry.Snapshots()
}

// withBackupProgress fills the live progress of the running backups in list.
func (s *Service) withBackupProgress(list []*models.BackupRecord) []*models.BackupRecord {
	if s.cfg.Registry == nil {
		return list
	}
	for _, b := range list {
		if b.Status == models.StatusInProgress {
			b.Progress = s.cfg.Registry.Progress(b.ID)
		}
	}
	return list
}

// withRestoreProgress fills the live progress of the running restores in list.
func (s *Service) withRestoreProgress(list []*models.RestoreRecord) []*models.RestoreRecord {
	if s.cfg.Registry == nil {
		return list
	}
	for _, r := range list {
		if r.Status == models.RestoreStatusInProgress {
			r.Progress = s.cfg.Registry.Progress(r.ID)
		}
	}
	return list
}

// OpenBackupLog opens the log of backup id (live while it runs). The caller closes
// the reader. Expected failures: ErrNotFound (unknown backup, or no log recorded:
// backups from older releases, or logs removed by retention).
func (s *Service) OpenBackupLog(ctx context.Context, id string) (*runlog.Reader, error) {
	if _, err := s.cfg.Store.GetBackupRecord(ctx, id); err != nil {
		return nil, notFound(err, "backup not found")
	}
	return s.openLog(id)
}

// OpenRestoreLog opens the log of restore id (see OpenBackupLog).
func (s *Service) OpenRestoreLog(ctx context.Context, id string) (*runlog.Reader, error) {
	if _, err := s.cfg.Store.GetRestoreRecord(ctx, id); err != nil {
		return nil, notFound(err, "restore not found")
	}
	return s.openLog(id)
}

// openLog opens the log of run id.
func (s *Service) openLog(id string) (*runlog.Reader, error) {
	r, err := s.cfg.Registry.OpenLog(id)
	if errors.Is(err, runlog.ErrNotFound) || errors.Is(err, runlog.ErrInvalidID) {
		return nil, public("no log was recorded for this run (older releases did not keep run logs; retention removes them after general.log_retention_days)", ErrNotFound, err)
	}
	if err != nil {
		return nil, fmt.Errorf("open run log: %w", err)
	}
	return r, nil
}

// RemoveRunLog deletes the log of a deleted backup or restore record. Failures are
// logged; log retention removes leftovers.
func (s *Service) RemoveRunLog(id string) {
	if err := s.cfg.Registry.RemoveLog(id); err != nil {
		s.logger.Warn("failed to delete the log of a deleted run", logsafe.Attr("run_id", id), logsafe.Error(err))
	}
}

// PruneRunLogs deletes the logs of runs that finished more than
// general.log_retention_days ago (0 keeps them forever) and returns how many it removed.
func (s *Service) PruneRunLogs() (int, error) {
	days := s.settings().General.LogRetentionDays
	if days <= 0 {
		return 0, nil
	}
	n, err := s.cfg.Registry.PruneLogs(time.Duration(days) * 24 * time.Hour)
	if err != nil {
		return n, fmt.Errorf("prune run logs: %w", err)
	}
	return n, nil
}
