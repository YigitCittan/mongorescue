package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auditlog"
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
	// haTakeoverAttempts is how often a takeover tries to read or restore the
	// newest snapshot before it gives up; it never falls back to an older one.
	haTakeoverAttempts = 4
	// haTakeoverBackoff is the first pause between two attempts; it doubles.
	haTakeoverBackoff = time.Second
	// haImportMargin widens the import of a takeover to archives that finished
	// shortly before the snapshot: their runs were still recorded as in progress.
	haImportMargin = 10 * time.Minute
	// haFinalSnapshotTimeout bounds the last snapshot of a graceful shutdown.
	haFinalSnapshotTimeout = time.Minute
	// haScanSource is the source of the storage scans of a takeover.
	haScanSource = "ha_takeover"
)

// Reasons of a refused takeover that suspects a rollback (the reason label of
// mongorescue_ha_rollback_suspected_total).
const (
	// rollbackLeaseUntrusted: the replaced lease was unreadable or its MAC missing
	// or wrong.
	rollbackLeaseUntrusted = "lease_untrusted"
	// rollbackLeaseMissing: no lease although snapshots exist, or although this
	// instance has seen the cluster or its database is populated.
	rollbackLeaseMissing = "lease_missing"
	// rollbackBehindLease: the newest verified snapshot is older than the one the
	// lease names.
	rollbackBehindLease = "snapshot_behind_lease"
	// rollbackBehindLocal: it is older than the newest this instance wrote or
	// restored.
	rollbackBehindLocal = "snapshot_behind_local"
	// rollbackLegacy: only snapshots without a header are left although the
	// cluster wrote snapshots with one.
	rollbackLegacy = "legacy_after_upgrade"
)

// rollbackReasons lists every reason, for the metric.
var rollbackReasons = []string{rollbackLeaseUntrusted, rollbackLeaseMissing, rollbackBehindLease, rollbackBehindLocal, rollbackLegacy}

// ErrNoSnapshotToTakeOver is returned by a takeover that finds no metadata snapshot
// although the lease had a holder before: the previous active instance's metadata
// would be lost, so the instance stays standby and the takeover failure is alerted.
var ErrNoSnapshotToTakeOver = errors.New("no metadata snapshot to take over from")

