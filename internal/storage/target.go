package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// ErrNoVaultCredentials is returned when a target reads its access keys from
// Vault but no resolver of them is configured.
var ErrNoVaultCredentials = errors.New("storage: the access keys of this target are read from Vault, which is not configured (MONGORESCUE_VAULT_ADDR)")

// VaultCredentials returns the credentials provider of the access keys ref names
// in Vault (implemented by vaultcreds.Resolver.S3Provider).
type VaultCredentials func(ref models.VaultRef) aws.CredentialsProvider

// NewForTarget builds the driver of a storage target with the DefaultStallTimeout.
// localPath is the resolved absolute directory of a local target (its stored path may
// be relative).
func NewForTarget(ctx context.Context, t *models.StorageTarget, localPath string) (Storage, error) {
	return TargetFactory(nil)(ctx, t, localPath)
}

// TargetFactory returns a NewForTarget whose drivers read the upload stall timeout
// from stallTimeout at the start of every upload (nil or a non-positive value means
// DefaultStallTimeout).
func TargetFactory(stallTimeout func() time.Duration) func(ctx context.Context, t *models.StorageTarget, localPath string) (Storage, error) {
	return TargetFactoryWith(stallTimeout, nil)
}

// TargetFactoryWith is TargetFactory whose S3 drivers read the access keys of
// targets with credential_source vault through vault (nil refuses such targets
// with ErrNoVaultCredentials).
func TargetFactoryWith(stallTimeout func() time.Duration, vault VaultCredentials) func(ctx context.Context, t *models.StorageTarget, localPath string) (Storage, error) {
	return func(ctx context.Context, t *models.StorageTarget, localPath string) (Storage, error) {
		return newForTarget(ctx, t, localPath, stallTimeout, vault)
	}
}

// newForTarget builds the driver of t.
func newForTarget(ctx context.Context, t *models.StorageTarget, localPath string, stallTimeout func() time.Duration, vault VaultCredentials) (Storage, error) {
	if t == nil {
		return nil, errors.New("storage: no target")
	}
	stall := StallWatch{Target: t.Name, Timeout: stallTimeout}
	switch t.Type {
	case models.StorageLocal:
		local, err := NewLocalStorage(localPath)
		if err != nil {
			return nil, err
		}
		return local.WithStallWatch(stall), nil
	case models.StorageS3:
		if t.S3 == nil {
			return nil, errors.New("storage: s3 settings are missing")
		}
		cfg := S3Config{
			Endpoint:     t.S3.Endpoint,
			Bucket:       t.S3.Bucket,
			Region:       t.S3.Region,
			Prefix:       t.S3.Prefix,
			AccessKey:    t.S3.AccessKeyID,
			SecretKey:    t.S3.SecretAccessKey,
			UsePathStyle: t.S3.UsePathStyle,
			PartSizeMB:   t.S3.PartSizeMB,
			ObjectLock:   ObjectLock{Mode: t.S3.ObjectLock, RetentionDays: t.S3.RetentionDays},
			Stall:        stall,
		}
		if t.S3.FromVault() {
			if vault != nil {
				cfg.Credentials = vault(t.S3.Vault)
			}
			if cfg.Credentials == nil {
				return nil, ErrNoVaultCredentials
			}
			// Never the default credential chain: without keys from Vault every
			// request fails.
			cfg.AccessKey, cfg.SecretKey = "", ""
		}
		return NewS3Storage(ctx, cfg)
	}
	return nil, fmt.Errorf("storage: unsupported storage type %q", t.Type)
}
