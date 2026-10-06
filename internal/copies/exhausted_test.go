package copies_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/copies"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestExhaustedCopyIsReportedAndLeavesTheQueue proves that the last failed attempt
// publishes backup.copy_exhausted, that the record is no longer loaded by the
// queue, and that a retry queues it again.
func TestExhaustedCopyIsReportedAndLeavesTheQueue(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	if err := s.SaveBackupRecord(ctx, record("copy")); err != nil {
		t.Fatal(err)
	}
	dst := &failingStorage{MockStorage: storage.NewMockStorage(), fail: true}
	drivers := map[string]storage.Storage{"primary": primary(t, archive), "copy": dst}
	now := time.Now()
	pub := &recorder{}
	svc := copies.New(copies.Config{Store: s, Storages: func(_ context.Context, id string) (storage.Storage, error) { return drivers[id], nil },
		MaxAttempts: 2, Publisher: pub, Now: func() time.Time { return now }, Logger: slog.New(slog.DiscardHandler)})
	for range 2 {
		_ = svc.RunDue(ctx)
		now = now.Add(24 * time.Hour)
	}
	got := pub.types()
	if len(got) != 2 || got[0] != events.BackupCopyFailed || got[1] != events.BackupCopyExhausted {
		t.Fatalf("events = %v", got)
	}
	if list, _ := s.PendingCopyRecords(ctx); len(list) != 0 {
		t.Fatalf("an exhausted copy must not be reloaded: %v", list)
	}
	if list, _ := s.ExhaustedCopyRecords(ctx); len(list) != 1 {
		t.Fatalf("ExhaustedCopyRecords = %v", list)
	}

	// A retry resets the attempts and the copy recovers.
	if _, err := s.UpdateBackupRecord(ctx, "bkp_1", func(r *models.BackupRecord) error {
		if !r.RetryCopies() {
			t.Fatal("RetryCopies found no failed copy")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	dst.setFail(false)
	if err := svc.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	rec, _ := s.GetBackupRecord(ctx, "bkp_1")
	if c := rec.Copies[0]; c.Status != models.CopyDone || c.Attempts != 1 {
		t.Fatalf("retried copy = %+v", c)
	}
	if got = pub.types(); got[len(got)-1] != events.BackupCopyRecovered {
		t.Fatalf("events = %v", got)
	}
}
