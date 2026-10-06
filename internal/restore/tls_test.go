package restore

import (
	"bytes"
	"context"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// TestRestoreTLSFilesArePassedAndRemoved restores over TLS: mongorestore gets the
// CA and client certificate files and the key password through --config, and the
// private directory is gone afterwards.
func TestRestoreTLSFilesArePassedAndRemoved(t *testing.T) {
	mock := storage.NewMockStorage()
	if _, err := mock.Save(context.Background(), "k/bkp.archive", bytes.NewReader([]byte("archive"))); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var args []string
	var config string
	runner := func(_ context.Context, _ string, stdin io.Reader, a ...string) (io.Reader, func() error, error) {
		_, _ = io.Copy(io.Discard, stdin)
		args = slices.Clone(a)
		for _, arg := range a {
			if path, ok := strings.CutPrefix(arg, "--config="); ok {
				b, err := os.ReadFile(path)
				if err != nil {
					t.Errorf("config missing while mongorestore runs: %v", err)
				}
				config = string(b)
			}
		}
		return strings.NewReader(""), func() error { return nil }, nil
	}
	e := NewEngine(mock, "", WithConfigDir(dir), WithRunner(runner))
	src := &models.BackupRecord{ID: "bkp_1", Database: "app", StorageKey: "k/bkp.archive", Status: models.StatusCompleted}
	req := models.RestoreRequest{BackupID: src.ID, MongoURI: "mongodb://h/?authMechanism=MONGODB-X509",
		MongoTLS: &models.ConnectionTLS{
			CAPEM:             "-----BEGIN CERTIFICATE-----\nca\n-----END CERTIFICATE-----\n",
			ClientCertPEM:     "-----BEGIN CERTIFICATE-----\ncert\n-----END CERTIFICATE-----\n",
			ClientKeyPEM:      "-----BEGIN ENCRYPTED PRIVATE KEY-----\nkey\n-----END ENCRYPTED PRIVATE KEY-----\n",
			ClientKeyPassword: "pem-pass-81f",
		}}
	rec, err := e.Run(context.Background(), req, src)
	if err != nil || rec.Status != models.RestoreStatusCompleted {
		t.Fatalf("restore = %+v, %v", rec, err)
	}
	if !slices.Contains(args, "--ssl") || !slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--sslCAFile=") }) ||
		!slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--sslPEMKeyFile=") }) {
		t.Fatalf("args = %v", args)
	}
	if strings.Contains(strings.Join(args, " "), "pem-pass-81f") || !strings.Contains(config, "sslPEMKeyPassword: 'pem-pass-81f'") {
		t.Fatalf("password not passed through --config only: args %v, config %q", args, config)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left behind %v", entries)
	}
}
