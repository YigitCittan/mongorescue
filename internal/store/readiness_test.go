package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func TestRPOBreaches(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	since := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	added, err := s.AddRPOBreach(ctx, models.RPOBreach{JobID: "job_a", Database: "shop", Since: since})
	if err != nil || !added {
		t.Fatalf("AddRPOBreach = %v, %v; want added", added, err)
	}
	// A second breach of the same job and database keeps the first one.
	added, err = s.AddRPOBreach(ctx, models.RPOBreach{JobID: "job_a", Database: "shop", Since: since.Add(time.Hour)})
	if err != nil || added {
		t.Fatalf("second AddRPOBreach = %v, %v; want not added", added, err)
	}
	if _, err = s.AddRPOBreach(ctx, models.RPOBreach{JobID: "job_a", Database: "crm", Since: since}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddRPOBreach(ctx, models.RPOBreach{Database: "x"}); err == nil {
		t.Error("a breach without a job was recorded")
	}
	list, err := s.ListRPOBreaches(ctx)
	if err != nil || len(list) != 2 || list[0].Database != "crm" || !list[1].Since.Equal(since) {
		t.Fatalf("ListRPOBreaches = %+v, %v", list, err)
	}
	deleted, err := s.DeleteRPOBreach(ctx, "job_a", "shop")
	if err != nil || !deleted {
		t.Fatalf("DeleteRPOBreach = %v, %v; want deleted", deleted, err)
	}
	if deleted, err = s.DeleteRPOBreach(ctx, "job_a", "shop"); err != nil || deleted {
		t.Fatalf("second DeleteRPOBreach = %v, %v; want nothing deleted", deleted, err)
	}
}

func TestLatestJobDatabaseBackupsAll(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	for _, b := range []*models.BackupRecord{
		{ID: "b1", JobID: "job_a", Database: "shop", Status: models.StatusCompleted, StartedAt: now.Add(-3 * time.Hour), Verification: models.VerificationOK},
		{ID: "b2", JobID: "job_a", Database: "shop", Status: models.StatusCompleted, StartedAt: now.Add(-2 * time.Hour), Verification: models.VerificationOK},
		{ID: "b3", JobID: "job_a", Database: "shop", Status: models.StatusCompleted, StartedAt: now.Add(-time.Hour), Verification: models.VerificationMismatch},
		{ID: "b4", JobID: "job_a", Database: "shop", Status: models.StatusCompleted, StartedAt: now},
		{ID: "b5", JobID: "job_a", Database: "crm", Status: models.StatusFailed, StartedAt: now, Verification: models.VerificationOK},
		{ID: "b6", JobID: "job_b", Database: "shop", Status: models.StatusCompleted, StartedAt: now, Verification: models.VerificationOK},
	} {
		if err := s.SaveBackupRecord(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.LatestJobDatabaseBackupsAll(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := all["job_a"]; len(got) != 1 || got["shop"] == nil || got["shop"].ID != "b2" {
		t.Fatalf("verified backups of job_a = %+v; want shop → b2", got)
	}
	if got := all["job_b"]; len(got) != 1 || got["shop"].ID != "b6" {
		t.Fatalf("verified backups of job_b = %+v; want shop → b6", got)
	}
	latest, err := s.LatestJobDatabaseBackupsAll(ctx, false)
	if err != nil || latest["job_a"]["shop"].ID != "b4" || latest["job_a"]["crm"] != nil {
		t.Fatalf("latest backups = %+v, %v; want job_a shop → b4 and no crm", latest["job_a"], err)
	}
}

func TestLatestCompletedRestores(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	for _, r := range []*models.RestoreRecord{
		{ID: "r1", SourceDatabase: "shop", SourceConnectionID: "c1", Status: models.RestoreStatusCompleted, StartedAt: now.Add(-3 * time.Hour), DurationSeconds: 30},
		{ID: "r2", SourceDatabase: "shop", SourceConnectionID: "c1", Status: models.RestoreStatusCompleted, StartedAt: now.Add(-2 * time.Hour), DurationSeconds: 40},
		{ID: "r3", SourceDatabase: "shop", SourceConnectionID: "c1", Status: models.RestoreStatusCompleted, StartedAt: now.Add(-time.Hour), DryRun: true},
		{ID: "r4", SourceDatabase: "shop", SourceConnectionID: "c1", Status: models.RestoreStatusFailed, StartedAt: now},
		{ID: "r5", SourceDatabase: "shop", SourceConnectionID: "c1", Status: models.RestoreStatusCompleted, StartedAt: now, SelectedCollections: []string{"orders"}},
		{ID: "r6", SourceDatabase: "shop", SourceConnectionID: "c2", Status: models.RestoreStatusCompleted, StartedAt: now},
	} {
		if err := s.SaveRestoreRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LatestCompletedRestores(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "r2" || got[1].ID != "r6" {
		ids := make([]string, len(got))
		for i, r := range got {
			ids[i] = r.ID
		}
		t.Fatalf("latest restores = %v; want [r2 r6]", ids)
	}
}

func TestLatestRestoreTestsAll(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	for i, rt := range []*models.RestoreTestResult{
		{ID: "t1", JobID: "job_a", Database: "shop", Status: models.RestoreTestOK},
		{ID: "t2", JobID: "job_a", Database: "shop", Status: models.RestoreTestOK},
		{ID: "t3", JobID: "job_a", Database: "shop", Status: models.RestoreTestError},
		{ID: "t4", JobID: "job_a", Database: "crm", Status: models.RestoreTestMismatch},
		{ID: "t5", JobID: "job_b", Database: "shop", Status: models.RestoreTestOK},
	} {
		rt.StartedAt = now.Add(time.Duration(i) * time.Minute)
		if err := s.SaveRestoreTest(ctx, rt); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LatestRestoreTestsAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	names := func(list []*models.RestoreTestResult) []string {
		out := []string{}
		for _, r := range list {
			out = append(out, r.ID)
		}
		return out
	}
	// Per job, newest first: each database's newest test and its newest passed one.
	if a := names(got["job_a"]); len(a) != 3 || a[0] != "t4" || a[1] != "t3" || a[2] != "t2" {
		t.Fatalf("job_a = %v; want [t4 t3 t2]", a)
	}
	if b := names(got["job_b"]); len(b) != 1 || b[0] != "t5" {
		t.Fatalf("job_b = %v; want [t5]", b)
	}
}

func TestDeleteJobDropsItsRPOState(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	if err := s.SaveJob(ctx, &models.Job{ID: "job_a", Name: "a", Database: "shop", KnownDatabases: []string{"shop"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddRPOBreach(ctx, models.RPOBreach{JobID: "job_a", Database: "shop", Since: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateJobKnownDatabases(ctx, "job_a", func(*models.Job) ([]string, bool) { return []string{"shop", "crm"}, true }); err != nil {
		t.Fatal(err)
	}
	if joins, err := s.JobDatabaseJoins(ctx); err != nil || len(joins["job_a"]) != 1 {
		t.Fatalf("joins = %v, %v", joins, err)
	}
	if err := s.DeleteJob(ctx, "job_a"); err != nil {
		t.Fatal(err)
	}
	breaches, _ := s.ListRPOBreaches(ctx)
	joins, _ := s.JobDatabaseJoins(ctx)
	if len(breaches) != 0 || len(joins) != 0 {
		t.Fatalf("after delete: breaches %v, joins %v", breaches, joins)
	}
	if err := s.DeleteJob(ctx, "job_a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete = %v; want ErrNotFound", err)
	}
}
