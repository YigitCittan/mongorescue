package metabackup_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/metabackup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// sqliteHeader starts every SQLite database file.
const sqliteHeader = "SQLite format 3\x00"

// fakeStorage is an in-memory storage that can fail uploads.
type fakeStorage struct {
	*storage.MockStorage
	failSave error
}

func (f *fakeStorage) Save(ctx context.Context, key string, r io.Reader) (*models.StorageObject, error) {
	if f.failSave != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(r, 10))
		return nil, f.failSave
	}
	return f.MockStorage.Save(ctx, key, r)
}

// fakeTargets serves one target.
type fakeTargets struct {
	target *models.StorageTarget
	driver storage.Storage
}

func (f *fakeTargets) Resolve(_ context.Context, id string) (*models.StorageTarget, error) {
	if id != "" && id != f.target.ID {
		return nil, errors.New("not found")
	}
	return f.target, nil
}

func (f *fakeTargets) Storage(context.Context, string) (storage.Storage, error) { return f.driver, nil }

// recorder collects published events and observed outcomes.
type recorder struct {
	mu       sync.Mutex
	events   []events.Event
	observed []bool
}

func (r *recorder) Publish(_ context.Context, e events.Event) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return true
}

func (r *recorder) observe(ok bool, _ int64, _ time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observed = append(r.observed, ok)
}

type fixture struct {
	svc     *metabackup.Service
	storage *fakeStorage
	rec     *recorder
	dataDir string
	cfg     settings.Settings
	enc     *encryption.Encryptor
	clock   time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureOn(t, &fakeStorage{MockStorage: storage.NewMockStorage()}, newInstallID(t))
}

// newInstallID derives the install ID of a fresh secret.key.
func newInstallID(t *testing.T) string {
	t.Helper()
	key, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	id, err := metabackup.InstallID(key)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// newFixtureOn returns an installation with installID writing to shared.
func newFixtureOn(t *testing.T, shared *fakeStorage, installID string) *fixture {
	t.Helper()
	f := &fixture{
		storage: shared,
		rec:     &recorder{},
		dataDir: t.TempDir(),
		cfg:     settings.Defaults(),
		clock:   time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC),
	}
	f.cfg.MetadataBackup.Enabled = true
	f.svc = metabackup.New(metabackup.Config{
		InstallID: installID,
		Store:     storetest.New(t),
		Targets:   &fakeTargets{target: &models.StorageTarget{ID: "stg_1", Name: "primary"}, driver: f.storage},
		DataDir:   f.dataDir,
		Settings:  func() settings.Settings { return f.cfg },
		Encryptor: func() *encryption.Encryptor { return f.enc },
		Publisher: f.rec,
		Observe:   f.rec.observe,
		Logger:    slog.New(slog.DiscardHandler),
		Now:       func() time.Time { return f.clock },
	})
	return f
}

// snapshotKeys lists the keys under this installation's prefix.
func (f *fixture) snapshotKeys(t *testing.T) []string {
	t.Helper()
	objs, err := f.storage.List(context.Background(), f.svc.Prefix())
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, o := range objs {
		keys = append(keys, o.Key)
	}
	return keys
}

// assertNoTemp fails when a temporary snapshot is left in the data directory.
func (f *fixture) assertNoTemp(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("temporary file left in the data directory: %s", e.Name())
	}
}

