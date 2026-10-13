package app

import (
	"context"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/ha"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// epochFence is a models.Fence that always passes with its epoch.
type epochFence struct{ epoch uint64 }

func (f *epochFence) Check() error  { return nil }
func (f *epochFence) Epoch() uint64 { return f.epoch }

// TestTakeoverRefusesAReplayedSnapshot copies the previous active instance's old
// snapshot to a key with a much higher epoch, as someone with write access to the
// HA target but without secret.key can. The standby refuses it and counts it, its
// new epoch follows the genuine snapshots, and it restores the newest genuine one.
func TestTakeoverRefusesAReplayedSnapshot(t *testing.T) {
	t.Parallel() // independent: own data directories, HA targets and ports
	ctx := context.Background()
	shared, key := t.TempDir(), haTestKey(t)
	target, err := storage.NewLocalStorage(shared)
	if err != nil {
		t.Fatal(err)
	}

	// The previous active instance wrote a snapshot under epoch 1, then one under
	// epoch 2 after a change.
	previous := newTestSupervisor(t, haTestConfig(t, shared, key))
	app, err := previous.open(false)
	if err != nil {
		t.Fatal(err)
	}
	meta := app.metaStore.(*store.SQLiteStore)
	passphrase, err := ha.SnapshotPassphrase(app.haKey)
	if err != nil {
		t.Fatal(err)
	}
	fence := &epochFence{epoch: 1}
	snap, err := ha.NewSnapshotter(ha.SnapshotterConfig{Cluster: previous.cfg.HA.Cluster, InstanceID: "previous", Store: meta,
		Storage: target, Fence: fence, Passphrase: passphrase, TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	write := func(marker string) ha.SnapshotInfo {
		t.Helper()
		if err = meta.SaveIntegrityState(ctx, haMarker, map[string]string{"from": marker}); err != nil {
			t.Fatal(err)
		}
		info, snapErr := snap.Snapshot(ctx)
		if snapErr != nil {
			t.Fatal(snapErr)
		}
		return info
	}
	old := write("before the change")
	fence.epoch = 2
	time.Sleep(2 * time.Millisecond) // distinct snapshot names
	write("after the change")
	if err = app.Close(); err != nil {
		t.Fatal(err)
	}
	rc, err := target.Retrieve(ctx, old.Key)
	if err != nil {
		t.Fatal(err)
	}
	_, err = target.Save(ctx, ha.SnapshotKey(previous.cfg.HA.Cluster, time.Now().Add(time.Hour), 50), rc)
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}

	sup := bootstrappedSupervisor(t, haTestConfig(t, shared, key))
	m := sup.rt.mgr()
	m.Step(ctx)
	if m.Role() != ha.RoleActive || m.Epoch() != 3 {
		t.Fatalf("role %s, epoch %d; want active under epoch 3, above the genuine snapshots and not the copy", m.Role(), m.Epoch())
	}
	if err = sup.prepareTakeover(ctx); err != nil {
		t.Fatalf("the takeover was refused: %v", err)
	}
	// Every decision read the copy (startup check, epoch floor, takeover); it was
	// downloaded and counted once, and stays listed as refused.
	if st := sup.rt.metricsState(); st.SnapshotsRejected != 1 || st.RefusedSnapshots != 1 {
		t.Fatalf("rejected %d, refused %d; want the copy counted once and listed", st.SnapshotsRejected, st.RefusedSnapshots)
	}
	active, err := sup.open(true)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	var v map[string]string
	if ok, loadErr := active.metaStore.(*store.SQLiteStore).LoadIntegrityState(ctx, haMarker, &v); loadErr != nil || !ok || v["from"] != "after the change" {
		t.Fatalf("restored the metadata %v (%v, %v); want the newest genuine snapshot's", v, ok, loadErr)
	}
}
