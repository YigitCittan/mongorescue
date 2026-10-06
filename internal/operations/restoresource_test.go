package operations_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// newCopyRestoreEnv returns a service whose backup bkp_copy has its primary on
// stg_p and a completed copy on stg_c, with the archive stored where withArchive
// names.
func newCopyRestoreEnv(t *testing.T, withArchive ...string) *operations.Service {
	t.Helper()
	const key = "shop/2026/10/bkp_copy.archive.gz"
	data := []byte("archive bytes")
	drivers := map[string]*storage.MockStorage{"stg_p": storage.NewMockStorage(), "stg_c": storage.NewMockStorage()}
	for _, id := range withArchive {
		if _, err := drivers[id].Save(context.Background(), key, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}
	st := storetest.New(t)
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	svc := operations.New(operations.Config{
		Store: st, Backup: backup.NewEngine(storage.NewMockStorage(), ""), Runs: manager,
		Restore:     &completingEngine{prep: restore.NewEngine(nil, "")},
		Connections: fakeConnections{"conn_a": {ID: "conn_a", Name: "prod", URI: "mongodb://db.internal:27017"}},
		Storage: func(_ context.Context, id string) (storage.Storage, error) {
			if d, ok := drivers[id]; ok {
				return d, nil
			}
			return nil, errors.New("unknown target")
		},
	})
	rec := &models.BackupRecord{ID: "bkp_copy", Database: "shop", ConnectionID: "conn_a", Status: models.StatusCompleted,
		StartedAt: time.Now().UTC(), StorageTargetID: "stg_p", StorageTargetName: "primary", StorageKey: key,
		SizeBytes: int64(len(data)), SHA256: "abc"}
	rec.PlanCopies([]models.CopyTarget{{ID: "stg_c", Name: "offsite"}}, "")
	rec.Copies[0].Status, rec.Copies[0].SHA256 = models.CopyDone, "abc"
	if err := st.SaveBackupRecord(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestRestoreFallsBackToAHealthyCopy(t *testing.T) {
	ctx := context.Background()
	svc := newCopyRestoreEnv(t, "stg_c")
	rec, err := svc.StartRestore(ctx, models.RestoreRequest{BackupID: "bkp_copy"})
	if err != nil {
		t.Fatal(err)
	}
	if rec.SourceTargetID != "stg_c" || rec.SourceTargetName != "offsite" || !strings.Contains(rec.SourceFallback, "is missing") {
		t.Fatalf("restore source = %q (%q), fallback %q", rec.SourceTargetID, rec.SourceTargetName, rec.SourceFallback)
	}
	pre, err := svc.PreflightRestore(ctx, models.RestoreRequest{BackupID: "bkp_copy"})
	if err != nil {
		t.Fatal(err)
	}
	if c := pre.Check(models.PreflightCheckSource); c == nil || c.Status != models.PreflightWarn {
		t.Fatalf("source check = %+v", c)
	}
}

func TestRestoreReadsThePrimaryWhenItIsThere(t *testing.T) {
	svc := newCopyRestoreEnv(t, "stg_p", "stg_c")
	rec, err := svc.StartRestore(context.Background(), models.RestoreRequest{BackupID: "bkp_copy"})
	if err != nil || rec.SourceTargetID != "stg_p" || rec.SourceFallback != "" {
		t.Fatalf("restore = %+v, %v", rec, err)
	}
}

func TestRestoreFromAChosenSourceTarget(t *testing.T) {
	ctx := context.Background()
	svc := newCopyRestoreEnv(t, "stg_p", "stg_c")
	rec, err := svc.StartRestore(ctx, models.RestoreRequest{BackupID: "bkp_copy", SourceTargetID: "stg_c"})
	if err != nil || rec.SourceTargetID != "stg_c" || rec.SourceFallback != "" {
		t.Fatalf("restore = %+v, %v", rec, err)
	}
	if _, err = svc.StartRestore(ctx, models.RestoreRequest{BackupID: "bkp_copy", SourceTargetID: "stg_other"}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("an unknown source target = %v, want ErrInvalid", err)
	}
}

func TestRestoreWithoutAHealthyCopyFailsItsSourceCheck(t *testing.T) {
	svc := newCopyRestoreEnv(t)
	pre, err := svc.PreflightRestore(context.Background(), models.RestoreRequest{BackupID: "bkp_copy"})
	if err != nil {
		t.Fatal(err)
	}
	if c := pre.Check(models.PreflightCheckSource); c == nil || c.Status != models.PreflightFail || pre.OK {
		t.Fatalf("source check = %+v, ok %v", c, pre.OK)
	}
}
