package targets_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

func vaultS3Input(name string, ref models.VaultRef) targets.Input {
	return targets.Input{Name: name, Type: models.StorageS3, S3: &models.S3Target{
		Endpoint: "https://minio.example.com", Bucket: "vault-bucket", UsePathStyle: true,
		CredentialSource: models.CredentialsVault, Vault: ref,
	}}
}

func newVaultTargets(t *testing.T, configured bool) *targets.Service {
	t.Helper()
	mock := storage.NewMockStorage()
	factory := func(_ context.Context, tg *models.StorageTarget, localPath string) (storage.Storage, error) {
		if tg.Type == models.StorageLocal {
			return storage.NewLocalStorage(localPath)
		}
		return mock, nil
	}
	return targets.NewService(storetest.New(t), factory, filepath.Join(t.TempDir(), "data"), targets.WithVault(configured))
}

func TestVaultS3TargetValidation(t *testing.T) {
	ctx := context.Background()
	ref := models.VaultRef{Engine: models.VaultKV2, Path: "minio"}
	if _, err := newVaultTargets(t, false).Create(ctx, vaultS3Input("v", ref)); !errors.Is(err, targets.ErrInvalid) ||
		!strings.Contains(err.Error(), "MONGORESCUE_VAULT_ADDR") {
		t.Fatalf("without Vault = %v", err)
	}
	svc := newVaultTargets(t, true)
	withKeys := vaultS3Input("v", ref)
	withKeys.S3.AccessKeyID, withKeys.S3.SecretAccessKey = "AKID", "stored-secret"
	if _, err := svc.Create(ctx, withKeys); !errors.Is(err, targets.ErrInvalid) {
		t.Fatalf("vault with stored keys = %v", err)
	}
	if _, err := svc.Create(ctx, vaultS3Input("v", models.VaultRef{Engine: models.VaultDatabase, Role: "r"})); !errors.Is(err, targets.ErrInvalid) {
		t.Fatalf("database engine for S3 = %v", err)
	}
	if _, err := svc.Create(ctx, vaultS3Input("v", models.VaultRef{Engine: models.VaultKV2, Path: "../sys/x"})); !errors.Is(err, targets.ErrInvalid) {
		t.Fatalf("path traversal = %v", err)
	}
	created, err := svc.Create(ctx, vaultS3Input("v", ref))
	if err != nil {
		t.Fatal(err)
	}
	if !created.FromVault() || created.S3.Vault.Mount != "secret" || created.S3.Vault.AccessKeyField != "access_key" {
		t.Fatalf("created = %+v", created.S3)
	}
	raw, _ := json.Marshal(created)
	if !strings.Contains(string(raw), `"credential_source":"vault"`) || strings.Contains(string(raw), "secret_access_key") {
		t.Fatalf("response = %s", raw)
	}
	if _, err = svc.RotateCredentials(ctx, created.ID, targets.Credentials{AccessKeyID: "A", SecretAccessKey: "B"}); !errors.Is(err, targets.ErrInvalid) {
		t.Fatalf("rotation of keys from Vault = %v", err)
	}
}

func TestSwitchingAStoredTargetToVaultDropsItsKey(t *testing.T) {
	ctx := context.Background()
	svc := newVaultTargets(t, true)
	in := s3Input("s", "bucket-s", "stored-secret-1")
	created, err := svc.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	// The form sends the masked key back with the switch; the key goes with it.
	in.S3.AccessKeyID, in.S3.SecretAccessKey = "", models.SecretMask
	in.S3.CredentialSource, in.S3.Vault = models.CredentialsVault, models.VaultRef{Engine: models.VaultAWS, Role: "backup"}
	updated, err := svc.Update(ctx, created.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	full, err := svc.Resolve(ctx, created.ID)
	if err != nil || !full.FromVault() || full.S3.SecretAccessKey != "" || full.S3.AccessKeyID != "" || updated.S3.Vault.Mount != "aws" {
		t.Fatalf("switched = %+v, %v", full.S3, err)
	}
}

func TestStorageFactoryReadsKeysFromVault(t *testing.T) {
	ctx := context.Background()
	tg := &models.StorageTarget{Name: "v", Type: models.StorageS3, S3: &models.S3Target{Bucket: "vault-bucket",
		CredentialSource: models.CredentialsVault, Vault: models.VaultRef{Engine: models.VaultKV2, Mount: "secret", Path: "minio"}}}
	if _, err := storage.TargetFactory(nil)(ctx, tg, ""); !errors.Is(err, storage.ErrNoVaultCredentials) {
		t.Fatalf("without Vault = %v", err)
	}
	if _, err := storage.TargetFactoryWith(nil, func(models.VaultRef) aws.CredentialsProvider { return nil })(ctx, tg, ""); !errors.Is(err, storage.ErrNoVaultCredentials) {
		t.Fatalf("without a provider = %v", err)
	}
	var asked models.VaultRef
	factory := storage.TargetFactoryWith(nil, func(ref models.VaultRef) aws.CredentialsProvider {
		asked = ref
		return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{}, errors.New("not reached")
		})
	})
	if _, err := factory(ctx, tg, ""); err != nil || asked.Path != "minio" {
		t.Fatalf("with Vault = %v (asked %+v)", err, asked)
	}
}
