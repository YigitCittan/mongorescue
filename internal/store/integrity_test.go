package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func TestManifestStoredApartFromRecord(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	rec := &models.BackupRecord{ID: "bkp_1", Database: "shop", Status: models.StatusCompleted, StartedAt: time.Now().UTC(), HasManifest: true,
		Manifest: &models.Manifest{CapturedAt: time.Now().UTC(), Collections: []models.CollectionManifest{
			{Name: "users", DocumentsMin: 2, DocumentsMax: 2},
			{Name: "orders", DocumentsMin: 5, DocumentsMax: 7, Indexes: []models.IndexSpec{{Name: "sku_1", Keys: "sku:1", Unique: true}, {Name: "_id_", Keys: "_id:1"}}},
		}}}
	if err := s.SaveBackupRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBackupRecord(ctx, "bkp_1")
	if err != nil || got.Manifest != nil || !got.HasManifest {
		t.Fatalf("record = %+v, %v; the manifest must not be inlined", got, err)
	}
	m, err := s.GetManifest(ctx, "bkp_1")
	if err != nil {
		t.Fatal(err)
	}
	if m.Collections[0].Name != "orders" || m.Collections[0].Indexes[0].Name != "_id_" {
		t.Fatalf("manifest not normalised: %+v", m)
	}
	if err := s.DeleteBackupRecord(ctx, "bkp_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetManifest(ctx, "bkp_1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("manifest after delete: %v", err)
	}
}

func TestUpdateBackupRecord(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	if err := s.SaveBackupRecord(ctx, &models.BackupRecord{ID: "bkp_1", Database: "shop", Status: models.StatusCompleted}); err != nil {
		t.Fatal(err)
	}
	got, err := s.UpdateBackupRecord(ctx, "bkp_1", func(r *models.BackupRecord) error {
		r.Pinned, r.PinNote = true, "audit"
		return nil
	})
	if err != nil || !got.Pinned {
		t.Fatalf("update = %+v, %v", got, err)
	}
	refuse := errors.New("refused")
	if _, err := s.UpdateBackupRecord(ctx, "bkp_1", func(r *models.BackupRecord) error {
		r.Pinned = false
		return refuse
	}); !errors.Is(err, refuse) {
		t.Fatalf("fn error = %v", err)
	}
	if rec, _ := s.GetBackupRecord(ctx, "bkp_1"); !rec.Pinned || rec.PinNote != "audit" {
		t.Fatalf("a refused update must not be written: %+v", rec)
	}
	if _, err := s.UpdateBackupRecord(ctx, "nope", func(*models.BackupRecord) error { return nil }); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown id = %v", err)
	}
	if _, err := s.UpdateBackupRecord(ctx, "bkp_1", func(r *models.BackupRecord) error { r.ID = "other"; return nil }); !errors.Is(err, store.ErrInvalidRecord) {
		t.Fatalf("id change = %v", err)
	}
}

func TestRestoreTestsAndJobSummary(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	job := &models.Job{ID: "job_1", Name: "n", Database: "shop", CronExpression: "@daily"}
	if err := s.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	for i := range store.MaxRestoreTestsPerJob + 3 {
		r := &models.RestoreTestResult{ID: fmt.Sprintf("rt_%03d", i), JobID: "job_1", BackupID: "bkp", Status: models.RestoreTestOK, StartedAt: base.Add(time.Duration(i) * time.Minute)}
		if err := s.SaveRestoreTest(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListRestoreTests(ctx, "job_1", 1000)
	if err != nil || len(list) != store.MaxRestoreTestsPerJob || list[0].ID != fmt.Sprintf("rt_%03d", store.MaxRestoreTestsPerJob+2) {
		t.Fatalf("restore tests: %d, %v", len(list), err)
	}
	sum := list[0].Summary()
	if err := s.UpdateJobRestoreTest(ctx, "job_1", sum); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetJob(ctx, "job_1")
	if got.LastRestoreTest == nil || got.LastRestoreTest.ID != sum.ID || !got.UpdatedAt.Equal(job.UpdatedAt) {
		t.Fatalf("job = %+v", got)
	}
	if err := s.UpdateJobRestoreTest(ctx, "missing", sum); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing job = %v", err)
	}
}

func TestRetentionLogAndState(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	for i := range 3 {
		e := &models.RetentionLogEntry{Time: time.Now().UTC(), JobID: "job_1", BackupID: fmt.Sprintf("bkp_%d", i), Reason: models.RetentionMaxCount}
		if err := s.AppendRetentionLog(ctx, e); err != nil || e.ID == 0 {
			t.Fatalf("append = %d, %v", e.ID, err)
		}
	}
	if err := s.AppendRetentionLog(ctx, &models.RetentionLogEntry{JobID: "job_2", BackupID: "x"}); err != nil {
		t.Fatal(err)
	}
	log, err := s.ListRetentionLog(ctx, "job_1", 2)
	if err != nil || len(log) != 2 || log[0].BackupID != "bkp_2" || log[0].ID == 0 {
		t.Fatalf("log = %+v, %v", log, err)
	}
	if err := s.AppendRetentionLog(ctx, &models.RetentionLogEntry{}); !errors.Is(err, store.ErrInvalidRecord) {
		t.Fatalf("empty entry = %v", err)
	}

	var v struct{ N int }
	if ok, err := s.LoadIntegrityState(ctx, "k", &v); ok || err != nil {
		t.Fatalf("missing state = %v, %v", ok, err)
	}
	if err := s.SaveIntegrityState(ctx, "k", struct{ N int }{7}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.LoadIntegrityState(ctx, "k", &v); !ok || err != nil || v.N != 7 {
		t.Fatalf("state = %+v, %v, %v", v, ok, err)
	}
}
