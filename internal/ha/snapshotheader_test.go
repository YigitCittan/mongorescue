package ha

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// snapshotFixture is a local HA target with a snapshotter of cluster prod.
type snapshotFixture struct {
	ctx   context.Context
	st    *storage.LocalStorage
	clock *fakeClock
	fence *staticFence
	db    *fileVacuum
	pass  string
	snap  *Snapshotter
}

func newSnapshotFixture(t *testing.T) *snapshotFixture {
	t.Helper()
	st, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &snapshotFixture{ctx: context.Background(), st: st, clock: newFakeClock(), fence: &staticFence{epoch: 1},
		db: &fileVacuum{content: "db"}, pass: testPassphrase(t)}
	f.snap = newTestSnapshotter(t, st, f.fence, f.db, f.pass, f.clock)
	return f
}

// write takes a snapshot of content under epoch, a minute after the previous one.
func (f *snapshotFixture) write(t *testing.T, epoch uint64, content string) SnapshotInfo {
	t.Helper()
	f.clock.Advance(time.Minute)
	f.fence.epoch, f.db.content = epoch, content
	info, err := f.snap.Snapshot(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// copyObject copies the stored object from to the key to, as someone with write
// access to the HA target (but not secret.key) can.
func (f *snapshotFixture) copyObject(t *testing.T, from, to string) {
	t.Helper()
	rc, err := f.st.Retrieve(f.ctx, from)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err = f.st.Save(f.ctx, to, rc); err != nil {
		t.Fatal(err)
	}
}

// writeLegacy stores a snapshot of content in the format written before the
// header existed: the encrypted database alone.
func (f *snapshotFixture) writeLegacy(t *testing.T, at time.Time, epoch uint64, content string) SnapshotInfo {
	t.Helper()
	enc, err := encryption.NewScryptEncryptor(f.pass, snapshotWorkFactor)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := enc.Encrypt(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(w, content); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	key := SnapshotKey("prod", at, epoch)
	if _, err = f.st.Save(f.ctx, key, &buf); err != nil {
		t.Fatal(err)
	}
	return SnapshotInfo{Key: key, CreatedAt: at.UTC().Truncate(time.Millisecond), Epoch: epoch}
}

// fetch restores snapshot key and returns its content.
func (f *snapshotFixture) fetch(t *testing.T, key string, allowLegacy bool) (string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "restored.db")
	if _, err := FetchSnapshot(f.ctx, f.st, "prod", key, f.pass, path, allowLegacy); err != nil {
		if _, statErr := os.Stat(path); statErr == nil {
			t.Fatalf("a refused snapshot left %s behind", path)
		}
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data), nil
}

// TestReplayedSnapshotUnderAHigherEpochIsRefused copies an old snapshot to a key
// with a higher epoch: its authenticated header still says the old epoch, so it is
// refused, never restored, and the newest genuine snapshot wins; the epoch it
// proves is the genuine one.
func TestReplayedSnapshotUnderAHigherEpochIsRefused(t *testing.T) {
	f := newSnapshotFixture(t)
	old := f.write(t, 3, "before the API key was revoked")
	current := f.write(t, 5, "current")
	forged := SnapshotKey("prod", f.clock.Now().Add(time.Hour), 99)
	f.copyObject(t, old.Key, forged)
	// Renamed within the same epoch, to a later time: refused too.
	sameEpoch := SnapshotKey("prod", f.clock.Now().Add(time.Hour), 5)
	f.copyObject(t, old.Key, sameEpoch)

	sel, err := SelectSnapshots(f.ctx, f.st, "prod", f.pass, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Refused) != 2 || sel.Refused[0].Key != forged || sel.Refused[1].Key != sameEpoch {
		t.Fatalf("refused %+v; want the two copies", sel.Refused)
	}
	for _, c := range sel.Refused {
		if !errors.Is(c.Err, ErrSnapshotRejected) || c.Header == nil || c.Header.Epoch != 3 {
			t.Fatalf("copy %s: %v (header %+v); want ErrSnapshotRejected with the authentic epoch 3", c.Key, c.Err, c.Header)
		}
	}
	candidates, legacy := sel.Candidates()
	if legacy || len(candidates) != 2 || candidates[0].Key != current.Key || candidates[1].Key != old.Key {
		t.Fatalf("candidates %+v (legacy %v); want the genuine snapshots, newest first", candidates, legacy)
	}
	if epoch, ok := sel.AuthenticEpoch("prod"); !ok || epoch != 5 {
		t.Fatalf("authentic epoch %d, %v; want 5, not the forged 99", epoch, ok)
	}
	for _, key := range []string{forged, sameEpoch} {
		if _, err = f.fetch(t, key, true); !errors.Is(err, ErrSnapshotRejected) {
			t.Fatalf("FetchSnapshot of a copy = %v; want ErrSnapshotRejected", err)
		}
	}
	if got, fetchErr := f.fetch(t, current.Key, false); fetchErr != nil || got != "current" {
		t.Fatalf("FetchSnapshot of the genuine snapshot = %q, %v", got, fetchErr)
	}
}

// TestSnapshotOfAnotherClusterIsRefused copies a snapshot of a cluster that shares
// secret.key into this cluster's prefix under the same name.
func TestSnapshotOfAnotherClusterIsRefused(t *testing.T) {
	f := newSnapshotFixture(t)
	other := newTestSnapshotter(t, f.st, &staticFence{epoch: 7}, &fileVacuum{content: "other cluster"}, f.pass, f.clock)
	other.cfg.Cluster = "staging"
	info, err := other.Snapshot(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	copied := SnapshotPrefix("prod") + strings.TrimPrefix(info.Key, SnapshotPrefix("staging"))
	f.copyObject(t, info.Key, copied)

	sel, err := SelectSnapshots(f.ctx, f.st, "prod", f.pass, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Refused) != 1 || !errors.Is(sel.Refused[0].Err, ErrSnapshotRejected) || sel.Refused[0].Header.Cluster != "staging" {
		t.Fatalf("refused %+v; want the copy from staging", sel.Refused)
	}
	if candidates, _ := sel.Candidates(); len(candidates) != 0 {
		t.Fatalf("candidates %+v; want none", candidates)
	}
	if epoch, ok := sel.AuthenticEpoch("prod"); !ok || epoch != 0 {
		t.Fatalf("authentic epoch %d, %v; another cluster's epoch must not count", epoch, ok)
	}
	if _, err = f.fetch(t, copied, true); !errors.Is(err, ErrSnapshotRejected) {
		t.Fatalf("FetchSnapshot of the copy = %v; want ErrSnapshotRejected", err)
	}
}

// TestTamperedSnapshotHeaderFailsAuthentication flips a byte of the encrypted
// header: age's authentication fails, so the snapshot does not decrypt.
func TestTamperedSnapshotHeaderFailsAuthentication(t *testing.T) {
	f := newSnapshotFixture(t)
	info := f.write(t, 2, "db")
	rc, err := f.st.Retrieve(f.ctx, info.Key)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	// The age header ends with the "---" MAC line; a 16-byte nonce follows, then
	// the first payload chunk, which begins with the snapshot header.
	mac := bytes.Index(data, []byte("\n--- "))
	end := bytes.IndexByte(data[mac+1:], '\n')
	if mac < 0 || end < 0 {
		t.Fatal("not an age file")
	}
	payload := mac + 1 + end + 1 + 16
	data[payload+len(snapshotMagic)+5] ^= 0x01
	if _, err = f.st.Save(f.ctx, info.Key, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	c := CheckSnapshot(f.ctx, f.st, "prod", f.pass, info)
	if !errors.Is(c.Err, ErrSnapshotUnreadable) || !c.Refused() || c.Header != nil {
		t.Fatalf("CheckSnapshot of a tampered snapshot = %v (header %+v); want ErrSnapshotUnreadable", c.Err, c.Header)
	}
	if _, err = f.fetch(t, info.Key, true); !errors.Is(err, ErrSnapshotUnreadable) {
		t.Fatalf("FetchSnapshot of a tampered snapshot = %v; want ErrSnapshotUnreadable", err)
	}
}

// TestLegacySnapshotsAreRestoredOnlyWithoutAHeader covers the upgrade: snapshots
// written before the header existed are restored while the cluster has no
// snapshot with a header, and never once one exists, whatever their epochs.
func TestLegacySnapshotsAreRestoredOnlyWithoutAHeader(t *testing.T) {
	f := newSnapshotFixture(t)
	base := f.clock.Now()
	f.writeLegacy(t, base, 3, "legacy old")
	newest := f.writeLegacy(t, base.Add(time.Minute), 4, "legacy new")

	sel, err := SelectSnapshots(f.ctx, f.st, "prod", f.pass, 3)
	if err != nil {
		t.Fatal(err)
	}
	candidates, legacy := sel.Candidates()
	if !legacy || len(candidates) != 2 || candidates[0].Key != newest.Key {
		t.Fatalf("candidates %+v (legacy %v); want the legacy snapshots, newest first", candidates, legacy)
	}
	if epoch, ok := sel.AuthenticEpoch("prod"); !ok || epoch != 4 {
		t.Fatalf("epoch %d, %v; want the legacy snapshots' 4", epoch, ok)
	}
	if _, err = f.fetch(t, newest.Key, false); !errors.Is(err, ErrSnapshotLegacy) {
		t.Fatalf("FetchSnapshot of a legacy snapshot without allowLegacy = %v; want ErrSnapshotLegacy", err)
	}
	if got, fetchErr := f.fetch(t, newest.Key, true); fetchErr != nil || got != "legacy new" {
		t.Fatalf("FetchSnapshot of a legacy snapshot = %q, %v", got, fetchErr)
	}

	// Snapshots with a header are written on another target and copied in, so no
	// retention runs on this one.
	elsewhere := func(epoch uint64) SnapshotInfo {
		t.Helper()
		st, stErr := storage.NewLocalStorage(t.TempDir())
		if stErr != nil {
			t.Fatal(stErr)
		}
		f.clock.Advance(time.Minute)
		info, stErr := newTestSnapshotter(t, st, &staticFence{epoch: epoch}, &fileVacuum{content: "with header"}, f.pass, f.clock).Snapshot(f.ctx)
		if stErr != nil {
			t.Fatal(stErr)
		}
		rc, stErr := st.Retrieve(f.ctx, info.Key)
		if stErr != nil {
			t.Fatal(stErr)
		}
		defer rc.Close()
		if _, stErr = f.st.Save(f.ctx, info.Key, rc); stErr != nil {
			t.Fatal(stErr)
		}
		return info
	}

	// A renamed snapshot of the cluster proves it writes headers: legacy snapshots
	// are no longer restored.
	f.clock.Advance(2 * time.Hour)
	genuine := elsewhere(2)
	f.copyObject(t, genuine.Key, SnapshotKey("prod", f.clock.Now().Add(time.Hour), 9))
	if err = f.st.Delete(f.ctx, genuine.Key); err != nil {
		t.Fatal(err)
	}
	sel, err = SelectSnapshots(f.ctx, f.st, "prod", f.pass, 3)
	if err != nil {
		t.Fatal(err)
	}
	if candidates, _ = sel.Candidates(); len(candidates) != 0 {
		t.Fatalf("candidates %+v; legacy snapshots must not be restored once the cluster wrote a header", candidates)
	}

	// A verified snapshot wins over legacy ones, even of a lower epoch.
	genuine = elsewhere(2)
	sel, err = SelectSnapshots(f.ctx, f.st, "prod", f.pass, 3)
	if err != nil {
		t.Fatal(err)
	}
	if candidates, legacy = sel.Candidates(); legacy || len(candidates) != 1 || candidates[0].Key != genuine.Key {
		t.Fatalf("candidates %+v (legacy %v); want the verified snapshot only", candidates, legacy)
	}
}

// TestPruneIgnoresForgedKeys covers retention with copies planted under the
// current epoch and later times, and under a higher epoch: they never displace a
// genuine snapshot, are kept for investigation and reported once; legacy
// snapshots go once a verified one exists.
func TestPruneIgnoresForgedKeys(t *testing.T) {
	f := newSnapshotFixture(t)
	var rejected []string
	f.snap.cfg.Checker = NewSnapshotChecker(f.st, "prod", f.pass, func(c SnapshotCheck) { rejected = append(rejected, c.Key) })
	legacy := f.writeLegacy(t, f.clock.Now().Add(-time.Hour), 1, "legacy")
	old := f.write(t, 5, "old")
	var forged []string
	for i := range 5 {
		key := SnapshotKey("prod", f.clock.Now().Add(time.Duration(i+1)*time.Hour), 5)
		f.copyObject(t, old.Key, key)
		forged = append(forged, key)
	}
	high := SnapshotKey("prod", f.clock.Now(), 99)
	f.copyObject(t, old.Key, high)
	forged = append(forged, high)

	var genuine []SnapshotInfo
	for range 4 {
		genuine = append(genuine, f.write(t, 5, "current"))
	}
	list, err := ListSnapshots(f.ctx, f.st, "prod")
	if err != nil {
		t.Fatal(err)
	}
	stored := map[string]bool{}
	for _, sn := range list {
		stored[sn.Key] = true
	}
	for _, sn := range genuine[1:] {
		if !stored[sn.Key] {
			t.Errorf("a newest genuine snapshot was deleted: %s", sn.Key)
		}
	}
	for _, key := range forged {
		if !stored[key] {
			t.Errorf("a forged snapshot was deleted: %s", key)
		}
	}
	if stored[old.Key] || stored[genuine[0].Key] || stored[legacy.Key] {
		t.Error("retention kept a genuine snapshot beyond the three newest, or a legacy one")
	}
	if len(list) != 3+len(forged) {
		t.Fatalf("%d snapshots stored; want the three newest genuine ones and the forged ones", len(list))
	}
	if len(rejected) != len(forged) {
		t.Fatalf("%d refusals reported; want each forged snapshot once: %v", len(rejected), rejected)
	}
}

// TestSelectionReadsTheWholeListing checks that planted objects sorting first do
// not hide the snapshots below them: the selection reads past them.
func TestSelectionReadsTheWholeListing(t *testing.T) {
	var list []SnapshotInfo
	for i := range 200 {
		list = append(list, SnapshotInfo{Key: fmt.Sprintf("refused-%d", i)})
	}
	list = append(list, SnapshotInfo{Key: "legacy"})
	checks := 0
	sel, err := selectSnapshots(context.Background(), "prod", list, 1, func(info SnapshotInfo) SnapshotCheck {
		checks++
		if info.Key == "legacy" {
			return SnapshotCheck{SnapshotInfo: info, Err: ErrSnapshotLegacy}
		}
		return SnapshotCheck{SnapshotInfo: info, Err: ErrSnapshotUnreadable}
	})
	if err != nil {
		t.Fatal(err)
	}
	if checks != len(list) || !sel.Complete || len(sel.Refused) != 200 {
		t.Fatalf("%d checks, complete %v, %d refused; want the whole listing", checks, sel.Complete, len(sel.Refused))
	}
	if candidates, legacy := sel.Candidates(); !legacy || len(candidates) != 1 {
		t.Fatalf("candidates %+v; want the legacy snapshot", candidates)
	}
	// A storage error leaves the selection incomplete.
	sel, _ = selectSnapshots(context.Background(), "prod", list[len(list)-2:], 1, func(info SnapshotInfo) SnapshotCheck {
		if info.Key == "legacy" {
			return SnapshotCheck{SnapshotInfo: info, Err: ErrSnapshotLegacy}
		}
		return SnapshotCheck{SnapshotInfo: info, Err: errors.New("network")}
	})
	if candidates, _ := sel.Candidates(); len(candidates) != 0 || sel.Complete {
		t.Fatalf("candidates %+v after a storage error; want none", candidates)
	}
}

func TestParseSnapshotHeader(t *testing.T) {
	at := time.Date(2026, 10, 12, 10, 0, 0, 123_000_000, time.UTC)
	h := SnapshotHeader{Format: SnapshotFormat, Cluster: "prod", Epoch: 7, InstanceID: "i", CreatedAt: at}
	enc, err := h.encode()
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReaderSize(bytes.NewReader(append(enc, "SQLite format 3\x00rest"...)), 2*maxSnapshotHeader)
	got, err := parseSnapshotHeader(r)
	if err != nil || *got != h {
		t.Fatalf("parse = %+v, %v", got, err)
	}
	if rest, _ := io.ReadAll(r); string(rest) != "SQLite format 3\x00rest" {
		t.Fatalf("the database starts with %q", rest)
	}
	if err = got.verify("prod", SnapshotKey("prod", at, 7)); err != nil {
		t.Fatal(err)
	}
	for name, key := range map[string]string{
		"epoch":   SnapshotKey("prod", at, 8),
		"time":    SnapshotKey("prod", at.Add(time.Millisecond), 7),
		"cluster": SnapshotKey("staging", at, 7),
		"name":    SnapshotPrefix("prod") + "x.db.age",
	} {
		if err = got.verify("prod", key); !errors.Is(err, ErrSnapshotRejected) {
			t.Errorf("%s: verify = %v; want ErrSnapshotRejected", name, err)
		}
	}
	for name, in := range map[string]string{
		"legacy": "SQLite format 3\x00",
		"empty":  "",
		"short":  "mongo",
	} {
		r = bufio.NewReaderSize(strings.NewReader(in), 2*maxSnapshotHeader)
		if _, err = parseSnapshotHeader(r); !errors.Is(err, ErrSnapshotLegacy) {
			t.Errorf("%s: %v; want ErrSnapshotLegacy", name, err)
		}
	}
	for name, in := range map[string]string{
		"truncated": snapshotMagic + `{"format":1`,
		"malformed": snapshotMagic + "{]\n",
		"format 0":  snapshotMagic + `{"format":0}` + "\n",
		"long":      snapshotMagic + `{"cluster":"` + strings.Repeat("a", maxSnapshotHeader) + `"}` + "\n",
	} {
		r = bufio.NewReaderSize(strings.NewReader(in), 2*maxSnapshotHeader)
		if _, err = parseSnapshotHeader(r); !errors.Is(err, ErrSnapshotRejected) {
			t.Errorf("%s: %v; want ErrSnapshotRejected", name, err)
		}
	}
	r = bufio.NewReaderSize(strings.NewReader(snapshotMagic+`{"format":2}`+"\n"), 2*maxSnapshotHeader)
	if _, err = parseSnapshotHeader(r); !errors.Is(err, ErrSnapshotFormat) {
		t.Errorf("newer format: %v; want ErrSnapshotFormat", err)
	}
}

func FuzzParseSnapshotHeader(f *testing.F) {
	h := SnapshotHeader{Format: SnapshotFormat, Cluster: "prod", Epoch: 7, InstanceID: "i",
		CreatedAt: time.Date(2026, 10, 12, 10, 0, 0, 123_000_000, time.UTC)}
	enc, err := h.encode()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(append(enc, "SQLite"...))
	f.Add([]byte("SQLite format 3\x00"))
	f.Add([]byte(snapshotMagic + "{}\n"))
	f.Add([]byte(snapshotMagic + `{"format":1,"epoch":-1}` + "\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := parseSnapshotHeader(bufio.NewReaderSize(bytes.NewReader(data), 2*maxSnapshotHeader))
		if err != nil {
			if got != nil {
				t.Fatal("a header with an error")
			}
			return
		}
		again, err := got.encode()
		if err != nil {
			return // a time JSON cannot encode
		}
		back, err := parseSnapshotHeader(bufio.NewReaderSize(bytes.NewReader(again), 2*maxSnapshotHeader))
		if err != nil || back.Format != got.Format || back.Cluster != got.Cluster || back.Epoch != got.Epoch ||
			back.InstanceID != got.InstanceID || !back.CreatedAt.Equal(got.CreatedAt) {
			t.Fatalf("round trip of %+v = %+v, %v", got, back, err)
		}
	})
}

// TestEpochFloorWaitsForAFailedNewerSnapshot checks that the epoch a selection
// proves is unknown while a snapshot sorting before the first verified one could
// not be checked: it may carry the highest epoch, so a lease must not be acquired
// below it (the acquisition is deferred, as after a listing error).
func TestEpochFloorWaitsForAFailedNewerSnapshot(t *testing.T) {
	failed := SnapshotInfo{Key: "failed", Epoch: 9}
	verified := SnapshotInfo{Key: "verified", Epoch: 5}
	check := func(info SnapshotInfo) SnapshotCheck {
		if info.Key == "failed" {
			return SnapshotCheck{SnapshotInfo: info, Err: errors.New("network")}
		}
		return SnapshotCheck{SnapshotInfo: info, Header: &SnapshotHeader{Format: SnapshotFormat, Cluster: "prod", Epoch: 5}}
	}
	sel, err := selectSnapshots(context.Background(), "prod", []SnapshotInfo{failed, verified}, 1, check)
	if err != nil {
		t.Fatal(err)
	}
	if epoch, ok := sel.AuthenticEpoch("prod"); ok {
		t.Fatalf("authentic epoch %d known although a newer snapshot could not be checked", epoch)
	}
	// A failure after the first verified snapshot does not matter: it sorts lower.
	sel, err = selectSnapshots(context.Background(), "prod", []SnapshotInfo{verified, {Key: "failed", Epoch: 2}}, 2, check)
	if err != nil {
		t.Fatal(err)
	}
	if epoch, ok := sel.AuthenticEpoch("prod"); !ok || epoch != 5 {
		t.Fatalf("authentic epoch %d, %v; want 5", epoch, ok)
	}
}

// storedKeys returns the keys of the snapshots of prod on f's target.
func (f *snapshotFixture) storedKeys(t *testing.T) map[string]bool {
	t.Helper()
	list, err := ListSnapshots(f.ctx, f.st, "prod")
	if err != nil {
		t.Fatal(err)
	}
	keys := make(map[string]bool, len(list))
	for _, sn := range list {
		keys[sn.Key] = true
	}
	return keys
}

// TestRetentionStaysBoundedWithManyRefusedSnapshots plants 100 copies under higher
// epochs, which sort before every genuine snapshot: retention still reads the
// whole listing, keeps the genuine snapshots bounded, keeps the copies, and
// downloads and reports each copy once.
func TestRetentionStaysBoundedWithManyRefusedSnapshots(t *testing.T) {
	f := newSnapshotFixture(t)
	var reported int
	f.snap.cfg.Checker = NewSnapshotChecker(f.st, "prod", f.pass, func(SnapshotCheck) { reported++ })
	old := f.write(t, 5, "old")
	var planted []string
	for i := range 100 {
		key := SnapshotKey("prod", f.clock.Now(), uint64(100+i))
		f.copyObject(t, old.Key, key)
		planted = append(planted, key)
	}
	var genuine []SnapshotInfo
	for range 10 {
		genuine = append(genuine, f.write(t, 5, "current"))
	}
	stored := f.storedKeys(t)
	if len(stored) != 100+3 {
		t.Fatalf("%d snapshots stored; want the 100 planted ones and the three newest genuine ones", len(stored))
	}
	for _, sn := range genuine[len(genuine)-3:] {
		if !stored[sn.Key] {
			t.Errorf("a newest genuine snapshot was deleted: %s", sn.Key)
		}
	}
	for _, key := range planted {
		if !stored[key] {
			t.Errorf("a planted snapshot was deleted: %s", key)
		}
	}
	if reported != 100 || f.snap.cfg.Checker.Refused() != 100 {
		t.Fatalf("%d refusals reported, %d refused listed; want each planted snapshot once", reported, f.snap.cfg.Checker.Refused())
	}
}

// TestPruneAroundASnapshotThatCannotBeChecked covers a snapshot of a newer format
// (a permanent failure) between genuine ones: it may be the newest, so nothing
// that sorts before it is deleted, while the older ones are pruned as usual.
func TestPruneAroundASnapshotThatCannotBeChecked(t *testing.T) {
	f := newSnapshotFixture(t)
	f.snap.cfg.Keep = 100
	var genuine []SnapshotInfo
	for range 6 {
		genuine = append(genuine, f.write(t, 5, "db"))
	}
	// A snapshot of a newer format, taken between the second and the third.
	at := genuine[1].CreatedAt.Add(time.Second)
	enc, err := encryption.NewScryptEncryptor(f.pass, snapshotWorkFactor)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := enc.Encrypt(&buf)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, snapshotMagic+`{"format":2,"cluster":"prod","epoch":5}`+"\n")
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	newer := SnapshotKey("prod", at, 5)
	if _, err = f.st.Save(f.ctx, newer, &buf); err != nil {
		t.Fatal(err)
	}

	f.snap.cfg.Keep = 3
	latest := f.write(t, 5, "db")
	stored := f.storedKeys(t)
	for _, sn := range append(genuine[2:], latest) {
		if !stored[sn.Key] {
			t.Errorf("a snapshot sorting before the unreadable one was deleted: %s", sn.Key)
		}
	}
	if !stored[newer] {
		t.Error("the snapshot of a newer format was deleted")
	}
	for _, sn := range genuine[:2] {
		if stored[sn.Key] {
			t.Errorf("an older snapshot was not pruned: %s", sn.Key)
		}
	}
}
