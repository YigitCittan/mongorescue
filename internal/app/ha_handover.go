package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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

// ErrNoSnapshotToTakeOver is returned by a takeover that finds no metadata snapshot
// although the lease had a holder before: the previous active instance's metadata
// would be lost, so the instance stays standby and the takeover failure is alerted.
var ErrNoSnapshotToTakeOver = errors.New("no metadata snapshot to take over from")

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
		Cluster: cfg.HA.Cluster, InstanceID: rt.state.InstanceID, Store: metaStore, Storage: rt.storage(), Fence: rt,
		Passphrase: passphrase, TempDir: cfg.DataDir, Interval: cfg.HA.SnapshotInterval, Logger: logger,
		Checker: rt.snapshotChecker(),
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
		// The imported backups belong in the overview's history at once.
		if a.ops != nil {
			a.ops.InvalidateHistory()
		}
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
// instance: it restores the newest snapshot of the cluster whose authenticated
// header matches its name (the highest epoch, then the newest), unless this
// instance's own database is at least as new. Snapshots without a header (written
// by an older version) are restored only when the cluster has no snapshot with
// one. It fails when snapshots exist but none can be used.
func (s *haSupervisor) prepareTakeover(ctx context.Context) error {
	st := s.rt.storage()
	sel, err := s.selectSnapshots(ctx, haSnapshotsTried)
	if err != nil {
		return err
	}
	if sel.Listed == 0 {
		// Without a snapshot this instance's database is used only when there was
		// no lease either: the cluster's first activation, or a new HA target or
		// cluster name, whose instance keeps its own database (never an empty one
		// swapped in). A lease that existed (expired, released or unreadable) had a
		// holder whose metadata would be lost.
		epoch := s.rt.mgr().Epoch()
		if !s.rt.mgr().AcquiredFresh() {
			return fmt.Errorf("%w: a lease existed before (this is epoch %d), but the HA target holds no snapshot of its holder's metadata", ErrNoSnapshotToTakeOver, epoch)
		}
		s.setSince(time.Time{})
		if s.rt.state.LastActiveEpoch > 0 {
			s.logger.Warn("takeover: no lease and no metadata snapshot on the HA target (a new target or cluster name?); keeping this instance's database",
				slog.Uint64("epoch", epoch), slog.Uint64("last_active_epoch", s.rt.state.LastActiveEpoch))
		} else {
			s.logger.Info("takeover: the cluster's first activation; keeping this instance's database", slog.Uint64("epoch", epoch))
		}
		return nil
	}
	candidates, legacy := sel.Candidates()
	if len(candidates) == 0 {
		var errs []error
		for _, c := range slices.Concat(sel.Refused, sel.Failed, sel.Legacy) {
			errs = append(errs, c.Err)
		}
		return fmt.Errorf("no usable metadata snapshot among the %d on the HA target: %w", sel.Listed, errors.Join(errs...))
	}
	newest := candidates[0]
	if s.rt.state.Cluster == s.cfg.HA.Cluster && newest.Epoch != 0 && newest.Epoch == s.rt.state.LastActiveEpoch &&
		(legacy || newest.Header.InstanceID == s.rt.state.InstanceID) {
		s.setSince(newest.CreatedAt)
		s.logger.Info("takeover: the newest snapshot was written by this instance; its own database is at least as new and is kept",
			slog.Uint64("epoch", newest.Epoch))
		return nil
	}
	if legacy {
		s.logger.Warn("takeover: no metadata snapshot of the cluster has an authenticated header; restoring one written by an older version, whose epoch and cluster are not authenticated",
			logsafe.Attr("key", newest.Key))
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
	for _, snap := range candidates[:min(len(candidates), haSnapshotsTried)] {
		if err = s.restoreSnapshot(ctx, st, snap.SnapshotInfo, passphrase, box, legacy); err == nil {
			s.setSince(snap.CreatedAt)
			s.logger.Info("takeover: restored the metadata snapshot", logsafe.Attr("key", snap.Key),
				slog.Uint64("snapshot_epoch", snap.Epoch), slog.Time("snapshot_time", snap.CreatedAt), slog.Bool("authenticated", !legacy))
			return nil
		}
		if errors.Is(err, ha.ErrSnapshotRejected) || errors.Is(err, ha.ErrSnapshotUnreadable) {
			// Replaced since it was checked.
			s.reportRefused(ha.SnapshotCheck{SnapshotInfo: snap.SnapshotInfo, Err: err})
		} else {
			s.logger.Warn("takeover: cannot use a metadata snapshot", logsafe.Attr("key", snap.Key), logsafe.Error(err))
		}
		errs = append(errs, err)
	}
	return fmt.Errorf("no usable metadata snapshot: %w", errors.Join(errs...))
}

// selectSnapshots verifies the snapshots of the cluster until want are verified
// (ha.SnapshotChecker.Select); refused ones are reported once per stored object.
func (s *haSupervisor) selectSnapshots(ctx context.Context, want int) (ha.SnapshotSelection, error) {
	checker := s.rt.snapshotChecker()
	if checker == nil {
		return ha.SnapshotSelection{}, errors.New("the HA target is not open yet")
	}
	return checker.Select(ctx, want)
}

// reportRefused logs and counts a snapshot that failed authentication: it is
// never restored, never deleted, and the next one is used.
func (s *haSupervisor) reportRefused(c ha.SnapshotCheck) {
	s.rt.snapshotsRejected.Add(1)
	s.logger.Error("HA metadata snapshot refused: its authenticated header does not match its name or cluster, or it does not decrypt; someone with write access to the HA target may have copied or altered it. It is never restored",
		logsafe.Attr("key", c.Key), logsafe.Error(c.Err))
}

// setSince records the snapshot time of the takeover.
func (s *haSupervisor) setSince(t time.Time) {
	s.rt.mu.Lock()
	s.rt.since = t
	s.rt.mu.Unlock()
}

// restoreSnapshot downloads and decrypts snap into the data directory, checks it
// (its authenticated header, unless allowLegacy accepts one without) and swaps it in as the metadata database (the previous one is kept as .bak).
func (s *haSupervisor) restoreSnapshot(ctx context.Context, st storage.Storage, snap ha.SnapshotInfo, passphrase string, box *secretbox.Box, allowLegacy bool) error {
	dir, err := os.MkdirTemp(s.cfg.DataDir, ".ha-restore-*")
	if err != nil {
		return fmt.Errorf("restore directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	tmp := filepath.Join(dir, config.DefaultDatabaseFileName)
	if _, err = ha.FetchSnapshot(ctx, st, s.cfg.HA.Cluster, snap.Key, passphrase, tmp, allowLegacy); err != nil {
		return err
	}
	_, err = store.ReplaceDatabase(ctx, store.Replacement{Path: s.cfg.MetadataDBPath(), NewPath: tmp, Epoch: s.rt.mgr().Epoch(),
		Keep: store.DefaultBackupsKept, Logger: s.logger}, store.WithSecretBox(box))
	return err
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

// checkSnapshotKey checks at startup that this instance can decrypt the cluster's
// snapshots, so a wrong secret key fails now instead of at a takeover: it fails
// when none of the snapshots it checks decrypts. It reads headers in the order of
// a takeover until one is verified, reporting those refused.
func (s *haSupervisor) checkSnapshotKey(ctx context.Context) error {
	sel, err := s.selectSnapshots(ctx, 1)
	if err != nil {
		s.logger.Warn("cannot list the HA metadata snapshots at startup; the secret key is checked at the takeover", logsafe.Error(err))
		return nil
	}
	for _, c := range sel.Failed {
		s.logger.Warn("cannot read an HA metadata snapshot at startup", logsafe.Attr("key", c.Key), logsafe.Error(c.Err))
	}
	if len(sel.Verified) > 0 || len(sel.Legacy) > 0 {
		return nil
	}
	var unreadable error
	for _, c := range sel.Refused {
		if !errors.Is(c.Err, ha.ErrSnapshotUnreadable) {
			return nil // it decrypted: the key is right
		}
		if unreadable == nil {
			unreadable = c.Err
		}
	}
	return unreadable
}
