package operations_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	return newCopyRestoreEnvWith(t, &completingEngine{prep: restore.NewEngine(nil, "")}, withArchive...)
}

// newCopyRestoreEnvWith is newCopyRestoreEnv restoring with eng.
func newCopyRestoreEnvWith(t *testing.T, eng operations.RestoreEngine, withArchive ...string) *operations.Service {
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
		Restore:     eng,
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

// mismatchEngine fails every restore of the primary archive (stg_p) with a
// checksum mismatch and completes restores of copies.
type mismatchEngine struct {
	completingEngine
	sources []string
}

func (e *mismatchEngine) Execute(ctx context.Context, req models.RestoreRequest, src *models.BackupRecord, rec *models.RestoreRecord) (*models.RestoreRecord, error) {
	e.mu.Lock()
	e.sources = append(e.sources, src.StorageTargetID)
	e.mu.Unlock()
	if src.StorageTargetID == "stg_p" {
		rec.Status = models.RestoreStatusFailed
		return rec, fmt.Errorf("verify archive: %w: recorded abc, stored artifact def", restore.ErrChecksumMismatch)
	}
	return e.completingEngine.Execute(ctx, req, src, rec)
}

// TestRestoreMismatchMakesTheNextRestoreUseACopy proves that a restore that finds
// the primary archive damaged records the mismatch on the backup without retrying,
// and that the next restore reads the healthy copy by itself.
func TestRestoreMismatchMakesTheNextRestoreUseACopy(t *testing.T) {
	ctx := context.Background()
	eng := &mismatchEngine{completingEngine: completingEngine{prep: restore.NewEngine(nil, "")}}
	svc := newCopyRestoreEnvWith(t, eng, "stg_p", "stg_c")
	first, err := svc.StartRestore(ctx, models.RestoreRequest{BackupID: "bkp_copy"})
	if err != nil || first.SourceTargetID != "stg_p" {
		t.Fatalf("first restore = %+v, %v", first, err)
	}
	if done := waitRestoreDone(t, svc, first.ID); done.Status != models.RestoreStatusFailed {
		t.Fatalf("first restore = %s", done.Status)
	}
	rec, err := svc.GetBackup(ctx, "bkp_copy")
	if err != nil || rec.Verification != models.VerificationMismatch {
		t.Fatalf("backup verification = %q, %v", rec.Verification, err)
	}
	second, err := svc.StartRestore(ctx, models.RestoreRequest{BackupID: "bkp_copy"})
	if err != nil || second.SourceTargetID != "stg_c" || !strings.Contains(second.SourceFallback, "verification") {
		t.Fatalf("second restore = %+v, %v", second, err)
	}
	waitRestoreDone(t, svc, second.ID)
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if len(eng.sources) != 2 || eng.sources[0] != "stg_p" || eng.sources[1] != "stg_c" {
		t.Fatalf("restores read %v; want the primary once (no retry), then the copy", eng.sources)
	}
}
