package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotls"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

const tlsTestKey = "-----BEGIN PRIVATE KEY-----\nkey-material-3c7a\n-----END PRIVATE KEY-----\n"

// tlsBackupMaterial is TLS material whose PEM contents are not parsed by the
// engine (the tools read them), so placeholders do.
func tlsBackupMaterial() *models.ConnectionTLS {
	return &models.ConnectionTLS{
		CAPEM:         "-----BEGIN CERTIFICATE-----\nca\n-----END CERTIFICATE-----\n",
		ClientCertPEM: "-----BEGIN CERTIFICATE-----\ncert\n-----END CERTIFICATE-----\n",
		ClientKeyPEM:  tlsTestKey,
	}
}

// tlsFilesRunner checks, while "mongodump" runs, that the TLS files it is given
// exist with private permissions, then ends as outcome says.
func tlsFilesRunner(t *testing.T, outcome string, seen *[]string) ProcessRunner {
	return func(_ context.Context, _ string, args ...string) (io.ReadCloser, io.Reader, func() error, error) {
		for _, a := range args {
			if strings.Contains(a, "key-material") {
				t.Errorf("argument carries key material: %q", a)
			}
			for _, flag := range []string{"--config=", "--sslCAFile=", "--sslPEMKeyFile="} {
				if path, ok := strings.CutPrefix(a, flag); ok {
					*seen = append(*seen, path)
					info, err := os.Stat(path)
					if err != nil {
						t.Errorf("%s missing while mongodump runs: %v", flag, err)
					} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
						t.Errorf("%s has mode %v", flag, info.Mode().Perm())
					}
				}
			}
		}
		switch outcome {
		case "panic":
			panic("mongodump runner panicked")
		case "error":
			return nil, nil, nil, errors.New("start failed")
		}
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
	}
}

// TestBackupTLSFilesAreRemovedOnEveryPath runs a backup over TLS that succeeds,
// fails and panics, and checks that the private files the tools got are gone
// afterwards each time.
func TestBackupTLSFilesAreRemovedOnEveryPath(t *testing.T) {
	for _, outcome := range []string{"success", "error", "panic"} {
		t.Run(outcome, func(t *testing.T) {
			dir := t.TempDir()
			var seen []string
			e := NewEngine(storage.NewMockStorage(), "", WithConfigDir(dir), WithRunner(tlsFilesRunner(t, outcome, &seen)))
			opts := models.BackupOptions{Database: "app", MongoURI: "mongodb://h/?authMechanism=MONGODB-X509", MongoTLS: tlsBackupMaterial()}
			func() {
				defer func() {
					if r := recover(); r != nil && outcome != "panic" {
						t.Fatalf("unexpected panic: %v", r)
					}
				}()
				rec, err := e.Run(context.Background(), opts)
				if outcome == "success" && (err != nil || rec.Status != models.StatusCompleted) {
					t.Fatalf("backup = %+v, %v", rec, err)
				}
			}()
			if len(seen) != 3 {
				t.Fatalf("mongodump got files %v; want config, CA and client certificate", seen)
			}
			for _, p := range seen {
				if filepath.Dir(filepath.Dir(p)) != dir {
					t.Fatalf("%s is not in a private directory of %s", p, dir)
				}
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Fatalf("left behind after %s: %v", outcome, entries)
			}
		})
	}
}

// TestBackupTLSReachesTheDriver checks that the driver-based steps of a backup
// (collection listing) see the connection's TLS material.
func TestBackupTLSReachesTheDriver(t *testing.T) {
	var got *models.ConnectionTLS
	lister := func(ctx context.Context, _, _ string) ([]string, error) {
		got = mongotls.FromContext(ctx)
		return []string{"a", "b", "c"}, nil
	}
	var seen []string
	e := NewEngine(storage.NewMockStorage(), "", WithConfigDir(t.TempDir()),
		WithRunner(tlsFilesRunner(t, "success", &seen)), WithCollectionLister(lister))
	material := tlsBackupMaterial()
	rec, err := e.Run(context.Background(), models.BackupOptions{Database: "app", Collections: []string{"a", "b"},
		MongoURI: "mongodb://h/", MongoTLS: material})
	if err != nil || rec.Status != models.StatusCompleted {
		t.Fatalf("backup = %+v, %v", rec, err)
	}
	if got == nil || got.ClientKeyPEM != material.ClientKeyPEM {
		t.Fatalf("collection lister got TLS %+v", got)
	}
	if r := (models.BackupOptions{MongoTLS: material}).Redacted(); strings.Contains(r.MongoTLS.ClientKeyPEM, "key-material") {
		t.Fatal("redacted options leak the client key")
	}
}

// TestBackupRunLogHoldsNoTLSSecret runs a backup with TLS material that succeeds
// and one that fails, and checks that the engine's log holds neither the client
// key nor its password.
func TestBackupRunLogHoldsNoTLSSecret(t *testing.T) {
	const password = "log-must-not-see-5b9e"
	for _, outcome := range []string{"success", "error"} {
		t.Run(outcome, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			var seen []string
			e := NewEngine(storage.NewMockStorage(), "", WithLogger(logger), WithConfigDir(t.TempDir()),
				WithRunner(tlsFilesRunner(t, outcome, &seen)))
			material := tlsBackupMaterial()
			material.ClientKeyPassword = password
			rec, err := e.Run(context.Background(), models.BackupOptions{Database: "app",
				MongoURI: "mongodb://h/?tls=true&authMechanism=MONGODB-X509", MongoTLS: material})
			out := buf.String()
			if err != nil {
				out += err.Error()
			}
			if rec != nil {
				out += rec.ErrorMessage
			}
			if strings.Contains(out, password) || strings.Contains(out, "key-material") {
				t.Fatalf("log or error holds a TLS secret:\n%s", out)
			}
			if !strings.Contains(buf.String(), "mongodb streaming backup") {
				t.Fatalf("no backup log captured:\n%s", buf.String())
			}
		})
	}
}