// ErrRollbackSuspected is returned by a takeover that could restore an older
// metadata state than the cluster's: the lease is not authentic or missing, or the
// newest verified snapshot is older than the one the lease or this instance knows.
// The instance stays standby; an operator checks the HA target and forces the
// takeover with `mongorescue ha force-takeover`.
var ErrRollbackSuspected = errors.New("rollback suspected: the takeover is refused until an operator forces it (mongorescue ha force-takeover)")

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
		// The lease names every snapshot as the cluster's last one, and the
		// instance's high-water mark rises: a takeover must find one as new.
		OnWritten: func(ctx context.Context, info ha.SnapshotInfo) {
			if m := rt.mgr(); m != nil {
				m.RecordSnapshot(ctx, info.Mark())
			}
			if err := rt.updateState(func(st *ha.State) { st.Observe(cfg.HA.Cluster, info.Mark(), false) }); err != nil {
				logger.Warn("could not record the HA high-water mark in the data directory", logsafe.Error(err))
			}
		},
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
// instance, and confirms the takeover to the lease manager (which signs the lease
// from then on). It restores the newest snapshot of the cluster whose
// authenticated header matches its name, unless this instance's own database is
// at least as new, and refuses (ErrRollbackSuspected) when that could roll the
// metadata back: the replaced lease is not authentic or missing, or the newest
// verified snapshot is older than the last snapshot the lease names or than the
// newest this instance wrote or restored. An older snapshot never stands in for
// a newer one that cannot be read. Snapshots without a header (written by an
// older version) are restored only while neither the lease nor this instance
// knows a snapshot with one. An operator override (ha.Override) skips the
// checks. It fails when snapshots exist but none can be used.
func (s *haSupervisor) prepareTakeover(ctx context.Context) error {
	cluster := s.cfg.HA.Cluster
	mgr := s.rt.mgr()
	override, err := ha.TakeOverride(s.cfg.DataDir)
	switch {
	case err != nil:
		s.logger.Error("ignoring the forced takeover request: it cannot be used", logsafe.Error(err))
	case override != nil && override.Cluster != cluster:
		s.logger.Error("ignoring the forced takeover request: it is for another cluster", slog.String("request_cluster", override.Cluster))
	case override != nil:
		return s.forceTakeover(ctx, override)
	}
	pred := mgr.Predecessor()
	sel, err := s.selectForTakeover(ctx)
	if err != nil {
		return err
	}
	local := s.rt.stateCopy()
	epoch := mgr.Epoch()
	if sel.Listed == 0 {
		// Without a snapshot this instance's database is used only on a genuine
		// first activation: no lease, an instance that never saw the cluster, and
		// a database without jobs or storage targets of its own. A lease that
		// existed had a holder whose metadata would be lost.
		switch {
		case pred.Trust == ha.LeaseAbsent && !local.Seen(cluster) && !s.rt.localPopulated:
			s.setSince(time.Time{})
			s.logger.Info("takeover: the cluster's first activation; keeping this instance's database", slog.Uint64("epoch", epoch))
			return s.confirmTakeover(ctx, ha.SnapshotMark{}, false)
		case pred.Trust == ha.LeaseAbsent:
			return s.suspectRollback(rollbackLeaseMissing, fmt.Errorf("%w: no lease and no metadata snapshot on the HA target, but this instance has seen the cluster before or its database holds jobs or storage targets: the cluster's objects may have been deleted", ErrNoSnapshotToTakeOver))
		default:
			return fmt.Errorf("%w: a lease existed before (this is epoch %d), but the HA target holds no snapshot of its holder's metadata", ErrNoSnapshotToTakeOver, epoch)
		}
	}
	switch pred.Trust {
	case ha.LeaseAbsent:
		return s.suspectRollback(rollbackLeaseMissing, errors.New("no lease exists although the HA target holds metadata snapshots: the lease was deleted, so the cluster's last snapshot is unknown"))
	case ha.LeaseUntrusted:
		return s.suspectRollback(rollbackLeaseUntrusted, fmt.Errorf("the replaced lease is not trusted: %s", pred.Reason))
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
	high := local.HighWater(cluster)
	switch {
	case legacy && (!pred.Mark.IsZero() || !high.IsZero()):
		return s.suspectRollback(rollbackLegacy, fmt.Errorf("only snapshots without an authenticated header are left, but the cluster wrote snapshots with one (the lease names %s, this instance knows %s)", pred.Mark, high))
	case !legacy && newest.Mark().Before(pred.Mark):
		return s.suspectRollback(rollbackBehindLease, fmt.Errorf("the newest verified snapshot (%s) is older than the last snapshot the lease names (%s): newer snapshots were deleted", newest.Mark(), pred.Mark))
	case !legacy && newest.Mark().Before(high):
		return s.suspectRollback(rollbackBehindLocal, fmt.Errorf("the newest verified snapshot (%s) is older than the newest this instance wrote or restored (%s): newer snapshots were deleted", newest.Mark(), high))
	}
	mark := newest.Mark()
	if legacy {
		mark = ha.SnapshotMark{} // its name is not authenticated
	}
	if local.Cluster == cluster && newest.Epoch != 0 && newest.Epoch == local.LastActiveEpoch &&
		(legacy || newest.Header.InstanceID == s.rt.state.InstanceID) {
		s.setSince(newest.CreatedAt)
		s.logger.Info("takeover: the newest snapshot was written by this instance; its own database is at least as new and is kept",
			slog.Uint64("epoch", newest.Epoch))
		return s.confirmTakeover(ctx, mark, false)
	}
	if legacy {
		s.logger.Warn("takeover: no metadata snapshot of the cluster has an authenticated header; restoring one written by an older version, whose epoch and cluster are not authenticated",
			logsafe.Attr("key", newest.Key))
	}
	if _, err = s.restoreWithRetry(ctx, newest.SnapshotInfo, legacy); err != nil {
		return err
	}
	s.setSince(newest.CreatedAt)
	s.logger.Info("takeover: restored the metadata snapshot", logsafe.Attr("key", newest.Key),
		slog.Uint64("snapshot_epoch", newest.Epoch), slog.Time("snapshot_time", newest.CreatedAt), slog.Bool("authenticated", !legacy))
	return s.confirmTakeover(ctx, mark, false)
}

// selectForTakeover selects the newest verified snapshot. While a snapshot that
// sorts before it cannot be checked (it may be newer), it tries again with a
// growing pause, and gives up after haTakeoverAttempts: an older snapshot never
// stands in for it.
func (s *haSupervisor) selectForTakeover(ctx context.Context) (ha.SnapshotSelection, error) {
	for attempt := 1; ; attempt++ {
		sel, err := s.selectSnapshots(ctx, 1)
		if err != nil {
			return ha.SnapshotSelection{}, err
		}
		failed, ahead := sel.FailedAhead()
		if !ahead {
			return sel, nil
		}
		if attempt >= haTakeoverAttempts {
			return ha.SnapshotSelection{}, fmt.Errorf("the metadata snapshot %s, which may be the newest, cannot be checked after %d attempts (an older one is never restored in its place): %w",
				failed.Key, attempt, failed.Err)
		}
		s.logger.Warn("takeover: a metadata snapshot that may be the newest cannot be checked; trying again", logsafe.Attr("key", failed.Key),
			slog.Int("attempt", attempt), logsafe.Error(failed.Err))
		if err = s.backoff(ctx, attempt); err != nil {
			return ha.SnapshotSelection{}, err
		}
	}
}

// restoreWithRetry restores snapshot snap (see restoreSnapshot), trying again
// with a growing pause after a failure that may pass, up to haTakeoverAttempts
// times. It returns the snapshot's header (nil for a legacy one).
func (s *haSupervisor) restoreWithRetry(ctx context.Context, snap ha.SnapshotInfo, allowLegacy bool) (*ha.SnapshotHeader, error) {
	passphrase, err := ha.SnapshotPassphrase(s.rt.key)
	if err != nil {
		return nil, err
	}
	box, err := secretbox.New(s.rt.key)
	if err != nil {
		return nil, err
	}
	for attempt := 1; ; attempt++ {
		h, err := s.restoreSnapshot(ctx, s.rt.storage(), snap, passphrase, box, allowLegacy)
		switch {
		case err == nil:
			return h, nil
		case errors.Is(err, ha.ErrSnapshotRejected) || errors.Is(err, ha.ErrSnapshotUnreadable):
			// Replaced since it was checked.
			s.reportRefused(ha.SnapshotCheck{SnapshotInfo: snap, Err: err})
			return nil, err
		case errors.Is(err, ha.ErrSnapshotLegacy) || errors.Is(err, ha.ErrSnapshotFormat):
			return nil, err
		case attempt >= haTakeoverAttempts:
			return nil, fmt.Errorf("cannot restore the metadata snapshot %s after %d attempts (an older one is never restored in its place): %w", snap.Key, attempt, err)
		}
		s.logger.Warn("takeover: cannot restore the metadata snapshot; trying again", logsafe.Attr("key", snap.Key),
			slog.Int("attempt", attempt), logsafe.Error(err))
		if err = s.backoff(ctx, attempt); err != nil {
			return nil, err
		}
	}
}

// backoff waits before attempt+1: the first pause doubled attempt-1 times.
func (s *haSupervisor) backoff(ctx context.Context, attempt int) error {
	pause := s.retryPause
	if pause <= 0 {
		pause = haTakeoverBackoff
	}
	timer := time.NewTimer(pause << (attempt - 1))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// forceTakeover runs the takeover an operator forced (ha.Override): it restores
// the named snapshot, whose header is still verified when it has one (a legacy
// snapshot is accepted), or keeps this instance's database, without the rollback
// checks. The override is recorded in the audit log once the instance serves.
func (s *haSupervisor) forceTakeover(ctx context.Context, o *ha.Override) error {
	s.logger.Error("takeover forced by an operator: the rollback checks are skipped", slog.String("requested_by", o.RequestedBy),
		slog.Time("requested_at", o.RequestedAt), logsafe.Attr("snapshot", o.Snapshot), slog.Bool("keep_local_database", o.KeepLocal))
	mark := ha.SnapshotMark{}
	if o.KeepLocal {
		s.setSince(time.Time{})
	} else {
		at, epoch, _ := ha.ParseSnapshotKey(o.Snapshot)
		info := ha.SnapshotInfo{Key: o.Snapshot, CreatedAt: at, Epoch: epoch}
		h, err := s.restoreWithRetry(ctx, info, true)
		if err != nil {
			return fmt.Errorf("the forced takeover from %s failed: %w", o.Snapshot, err)
		}
		if h != nil {
			mark = info.Mark()
		}
		s.setSince(at)
	}
	s.rt.mu.Lock()
	s.rt.forced = o
	s.rt.mu.Unlock()
	return s.confirmTakeover(ctx, mark, true)
}

// confirmTakeover records mark as this instance's high-water mark (reset by a
// forced takeover, else raised) and confirms the takeover to the lease manager,
// which signs the lease from then on.
func (s *haSupervisor) confirmTakeover(ctx context.Context, mark ha.SnapshotMark, reset bool) error {
	s.observeCluster(mark, reset)
	return s.rt.mgr().Confirm(ctx, mark)
}

// observeCluster records that this instance has seen the cluster, with mark as its
// high-water mark (reset, else raised).
func (s *haSupervisor) observeCluster(mark ha.SnapshotMark, reset bool) {
	cluster := s.cfg.HA.Cluster
	if err := s.rt.updateState(func(st *ha.State) { st.Observe(cluster, mark, reset) }); err != nil {
		s.logger.Warn("could not record the HA high-water mark in the data directory", logsafe.Error(err))
	}
}

// suspectRollback counts and alerts a takeover refused for reason and returns
// err wrapped in ErrRollbackSuspected.
func (s *haSupervisor) suspectRollback(reason string, err error) error {
	s.rt.countRollback(reason)
	s.logger.Error("takeover refused: it could roll the cluster's metadata back. Check the HA target (who can write to it, which snapshots and lease are there), then force the takeover with `mongorescue ha force-takeover --from-snapshot <key>` or `--keep-local-database`",
		slog.String("reason", reason), logsafe.Error(err))
	return fmt.Errorf("%w (%s): %w", ErrRollbackSuspected, reason, err)
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
// (its authenticated header, unless allowLegacy accepts one without; the header is
// returned, nil for a legacy snapshot) and swaps it in as the metadata database (the previous one is kept as .bak).
func (s *haSupervisor) restoreSnapshot(ctx context.Context, st storage.Storage, snap ha.SnapshotInfo, passphrase string, box *secretbox.Box, allowLegacy bool) (*ha.SnapshotHeader, error) {
	dir, err := os.MkdirTemp(s.cfg.DataDir, ".ha-restore-*")
	if err != nil {
		return nil, fmt.Errorf("restore directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	tmp := filepath.Join(dir, config.DefaultDatabaseFileName)
	h, err := ha.FetchSnapshot(ctx, st, s.cfg.HA.Cluster, snap.Key, passphrase, tmp, allowLegacy)
	if err != nil {
		return nil, err
	}
	_, err = store.ReplaceDatabase(ctx, store.Replacement{Path: s.cfg.MetadataDBPath(), NewPath: tmp, Epoch: s.rt.mgr().Epoch(),
		Keep: store.DefaultBackupsKept, Logger: s.logger}, store.WithSecretBox(box))
	return h, err
}

// auditForcedTakeover records a takeover an operator forced in the audit log and
// sends it as a security event to the notification channels.
func (s *haSupervisor) auditForcedTakeover(ctx context.Context, app *App, o *ha.Override, epoch uint64) {
	targets := map[string]string{"cluster": o.Cluster, "requested_by": o.RequestedBy, "epoch": strconv.FormatUint(epoch, 10)}
	what := "kept its own database"
	if o.KeepLocal {
		targets["keep_local_database"] = "true"
	} else {
		targets["snapshot"] = o.Snapshot
		what = "restored the metadata snapshot " + o.Snapshot
	}
	app.auditLog.Record(ctx, auditlog.Event{ActorKind: auditlog.ActorSystem, ActorName: "ha force-takeover", Action: "SYSTEM ha.force_takeover",
		Targets: targets, Outcome: auditlog.OutcomeOK})
	if app.bus != nil {
		detail := fmt.Sprintf("operator %s forced a takeover of HA cluster %s: instance %s (%s) became active under epoch %d and %s, without the rollback checks",
			o.RequestedBy, o.Cluster, s.rt.name, s.rt.state.InstanceID, epoch, what)
		app.bus.Publish(ctx, events.SecurityEvent(events.SecurityDestructiveAction, time.Now(), "ha_force_takeover", o.RequestedBy, "", detail))
	}
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
	if sel.Listed > 0 {
		s.observeCluster(ha.SnapshotMark{}, false)
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
