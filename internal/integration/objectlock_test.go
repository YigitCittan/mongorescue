//go:build integration

package integration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

// envLockBucket names a bucket with S3 Object Lock on the MinIO provider; the
// docker script sets it and the test creates it with Object Lock enabled (the
// equivalent of mc mb --with-lock).
const envLockBucket = envS3Prefix + "MINIO_LOCK_BUCKET"

// lockFixture is the MinIO provider with a lock-enabled bucket and a raw client.
type lockFixture struct {
	plain  storage.S3Config
	locked storage.S3Config
	client *s3.Client
}

func requireLockBucket(t *testing.T) *lockFixture {
	t.Helper()
	bucket := strings.TrimSpace(os.Getenv(envLockBucket))
	var minio *s3Provider
	for _, p := range configuredS3Providers() {
		if p.Name == "minio" {
			minio = &p
		}
	}
	if bucket == "" || minio == nil {
		t.Skipf("set %s and the MinIO provider to run the Object Lock tests", envLockBucket)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	cfg := minio.Config
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")))
	if err != nil {
		t.Fatal(err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint, o.UsePathStyle = aws.String(cfg.Endpoint), cfg.UsePathStyle
	})
	if minio.CreateBucket {
		ensureBucket(ctx, t, cfg)
		_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket), ObjectLockEnabledForBucket: aws.Bool(true)})
		var owned *s3types.BucketAlreadyOwnedByYou
		if err != nil && !errors.As(err, &owned) {
			t.Fatalf("create the lock bucket: %v", err)
		}
	}
	locked := cfg
	locked.Bucket, locked.Prefix, locked.PartSizeMB = bucket, "it-lock/"+randomHex(t, 6)+"/", models.MinS3PartSizeMB
	return &lockFixture{plain: cfg, locked: locked, client: client}
}

func (f *lockFixture) driver(t *testing.T, mode models.ObjectLockMode, days int) *storage.S3Storage {
	t.Helper()
	cfg := f.locked
	cfg.ObjectLock = storage.ObjectLock{Mode: mode, RetentionDays: days}
	st, err := storage.NewS3Storage(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// TestObjectLockOnMinIO proves on a real lock-enabled bucket that every upload,
// single-part and multipart, gets the target's lock mode and a retain-until date
// one retention after now, that S3 refuses to delete the version before then
// (MongoRescue's own purge waits, see the unit tests), that deleting the key only
// adds a delete marker the recorded version survives, and that legal holds toggle.
func TestObjectLockOnMinIO(t *testing.T) {
	f := requireLockBucket(t)
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	plain, err := storage.NewS3Storage(ctx, f.plain)
	if err != nil {
		t.Fatal(err)
	}
	if err = plain.CheckObjectLock(ctx); !errors.Is(err, storage.ErrObjectLockUnavailable) {
		t.Fatalf("check of a bucket without Object Lock = %v; want ErrObjectLockUnavailable", err)
	}

	for _, tc := range []struct {
		mode models.ObjectLockMode
		size int
		want s3types.ObjectLockMode
	}{
		{models.ObjectLockGovernance, 1 << 10, s3types.ObjectLockModeGovernance},
		{models.ObjectLockCompliance, multipartPayloadSize, s3types.ObjectLockModeCompliance},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			st := f.driver(t, tc.mode, 1)
			if err := st.CheckObjectLock(ctx); err != nil {
				t.Fatalf("check of the lock bucket: %v", err)
			}
			payload := bytes.Repeat([]byte("L"), tc.size)
			key := "archive-" + string(tc.mode)
			before := time.Now()
			obj, err := st.Save(ctx, key, io.MultiReader(bytes.NewReader(payload)))
			if err != nil {
				t.Fatalf("locked upload: %v", err)
			}
			if obj.VersionID == "" || obj.RetainUntil == nil || obj.ObjectLockMode != tc.mode {
				t.Fatalf("stored object = %+v; want a version and a %s retention", obj, tc.mode)
			}
			if d := obj.RetainUntil.Sub(before); d < 23*time.Hour || d > 25*time.Hour {
				t.Fatalf("retain-until %s is %s after the upload; want one day", obj.RetainUntil, d)
			}

			head, err := f.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(f.locked.Bucket),
				Key: aws.String(f.locked.Prefix + key), VersionId: aws.String(obj.VersionID)})
			if err != nil {
				t.Fatal(err)
			}
			if head.ObjectLockMode != tc.want || head.ObjectLockRetainUntilDate == nil {
				t.Fatalf("lock headers = %s until %v; want %s", head.ObjectLockMode, head.ObjectLockRetainUntilDate, tc.want)
			}

			// S3 itself refuses to delete the locked version.
			if err = st.DeleteVersion(ctx, key, obj.VersionID); err == nil || errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("deleting a locked version = %v; want a refusal", err)
			}
			if until, delErr := storage.DeleteUnlocked(ctx, st, key, time.Now()); delErr != nil || until == nil {
				t.Fatalf("DeleteUnlocked before the lock ends = %v, %v; want it kept", until, delErr)
			}

			// Legal hold on and off.
			for _, on := range []bool{true, false} {
				if err = st.SetLegalHold(ctx, key, obj.VersionID, on); err != nil {
					t.Fatalf("legal hold %v: %v", on, err)
				}
				hold, getErr := f.client.GetObjectLegalHold(ctx, &s3.GetObjectLegalHoldInput{Bucket: aws.String(f.locked.Bucket),
					Key: aws.String(f.locked.Prefix + key), VersionId: aws.String(obj.VersionID)})
				want := map[bool]s3types.ObjectLockLegalHoldStatus{true: s3types.ObjectLockLegalHoldStatusOn, false: s3types.ObjectLockLegalHoldStatusOff}[on]
				if getErr != nil || hold.LegalHold == nil || hold.LegalHold.Status != want {
					t.Fatalf("legal hold after %v = %+v, %v", on, hold, getErr)
				}
			}

			// Deleting the key adds a delete marker: the current object is gone but
			// the recorded version is still readable (and still billed).
			if err = st.Delete(ctx, key); err != nil {
				t.Fatalf("delete marker: %v", err)
			}
			if _, err = st.Retrieve(ctx, key); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("current object after the delete marker = %v; want ErrNotFound", err)
			}
			rc, err := storage.RetrieveVersion(ctx, st, key, obj.VersionID)
			if err != nil {
				t.Fatalf("read the locked version: %v", err)
			}
			got, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("locked version content differs (%d bytes, %v)", len(got), err)
			}
		})
	}
}

