package ha

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

// testMACKey returns a lease MAC key of a fresh secret key.
func testMACKey(t *testing.T) []byte {
	t.Helper()
	secret, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	key, err := LeaseMACKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// newMACManager returns a manager of cluster prod that signs its leases with key.
func newMACManager(t *testing.T, st *memStore, clock *fakeClock, id string, key []byte) *Manager {
	t.Helper()
	m, err := New(Config{Store: st, Cluster: "prod", InstanceID: id, TTL: testTTL, Renew: testRenew, MACKey: key,
		Now: clock.Now, Wall: clock.Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestLeaseMAC(t *testing.T) {
	key := testMACKey(t)
	at := time.Date(2026, 10, 12, 10, 0, 0, 123_000_000, time.UTC)
	l := Lease{Cluster: "prod", Holder: "a", Epoch: 4, Seq: 9, LastSnapshotEpoch: 4, LastSnapshotAt: at}.sign(key)
	if !l.Authentic(key) {
		t.Fatal("a signed lease is not authentic")
	}
	// Seq, the holder's name and the renewal time are not authenticated.
	other := l
	other.Seq, other.HolderName, other.RenewedAt = 10, "host", at
	if !other.Authentic(key) {
		t.Fatal("a renewal broke the MAC")
	}
	for name, change := range map[string]func(*Lease){
		"cluster":        func(l *Lease) { l.Cluster = "staging" },
		"epoch":          func(l *Lease) { l.Epoch = 5 },
		"holder":         func(l *Lease) { l.Holder = "" },
		"snapshot epoch": func(l *Lease) { l.LastSnapshotEpoch = 3 },
		"snapshot time":  func(l *Lease) { l.LastSnapshotAt = at.Add(-time.Second) },
		"no MAC":         func(l *Lease) { l.MAC = "" },
		"bad MAC":        func(l *Lease) { l.MAC = "zz" },
	} {
		forged := l
		change(&forged)
		if forged.Authentic(key) {
			t.Errorf("%s: an altered lease is authentic", name)
		}
	}
	if l.Authentic(testMACKey(t)) {
		t.Fatal("a lease is authentic under another key")
	}
}

// TestLeaseTakenOverFromAnUntrustedOneStaysUnsigned covers a lease written without
// a valid MAC: the instance that takes it over reports it untrusted and writes its
// own lease unsigned, so it vouches for nothing, until the takeover is confirmed.
func TestLeaseTakenOverFromAnUntrustedOneStaysUnsigned(t *testing.T) {
	ctx := context.Background()
	key, clock, st := testMACKey(t), newFakeClock(), newMemStore()
	forged := Lease{Cluster: "prod", Epoch: 7, Seq: 1, LastSnapshotEpoch: 1}
	data, err := forged.encode()
	if err != nil {
		t.Fatal(err)
	}
	st.data[LeaseKey("prod")] = data // released, no MAC
	m := newMACManager(t, st, clock, "b", key)
	m.Step(ctx)
	if m.Role() != RoleActive {
		t.Fatal("the released lease was not acquired")
	}
	pred := m.Predecessor()
	if pred.Trust != LeaseUntrusted || pred.Reason == "" || !pred.Mark.IsZero() || !pred.Legacy {
		t.Fatalf("predecessor %+v; want untrusted and legacy (no MAC), with no mark", pred)
	}
	if l := st.lease(t); l.MAC != "" || l.Authentic(key) {
		t.Fatalf("the lease taken over from an untrusted one is signed: %+v", l)
	}
	mark := SnapshotMark{Epoch: 3, At: clock.Now()}
	if err = m.Confirm(ctx, mark); err != nil {
		t.Fatal(err)
	}
	if l := st.lease(t); !l.Authentic(key) || l.SnapshotMark() != mark {
		t.Fatalf("lease after Confirm %+v; want signed, naming %s", l, mark)
	}
	newer := SnapshotMark{Epoch: m.Epoch(), At: clock.Now().Add(time.Minute)}
	m.RecordSnapshot(ctx, newer)
	m.RecordSnapshot(ctx, mark) // older: ignored
	if l := st.lease(t); !l.Authentic(key) || !l.SnapshotMark().At.Equal(newer.At) || l.LastSnapshotEpoch != newer.Epoch {
		t.Fatalf("lease after RecordSnapshot %+v; want signed, naming %s", l, newer)
	}
}

// TestAuthenticLeaseCarriesItsLastSnapshot covers a takeover from a signed lease:
// the new lease is signed at once and names the same last snapshot, and a replayed
// older lease cannot lower what this instance saw.
func TestAuthenticLeaseCarriesItsLastSnapshot(t *testing.T) {
	ctx := context.Background()
	key, clock, st := testMACKey(t), newFakeClock(), newMemStore()
	a := newMACManager(t, st, clock, "a", key)
	a.Step(ctx)
	if err := a.Confirm(ctx, SnapshotMark{}); err != nil {
		t.Fatal(err)
	}
	old := SnapshotMark{Epoch: a.Epoch(), At: clock.Now()}
	a.RecordSnapshot(ctx, old)
	oldLease := st.data[LeaseKey("prod")]
	b := newMACManager(t, st, clock, "b", key)
	authenticSeen := 0
	b.cfg.OnAuthenticLease = func() { authenticSeen++ }
	b.Step(ctx) // sees the lease naming old
	if authenticSeen == 0 {
		t.Fatal("reading a signed lease was not reported")
	}
	clock.Advance(5 * time.Second)
	a.Step(ctx) // renews
	newest := SnapshotMark{Epoch: a.Epoch(), At: clock.Now()}
	a.RecordSnapshot(ctx, newest)
	b.Step(ctx) // sees the lease naming newest
	if err := a.Release(ctx); err != nil {
		t.Fatal(err)
	}
	// Someone puts the older genuine lease back, released.
	replayed, err := DecodeLease(oldLease)
	if err != nil {
		t.Fatal(err)
	}
	replayed.Holder = ""
	replayed = replayed.sign(key) // as a genuine release of that time would be
	data, err := replayed.encode()
	if err != nil {
		t.Fatal(err)
	}
	st.data[LeaseKey("prod")] = data
	// A released lease is taken over at once.
	b.Step(ctx)
	if b.Role() != RoleActive {
		t.Fatal("b did not take over")
	}
	pred := b.Predecessor()
	if pred.Trust != LeaseAuthentic || pred.Mark != newest {
		t.Fatalf("predecessor %+v; want authentic, naming the newest snapshot b saw (%s)", pred, newest)
	}
	if l := st.lease(t); !l.Authentic(key) || l.SnapshotMark() != newest {
		t.Fatalf("b's lease %+v; want signed at once, naming %s", l, newest)
	}
}

func TestOverride(t *testing.T) {
	dir := t.TempDir()
	key := SnapshotKey("prod", time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC), 3)
	for name, o := range map[string]Override{
		"no choice":     {Cluster: "prod"},
		"both":          {Cluster: "prod", Snapshot: key, KeepLocal: true},
		"other cluster": {Cluster: "staging", Snapshot: key},
		"not a key":     {Cluster: "prod", Snapshot: "x.db.age"},
		"bad cluster":   {Cluster: "Prod!", KeepLocal: true},
	} {
		if err := WriteOverride(dir, o); !errors.Is(err, ErrInvalidOverride) {
			t.Errorf("%s: WriteOverride = %v; want ErrInvalidOverride", name, err)
		}
	}
	if o, err := TakeOverride(dir); o != nil || err != nil {
		t.Fatalf("TakeOverride without a request = %v, %v", o, err)
	}
	want := Override{Cluster: "prod", Snapshot: key, RequestedBy: "alice", RequestedAt: time.Now().UTC().Round(0)}
	if err := WriteOverride(dir, want); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dir, OverrideFileName)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("override file %v, %v; want mode 0600", info, err)
	}
	got, err := TakeOverride(dir)
	if err != nil || got == nil || got.Snapshot != key || got.RequestedBy != "alice" || !got.RequestedAt.Equal(want.RequestedAt) {
		t.Fatalf("TakeOverride = %+v, %v", got, err)
	}
	if again, againErr := TakeOverride(dir); again != nil || againErr != nil {
		t.Fatalf("an override was used twice: %v, %v", again, againErr)
	}
	// An unreadable request is removed and reported.
	if err = os.WriteFile(filepath.Join(dir, OverrideFileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = TakeOverride(dir); !errors.Is(err, ErrInvalidOverride) {
		t.Fatalf("TakeOverride of a broken file = %v", err)
	}
	if _, err = os.Stat(filepath.Join(dir, OverrideFileName)); !os.IsNotExist(err) {
		t.Fatal("the broken request was kept")
	}
}

func TestStateHighWater(t *testing.T) {
	var st State
	if st.Seen("prod") {
		t.Fatal("a fresh state has seen the cluster")
	}
	at := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	st.Observe("prod", SnapshotMark{Epoch: 3, At: at}, false)
	st.Observe("prod", SnapshotMark{Epoch: 2, At: at.Add(time.Hour)}, false)
	if !st.Seen("prod") || st.HighWater("prod") != (SnapshotMark{Epoch: 3, At: at}) {
		t.Fatalf("high-water %s; want epoch 3", st.HighWater("prod"))
	}
	st.Observe("prod", SnapshotMark{Epoch: 2, At: at}, true)
	if st.HighWater("prod").Epoch != 2 {
		t.Fatal("a forced takeover did not reset the high-water mark")
	}
	legacy := State{Cluster: "old", LastActiveEpoch: 4}
	if !legacy.Seen("old") || legacy.Seen("prod") {
		t.Fatal("an epoch held counts as seen, for that cluster only")
	}
}
