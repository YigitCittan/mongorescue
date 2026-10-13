package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/ha"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

// haWriter is the previous active instance of a test cluster: it holds a signed
// lease and writes snapshots the way an active App does, naming each in the lease.
type haWriter struct {
	cluster string
	target  *storage.LocalStorage
	meta    *store.SQLiteStore
	mgr     *ha.Manager
	snap    *ha.Snapshotter
	macKey  []byte
	pass    string
}

// newHAWriter returns the active instance of a cluster on shared, key.
func newHAWriter(t *testing.T, shared, key string) *haWriter {
	t.Helper()
	ctx := context.Background()
	sup := newTestSupervisor(t, haTestConfig(t, shared, key))
	app, err := sup.open(false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	target, err := storage.NewLocalStorage(shared)
	if err != nil {
		t.Fatal(err)
	}
	ls, err := storage.AsLeaseStore(target)
	if err != nil {
		t.Fatal(err)
	}
	w := &haWriter{cluster: sup.cfg.HA.Cluster, target: target, meta: app.metaStore.(*store.SQLiteStore)}
	if w.macKey, err = ha.LeaseMACKey(app.haKey); err != nil {
		t.Fatal(err)
	}
	if w.pass, err = ha.SnapshotPassphrase(app.haKey); err != nil {
		t.Fatal(err)
	}
	w.mgr, err = ha.New(ha.Config{Store: ls, Cluster: w.cluster, InstanceID: "previous", TTL: sup.cfg.HA.LeaseTTL,
		Renew: sup.cfg.HA.RenewInterval, MACKey: w.macKey, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	w.mgr.Step(ctx)
	if err = w.mgr.Confirm(ctx, ha.SnapshotMark{}); err != nil {
		t.Fatalf("the writer did not become active: %v", err)
	}
	w.snap, err = ha.NewSnapshotter(ha.SnapshotterConfig{Cluster: w.cluster, InstanceID: "previous", Store: w.meta, Storage: target,
		Fence: w.mgr, Passphrase: w.pass, TempDir: t.TempDir(), Keep: 100,
		OnWritten: func(ctx context.Context, info ha.SnapshotInfo) { w.mgr.RecordSnapshot(ctx, info.Mark()) }})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// write records marker in the database and snapshots it.
func (w *haWriter) write(t *testing.T, marker string) ha.SnapshotInfo {
	t.Helper()
	ctx := context.Background()
	if err := w.meta.SaveIntegrityState(ctx, haMarker, map[string]string{"from": marker}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // distinct snapshot names
	info, err := w.snap.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// release gives the lease up, so a standby takes over at once.
func (w *haWriter) release(t *testing.T) {
	t.Helper()
	if err := w.mgr.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// copyObject copies the object from to the key to on the HA target, as someone
// with write access (but without secret.key) can.
func (w *haWriter) copyObject(t *testing.T, from, to string) {
	t.Helper()
	rc, err := w.target.Retrieve(context.Background(), from)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err = w.target.Save(context.Background(), to, rc); err != nil {
		t.Fatal(err)
	}
}

// lease returns the lease on the HA target.
func (w *haWriter) lease(t *testing.T) ha.Lease {
	t.Helper()
	rc, err := w.target.Retrieve(context.Background(), ha.LeaseKey(w.cluster))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	l, err := ha.DecodeLease(data)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// standby returns a bootstrapped standby of the writer's cluster that acquired
// the lease (epoch want, 0: any).
func standbyThatAcquired(t *testing.T, shared, key string, want uint64) *haSupervisor {
	t.Helper()
	sup := bootstrappedSupervisor(t, haTestConfig(t, shared, key))
	sup.retryPause = time.Millisecond
	m := sup.rt.mgr()
	m.Step(context.Background())
	if m.Role() != ha.RoleActive || (want != 0 && m.Epoch() != want) {
		t.Fatalf("role %s, epoch %d; want active under epoch %d (last error %q)", m.Role(), m.Epoch(), want, m.Status().LastError)
	}
	return sup
}

// marker opens sup's data directory as the active instance and returns its marker.
func marker(t *testing.T, sup *haSupervisor) string {
	t.Helper()
	active, err := sup.open(true)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	var v map[string]string
	if ok, err := active.metaStore.(*store.SQLiteStore).LoadIntegrityState(context.Background(), haMarker, &v); err != nil || !ok {
		return ""
	}
	return v["from"]
}

// expectRollback checks that err refuses the takeover for reason, counted once.
func expectRollback(t *testing.T, sup *haSupervisor, err error, reason string) {
	t.Helper()
	if !errors.Is(err, ErrRollbackSuspected) || !strings.Contains(err.Error(), reason) {
		t.Fatalf("takeover = %v; want ErrRollbackSuspected (%s)", err, reason)
	}
	if n := sup.rt.metricsState().RollbackSuspected[reason]; n != 1 {
		t.Fatalf("rollback_suspected{reason=%q} = %d; want 1", reason, n)
	}
}

// TestTakeoverRefusesAReplayedSnapshot copies the previous active instance's old
// snapshot to a key with a much higher epoch, as someone with write access to the
// HA target but without secret.key can. The standby refuses it and counts it, its
// new epoch follows the genuine snapshots, and it restores the newest genuine one.
func TestTakeoverRefusesAReplayedSnapshot(t *testing.T) {
	t.Parallel() // independent: own data directories, HA targets and ports
	shared, key := t.TempDir(), haTestKey(t)
	w := newHAWriter(t, shared, key)
	old := w.write(t, "before the change")
	w.write(t, "after the change")
	w.copyObject(t, old.Key, ha.SnapshotKey(w.cluster, time.Now().Add(time.Hour), 50))
	w.release(t)

	sup := standbyThatAcquired(t, shared, key, 2)
	if err := sup.prepareTakeover(context.Background()); err != nil {
		t.Fatalf("the takeover was refused: %v", err)
	}
	// Every decision read the copy (startup check, epoch floor, takeover); it was
	// downloaded and counted once, and stays listed as refused.
	if st := sup.rt.metricsState(); st.SnapshotsRejected != 1 || st.RefusedSnapshots != 1 {
		t.Fatalf("rejected %d, refused %d; want the copy counted once and listed", st.SnapshotsRejected, st.RefusedSnapshots)
	}
	if got := marker(t, sup); got != "after the change" {
		t.Fatalf("restored the metadata of %q; want the newest genuine snapshot's", got)
	}
}

// TestTakeoverRefusesARollbackBehindTheLease deletes the newest snapshot, so an
// older genuine one is the newest left. The lease names the deleted one: the
// takeover is refused and alerted until an operator forces it, which is recorded.
func TestTakeoverRefusesARollbackBehindTheLease(t *testing.T) {
	t.Parallel() // independent: own data directories, HA targets and ports
	ctx := context.Background()
	shared, key := t.TempDir(), haTestKey(t)
	w := newHAWriter(t, shared, key)
	old := w.write(t, "before the change")
	newest := w.write(t, "after the change")
	if l := w.lease(t); l.SnapshotMark() != newest.Mark() || !l.Authentic(w.macKey) {
		t.Fatalf("the lease %+v does not name the newest snapshot %s", l, newest.Mark())
	}
	if err := w.target.Delete(ctx, newest.Key); err != nil {
		t.Fatal(err)
	}
	w.release(t)

	sup := standbyThatAcquired(t, shared, key, 0)
	expectRollback(t, sup, sup.prepareTakeover(ctx), rollbackBehindLease)
	if backups, _ := store.DatabaseBackups(sup.cfg.MetadataDBPath()); len(backups) != 0 {
		t.Fatal("the database was replaced although the takeover was refused")
	}

	// The operator checked the HA target and forces the takeover from the old one.
	if err := ha.WriteOverride(sup.cfg.DataDir, ha.Override{Cluster: w.cluster, Snapshot: old.Key, RequestedBy: "alice", RequestedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := sup.prepareTakeover(ctx); err != nil {
		t.Fatalf("the forced takeover failed: %v", err)
	}
	if l := w.lease(t); !l.Authentic(w.macKey) || l.Holder != sup.rt.state.InstanceID {
		t.Fatalf("the lease after the forced takeover %+v is not signed by the new holder", l)
	}
	active, err := sup.open(true)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	var v map[string]string
	if ok, loadErr := active.metaStore.(*store.SQLiteStore).LoadIntegrityState(ctx, haMarker, &v); loadErr != nil || !ok || v["from"] != "before the change" {
		t.Fatalf("restored %v; want the snapshot the operator named", v)
	}
	o := sup.rt.takeForced()
	if o == nil || o.RequestedBy != "alice" {
		t.Fatalf("forced takeover %+v not kept for the audit log", o)
	}
	sup.auditForcedTakeover(ctx, active, o, sup.rt.mgr().Epoch())
	page, err := active.auditLog.List(ctx, auditlog.Filter{Action: "ha.force_takeover"})
	if err != nil || len(page.Events) != 1 || page.Events[0].Targets["snapshot"] != old.Key || page.Events[0].Targets["requested_by"] != "alice" {
		t.Fatalf("audit log %+v, %v; want the forced takeover", page, err)
	}
}

// TestTakeoverRefusesAnUntrustedLease replaces the lease by one without a MAC
// (as an earlier version, or someone without secret.key, writes it). The standby
// that takes it over does not sign its own lease, refuses the takeover, and signs
// only once an operator forces it.
func TestTakeoverRefusesAnUntrustedLease(t *testing.T) {
	t.Parallel() // independent: own data directories, HA targets and ports
	ctx := context.Background()
	shared, key := t.TempDir(), haTestKey(t)
	w := newHAWriter(t, shared, key)
	w.write(t, "genuine")
	w.release(t)
	forged := w.lease(t)
	forged.MAC, forged.LastSnapshotEpoch, forged.LastSnapshotAt = "", 0, time.Time{}
	data, err := forgedLease(forged)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.target.Save(ctx, ha.LeaseKey(w.cluster), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}

	sup := standbyThatAcquired(t, shared, key, 0)
	expectRollback(t, sup, sup.prepareTakeover(ctx), rollbackLeaseUntrusted)
	if l := w.lease(t); l.MAC != "" {
		t.Fatal("a lease taken over from an untrusted one was signed before the takeover was confirmed")
	}
	if err = ha.WriteOverride(sup.cfg.DataDir, ha.Override{Cluster: w.cluster, KeepLocal: true, RequestedBy: "bob", RequestedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err = sup.prepareTakeover(ctx); err != nil {
		t.Fatalf("the forced takeover failed: %v", err)
	}
	if l := w.lease(t); !l.Authentic(w.macKey) {
		t.Fatal("the lease is not signed after the forced takeover")
	}
}

// forgedLease encodes l as the lease object.
func forgedLease(l ha.Lease) ([]byte, error) { return json.Marshal(l) }

// flakyStorage fails the downloads of the keys fail names.
type flakyStorage struct {
	storage.Storage
	mu   sync.Mutex
	fail map[string]bool
}

func (f *flakyStorage) Retrieve(ctx context.Context, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	failing := f.fail[key]
	f.mu.Unlock()
	if failing {
		return nil, errors.New("connection reset")
	}
	return f.Storage.Retrieve(ctx, key)
}

// TestTakeoverWaitsForANewerSnapshotItCannotRead makes the newest snapshot
// unreadable for a while: the takeover tries again, then gives up without
// restoring the older one, and succeeds once the newest can be read.
func TestTakeoverWaitsForANewerSnapshotItCannotRead(t *testing.T) {
	t.Parallel() // independent: own data directories, HA targets and ports
	ctx := context.Background()
	shared, key := t.TempDir(), haTestKey(t)
	w := newHAWriter(t, shared, key)
	w.write(t, "older")
	newest := w.write(t, "newest")
	w.release(t)

	sup := standbyThatAcquired(t, shared, key, 0)
	flaky := &flakyStorage{Storage: sup.rt.storage(), fail: map[string]bool{newest.Key: true}}
	sup.rt.mu.Lock()
	sup.rt.store = flaky
	sup.rt.checker = ha.NewSnapshotChecker(flaky, w.cluster, w.pass, sup.reportRefused)
	sup.rt.mu.Unlock()
	err := sup.prepareTakeover(ctx)
	if err == nil || !strings.Contains(err.Error(), "cannot be checked after 4 attempts") {
		t.Fatalf("takeover with the newest snapshot unreadable = %v; want a refusal after 4 attempts", err)
	}
	if backups, _ := store.DatabaseBackups(sup.cfg.MetadataDBPath()); len(backups) != 0 {
		t.Fatal("an older snapshot was restored in place of the newest")
	}
	flaky.mu.Lock()
	flaky.fail = nil
	flaky.mu.Unlock()
	if err = sup.prepareTakeover(ctx); err != nil {
		t.Fatalf("the takeover failed once the newest snapshot could be read: %v", err)
	}
	if got := marker(t, sup); got != "newest" {
		t.Fatalf("restored %q; want the newest snapshot", got)
	}
}

// TestLegacySnapshotsAreRefusedAfterTheUpgrade leaves only a snapshot without a
// header although the lease names one with a header: it is not restored.
func TestLegacySnapshotsAreRefusedAfterTheUpgrade(t *testing.T) {
	t.Parallel() // independent: own data directories, HA targets and ports
	ctx := context.Background()
	shared, key := t.TempDir(), haTestKey(t)
	w := newHAWriter(t, shared, key)
	genuine := w.write(t, "with header")
	if err := w.target.Delete(ctx, genuine.Key); err != nil {
		t.Fatal(err)
	}
	enc, err := encryption.NewScryptEncryptor(w.pass, 10)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	wc, err := enc.Encrypt(&buf)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(wc, "SQLite format 3\x00")
	if err = wc.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = w.target.Save(ctx, ha.SnapshotKey(w.cluster, time.Now(), 1), &buf); err != nil {
		t.Fatal(err)
	}
	w.release(t)

	sup := standbyThatAcquired(t, shared, key, 0)
	expectRollback(t, sup, sup.prepareTakeover(ctx), rollbackLegacy)
}

// TestTakeoverRefusesASnapshotOlderThanTheLocalHighWater covers a standby that
// restored or wrote a newer snapshot than any left on the HA target.
func TestTakeoverRefusesASnapshotOlderThanTheLocalHighWater(t *testing.T) {
	t.Parallel() // independent: own data directories, HA targets and ports
	shared, key := t.TempDir(), haTestKey(t)
	w := newHAWriter(t, shared, key)
	newest := w.write(t, "newest")
	w.release(t)

	sup := standbyThatAcquired(t, shared, key, 0)
	if err := sup.rt.updateState(func(st *ha.State) {
		st.Observe(w.cluster, ha.SnapshotMark{Epoch: newest.Epoch, At: newest.CreatedAt.Add(time.Hour)}, false)
	}); err != nil {
		t.Fatal(err)
	}
	expectRollback(t, sup, sup.prepareTakeover(context.Background()), rollbackBehindLocal)
}

// TestFirstActivationNeedsAFreshInstance covers a takeover without a lease and
// without snapshots: allowed only for an instance that never saw the cluster and
// whose database holds nothing of its own; otherwise refused until an operator
// keeps the local database.
func TestFirstActivationNeedsAFreshInstance(t *testing.T) {
	t.Parallel() // independent: own data directories, HA targets and ports
	ctx := context.Background()
	seen := standbyThatAcquiredFresh(t, func(sup *haSupervisor) {
		if err := sup.rt.updateState(func(st *ha.State) { st.Observe(sup.cfg.HA.Cluster, ha.SnapshotMark{}, false) }); err != nil {
			t.Fatal(err)
		}
	})
	err := seen.prepareTakeover(ctx)
	expectRollback(t, seen, err, rollbackLeaseMissing)
	if !errors.Is(err, ErrNoSnapshotToTakeOver) {
		t.Fatalf("takeover = %v; want ErrNoSnapshotToTakeOver too", err)
	}

	populated := standbyThatAcquiredFresh(t, func(sup *haSupervisor) { sup.rt.localPopulated = true })
	expectRollback(t, populated, populated.prepareTakeover(ctx), rollbackLeaseMissing)
	if err = ha.WriteOverride(populated.cfg.DataDir, ha.Override{Cluster: populated.cfg.HA.Cluster, KeepLocal: true, RequestedBy: "carol", RequestedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err = populated.prepareTakeover(ctx); err != nil {
		t.Fatalf("the forced takeover with the local database failed: %v", err)
	}
}

// standbyThatAcquiredFresh bootstraps a standby on an empty HA target, applies
// prepare, and lets it create the lease.
func standbyThatAcquiredFresh(t *testing.T, prepare func(*haSupervisor)) *haSupervisor {
	t.Helper()
	sup := bootstrappedSupervisor(t, haTestConfig(t, t.TempDir(), haTestKey(t)))
	prepare(sup)
	sup.rt.mgr().Step(context.Background())
	if !sup.rt.mgr().AcquiredFresh() {
		t.Fatal("the lease was not created")
	}
	return sup
}

// TestLocalPopulated checks what counts as a database of its own: a job or a
// storage target besides the default one.
func TestLocalPopulated(t *testing.T) {
	t.Parallel() // independent: own data directories, HA targets and ports
	ctx := context.Background()
	sup := newTestSupervisor(t, haTestConfig(t, t.TempDir(), haTestKey(t)))
	app, err := sup.open(false)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if localPopulated(ctx, app, logger) {
		t.Fatal("a fresh database counts as populated")
	}
	if _, err = app.targets.Create(ctx, targets.Input{Name: "second", Type: models.StorageLocal,
		Local: &models.LocalTarget{Path: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	if !localPopulated(ctx, app, logger) {
		t.Fatal("a database with a storage target of its own counts as fresh")
	}
}
