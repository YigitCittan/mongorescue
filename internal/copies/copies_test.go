package copies_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/copies"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

const key = "shop/shop_20260101.archive.gz"

var archive = bytes.Repeat([]byte("mongodump archive bytes "), 4096)

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// failingStorage fails every Save while fail is set.
type failingStorage struct {
	*storage.MockStorage
	mu   sync.Mutex
	fail bool
}

func (f *failingStorage) setFail(v bool) {
	f.mu.Lock()
	f.fail = v
	f.mu.Unlock()
}

func (f *failingStorage) Save(ctx context.Context, k string, r io.Reader) (*models.StorageObject, error) {
	f.mu.Lock()
	fail := f.fail
	f.mu.Unlock()
	if fail {
		return nil, errors.New("copy target unreachable")
	}
	return f.MockStorage.Save(ctx, k, r)
}

type recorder struct {
	mu  sync.Mutex
	got []events.Event
}

func (r *recorder) Publish(_ context.Context, e events.Event) bool {
	r.mu.Lock()
	r.got = append(r.got, e)
	r.mu.Unlock()
	return true
}

func (r *recorder) types() []events.EventType {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []events.EventType
	for _, e := range r.got {
		out = append(out, e.Type)
	}
	return out
}

func primary(t *testing.T, data []byte) *storage.MockStorage {
	t.Helper()
	p := storage.NewMockStorage()
	if _, err := p.Save(context.Background(), key, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	return p
}

func record(copyTargets ...string) *models.BackupRecord {
	rec := &models.BackupRecord{ID: "bkp_1", Database: "shop", Status: models.StatusCompleted, StorageTargetID: "primary",
		StorageKey: key, SHA256: sum(archive), SizeBytes: int64(len(archive)), StartedAt: time.Now().UTC()}
	var targets []models.CopyTarget
	for _, id := range copyTargets {
		targets = append(targets, models.CopyTarget{ID: id, Name: id + " name"})
	}
	rec.PlanCopies(targets, "")
	return rec
}

func TestCopyMatchesThePrimaryChecksum(t *testing.T) {
	ctx := context.Background()
	dst := storage.NewMockStorage()
	if _, err := copies.Copy(ctx, primary(t, archive), record(), dst, key, 0); err != nil {
		t.Fatalf("Copy = %v", err)
	}
	r, err := dst.Retrieve(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	if !bytes.Equal(got, archive) {
		t.Fatal("the copy differs from the primary")
	}
}

func TestCopyRefusesADamagedPrimary(t *testing.T) {
	ctx := context.Background()
	damaged := bytes.Clone(archive)
	damaged[100] ^= 0xff
	dst := storage.NewMockStorage()
	_, err := copies.Copy(ctx, primary(t, damaged), record(), dst, key, 0)
	if !errors.Is(err, copies.ErrChecksumMismatch) {
		t.Fatalf("Copy = %v, want ErrChecksumMismatch", err)
	}
	if _, statErr := dst.Stat(ctx, key); !errors.Is(statErr, storage.ErrNotFound) {
		t.Fatalf("a mismatching copy must never be stored: %v", statErr)
	}
	if _, err = copies.Copy(ctx, primary(t, archive[:len(archive)-1]), record(), dst, key, 0); !errors.Is(err, copies.ErrChecksumMismatch) {
		t.Fatalf("a truncated primary = %v, want ErrChecksumMismatch", err)
	}
}

func TestCopyAllSyncRecordsEveryCopy(t *testing.T) {
	ctx := context.Background()
	good, bad := storage.NewMockStorage(), &failingStorage{MockStorage: storage.NewMockStorage(), fail: true}
	src := primary(t, archive)
	drivers := map[string]storage.Storage{"primary": src, "good": good, "bad": bad}
	svc := copies.New(copies.Config{Storages: func(_ context.Context, id string) (storage.Storage, error) { return drivers[id], nil },
		SyncBackoff: time.Millisecond, Logger: slog.New(slog.DiscardHandler)})
	rec := record("good")
	if err := svc.CopyAll(ctx, rec, 0); err != nil || rec.Copies[0].Status != models.CopyDone || !rec.Copies[0].SHA256OK {
		t.Fatalf("CopyAll = %v, %+v", err, rec.Copies)
	}
	rec = record("good", "bad")
	err := svc.CopyAll(ctx, rec, 0)
	if err == nil || rec.Copies[1].Status != models.CopyFailed || rec.Copies[1].Attempts != copies.SyncAttempts || rec.Copies[0].Status != models.CopyDone {
		t.Fatalf("CopyAll with a failing target = %v, %+v", err, rec.Copies)
	}
}

func TestQueueRetriesSurvivesRestartAndReportsRecovery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mongorescue.db")
	box := storetest.NewBox(t)
	first, err := store.OpenSQLite(ctx, path, slog.New(slog.DiscardHandler), store.WithSecretBox(box))
	if err != nil {
		t.Fatal(err)
	}
	if err = first.SaveBackupRecord(ctx, record("copy")); err != nil {
		t.Fatal(err)
	}
	dst := &failingStorage{MockStorage: storage.NewMockStorage(), fail: true}
	drivers := map[string]storage.Storage{"primary": primary(t, archive), "copy": dst}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	pub := &recorder{}
	var results []string
	cfg := copies.Config{Store: first, Storages: func(_ context.Context, id string) (storage.Storage, error) { return drivers[id], nil },
		Publisher: pub, Observe: func(r string) { results = append(results, r) }, Now: func() time.Time { return now },
		Logger: slog.New(slog.DiscardHandler)}
	svc := copies.New(cfg)
	if err = svc.RunDue(ctx); err == nil {
		t.Fatal("a failing copy must be reported")
	}
	rec, _ := first.GetBackupRecord(ctx, "bkp_1")
	c := rec.Copies[0]
	if c.Status != models.CopyFailed || c.Attempts != 1 || c.NextAttemptAt == nil || !c.NextAttemptAt.Equal(now.Add(copies.DefaultBaseBackoff)) {
		t.Fatalf("after a failed attempt: %+v", c)
	}
	if svc.QueueDepth() != 1 {
		t.Fatalf("QueueDepth = %d, want 1", svc.QueueDepth())
	}
	// Not due yet: nothing is tried.
	if err = svc.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart: a new store and service over the same database pick the copy up.
	second := storetest.OpenWithBox(t, path, box)
	cfg.Store = second
	dst.setFail(false)
	now = now.Add(2 * copies.DefaultBaseBackoff)
	svc = copies.New(cfg)
	if err = svc.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	rec, _ = second.GetBackupRecord(ctx, "bkp_1")
	if c = rec.Copies[0]; c.Status != models.CopyDone || !c.SHA256OK || c.Attempts != 2 || c.CopiedAt == nil || c.Error != "" {
		t.Fatalf("after the restart: %+v", c)
	}
	if got := pub.types(); len(got) != 2 || got[0] != events.BackupCopyFailed || got[1] != events.BackupCopyRecovered {
		t.Fatalf("events = %v", got)
	}
	if len(results) != 2 || results[0] != "error" || results[1] != "ok" {
		t.Fatalf("results = %v", results)
	}
	if svc.QueueDepth() != 0 {
		t.Fatalf("QueueDepth = %d, want 0", svc.QueueDepth())
	}
}

func TestQueueGivesUpAfterMaxAttempts(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	if err := s.SaveBackupRecord(ctx, record("copy")); err != nil {
		t.Fatal(err)
	}
	drivers := map[string]storage.Storage{"primary": primary(t, archive), "copy": &failingStorage{MockStorage: storage.NewMockStorage(), fail: true}}
	now := time.Now()
	svc := copies.New(copies.Config{Store: s, Storages: func(_ context.Context, id string) (storage.Storage, error) { return drivers[id], nil },
		MaxAttempts: 2, Now: func() time.Time { return now }, Logger: slog.New(slog.DiscardHandler)})
	for range 3 {
		_ = svc.RunDue(ctx)
		now = now.Add(24 * time.Hour)
	}
	rec, _ := s.GetBackupRecord(ctx, "bkp_1")
	if c := rec.Copies[0]; c.Status != models.CopyFailed || c.Attempts != 2 || c.NextAttemptAt != nil {
		t.Fatalf("after exhausted attempts: %+v", c)
	}
	if svc.QueueDepth() != 0 {
		t.Fatalf("an exhausted copy leaves the queue: depth %d", svc.QueueDepth())
	}
}

func TestQueueStartStop(t *testing.T) {
	s := storetest.New(t)
	if err := s.SaveBackupRecord(context.Background(), record("copy")); err != nil {
		t.Fatal(err)
	}
	dst := storage.NewMockStorage()
	drivers := map[string]storage.Storage{"primary": primary(t, archive), "copy": dst}
	svc := copies.New(copies.Config{Store: s, Storages: func(_ context.Context, id string) (storage.Storage, error) { return drivers[id], nil },
		StartDelay: time.Hour, Logger: slog.New(slog.DiscardHandler)})
	svc.Start(context.Background())
	svc.HandleEvent(context.Background(), events.Event{Type: events.BackupSucceeded})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := dst.Stat(context.Background(), key); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the woken queue did not copy the backup")
		}
		time.Sleep(10 * time.Millisecond)
	}
	svc.Stop()
	svc.Stop()
}
