package metabackup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// retiredStore lists and settles the install IDs that secret key rotations retired
// (implemented by *store.SQLiteStore).
type retiredStore interface {
	RetiredInstalls(ctx context.Context) ([]store.RetiredInstall, error)
	MarkInstallPruned(ctx context.Context, id string, at time.Time) error
}

// targetLister lists every storage target (implemented by *targets.Service).
type targetLister interface {
	List(ctx context.Context) ([]*models.StorageTarget, error)
}

// PruneRetired deletes the snapshots of install IDs that a secret key rotation
// retired once the delete grace period has passed since the rotation: they hold the
// credentials of their time sealed with the old, possibly leaked key. Like a soft
// delete, the grace period leaves time to restore one with the previous key. Every
// storage target is searched, only snapshot names directly below the retired prefix
// are deleted, and the install ID is then marked pruned. When every retired install
// ID is pruned, Config.OnRetiredPruned runs with the time of the last rotation. The
// background loop calls it.
func (s *Service) PruneRetired(ctx context.Context) error {
	rs, ok := s.cfg.Store.(retiredStore)
	if !ok {
		return nil
	}
	list, err := rs.RetiredInstalls(ctx)
	if err != nil || len(list) == 0 {
		return err
	}
	now, grace := s.now(), s.deleteGrace()
	var last time.Time
	pending := false
	var errs []error
	for _, ri := range list {
		if ri.RetiredAt.After(last) {
			last = ri.RetiredAt
		}
		if ri.PrunedAt != nil || ri.InstallID == s.InstallID() || !installIDPattern.MatchString(ri.InstallID) {
			continue
		}
		if now.Before(ri.RetiredAt.Add(grace)) {
			pending = true
			continue
		}
		if err := s.pruneInstall(ctx, ri.InstallID); err != nil {
			pending = true
			errs = append(errs, err)
			continue
		}
		if err := rs.MarkInstallPruned(ctx, ri.InstallID, now); err != nil {
			pending = true
			errs = append(errs, err)
			continue
		}
		s.logger.Info("deleted the metadata snapshots sealed with a rotated secret key", logsafe.Attr("install_id", ri.InstallID))
	}
	if !pending && len(errs) == 0 && s.cfg.OnRetiredPruned != nil {
		s.cfg.OnRetiredPruned(ctx, last)
	}
	return errors.Join(errs...)
}

// pruneInstall deletes the snapshots below Prefix + id + "/" on every target.
func (s *Service) pruneInstall(ctx context.Context, id string) error {
	lister, ok := s.cfg.Targets.(targetLister)
	if !ok {
		return errors.New("metabackup: storage targets cannot be listed")
	}
	targets, err := lister.List(ctx)
	if err != nil {
		return fmt.Errorf("metabackup: list storage targets: %w", err)
	}
	prefix := Prefix + id + "/"
	var errs []error
	for _, t := range targets {
		driver, err := s.cfg.Targets.Storage(ctx, t.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("metabackup: storage target %s: %w", t.ID, err))
			continue
		}
		objects, err := driver.List(ctx, prefix)
		if err != nil {
			errs = append(errs, fmt.Errorf("metabackup: list retired snapshots on %s: %w", t.ID, err))
			continue
		}
		for _, o := range objects {
			if o == nil || !strings.HasPrefix(o.Key, prefix) || !snapshotName.MatchString(strings.TrimPrefix(o.Key, prefix)) {
				continue
			}
			if err := driver.Delete(ctx, o.Key); err != nil && !errors.Is(err, storage.ErrNotFound) {
				errs = append(errs, fmt.Errorf("metabackup: delete retired snapshot: %w", err))
			}
		}
	}
	return errors.Join(errs...)
}
