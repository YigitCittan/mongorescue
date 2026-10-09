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

// TestRegionIsDetectedKeptAndLabelled proves that an S3 target without a region
// takes its bucket's location, that an explicit label wins, that an update without
// a region keeps the stored one, and that a failed lookup leaves it unknown.
func TestRegionIsDetectedKeptAndLabelled(t *testing.T) {
	bucket := &locatingStorage{MockStorage: storage.NewMockStorage(), region: "eu-central-1"}
	svc := targets.NewService(storetest.New(t), func(context.Context, *models.StorageTarget, string) (storage.Storage, error) {
		return bucket, nil
	}, t.TempDir())
	ctx := context.Background()
	in := s3Input("auto", "bucket-one", "secret")
	created, err := svc.Create(ctx, in)
	if err != nil || created.Region != "eu-central-1" || created.DRRegion() != "eu-central-1" {
		t.Fatalf("create = %+v, %v; want the bucket's region", created, err)
	}
	label := "dc-two"
	in.Region, in.S3.SecretAccessKey = &label, models.SecretMask
	updated, err := svc.Update(ctx, created.ID, in)
	if err != nil || updated.Region != "dc-two" {
		t.Fatalf("labelled update = %+v, %v", updated, err)
	}
	in.Region = nil
	if updated, err = svc.Update(ctx, created.ID, in); err != nil || updated.Region != "dc-two" {
		t.Fatalf("update without a region = %+v, %v; want it kept", updated, err)
	}
	empty := ""
	bucket.region, bucket.err = "", errors.New("access denied")
	in.Region = &empty
	if updated, err = svc.Update(ctx, created.ID, in); err != nil || updated.Region != "" {
		t.Fatalf("cleared region with a failed lookup = %+v, %v; want unknown", updated, err)
	}
	long := string(make([]byte, models.MaxRegionLength+1))
	in.Region = &long
	if _, err = svc.Update(ctx, created.ID, in); !errors.Is(err, targets.ErrInvalid) {
		t.Fatalf("overlong region = %v; want ErrInvalid", err)
	}
}
