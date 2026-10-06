package readiness_test

import (
	"context"
	"errors"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/readiness"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestReportListsStorageHints checks that the report carries the immutability hint
// of every storage target, sorted by name, and none for a compliance-locked one.
func TestReportListsStorageHints(t *testing.T) {
	targets := []*models.StorageTarget{
		{ID: "stg_c", Name: "vault", Type: models.StorageS3, S3: &models.S3Target{Bucket: "v", ObjectLock: models.ObjectLockCompliance, RetentionDays: 30}},
		{ID: "stg_b", Name: "s3-plain", Type: models.StorageS3, S3: &models.S3Target{Bucket: "p"}},
		{ID: "stg_a", Name: "governed", Type: models.StorageS3, S3: &models.S3Target{Bucket: "g", ObjectLock: models.ObjectLockGovernance, RetentionDays: 7}},
	}
	svc := readiness.New(readiness.Config{Store: storetest.New(t),
		Targets: func(context.Context) ([]*models.StorageTarget, error) { return targets, nil }})
	rep, err := svc.Report(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.StorageHints) != 2 || rep.StorageHints[0].TargetID != "stg_a" || rep.StorageHints[0].Code != models.HintGovernanceBypass ||
		rep.StorageHints[0].Level != models.HintWarn || rep.StorageHints[1].Code != models.HintNoObjectLock {
		t.Fatalf("storage hints = %+v", rep.StorageHints)
	}
	if rep.Summary.Warn != 0 || rep.Summary.Fail != 0 {
		t.Fatalf("hints changed the summary: %+v", rep.Summary)
	}

	failing := readiness.New(readiness.Config{Store: storetest.New(t),
		Targets: func(context.Context) ([]*models.StorageTarget, error) { return nil, errors.New("boom") }})
	if rep, err = failing.Report(context.Background()); err != nil || rep.StorageHints == nil || len(rep.StorageHints) != 0 {
		t.Fatalf("report with a failing target list = %+v, %v; want no hints", rep, err)
	}
}
