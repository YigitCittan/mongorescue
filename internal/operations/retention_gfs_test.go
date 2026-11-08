package operations_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
)

func TestValidateJobRetentionBuckets(t *testing.T) {
	e := newMultiEnv(t, "shop")
	ctx := context.Background()
	ok := &models.Job{ID: "job_ok", Database: "shop", ConnectionID: "conn_a", RetentionDays: 30, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 12, KeepYearly: 7}
	if err := e.svc.ValidateJob(ctx, ok); err != nil {
		t.Fatalf("a valid GFS policy: %v", err)
	}
	for name, bad := range map[string]*models.Job{
		"negative daily":       {ID: "j1", Database: "shop", ConnectionID: "conn_a", KeepDaily: -1},
		"daily too large":      {ID: "j2", Database: "shop", ConnectionID: "conn_a", KeepDaily: models.MaxKeepDaily + 1},
		"weekly too large":     {ID: "j3", Database: "shop", ConnectionID: "conn_a", KeepWeekly: models.MaxKeepWeekly + 1},
		"monthly too large":    {ID: "j4", Database: "shop", ConnectionID: "conn_a", KeepMonthly: models.MaxKeepMonthly + 1},
		"yearly too large":     {ID: "j5", Database: "shop", ConnectionID: "conn_a", KeepYearly: models.MaxKeepYearly + 1},
		"override w/o a copy":  {ID: "j6", Database: "shop", ConnectionID: "conn_a", CopyRetention: map[string]models.RetentionPolicy{"tgt": {KeepMonthly: 1}}},
		"negative days (base)": {ID: "j7", Database: "shop", ConnectionID: "conn_a", RetentionDays: -1},
	} {
		if err := e.svc.ValidateJob(ctx, bad); !errors.Is(err, operations.ErrInvalid) {
			t.Errorf("%s: %v; want ErrInvalid", name, err)
		}
	}
	// An empty override map is dropped, not stored.
	empty := &models.Job{ID: "job_empty", Database: "shop", ConnectionID: "conn_a", CopyRetention: map[string]models.RetentionPolicy{}}
	if err := e.svc.ValidateJob(ctx, empty); err != nil || empty.CopyRetention != nil {
		t.Fatalf("empty override: %v, %v", err, empty.CopyRetention)
	}
}

func TestUpdateJobKeepsAndReplacesBuckets(t *testing.T) {
	e := newMultiEnv(t, "shop")
	ctx := context.Background()
	e.create(t, &models.Job{ID: "job_b", Name: "b", CronExpression: "@daily", Database: "shop", KeepDaily: 7, KeepMonthly: 12})
	// An update without the fields (an older client) keeps them.
	job, err := e.svc.UpdateJob(ctx, "job_b", operations.JobUpdate{Name: "b", CronExpression: "@daily", ConnectionID: "conn_a", Database: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if job.KeepDaily != 7 || job.KeepMonthly != 12 {
		t.Fatalf("buckets after an update without them: %+v", job)
	}
	weekly, daily := 4, 0
	job, err = e.svc.UpdateJob(ctx, "job_b", operations.JobUpdate{Name: "b", CronExpression: "@daily", ConnectionID: "conn_a", Database: "shop",
		KeepWeekly: &weekly, KeepDaily: &daily})
	if err != nil {
		t.Fatal(err)
	}
	if job.KeepWeekly != 4 || job.KeepDaily != 0 || job.KeepMonthly != 12 {
		t.Fatalf("buckets after replacing two: %+v", job)
	}
	over := 9999
	if _, err = e.svc.UpdateJob(ctx, "job_b", operations.JobUpdate{Name: "b", CronExpression: "@daily", ConnectionID: "conn_a", Database: "shop", KeepYearly: &over}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("yearly out of range: %v", err)
	}
}

// TestRetentionPreviewNamesTheBucketsAndTheCopies proves the preview of a job with
// buckets lists every backup that stays with its keep reasons, previews unsaved
// bucket values, cuts the buckets in the schedule's zone and shows the copies that
// would outlive their primary.
func TestRetentionPreviewNamesTheBucketsAndTheCopies(t *testing.T) {
	svc, st := newService(t)
	ctx := context.Background()
	job := &models.Job{ID: "job_g", Name: "g", Database: "shop", CronExpression: "CRON_TZ=UTC @daily", RetentionCount: 1, KeepMonthly: 2,
		CopyTargets: []string{"tgt_lt"}, CopyRetention: map[string]models.RetentionPolicy{"tgt_lt": {KeepMonthly: 3}}}
	if err := st.SaveJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// Backups 10, 40 and 70 days old: three different months apart from boundary luck,
	// so the test uses the preview's own reasons rather than fixed months.
	for i, age := range []int{10, 40, 70} {
		rec := &models.BackupRecord{ID: "bkp_" + string(rune('a'+i)), JobID: job.ID, Database: "shop", Status: models.StatusCompleted,
			StartedAt: now.AddDate(0, 0, -age), Copies: []models.BackupCopy{{TargetID: "tgt_lt", Status: models.CopyDone}}}
		if err := st.SaveBackupRecord(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	p, err := svc.RetentionPreview(ctx, job.ID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.KeepMonthly != 2 || p.TimeZone != "UTC" || len(p.Kept)+len(p.Delete) != 3 {
		t.Fatalf("preview = %+v", p)
	}
	for _, k := range p.Kept {
		if len(k.Reasons) == 0 {
			t.Errorf("%s is kept without a reason", k.BackupID)
		}
	}
	// The newest is kept (floor + its month's bucket) and the monthly bucket names it.
	if p.Kept[0].BackupID != "bkp_a" || !slices.ContainsFunc(p.Kept[0].Reasons, func(r scheduler.KeepReason) bool {
		return r.Rule == scheduler.KeepMonthly && r.Bucket == now.AddDate(0, 0, -10).Format("2006-01")
	}) {
		t.Fatalf("kept = %+v", p.Kept)
	}
	// The override keeps the copies of the last three months; those whose primary the
	// policy deletes will outlive it.
	if len(p.Copies) != 1 || p.Copies[0].TargetID != "tgt_lt" || len(p.Copies[0].Kept) == 0 {
		t.Fatalf("copies = %+v", p.Copies)
	}
	for _, id := range p.Copies[0].Outliving {
		deleted := false
		for _, d := range p.Delete {
			deleted = deleted || d.Backup.ID == id
		}
		if !deleted {
			t.Errorf("%s outlives its primary but its primary is not deleted", id)
		}
	}

	// Unsaved values: without the bucket and count nothing but the floor is kept.
	zero, one := 0, 1
	p, err = svc.RetentionPreviewWith(ctx, job.ID, operations.RetentionOverride{Count: &zero, Monthly: &zero, Yearly: &one})
	if err != nil {
		t.Fatal(err)
	}
	if p.KeepYearly != 1 || p.KeepMonthly != 0 || len(p.Kept) == 0 {
		t.Fatalf("override preview = %+v", p)
	}
	over := models.MaxKeepYearly + 1
	if _, err = svc.RetentionPreviewWith(ctx, job.ID, operations.RetentionOverride{Yearly: &over}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("out of range preview = %v", err)
	}
}
