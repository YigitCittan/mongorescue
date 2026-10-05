package operations

import (
	"context"
	"fmt"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// VisibleTargets returns the IDs of the storage targets a caller limited to some
// connections may see: the targets of its jobs and of the backups taken from its
// connections, and the default target its manual backups go to. It returns nil for
// a caller that may touch every connection (every target is visible).
func (s *Service) VisibleTargets(ctx context.Context) (map[string]bool, error) {
	set := auth.ConnectionFilter(ctx)
	if !set.Limited() {
		return nil, nil
	}
	out := map[string]bool{}
	jobs, err := s.store.ListJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	for _, j := range jobs {
		if j.StorageTargetID != "" {
			out[j.StorageTargetID] = true
		}
	}
	ids, err := s.cfg.Store.ListBackupTargetsIn(ctx, set)
	if err != nil {
		return nil, fmt.Errorf("list backup targets: %w", err)
	}
	for _, id := range ids {
		out[id] = true
	}
	// The PITR streams of the caller's connections write to their targets too.
	if s.cfg.PITR != nil {
		streams, listErr := s.cfg.PITR.ListStreams(ctx)
		if listErr != nil {
			return nil, fmt.Errorf("list PITR streams: %w", listErr)
		}
		for _, st := range streams {
			if set.Allows(st.ConnectionID) && st.TargetID != "" {
				out[st.TargetID] = true
			}
		}
	}
	if s.cfg.Targets != nil {
		if def, defErr := s.cfg.Targets.Resolve(ctx, ""); defErr == nil {
			out[def.ID] = true
		}
	}
	return out, nil
}

// FilterTargets keeps the storage targets of list the caller may see (see
// VisibleTargets).
func (s *Service) FilterTargets(ctx context.Context, list []*models.StorageTarget) ([]*models.StorageTarget, error) {
	visible, err := s.VisibleTargets(ctx)
	if err != nil || visible == nil {
		return list, err
	}
	out := make([]*models.StorageTarget, 0, len(list))
	for _, t := range list {
		if visible[t.ID] {
			out = append(out, t)
		}
	}
	return out, nil
}

// TargetVisible reports whether the caller may see storage target id.
func (s *Service) TargetVisible(ctx context.Context, id string) (bool, error) {
	visible, err := s.VisibleTargets(ctx)
	if err != nil {
		return false, err
	}
	return visible == nil || visible[id], nil
}

// ActiveRunsFor returns ActiveRuns as the caller in ctx may see them: the backups and
// restores of the connections it may touch.
func (s *Service) ActiveRunsFor(ctx context.Context) []models.RunProgress {
	list := s.ActiveRuns()
	if !auth.ConnectionFilter(ctx).Limited() {
		return list
	}
	out := make([]models.RunProgress, 0, len(list))
	for _, r := range list {
		var err error
		switch r.Kind {
		case models.RunBackup:
			_, err = s.store.GetBackupRecord(ctx, r.ID)
		case models.RunRestore:
			_, err = s.store.GetRestoreRecord(ctx, r.ID)
		default:
			continue
		}
		if err == nil {
			out = append(out, r)
		}
	}
	return out
}