func read(t *testing.T, s storage.Storage, key string) []byte {
	t.Helper()
	rc, err := s.Retrieve(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRunStoresEncryptedSnapshot checks that a snapshot is encrypted with the backup
// encryption keys, decrypts to a SQLite database, is recorded in the status and
// leaves no temporary file behind.
func TestRunStoresEncryptedSnapshot(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	identity, recipient, err := encryption.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	if f.enc, err = encryption.NewX25519Encryptor([]string{recipient}); err != nil {
		t.Fatal(err)
	}
	snap, err := f.svc.Run(ctx, metabackup.TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(snap.Key, f.svc.Prefix()) || !strings.HasPrefix(f.svc.Prefix(), metabackup.Prefix+snap.InstallID+"/") ||
		!strings.HasSuffix(snap.Key, ".db.age") || !snap.Encrypted || snap.TargetID != "stg_1" {
		t.Fatalf("snapshot = %+v", snap)
	}
	dec, err := encryption.NewDecryptor(encryption.DecryptorConfig{Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := dec.Decrypt(bytes.NewReader(read(t, f.storage, snap.Key)))
	if err != nil {
		t.Fatal(err)
	}
	db, err := io.ReadAll(plain)
	if err != nil || !bytes.HasPrefix(db, []byte(sqliteHeader)) {
		t.Fatalf("decrypted snapshot is not a SQLite database (%v)", err)
	}
	st := f.svc.Status(ctx)
	if st.Last == nil || st.Last.Key != snap.Key || st.LastError != "" || st.LastRunAt == nil || st.Running {
		t.Fatalf("status = %+v", st)
	}
	if latest := f.svc.Latest(ctx); latest == nil || latest.Key != snap.Key {
		t.Fatalf("Latest = %+v", latest)
	}
	f.assertNoTemp(t)
	if len(f.rec.observed) != 1 || !f.rec.observed[0] || len(f.rec.events) != 0 {
		t.Fatalf("observed %v, events %v", f.rec.observed, f.rec.events)
	}
}

// TestRunWithoutEncryptionUploadsPlainSnapshot checks that snapshots are uploaded
// when encryption is off.
func TestRunWithoutEncryptionUploadsPlainSnapshot(t *testing.T) {
	f := newFixture(t)
	snap, err := f.svc.Run(context.Background(), metabackup.TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Encrypted || !strings.HasSuffix(snap.Key, ".db") || !bytes.HasPrefix(read(t, f.storage, snap.Key), []byte(sqliteHeader)) {
		t.Fatalf("snapshot = %+v", snap)
	}
	f.assertNoTemp(t)
}

// TestRetentionKeepsTheNewestSnapshots checks that only the newest snapshots are
// kept and that other objects under the prefix are never deleted.
func TestRetentionKeepsTheNewestSnapshots(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.cfg.MetadataBackup.RetentionCount = 2
	for _, k := range []string{f.svc.Prefix() + "notes.txt", "shop/2026/10/bkp_1.archive.gz"} {
		if _, err := f.storage.MockStorage.Save(ctx, k, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	var keys []string
	for i := range 4 {
		// A week and a day apart: older snapshots are past the delete grace period.
		f.clock = f.clock.Add(time.Duration(i+1) * 8 * 24 * time.Hour)
		snap, err := f.svc.Run(ctx, metabackup.TriggerScheduled)
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, snap.Key)
	}
	got := f.snapshotKeys(t)
	want := map[string]bool{keys[2]: true, keys[3]: true, f.svc.Prefix() + "notes.txt": true}
	if len(got) != len(want) {
		t.Fatalf("kept %v, want %v", got, want)
	}
	for _, k := range got {
		if !want[k] {
			t.Fatalf("kept %v, want %v", got, want)
		}
	}
	if _, err := f.storage.Stat(ctx, "shop/2026/10/bkp_1.archive.gz"); err != nil {
		t.Fatal("retention must not touch backup archives")
	}
}

// TestRetentionKeepsSnapshotsWithinTheGracePeriod checks that retention never deletes
// a metadata snapshot younger than the delete grace period, whatever the count.
func TestRetentionKeepsSnapshotsWithinTheGracePeriod(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.cfg.MetadataBackup.RetentionCount = 1
	for range 3 {
		f.clock = f.clock.Add(24 * time.Hour)
		if _, err := f.svc.Run(ctx, metabackup.TriggerScheduled); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.snapshotKeys(t); len(got) != 3 {
		t.Fatalf("kept %v; want all three (each younger than the 7-day grace period)", got)
	}
	f.clock = f.clock.Add(8 * 24 * time.Hour)
	if _, err := f.svc.Run(ctx, metabackup.TriggerScheduled); err != nil {
		t.Fatal(err)
	}
	if got := f.snapshotKeys(t); len(got) != 1 {
		t.Fatalf("kept %v; want only the newest once the others are past the grace period", got)
	}
}

// TestInstallsSharingAStorageKeepTheirSnapshots checks that two installations
// writing to the same bucket and prefix use their own sub-prefix and that
// retention of one never deletes the other's snapshots.
func TestInstallsSharingAStorageKeepTheirSnapshots(t *testing.T) {
	ctx := context.Background()
	shared := &fakeStorage{MockStorage: storage.NewMockStorage()}
	a := newFixtureOn(t, shared, newInstallID(t))
	b := newFixtureOn(t, shared, newInstallID(t))
	if a.svc.Prefix() == b.svc.Prefix() {
		t.Fatal("installations with different keys must not share a prefix")
	}
	a.cfg.MetadataBackup.RetentionCount, b.cfg.MetadataBackup.RetentionCount = 1, 1
	for i := range 3 {
		for _, f := range []*fixture{a, b} {
			f.clock = f.clock.Add(time.Duration(i+1) * 8 * 24 * time.Hour)
			if _, err := f.svc.Run(ctx, metabackup.TriggerScheduled); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, f := range []*fixture{a, b} {
		keys := f.snapshotKeys(t)
		if last := f.svc.Latest(ctx); len(keys) != 1 || last == nil || keys[0] != last.Key {
			t.Fatalf("%s keeps %v, want only its latest snapshot", f.svc.Prefix(), keys)
		}
	}
	all, err := shared.List(ctx, metabackup.Prefix)
	if err != nil || len(all) != 2 {
		t.Fatalf("shared storage holds %d snapshots (%v), want one per installation", len(all), err)
	}
}

// TestInstallIDIsStable checks that the install ID follows secret.key.
func TestInstallIDIsStable(t *testing.T) {
	key, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	first, err := metabackup.InstallID(key)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := metabackup.InstallID(key); again != first || len(first) != 16 || strings.Contains(first, secretbox.EncodeKey(key)) {
		t.Fatalf("install ID %q is not stable", first)
	}
	if _, err = metabackup.InstallID([]byte("short")); err == nil {
		t.Fatal("an invalid key must be refused")
	}
}

// TestFailedSnapshotIsReported checks the failure path: the error is recorded, an
// event and a failed outcome are published, nothing is stored and the temporary
// file is removed.
func TestFailedSnapshotIsReported(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.storage.failSave = errors.New("bucket unreachable")
	if _, err := f.svc.Run(ctx, metabackup.TriggerScheduled); err == nil {
		t.Fatal("Run succeeded although the upload failed")
	}
	st := f.svc.Status(ctx)
	if !strings.Contains(st.LastError, "bucket unreachable") || st.Last != nil {
		t.Fatalf("status = %+v", st)
	}
	if len(f.rec.events) != 1 || f.rec.events[0].Type != events.MetadataBackupFailed || f.rec.events[0].TargetID != "stg_1" {
		t.Fatalf("events = %+v", f.rec.events)
	}
	if len(f.rec.observed) != 1 || f.rec.observed[0] {
		t.Fatalf("observed = %v", f.rec.observed)
	}
	if keys := f.snapshotKeys(t); len(keys) != 0 {
		t.Fatalf("stored %v", keys)
	}
	f.assertNoTemp(t)
	// A failure is retried after an hour, not after the full interval.
	if st.NextRunAt == nil || !st.NextRunAt.Equal(f.clock.Add(time.Hour)) {
		t.Fatalf("next run = %v", st.NextRunAt)
	}
}

// TestRunDueFollowsTheSchedule checks that scheduled snapshots only run when on and
// due.
func TestRunDueFollowsTheSchedule(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.cfg.MetadataBackup.Enabled = false
	f.svc.RunDue(ctx)
	if len(f.snapshotKeys(t)) != 0 || f.svc.Status(ctx).NextRunAt != nil {
		t.Fatal("a disabled schedule must not take snapshots")
	}
	f.cfg.MetadataBackup.Enabled = true
	f.svc.RunDue(ctx)
	if n := len(f.snapshotKeys(t)); n != 1 {
		t.Fatalf("first due run stored %d snapshots", n)
	}
	f.clock = f.clock.Add(time.Hour)
	f.svc.RunDue(ctx)
	if n := len(f.snapshotKeys(t)); n != 1 {
		t.Fatal("a snapshot ran before the interval passed")
	}
	f.clock = f.clock.Add(24 * time.Hour)
	f.svc.RunDue(ctx)
	if n := len(f.snapshotKeys(t)); n != 2 {
		t.Fatalf("due run stored %d snapshots, want 2", n)
	}
}

// TestTriggerNeedsARunningService checks Trigger's lifecycle and that Start removes
// stale temporary snapshots.
func TestTriggerNeedsARunningService(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.Trigger(); !errors.Is(err, metabackup.ErrUnavailable) {
		t.Fatalf("Trigger before Start = %v", err)
	}
	stale := filepath.Join(f.dataDir, ".metabackup-123")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	f.svc.Start(context.Background())
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("Start must remove stale temporary snapshots")
	}
	if err := f.svc.Trigger(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); f.svc.Latest(context.Background()) == nil; {
		if time.Now().After(deadline) {
			t.Fatal("the triggered snapshot did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.svc.Stop()
	if err := f.svc.Trigger(); !errors.Is(err, metabackup.ErrUnavailable) {
		t.Fatalf("Trigger after Stop = %v", err)
	}
	if n := len(f.snapshotKeys(t)); n != 1 {
		t.Fatalf("triggered snapshot stored %d objects", n)
	}
	f.assertNoTemp(t)
}
