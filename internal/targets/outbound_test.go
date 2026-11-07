package targets_test

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/netguard"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

func outboundInput(endpoint string) targets.Input {
	return targets.Input{Name: "s3", Type: models.StorageS3, S3: &models.S3Target{
		Endpoint: endpoint, Bucket: "backups", UsePathStyle: true,
	}}
}

func outboundService(t *testing.T, requireHTTPS *atomic.Bool, seenCA *atomic.Value) *targets.Service {
	t.Helper()
	mock := storage.NewMockStorage()
	factory := func(_ context.Context, tg *models.StorageTarget, localPath string) (storage.Storage, error) {
		if tg.Type == models.StorageLocal {
			return storage.NewLocalStorage(localPath)
		}
		if seenCA != nil {
			seenCA.Store(tg.S3.CAPEM)
		}
		return mock, nil
	}
	return targets.NewService(storetest.New(t), factory, filepath.Join(t.TempDir(), "data"),
		targets.WithRequireHTTPS(requireHTTPS.Load))
}

func TestRequireHTTPSRefusesHTTPEndpoints(t *testing.T) {
	var on atomic.Bool
	svc := outboundService(t, &on, nil)
	ctx := context.Background()

	if _, err := svc.Create(ctx, outboundInput("http://minio.internal:9000")); err != nil {
		t.Fatalf("http endpoint while https is not required: %v", err)
	}
	on.Store(true)
	for _, endpoint := range []string{"http://minio.internal:9000", "HTTP://minio.internal:9000"} {
		in := outboundInput(endpoint)
		in.Name = "other"
		_, err := svc.Create(ctx, in)
		if !errors.Is(err, targets.ErrInvalid) || !strings.Contains(err.Error(), "security.require_https_endpoints") {
			t.Errorf("%s: %v; want ErrInvalid naming the setting", endpoint, err)
		}
		if _, err = svc.TestInput(ctx, in, ""); !errors.Is(err, targets.ErrInvalid) {
			t.Errorf("%s: test connection = %v; want ErrInvalid", endpoint, err)
		}
	}
	in := outboundInput("https://minio.internal:9000")
	in.Name = "secure"
	in.S3.Bucket = "other-bucket"
	if _, err := svc.Create(ctx, in); err != nil {
		t.Fatalf("https endpoint: %v", err)
	}
}

func TestEndpointLiteralOfTheMetadataServiceIsRefused(t *testing.T) {
	svc := outboundService(t, &atomic.Bool{}, nil)
	for _, endpoint := range []string{"http://169.254.169.254", "https://[fd00:ec2::254]", "http://[64:ff9b::a9fe:a9fe]:80", "http://0x7f.1"} {
		_, err := svc.Create(context.Background(), outboundInput(endpoint))
		if !errors.Is(err, targets.ErrInvalid) || !errors.Is(err, netguard.ErrBlockedDestination) {
			t.Errorf("%s: %v; want ErrInvalid wrapping ErrBlockedDestination", endpoint, err)
		}
	}
}

func TestCAPEMIsValidatedAndReachesTheDriver(t *testing.T) {
	srv := httptest.NewTLSServer(nil)
	t.Cleanup(srv.Close)
	good := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))

	var seen atomic.Value
	svc := outboundService(t, &atomic.Bool{}, &seen)
	ctx := context.Background()

	for _, bad := range []string{"not pem", "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----"} {
		in := outboundInput("https://minio.internal:9000")
		in.S3.CAPEM = bad
		if _, err := svc.Create(ctx, in); !errors.Is(err, targets.ErrInvalid) || !errors.Is(err, storage.ErrInvalidCA) {
			t.Errorf("ca_pem %.12q: %v; want ErrInvalid wrapping ErrInvalidCA", bad, err)
		}
	}
	in := outboundInput("https://minio.internal:9000")
	in.S3.CAPEM = "\n" + good + "\n"
	created, err := svc.Create(ctx, in)
	if err != nil {
		t.Fatalf("Create with ca_pem: %v", err)
	}
	if created.S3.CAPEM != strings.TrimSpace(good) {
		t.Fatalf("stored ca_pem = %q", created.S3.CAPEM)
	}
	if _, err = svc.Storage(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := seen.Load().(string); got != strings.TrimSpace(good) {
		t.Fatalf("the driver factory saw ca_pem %q", got)
	}
	// Removing the bundle on update is possible.
	in.S3.CAPEM = ""
	updated, err := svc.Update(ctx, created.ID, in)
	if err != nil || updated.S3.CAPEM != "" {
		t.Fatalf("Update without ca_pem = %+v, %v", updated, err)
	}
}
