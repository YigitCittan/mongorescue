package copies_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/copies"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// shortStorage stores only the first bytes of every upload and reports success, as
// a driver that stops reading early would.
type shortStorage struct{ *storage.MockStorage }

func (s shortStorage) Save(ctx context.Context, k string, r io.Reader) (*models.StorageObject, error) {
	return s.MockStorage.Save(ctx, k, io.LimitReader(r, 16))
}

// TestIncompleteCopyIsTrackedAndPurged proves that a copy target that stored an
// object without reading the whole archive fails the copy but keeps the object
// tracked: the target stays in use and the purge of the deleted backup removes it.
func TestIncompleteCopyIsTrackedAndPurged(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	if err := st.SaveBackupRecord(ctx, record("copy")); err != nil {
		t.Fatal(err)
	}
	dst := shortStorage{storage.NewMockStorage()}
	drivers := map[string]storage.Storage{"primary": primary(t, archive), "copy": dst}
	resolve := func(_ context.Context, id string) (storage.Storage, error) { return drivers[id], nil }
	svc := copies.New(copies.Config{Store: st, Storages: resolve, Logger: slog.New(slog.DiscardHandler)})
	if err := svc.RunDue(ctx); !errors.Is(err, copies.ErrIncomplete) {
		t.Fatalf("RunDue = %v, want ErrIncomplete", err)
	}
	rec, _ := st.GetBackupRecord(ctx, "bkp_1")
	if c := rec.Copies[0]; c.Status != models.CopyFailed || !c.Written || !c.MayExist() || !rec.HoldsTarget("copy") {
		t.Fatalf("incomplete copy = %+v", c)
	}
	past := time.Now().Add(-48 * time.Hour)
	if _, err := st.UpdateBackupRecord(ctx, "bkp_1", func(r *models.BackupRecord) error {
		r.MarkDeleted(models.SoftDelete{At: past, PurgeAfter: past.Add(time.Hour), By: "admin"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if ids, err := scheduler.PurgeDeleted(ctx, time.Now(), time.Hour, st, resolve, nil, nil); err != nil || len(ids) != 1 {
		t.Fatalf("purge = %v, %v", ids, err)
	}
	if _, err := dst.Stat(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("the incomplete copy must be purged: %v", err)
	}
}
