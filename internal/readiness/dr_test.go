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
	st := storetest.New(t)
	svc := readiness.New(readiness.Config{Store: unedited{st}, Now: func() time.Time { return t0.Add(time.Hour) },
		Targets: func(context.Context) ([]*models.StorageTarget, error) { return drTargets(), nil }})
	saveJob(t, st, &models.Job{ID: "job_dr", Name: "dr", Database: "shop", CronExpression: "@daily", Enabled: true,
		ConnectionID: "c1", StorageTargetID: "tgt_primary", CopyTargets: copies})
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
