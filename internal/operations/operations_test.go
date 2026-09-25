package operations_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func newService(t *testing.T) (*operations.Service, *store.SQLiteStore) {
	t.Helper()
	st := storetest.New(t)
	mock := storage.NewMockStorage()
	bRunner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
	}
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	return operations.New(operations.Config{
		Store:   st,
		Backup:  backup.NewEngine(mock, "", backup.WithRunner(bRunner)),
		Restore: restore.NewEngine(mock, ""),
		Runs:    manager,
		Version: "v-test",
	}), st
}

func TestStatus(t *testing.T) {
	svc, st := newService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	done := now.Add(-time.Hour)
	for _, j := range []*models.Job{{ID: "job_a", Name: "a", Database: "shop", Enabled: true}, {ID: "job_b", Name: "b", Database: "crm"}} {
		if err := st.SaveJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range []*models.BackupRecord{
		{ID: "bkp_old", JobID: "job_a", Database: "shop", Status: models.StatusCompleted, StartedAt: now.Add(-48 * time.Hour), SizeBytes: 5},
		{ID: "bkp_new", JobID: "job_a", Database: "shop", Status: models.StatusCompleted, StartedAt: now.Add(-2 * time.Hour), CompletedAt: &done, SizeBytes: 7},
		{ID: "bkp_fail", JobID: "job_b", Database: "crm", Status: models.StatusFailed, StartedAt: now.Add(-time.Hour), ErrorMessage: "boom"},
		{ID: "bkp_oldfail", Database: "crm", Status: models.StatusFailed, StartedAt: now.Add(-30 * time.Hour)},
		{ID: "bkp_run", Database: "crm", Status: models.StatusInProgress, StartedAt: now},
	} {
		if err := st.SaveBackupRecord(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	s, err := svc.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Health != "healthy" || s.Version != "v-test" || s.Jobs != 2 || s.RunningBackups != 1 || s.Stats.TotalBackups != 5 ||
		s.Stats.CompletedBackups != 2 || s.Stats.TotalBytes != 12 || s.Stats.ActiveJobs != 1 {
		t.Fatalf("status = %+v", s)
	}
	if len(s.FailedLast24h) != 1 || s.FailedLast24h[0].ID != "bkp_fail" || s.FailedLast24h[0].Error != "boom" {
		t.Fatalf("failed_last_24h = %+v", s.FailedLast24h)
	}
	for _, js := range s.JobStatus {
		switch js.ID {
		case "job_a":
			if js.LastSuccessBackupID != "bkp_new" || js.LastSuccessAt == nil || !js.LastSuccessAt.Equal(done) {
				t.Fatalf("job_a = %+v; want the newest completed backup", js)
			}
		case "job_b":
			if js.LastSuccessAt != nil {
				t.Fatalf("job_b never succeeded: %+v", js)
			}
		}
	}

	failed, err := svc.ListBackups(ctx, operations.BackupFilter{Database: "crm", Status: models.StatusFailed})
	if err != nil || len(failed) != 2 {
		t.Fatalf("filtered backups = %+v, %v", failed, err)
	}
}

func TestErrorsAreClassified(t *testing.T) {
	svc, _ := newService(t)
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{Method: auth.MethodAPIKey, Scope: auth.ScopeOperator})
	cases := []struct {
		err  error
		want error
		msg  string
	}{
		{func() error { _, err := svc.StartBackup(ctx, operations.BackupRequest{}); return err }(), operations.ErrConnectionRequired, "connection_id"},
		{func() error {
			_, err := svc.StartBackup(ctx, operations.BackupRequest{BackupOptions: models.BackupOptions{ConnectionID: "c"}})
			return err
		}(), operations.ErrUnknownConnection, "unknown connection_id"},
		{func() error { _, err := svc.RunJob(ctx, "job_x", models.TriggerOnDemand); return err }(), operations.ErrSchedulerUnavailable, "scheduler"},
		{func() error { _, err := svc.StartRestore(ctx, models.RestoreRequest{}); return err }(), operations.ErrInvalid, "backup_id required"},
		{func() error {
			f := false
			_, err := svc.StartRestore(ctx, models.RestoreRequest{BackupID: "b", SafeClone: &f})
			return err
		}(), operations.ErrInvalid, "confirm_in_place"},
		{func() error {
			f := false
			_, err := svc.StartRestore(ctx, models.RestoreRequest{BackupID: "b", SafeClone: &f, ConfirmInPlace: true})
			return err
		}(), auth.ErrForbidden, "admin"},
		{func() error { _, err := svc.StartRestore(ctx, models.RestoreRequest{BackupID: "b"}); return err }(), operations.ErrNotFound, "source backup not found"},
		{func() error { _, err := svc.GetBackup(ctx, "b"); return err }(), operations.ErrNotFound, "backup not found"},
		{func() error { _, err := svc.GetRestore(ctx, "r"); return err }(), operations.ErrNotFound, "restore not found"},
		{func() error { _, err := svc.GetJob(ctx, "j"); return err }(), operations.ErrNotFound, "job not found"},
	}
	for i, tc := range cases {
		if !errors.Is(tc.err, tc.want) || !strings.Contains(tc.err.Error(), tc.msg) {
			t.Errorf("case %d: %v; want %v mentioning %q", i, tc.err, tc.want, tc.msg)
		}
	}
}

func TestLegacyBackupWithoutConnectionNeedsAnAdminToChooseTheTarget(t *testing.T) {
	svc, st := newService(t)
	if err := st.SaveBackupRecord(context.Background(), &models.BackupRecord{ID: "bkp_legacy", Database: "shop", Status: models.StatusCompleted}); err != nil {
		t.Fatal(err)
	}
	for scope, want := range map[auth.Scope]string{
		auth.ScopeOperator: "an admin must choose the target connection",
		auth.ScopeAdmin:    "choose the target connection (target_connection_id)",
	} {
		ctx := auth.WithPrincipal(context.Background(), &auth.Principal{Method: auth.MethodAPIKey, Scope: scope})
		_, err := svc.StartRestore(ctx, models.RestoreRequest{BackupID: "bkp_legacy"})
		if !errors.Is(err, operations.ErrConnectionRequired) || !strings.Contains(err.Error(), "no source connection recorded") || !strings.Contains(err.Error(), want) {
			t.Errorf("%s restore of a legacy backup = %v; want ErrConnectionRequired saying %q", scope, err, want)
		}
	}
}
