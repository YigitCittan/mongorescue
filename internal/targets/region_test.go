package targets_test

import (
	"context"
	"errors"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

// locatingStorage is a mock bucket that tells its region.
type locatingStorage struct {
	*storage.MockStorage
	region string
	err    error
}

func (l *locatingStorage) BucketRegion(context.Context) (string, error) { return l.region, l.err }

// regionFixture is a targets service whose every bucket reports bucket.region.
func regionFixture(t *testing.T) (*targets.Service, *locatingStorage) {
	t.Helper()
	bucket := &locatingStorage{MockStorage: storage.NewMockStorage(), region: "eu-central-1"}
	svc := targets.NewService(storetest.New(t), func(context.Context, *models.StorageTarget, string) (storage.Storage, error) {
		return bucket, nil
	}, t.TempDir())
	return svc, bucket
}

// awsInput is an AWS S3 target (no endpoint) on bucket without a region.
func awsInput(bucket string) targets.Input {
	return targets.Input{Name: bucket, Type: models.StorageS3, S3: &models.S3Target{Bucket: bucket, AccessKeyID: "AKID", SecretAccessKey: "secret"}}
}

// TestRegionIsDetectedOnAWSKeptAndLabelled proves that an AWS S3 target without a
// region takes its bucket's location as a detected region, that an explicit label
// wins, that an update without a region keeps the stored one, that a detected
// region is detected again for another bucket, and that a failed lookup leaves it
// unknown.
func TestRegionIsDetectedOnAWSKeptAndLabelled(t *testing.T) {
	svc, bucket := regionFixture(t)
	ctx := context.Background()
	in := awsInput("bucket-one")
	created, err := svc.Create(ctx, in)
	if err != nil || created.Region != "eu-central-1" || !created.RegionDetected || created.DRRegion() != "eu-central-1" {
		t.Fatalf("create = %+v, %v; want the detected bucket region", created, err)
	}
	bucket.region = "us-west-2"
	in.S3.Bucket = "bucket-two"
	moved, err := svc.Update(ctx, created.ID, in)
	if err != nil || moved.Region != "us-west-2" || !moved.RegionDetected {
		t.Fatalf("update to another bucket = %+v, %v; want its region detected again", moved, err)
	}
	label := "dc-two"
	in.Region = &label
	updated, err := svc.Update(ctx, created.ID, in)
	if err != nil || updated.Region != "dc-two" || updated.RegionDetected {
		t.Fatalf("labelled update = %+v, %v", updated, err)
	}
	in.Region = nil
	in.S3.Bucket = "bucket-three"
	if updated, err = svc.Update(ctx, created.ID, in); err != nil || updated.Region != "dc-two" || updated.RegionDetected {
		t.Fatalf("update without a region = %+v, %v; want the label kept", updated, err)
	}
	empty := ""
	bucket.region, bucket.err = "", errors.New("access denied")
	in.Region = &empty
	if updated, err = svc.Update(ctx, created.ID, in); err != nil || updated.Region != "" || updated.DRRegion() != "" {
		t.Fatalf("cleared region with a failed lookup = %+v, %v; want unknown", updated, err)
	}
	long := string(make([]byte, models.MaxRegionLength+1))
	in.Region = &long
	if _, err = svc.Update(ctx, created.ID, in); !errors.Is(err, targets.ErrInvalid) {
		t.Fatalf("overlong region = %v; want ErrInvalid", err)
	}
}

// TestRegionOnOtherEndpointsNeedsALabel proves that on a non-AWS endpoint the
// region is not detected and the S3 region does not count: only a label does.
func TestRegionOnOtherEndpointsNeedsALabel(t *testing.T) {
	svc, _ := regionFixture(t)
	ctx := context.Background()
	in := s3Input("minio", "bucket-one", "secret")
	in.S3.Region = "us-east-1"
	created, err := svc.Create(ctx, in)
	if err != nil || created.Region != "" || created.DRRegion() != "" {
		t.Fatalf("create = %+v, %v; want no region", created, err)
	}
	label := "dc-ams"
	in.Region, in.S3.SecretAccessKey = &label, models.SecretMask
	if updated, err := svc.Update(ctx, created.ID, in); err != nil || updated.DRRegion() != "dc-ams" {
		t.Fatalf("labelled = %+v, %v; want dc-ams", updated, err)
	}
}
