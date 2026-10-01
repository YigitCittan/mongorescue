package operations_test

import (
	"context"
	"errors"
	"fmt"
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

// defaultTarget resolves "" (and its own ID) to the default target tgt_default.
type defaultTarget struct{}

func (defaultTarget) Resolve(_ context.Context, id string) (*models.StorageTarget, error) {
	if id != "" && id != "tgt_default" {
		return nil, errors.New("targets: storage target not found")
	}
	return &models.StorageTarget{ID: "tgt_default", Name: "Default", Type: models.StorageLocal, IsDefault: true}, nil
}

func (defaultTarget) List(context.Context) ([]*models.StorageTarget, error) { return nil, nil }

func TestRetentionPreviewOfALegacyJobUsesTheDefaultTarget(t *testing.T) {
	st := storetest.New(t)
	mock := storage.NewMockStorage()
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	svc := operations.New(operations.Config{Store: st, Backup: backup.NewEngine(mock, ""), Restore: restore.NewEngine(mock, ""),
		Runs: manager, Targets: defaultTarget{}})
	ctx := context.Background()
	// A job saved before storage targets existed: no storage_target_id. Its
	// scheduled runs write to (and prune on) the default target.
	job := &models.Job{ID: "job_legacy", Name: "legacy", Database: "shop", CronExpression: "@daily", RetentionCount: 1}
	if err := st.SaveJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := range 3 {
		if err := st.SaveBackupRecord(ctx, &models.BackupRecord{ID: fmt.Sprintf("bkp_%d", i), JobID: job.ID, Trigger: models.TriggerScheduled,
			Database: "shop", Status: models.StatusCompleted, StorageTargetID: "tgt_default", StartedAt: now.Add(-time.Duration(i+2) * 24 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	p, err := svc.RetentionPreview(ctx, job.ID, nil, nil)
	if err != nil || len(p.Delete) != 2 || p.Considered != 3 {
		t.Fatalf("preview = %+v, %v; want the two older backups on the default target", p, err)
	}
}

func TestUnpinNeedsAnAdminPrincipal(t *testing.T) {
	svc, st := newService(t)
	if err := st.SaveBackupRecord(context.Background(), &models.BackupRecord{ID: "bkp_1", Database: "shop", Status: models.StatusCompleted, Pinned: true}); err != nil {
		t.Fatal(err)
	}
	operator := auth.WithPrincipal(context.Background(), &auth.Principal{APIKeyName: "ci", Method: auth.MethodAPIKey, Scope: auth.ScopeOperator})
	if _, err := svc.UnpinBackup(operator, "bkp_1"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("operator unpin = %v, want ErrForbidden", err)
	}
	if _, err := svc.UnpinBackup(context.Background(), "bkp_1"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("unpin without a principal = %v", err)
	}
	if rec, _ := st.GetBackupRecord(context.Background(), "bkp_1"); !rec.Pinned {
		t.Fatal("a refused unpin must keep the pin")
	}
	if _, err := svc.PinBackup(operator, "bkp_1", "still held"); err != nil {
		t.Fatalf("operators may pin: %v", err)
	}
}
