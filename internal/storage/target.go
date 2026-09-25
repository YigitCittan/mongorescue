package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// NewForTarget builds the driver of a storage target. localPath is the resolved
// absolute directory of a local target (its stored path may be relative).
func NewForTarget(ctx context.Context, t *models.StorageTarget, localPath string) (Storage, error) {
	if t == nil {
		return nil, errors.New("storage: no target")
	}
	switch t.Type {
	case models.StorageLocal:
		return NewLocalStorage(localPath)
	case models.StorageS3:
		if t.S3 == nil {
			return nil, errors.New("storage: s3 settings are missing")
		}
		return NewS3Storage(ctx, S3Config{
			Endpoint:     t.S3.Endpoint,
			Bucket:       t.S3.Bucket,
			Region:       t.S3.Region,
			Prefix:       t.S3.Prefix,
			AccessKey:    t.S3.AccessKeyID,
			SecretKey:    t.S3.SecretAccessKey,
			UsePathStyle: t.S3.UsePathStyle,
		})
	}
	return nil, fmt.Errorf("storage: unsupported storage type %q", t.Type)
}
