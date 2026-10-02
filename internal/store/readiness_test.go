package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
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

func TestLatestVerifiedJobDatabaseBackups(t *testing.T) {
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
	got, err := s.LatestVerifiedJobDatabaseBackups(ctx, "job_a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["shop"] == nil || got["shop"].ID != "b2" {
		t.Fatalf("verified backups = %+v; want shop → b2", got)
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
