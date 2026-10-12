package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/ha"
	"github.com/yigitcittan/mongorescue/internal/integrity"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// Metadata handover of active-passive HA (docs/design/ha.md#metadata-snapshots).
const (
	// haSnapshotsTried is how many of the newest snapshots a takeover tries.
	haSnapshotsTried = 3
	// haImportMargin widens the import of a takeover to archives that finished
	// shortly before the snapshot: their runs were still recorded as in progress.
	haImportMargin = 10 * time.Minute
	// haFinalSnapshotTimeout bounds the last snapshot of a graceful shutdown.
	haFinalSnapshotTimeout = time.Minute
	// haScanSource is the source of the storage scans of a takeover.
	haScanSource = "ha_takeover"
)

// haTasks owns the goroutines of the active instance's HA work.
type haTasks struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// newHASnapshotter builds the snapshotter of an active instance.
func newHASnapshotter(cfg *config.Config, rt *haRuntime, metaStore ha.VacuumStore, key []byte, logger *slog.Logger) (*ha.Snapshotter, error) {
	passphrase, err := ha.SnapshotPassphrase(key)
	if err != nil {
		return nil, err
	}
	return ha.NewSnapshotter(ha.SnapshotterConfig{
		Cluster: cfg.HA.Cluster, Store: metaStore, Storage: rt.storage(), Fence: rt, Passphrase: passphrase,
		TempDir: cfg.DataDir, Interval: cfg.HA.SnapshotInterval, Logger: logger,
	})
}

// kickOnFinishedRun asks for a snapshot after every finished backup or restore, so
// the standby's view of the runs is current.
func kickOnFinishedRun(snap *ha.Snapshotter) func(context.Context, events.Event) {
	return func(_ context.Context, e events.Event) {
		switch e.Type {
		case events.BackupSucceeded, events.BackupFailed, events.BackupCancelled,
			events.RestoreSucceeded, events.RestoreFailed, events.RestoreCancelled:
			snap.Kick()
		}
	}
}

// startHAActive starts the snapshot loop and the takeover tasks of the active
// instance; stopHAActive stops them.
func (a *App) startHAActive(ctx context.Context) {
	taskCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	a.haTasks.cancel = cancel
	if a.haSnap != nil {
		a.haTasks.wg.Go(func() { a.haSnap.Run(taskCtx) })
	}
	a.haTasks.wg.Go(func() { a.takeoverTasks(taskCtx) })
}

// stopHAActive stops what startHAActive started and waits for it.
func (a *App) stopHAActive() {
	if a.haTasks.cancel != nil {
		a.haTasks.cancel()
	}
	a.haTasks.wg.Wait()
}

// takeoverTasks runs once the active instance has started: it imports the archives
// written after the snapshot it took over from, then runs the jobs whose schedule
// was missed. Nothing is deleted.
func (a *App) takeoverTasks(ctx context.Context) {
	imported := map[string]error{}
	since := a.ha.rt.takeoverSince()
	if !since.IsZero() {
		imported = a.importNewerArchives(ctx, since.Add(-haImportMargin))
	}
	if ctx.Err() != nil {
		return
	}
	// A missed fire is caught up only when the archives of its target were checked.
	check := func(ctx context.Context, targetID string) error {
		if since.IsZero() {
			return errors.New("there was no metadata snapshot to compare the storage with")
		}
		t, err := a.targets.Resolve(ctx, targetID)
		if err != nil {
			return fmt.Errorf("the storage target cannot be resolved: %w", err)
		}
		status, ok := imported[t.ID]
		if !ok {
			return errors.New("the storage target was not scanned")
		}
		return status
	}
	if started := a.scheduler.CatchUp(ctx, time.Now(), check); len(started) > 0 {
		a.logger.Info("ran the scheduled backups missed during the takeover", slog.Int("jobs", len(started)))
	}
}

// importNewerArchives scans every storage target and imports the orphan archives
// (never oplog chunks) that changed at or after since: backups the previous active
// instance wrote after its last snapshot. It returns, per scanned target ID, nil
// when every such archive is known (imported, or recorded already), or why not.
func (a *App) importNewerArchives(ctx context.Context, since time.Time) map[string]error {
	status := map[string]error{}
	if a.integrity == nil {
		return status
	}
	targets, err := a.targets.List(ctx)
	if err != nil {
		a.logger.Warn("takeover: cannot list the storage targets to import newer archives", logsafe.Error(err))
		return status
	}
	imported := 0
	for _, t := range targets {
		report, scanErr := a.integrity.ScanTarget(ctx, t.ID, haScanSource)
		switch {
		case scanErr != nil:
			status[t.ID] = fmt.Errorf("the storage scan failed: %w", scanErr)
		case report == nil:
			status[t.ID] = errors.New("the storage scan returned nothing")
		case report.Error != "":
			status[t.ID] = errors.New("the storage scan failed: " + report.Error)
		case report.OrphanCount > len(report.Orphans):
			status[t.ID] = fmt.Errorf("the storage holds more orphan archives (%d) than one scan reports", report.OrphanCount)
		}
		if status[t.ID] != nil {
			a.logger.Warn("takeover: storage scan incomplete; its newer archives may be unknown", logsafe.Attr("storage_target_id", t.ID),
				logsafe.Error(status[t.ID]))
			continue
		}
		status[t.ID] = nil
		for _, o := range report.Orphans {
			if integrity.IsChunkKey(o.Key) || o.ModTime.Before(since) {
				continue
			}
			if _, impErr := a.integrity.StartImport(ctx, t.ID, o.Key); impErr != nil {
				a.logger.Warn("takeover: cannot import an archive written after the last snapshot",
					logsafe.Attr("storage_key", o.Key), logsafe.Error(impErr))
				status[t.ID] = fmt.Errorf("an archive written after the last snapshot could not be imported: %w", impErr)
				continue
			}
			imported++
		}
	}
	if imported > 0 {
		a.logger.Info("takeover: importing archives written after the last metadata snapshot", slog.Int("archives", imported))
	}
	return status
}

