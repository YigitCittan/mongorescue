package targets_test

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

var errAccessDenied = errors.New("AccessDenied: not allowed")

// policyStorage enforces what the access key of its target may do: "readonly"
// keys can only read, "nodelete" keys cannot delete.
type policyStorage struct {
	*storage.MockStorage
	key string
}

func (p *policyStorage) Save(ctx context.Context, key string, r io.Reader) (*models.StorageObject, error) {
	if p.key == "readonly" {
		return nil, errAccessDenied
	}
	return p.MockStorage.Save(ctx, key, r)
}

func (p *policyStorage) Delete(ctx context.Context, key string) error {
	if p.key == "readonly" || p.key == "nodelete" {
		return errAccessDenied
	}
	return p.MockStorage.Delete(ctx, key)
}

func rotationService(t *testing.T) (*targets.Service, *models.StorageTarget, *storage.MockStorage) {
	t.Helper()
	bucket := storage.NewMockStorage()
	factory := func(_ context.Context, tg *models.StorageTarget, localPath string) (storage.Storage, error) {
		if tg.S3 == nil {
			return storage.NewLocalStorage(localPath)
		}
		return &policyStorage{MockStorage: bucket, key: tg.S3.AccessKeyID}, nil
	}
	svc := targets.NewService(storetest.New(t), factory, filepath.Join(t.TempDir(), "data"), targets.WithTestTimeout(2*time.Second))
	tg, err := svc.Create(context.Background(), s3Input("Offsite", "offsite-bucket", "old-secret"))
	if err != nil {
		t.Fatal(err)
	}
	return svc, tg, bucket
}

func TestRotateCredentialsSwapsAfterTheProbes(t *testing.T) {
	ctx := context.Background()
	svc, tg, bucket := rotationService(t)
	res, err := svc.RotateCredentials(ctx, tg.ID, targets.Credentials{AccessKeyID: "AKNEW", SecretAccessKey: "new-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Steps) != 4 || res.Target == nil || res.Target.S3.SecretAccessKey != models.SecretMask {
		t.Fatalf("result = %+v", res)
	}
	full, _ := svc.Resolve(ctx, tg.ID)
	if full.S3.AccessKeyID != "AKNEW" || full.S3.SecretAccessKey != "new-secret" {
		t.Fatal("the credentials were not swapped")
	}
	if objs, _ := bucket.List(ctx, ""); len(objs) != 0 {
		t.Fatalf("probe objects left behind: %d", len(objs))
	}
}

func TestRotateCredentialsRefusesWeakCredentials(t *testing.T) {
	ctx := context.Background()
	for key, step := range map[string]string{"readonly": "write", "nodelete": "delete"} {
		t.Run(key, func(t *testing.T) {
			svc, tg, _ := rotationService(t)
			res, err := svc.RotateCredentials(ctx, tg.ID, targets.Credentials{AccessKeyID: key, SecretAccessKey: "s3cr3t-value"})
			if !errors.Is(err, targets.ErrProbeFailed) {
				t.Fatalf("rotate = %v; want ErrProbeFailed", err)
			}
			last := res.Steps[len(res.Steps)-1]
			if last.Name != step || last.OK || strings.Contains(last.Error, "s3cr3t-value") {
				t.Fatalf("failing step = %+v; want %s", last, step)
			}
			full, _ := svc.Resolve(ctx, tg.ID)
			if full.S3.AccessKeyID != "AKID" || full.S3.SecretAccessKey != "old-secret" {
				t.Fatal("the credentials changed although a probe failed")
			}
		})
	}
}

func TestRotateCredentialsValidates(t *testing.T) {
	ctx := context.Background()
	svc, tg, _ := rotationService(t)
	for _, c := range []targets.Credentials{{}, {AccessKeyID: "A"}, {AccessKeyID: "A", SecretAccessKey: models.SecretMask},
		{AccessKeyID: "AKID", SecretAccessKey: "old-secret"}} {
		if _, err := svc.RotateCredentials(ctx, tg.ID, c); !errors.Is(err, targets.ErrInvalid) {
			t.Fatalf("%+v: %v; want ErrInvalid", c, err)
		}
	}
	local, _ := svc.Create(ctx, targets.Input{Name: "Local", Type: models.StorageLocal, Local: &models.LocalTarget{Path: t.TempDir()}})
	if _, err := svc.RotateCredentials(ctx, local.ID, targets.Credentials{AccessKeyID: "A", SecretAccessKey: "B"}); !errors.Is(err, targets.ErrInvalid) {
		t.Fatalf("local target: %v; want ErrInvalid", err)
	}
}
