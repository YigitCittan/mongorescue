package operations_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func TestRetryCopiesIsAdminOnlyAndWakesTheQueue(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	rec := &models.BackupRecord{ID: "bkp_x", Database: "shop", Status: models.StatusCompleted, StorageTargetID: "tgt_p",
		StorageKey: "shop/k.archive.gz", SHA256: "abc", StartedAt: time.Now().UTC()}
	rec.PlanCopies([]models.CopyTarget{{ID: "tgt_c"}}, "")
	rec.Copies[0].Status, rec.Copies[0].Attempts, rec.Copies[0].Error = models.CopyFailed, 10, "unreachable"
	if err := st.SaveBackupRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	woken := 0
	svc := operations.New(operations.Config{Store: st, Backup: backup.NewEngine(storage.NewMockStorage(), ""), Runs: manager,
		Restore:    &completingEngine{prep: restore.NewEngine(nil, "")},
		WakeCopies: func() { woken++ }})

	operator := auth.WithPrincipal(ctx, &auth.Principal{Method: auth.MethodAPIKey, Scope: auth.ScopeOperator})
	if _, err := svc.RetryCopies(operator, "bkp_x"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("an operator retry = %v, want ErrForbidden", err)
	}
	admin := auth.WithPrincipal(ctx, auth.SystemPrincipal())
	got, err := svc.RetryCopies(admin, "bkp_x")
	if err != nil || got.Copies[0].Status != models.CopyPending || got.Copies[0].Attempts != 0 || woken != 1 {
		t.Fatalf("retry = %+v, %v, woken %d", got, err, woken)
	}
	if _, err = svc.RetryCopies(admin, "bkp_x"); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("a retry without failed copies = %v, want ErrInvalid", err)
	}
}