// prepareTakeover readies the data directory before the App opens as the active
// instance: it restores the newest usable snapshot of the cluster (the highest
// epoch, then the newest), unless this instance's own database is at least as new.
// It fails when snapshots exist but none can be used.
func (s *haSupervisor) prepareTakeover(ctx context.Context) error {
	st := s.rt.storage()
	snaps, err := ha.ListSnapshots(ctx, st, s.cfg.HA.Cluster)
	if err != nil {
		return err
	}
	if len(snaps) == 0 {
		s.setSince(time.Time{})
		s.logger.Info("takeover: the cluster has no metadata snapshot yet; keeping this instance's database")
		return nil
	}
	newest := snaps[0]
	if s.rt.state.Cluster == s.cfg.HA.Cluster && newest.Epoch != 0 && newest.Epoch == s.rt.state.LastActiveEpoch {
		s.setSince(newest.CreatedAt)
		s.logger.Info("takeover: the newest snapshot was written by this instance; its own database is at least as new and is kept",
			slog.Uint64("epoch", newest.Epoch))
		return nil
	}
	passphrase, err := ha.SnapshotPassphrase(s.rt.key)
	if err != nil {
		return err
	}
	box, err := secretbox.New(s.rt.key)
	if err != nil {
		return err
	}
	var errs []error
	for _, snap := range snaps[:min(len(snaps), haSnapshotsTried)] {
		if err = s.restoreSnapshot(ctx, st, snap, passphrase, box); err == nil {
			s.setSince(snap.CreatedAt)
			s.logger.Info("takeover: restored the metadata snapshot", logsafe.Attr("key", snap.Key),
				slog.Uint64("snapshot_epoch", snap.Epoch), slog.Time("snapshot_time", snap.CreatedAt))
			return nil
		}
		s.logger.Warn("takeover: cannot use a metadata snapshot", logsafe.Attr("key", snap.Key), logsafe.Error(err))
		errs = append(errs, err)
	}
	return fmt.Errorf("no usable metadata snapshot: %w", errors.Join(errs...))
}

// setSince records the snapshot time of the takeover.
func (s *haSupervisor) setSince(t time.Time) {
	s.rt.mu.Lock()
	s.rt.since = t
	s.rt.mu.Unlock()
}

// restoreSnapshot downloads and decrypts snap into the data directory, checks it
// and swaps it in as the metadata database (the previous one is kept as .bak).
func (s *haSupervisor) restoreSnapshot(ctx context.Context, st storage.Storage, snap ha.SnapshotInfo, passphrase string, box *secretbox.Box) error {
	dir, err := os.MkdirTemp(s.cfg.DataDir, ".ha-restore-*")
	if err != nil {
		return fmt.Errorf("restore directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	tmp := filepath.Join(dir, config.DefaultDatabaseFileName)
	if err = ha.FetchSnapshot(ctx, st, snap.Key, passphrase, tmp); err != nil {
		return err
	}
	return store.ReplaceDatabase(ctx, s.cfg.MetadataDBPath(), tmp, s.logger, store.WithSecretBox(box))
}

// finalSnapshot writes the last snapshot of a gracefully stopping active instance,
// after its runs stopped and before it releases the lease.
func (s *haSupervisor) finalSnapshot(app *App) {
	if app.haSnap == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), haFinalSnapshotTimeout)
	defer cancel()
	if _, err := app.haSnap.Snapshot(ctx); err != nil {
		s.logger.Warn("the last metadata snapshot before the shutdown failed; the standby takes over from the previous one", logsafe.Error(err))
	}
}

// checkSnapshotKey checks at startup that this instance can decrypt the newest
// snapshot of the cluster, so a wrong secret key fails now instead of at a takeover.
func (s *haSupervisor) checkSnapshotKey(ctx context.Context, st storage.Storage, key []byte) error {
	snaps, err := ha.ListSnapshots(ctx, st, s.cfg.HA.Cluster)
	if err != nil {
		s.logger.Warn("cannot list the HA metadata snapshots at startup; the secret key is checked at the takeover", logsafe.Error(err))
		return nil
	}
	if len(snaps) == 0 {
		return nil
	}
	passphrase, err := ha.SnapshotPassphrase(key)
	if err != nil {
		return err
	}
	if err = ha.CheckSnapshotReadable(ctx, st, snaps[0].Key, passphrase); errors.Is(err, ha.ErrSnapshotUnreadable) {
		return err
	} else if err != nil {
		s.logger.Warn("cannot read the newest HA metadata snapshot at startup", logsafe.Error(err))
	}
	return nil
}
