package readiness_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/readiness"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// drTargets is a primary in eu-west-1 and copy targets in the same and in another
// region, with the same and with other credentials.
func drTargets() []*models.StorageTarget {
	s3 := func(id, region, key string, lock models.ObjectLockMode) *models.StorageTarget {
		days := 0
		if lock != "" {
			days = 30
		}
		return &models.StorageTarget{ID: id, Name: id, Type: models.StorageS3, S3: &models.S3Target{
			Region: region, Bucket: id, AccessKeyID: key, ObjectLock: lock, RetentionDays: days}}
	}
	return []*models.StorageTarget{
		s3("tgt_primary", "eu-west-1", "AKIA_PRIMARY", ""),
		s3("tgt_same", "eu-west-1", "AKIA_PRIMARY", ""),
		s3("tgt_far", "us-east-2", "AKIA_DR", models.ObjectLockCompliance),
	}
}

// drRow returns the only row of the report of one job copying to copies.
func drRow(t *testing.T, copies ...string) readiness.Row {
	t.Helper()
	return drRowWith(t, t0.Add(time.Hour), nil, copies...)
}

// drRowWith is drRow at now, after storing drills.
func drRowWith(t *testing.T, now time.Time, drills []*models.RestoreTestResult, copies ...string) readiness.Row {
	t.Helper()
	st := storetest.New(t)
	svc := readiness.New(readiness.Config{Store: unedited{st}, Now: func() time.Time { return now },
		Targets: func(context.Context) ([]*models.StorageTarget, error) { return drTargets(), nil }})
	saveJob(t, st, &models.Job{ID: "job_dr", Name: "dr", Database: "shop", CronExpression: "@daily", Enabled: true,
		ConnectionID: "c1", StorageTargetID: "tgt_primary", CopyTargets: copies})
	for _, d := range drills {
		if err := st.SaveRestoreTest(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	report, err := svc.Report(context.Background())
	if err != nil || len(report.Rows) != 1 {
		t.Fatalf("report = %+v, %v", report, err)
	}
	return report.Rows[0]
}

// TestReportWarnsAboutSameRegionAndSameCredentials proves dr_same_region and
// dr_same_credentials, and that a copy in another region with its own key clears
// both.
func TestReportWarnsAboutSameRegionAndSameCredentials(t *testing.T) {
	r := drRow(t, "tgt_same")
	if r.DR == nil || r.DR.Level != readiness.DRLevelSameRegion || r.DR.CrossRegion || r.DR.SeparateCredentials ||
		!slices.Contains(r.Reasons, readiness.ReasonDRSameRegion) || !slices.Contains(r.Reasons, readiness.ReasonDRSameCredentials) {
		t.Fatalf("same region copy: dr %+v, reasons %v", r.DR, r.Reasons)
	}
	r = drRow(t, "tgt_same", "tgt_far")
	if r.DR == nil || r.DR.Level != readiness.DRLevelCrossRegion || !r.DR.CrossRegion || !r.DR.SeparateCredentials || !r.DR.LockedCopy ||
		slices.Contains(r.Reasons, readiness.ReasonDRSameRegion) || slices.Contains(r.Reasons, readiness.ReasonDRSameCredentials) {
		t.Fatalf("cross-region copy: dr %+v, reasons %v", r.DR, r.Reasons)
	}
	if !slices.Equal(r.DR.PrimaryRegions, []string{"eu-west-1"}) || !slices.Equal(r.DR.CopyRegions, []string{"eu-west-1", "us-east-2"}) {
		t.Fatalf("regions = %v / %v", r.DR.PrimaryRegions, r.DR.CopyRegions)
	}
	if r = drRow(t); r.DR != nil {
		t.Fatalf("a job without copies has DR %+v", r.DR)
	}
}

// drill returns a drill of job_dr from source that finished at at.
func drill(id, source string, at time.Time, status models.RestoreTestStatus) *models.RestoreTestResult {
	done := at
	return &models.RestoreTestResult{ID: id, JobID: "job_dr", Database: "shop", Status: status, Trigger: "scheduled",
		StartedAt: at.Add(-time.Minute), CompletedAt: &done, SourceTargetID: source}
}

// TestReportShowsDrillsAndWarnsWhenStale proves that the DR status shows the
// newest drill and the newest passed one, and that dr_drill_stale warns without a
// passed drill in the last 30 days.
func TestReportShowsDrillsAndWarnsWhenStale(t *testing.T) {
	now := t0.Add(60 * 24 * time.Hour)
	r := drRowWith(t, now, nil, "tgt_far")
	if r.DR == nil || !r.DR.DrillStale || !slices.Contains(r.Reasons, readiness.ReasonDRDrillStale) {
		t.Fatalf("no drill: dr %+v, reasons %v", r.DR, r.Reasons)
	}
	old := drill("rt_old", "tgt_far", now.Add(-40*24*time.Hour), models.RestoreTestOK)
	r = drRowWith(t, now, []*models.RestoreTestResult{old}, "tgt_far")
	if !r.DR.DrillStale || r.DR.LastGoodDrill == nil || r.DR.LastGoodDrill.ID != "rt_old" {
		t.Fatalf("old drill: dr %+v", r.DR)
	}
	fresh := drill("rt_fresh", "tgt_far", now.Add(-2*24*time.Hour), models.RestoreTestOK)
	failed := drill("rt_failed", "tgt_far", now.Add(-time.Hour), models.RestoreTestError)
	primary := drill("rt_primary", "", now.Add(-time.Minute), models.RestoreTestOK)
	r = drRowWith(t, now, []*models.RestoreTestResult{old, fresh, failed, primary}, "tgt_far")
	if r.DR.DrillStale || slices.Contains(r.Reasons, readiness.ReasonDRDrillStale) ||
		r.DR.LastDrill == nil || r.DR.LastDrill.ID != "rt_failed" || r.DR.LastGoodDrill.ID != "rt_fresh" ||
		r.DR.LastGoodDrill.SourceTargetID != "tgt_far" {
		t.Fatalf("fresh drill: dr %+v, reasons %v", r.DR, r.Reasons)
	}
}
