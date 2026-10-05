package targets_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

// TestS3PartSize proves part_size_mb is validated (5 to 512 MiB), defaults to 16 MiB,
// reaches the driver factory and can change on a target in use without a test.
func TestS3PartSize(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, size := range []int{-1, 1, 4, 513, 4096} {
		in := s3Input("x", "bucket", "k")
		in.S3.PartSizeMB = size
		if _, err := f.svc.Create(ctx, in); !errors.Is(err, targets.ErrInvalid) || !strings.Contains(err.Error(), "part_size_mb") {
			t.Errorf("part_size_mb %d = %v; want ErrInvalid naming part_size_mb", size, err)
		}
	}
	created, err := f.svc.Create(ctx, s3Input("default", "bucket-a", "k"))
	if err != nil {
		t.Fatal(err)
	}
	if created.S3.PartSizeMB != models.DefaultS3PartSizeMB {
		t.Fatalf("default part size = %d; want %d", created.S3.PartSizeMB, models.DefaultS3PartSizeMB)
	}
	now := time.Now().UTC()
	rec := &models.BackupRecord{ID: "b", Status: models.StatusCompleted, StorageTargetID: created.ID, StartedAt: now}
	if err = f.store.SaveBackupRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	// The part size changes neither the location nor the credentials: a target in
	// use takes it even while its bucket cannot be reached.
	f.failS3.Store(true)
	in := s3Input("default", "bucket-a", models.SecretMask)
	in.S3.PartSizeMB = 64
	updated, err := f.svc.Update(ctx, created.ID, in)
	if err != nil {
		t.Fatalf("changing the part size of a target in use = %v", err)
	}
	if updated.S3.PartSizeMB != 64 || updated.MaxArchiveBytes() != 64<<20*models.S3MaxUploadParts {
		t.Fatalf("updated = %+v", updated.S3)
	}
	var seen int
	factory := func(_ context.Context, tg *models.StorageTarget, _ string) (storage.Storage, error) {
		seen = tg.S3.PartSizeMB
		return storage.NewMockStorage(), nil
	}
	svc := targets.NewService(f.store, factory, filepath.Join(f.base, "data"))
	if _, err = svc.Storage(ctx, created.ID); err != nil || seen != 64 {
		t.Fatalf("driver built with part size %d (%v); want 64", seen, err)
	}
}
