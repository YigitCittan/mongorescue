package ha

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// fileVacuum is a VacuumStore whose "database" is a fixed content.
type fileVacuum struct {
	content string
	calls   atomic.Int32
}

func (f *fileVacuum) VacuumInto(_ context.Context, path string) error {
	f.calls.Add(1)
	return os.WriteFile(path, []byte(f.content), 0o600)
}

// staticFence is a models.Fence with a fixed outcome.
type staticFence struct {
	err   error
	epoch uint64
}

func (f *staticFence) Check() error  { return f.err }
func (f *staticFence) Epoch() uint64 { return f.epoch }

func testPassphrase(t *testing.T) string {
	t.Helper()
	key, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	p, err := SnapshotPassphrase(key)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newTestSnapshotter(t *testing.T, st storage.Storage, fence *staticFence, db *fileVacuum, passphrase string, clock *fakeClock) *Snapshotter {
	t.Helper()
	s, err := NewSnapshotter(SnapshotterConfig{
		Cluster: "prod", InstanceID: "instance-a", Store: db, Storage: st, Fence: fence, Passphrase: passphrase, TempDir: t.TempDir(),
		Interval: time.Hour, MinGap: 10 * time.Millisecond, Keep: 3, Now: clock.Now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSnapshotKeyRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 12, 10, 0, 0, 123_000_000, time.UTC)
	key := SnapshotKey("prod", at, 42)
	if key != "_mongorescue/metadata/ha/prod/mongorescue-20261012T100000123Z-e00000000000000000042.db.age" {
		t.Fatalf("SnapshotKey = %q", key)
	}
	got, epoch, ok := parseSnapshotKey(key)
	if !ok || !got.Equal(at) || epoch != 42 {
		t.Fatalf("parseSnapshotKey = %v %d %v", got, epoch, ok)
	}
	for _, bad := range []string{"mongorescue-20261012T100000123Z.db.age", "x/mongorescue-2026T1Z-e1.db.age", "lease.json"} {
		if _, _, ok = parseSnapshotKey(bad); ok {
			t.Errorf("parseSnapshotKey(%q) accepted", bad)
		}
	}
}

func TestSnapshotWriteFetchAndRetention(t *testing.T) {
	ctx := context.Background()
	st, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clock, fence, db, pass := newFakeClock(), &staticFence{epoch: 5}, &fileVacuum{content: "sqlite bytes"}, testPassphrase(t)
	s := newTestSnapshotter(t, st, fence, db, pass, clock)

	info, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Epoch != 5 || !strings.HasPrefix(info.Key, SnapshotPrefix("prod")) {
		t.Fatalf("snapshot = %+v", info)
	}
	// The stored object is encrypted, and decrypts with the passphrase only.
	raw, err := st.Retrieve(ctx, info.Key)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := io.ReadAll(raw)
	_ = raw.Close()
	if strings.Contains(string(stored), "sqlite bytes") {
		t.Fatal("the snapshot is stored in clear text")
	}
	out := filepath.Join(t.TempDir(), "restored.db")
	h, err := FetchSnapshot(ctx, st, "prod", info.Key, pass, out, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); string(got) != "sqlite bytes" {
		t.Fatalf("restored %q", got)
	}
	want := SnapshotHeader{Format: SnapshotFormat, Cluster: "prod", Epoch: 5, InstanceID: "instance-a", CreatedAt: info.CreatedAt}
	if h == nil || *h != want {
		t.Fatalf("header %+v; want %+v", h, want)
	}
	if c := CheckSnapshot(ctx, st, "prod", pass, info); !c.Verified() {
		t.Fatalf("CheckSnapshot = %v", c.Err)
	}
	other := testPassphrase(t)
	if c := CheckSnapshot(ctx, st, "prod", other, info); !errors.Is(c.Err, ErrSnapshotUnreadable) || !c.Refused() {
		t.Fatalf("CheckSnapshot with another key = %v; want ErrSnapshotUnreadable", c.Err)
	}
	if _, err = FetchSnapshot(ctx, st, "prod", info.Key, other, filepath.Join(t.TempDir(), "x.db"), false); !errors.Is(err, ErrSnapshotUnreadable) {
		t.Fatalf("FetchSnapshot with another key = %v; want ErrSnapshotUnreadable", err)
	}

	// Retention keeps the first three (the highest epoch, then the newest); a late
	// snapshot of an older epoch sorts last, but as the newest by time it is kept too.
	for range 4 {
		clock.Advance(time.Minute)
		if _, err = s.Snapshot(ctx); err != nil {
			t.Fatal(err)
		}
	}
	fence.epoch = 4
	clock.Advance(time.Minute)
	stale, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	list, err := ListSnapshots(ctx, st, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 {
		t.Fatalf("%d snapshots kept; want the three newest of epoch 5 and the newest by time", len(list))
	}
	if list[0].Epoch != 5 || !list[0].CreatedAt.Equal(clock.Now().Add(-time.Minute)) {
		t.Fatalf("first snapshot = %+v; want the newest of epoch 5", list[0])
	}
	if list[3].Key != stale.Key {
		t.Fatalf("the late snapshot of an older epoch must sort last: %+v", list)
	}
}

// TestPruneKeepsTheCurrentEpochAndTheNewest covers retention with mixed epochs: a
// stale epoch 9 left by a lease deleted by hand sorts first, yet the current epoch's
// newest snapshots and the newest one by time are never deleted.
func TestPruneKeepsTheCurrentEpochAndTheNewest(t *testing.T) {
	base := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	var list []SnapshotInfo
	add := func(epoch uint64, minute int) SnapshotInfo {
		at := base.Add(time.Duration(minute) * time.Minute)
		sn := SnapshotInfo{Key: SnapshotKey("prod", at, epoch), CreatedAt: at, Epoch: epoch}
		list = append(list, sn)
		return sn
	}
	for m := range 4 {
		add(9, m) // old, from before the lease was lost
	}
	var current []SnapshotInfo
	for m := 10; m < 15; m++ {
		current = append(current, add(2, m))
	}
	late := add(1, 20) // a late write of a fenced-off instance, newest by time
	st, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sorted := append([]SnapshotInfo(nil), list...)
	for _, sn := range sorted {
		if _, err = st.Save(context.Background(), sn.Key, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	sorted, err = ListSnapshots(context.Background(), st, "prod")
	if err != nil {
		t.Fatal(err)
	}
	kept := keptSnapshots(sorted, 2, 3)
	for _, sn := range current[2:] {
		if !kept[sn.Key] {
			t.Errorf("a newest snapshot of the current epoch is not kept: %s", sn.Key)
		}
	}
	if !kept[late.Key] {
		t.Error("the newest snapshot by time is not kept")
	}
	for _, sn := range current[:2] {
		if kept[sn.Key] {
			t.Errorf("an old snapshot of the current epoch beyond the three newest is kept: %s", sn.Key)
		}
	}
	if len(kept) != 3+3+1 {
		t.Fatalf("%d kept; want the first three, the three newest of the current epoch and the newest by time", len(kept))
	}
}

func TestSnapshotRefusedWithoutTheLease(t *testing.T) {
	st, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db := &fileVacuum{content: "x"}
	s := newTestSnapshotter(t, st, &staticFence{err: ErrNotActive}, db, testPassphrase(t), newFakeClock())
	if _, err = s.Snapshot(context.Background()); !errors.Is(err, ErrNotActive) {
		t.Fatalf("Snapshot without the lease = %v; want ErrNotActive", err)
	}
	if db.calls.Load() != 0 {
		t.Fatal("the database was copied without the lease")
	}
	if list, _ := ListSnapshots(context.Background(), st, "prod"); len(list) != 0 {
		t.Fatalf("snapshots written without the lease: %v", list)
	}
}

func TestSnapshotterRunSnapshotsAfterAKick(t *testing.T) {
	st, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := newTestSnapshotter(t, st, &staticFence{epoch: 1}, &fileVacuum{content: "x"}, testPassphrase(t), newFakeClock())
	s.cfg.Now = time.Now
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()
	waitSnapshots := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if list, _ := ListSnapshots(context.Background(), st, "prod"); len(list) >= n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("fewer than %d snapshots", n)
	}
	waitSnapshots(1) // the first one after MinGap
	time.Sleep(5 * time.Millisecond)
	s.Kick()
	waitSnapshots(2)
	cancel()
	<-done
}

func TestNewSnapshotterValidates(t *testing.T) {
	if _, err := NewSnapshotter(SnapshotterConfig{Cluster: "prod"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NewSnapshotter without dependencies = %v", err)
	}
}