// TestObjectLockTargetsOnMinIO proves that the targets service refuses a locked
// target on a bucket without Object Lock, accepts one on a lock-enabled bucket,
// and that its connection test leaves no probe version behind.
func TestObjectLockTargetsOnMinIO(t *testing.T) {
	f := requireLockBucket(t)
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	svc := targets.NewService(storetest.New(t), storage.NewForTarget, t.TempDir())
	input := func(bucket string) targets.Input {
		return targets.Input{Name: "lock-" + bucket, Type: models.StorageS3, S3: &models.S3Target{
			Endpoint: f.plain.Endpoint, Region: f.plain.Region, Bucket: bucket, Prefix: f.locked.Prefix,
			AccessKeyID: f.plain.AccessKey, SecretAccessKey: f.plain.SecretKey, UsePathStyle: f.plain.UsePathStyle,
			ObjectLock: models.ObjectLockCompliance, RetentionDays: 1, LegalHoldOnPin: true,
		}}
	}
	if _, err := svc.Create(ctx, input(f.plain.Bucket)); !errors.Is(err, targets.ErrInvalid) || !strings.Contains(err.Error(), "Object Lock") {
		t.Fatalf("locked target on a bucket without Object Lock = %v; want ErrInvalid naming Object Lock", err)
	}
	created, err := svc.Create(ctx, input(f.locked.Bucket))
	if err != nil {
		t.Fatalf("locked target on the lock bucket: %v", err)
	}
	if res, testErr := svc.Test(ctx, created.ID); testErr != nil || !res.OK {
		t.Fatalf("test = %+v, %v", res, testErr)
	}
	versions, err := f.client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(f.locked.Bucket),
		Prefix: aws.String(f.locked.Prefix + targets.ProbePrefix)})
	if err != nil {
		t.Fatal(err)
	}
	if len(versions.Versions) != 0 || len(versions.DeleteMarkers) != 0 {
		t.Fatalf("the probe left %d versions and %d delete markers", len(versions.Versions), len(versions.DeleteMarkers))
	}
}

// TestObjectLockBackupRestoreOnMinIO runs a real backup to a locked target: the
// record carries the version and the retention, and a restore reads the recorded
// version even after the key got a delete marker.
func TestObjectLockBackupRestoreOnMinIO(t *testing.T) {
	f := requireLockBucket(t)
	env := requireMongo(t)
	st := f.driver(t, models.ObjectLockGovernance, 1)
	db := seedSource(t, env, "lock")
	bkp := runBackup(t, env, st, db, true)
	if bkp.StorageVersionID == "" || bkp.RetainUntil == nil || bkp.ObjectLockMode != models.ObjectLockGovernance {
		t.Fatalf("backup record = version %q, retain until %v, mode %q; want all set", bkp.StorageVersionID, bkp.RetainUntil, bkp.ObjectLockMode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if err := st.Delete(ctx, bkp.StorageKey); err != nil {
		t.Fatal(err)
	}
	rst := runRestore(t, env, st, models.RestoreRequest{BackupID: bkp.ID}, bkp)
	if got := env.count(t, rst.TargetDatabase, "orders"); got != ordersCount {
		t.Fatalf("restored orders = %d; want %d", got, ordersCount)
	}
}
