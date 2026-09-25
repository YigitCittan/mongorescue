package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

func TestBackupEngineSuccess(t *testing.T) {
	mockStore := storage.NewMockStorage()
	mockPayload := []byte("fake-mongodump-archive-stream-bytes-12345")

	expectedHashBytes := sha256.Sum256(mockPayload)
	expectedHash := hex.EncodeToString(expectedHashBytes[:])

	customRunner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		stdout := io.NopCloser(bytes.NewReader(mockPayload))
		stderr := strings.NewReader("dumping mydb.users 100 documents\n")
		wait := func() error { return nil }
		return stdout, stderr, wait, nil
	}

	engine := NewEngine(mockStore, "mongodb://localhost:27017", WithRunner(customRunner))

	ctx := context.Background()
	record, err := engine.Run(ctx, models.BackupOptions{
		Database: "analytics_prod",
		Gzip:     true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if record.Status != models.StatusCompleted {
		t.Errorf("expected status completed, got %s", record.Status)
	}
	if record.SizeBytes != int64(len(mockPayload)) {
		t.Errorf("expected size %d, got %d", len(mockPayload), record.SizeBytes)
	}
	if record.SHA256 != expectedHash {
		t.Errorf("expected sha256 %s, got %s", expectedHash, record.SHA256)
	}

	// Verify data stored in mock storage
	stored, err := mockStore.Retrieve(ctx, record.StorageKey)
	if err != nil {
		t.Fatalf("failed to retrieve stored backup: %v", err)
	}
	defer stored.Close()

	data, err := io.ReadAll(stored)
	if err != nil {
		t.Fatalf("failed to read stored backup data: %v", err)
	}
	if !bytes.Equal(data, mockPayload) {
		t.Errorf("expected payload %q, got %q", mockPayload, data)
	}
}

func TestBackupEngineSubprocessFailure(t *testing.T) {
	mockStore := storage.NewMockStorage()

	customRunner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		stdout := io.NopCloser(bytes.NewReader([]byte("partial-data")))
		stderr := strings.NewReader("Failed: connection error: auth failed for user admin:secretPassword123\n")
		wait := func() error { return errors.New("exit status 1") }
		return stdout, stderr, wait, nil
	}

	engine := NewEngine(mockStore, "mongodb://localhost:27017", WithRunner(customRunner))

	ctx := context.Background()
	record, err := engine.Run(ctx, models.BackupOptions{
		Database: "analytics_prod",
	})
	if err == nil {
		t.Fatal("expected error from failed subprocess, got nil")
	}

	if record.Status != models.StatusFailed {
		t.Errorf("expected status failed, got %s", record.Status)
	}

	// Verify partial file was cleaned up from storage
	_, err = mockStore.Retrieve(ctx, record.StorageKey)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("expected stored file to be cleaned up after failure, got %v", err)
	}
}

func TestBackupEngineValidation(t *testing.T) {
	mockStore := storage.NewMockStorage()
	engine := NewEngine(mockStore, "")

	ctx := context.Background()

	// Missing database
	_, err := engine.Run(ctx, models.BackupOptions{
		Database: "",
	})
	if err == nil {
		t.Error("expected error for empty database, got nil")
	}

	// Missing Mongo URI
	_, err = engine.Run(ctx, models.BackupOptions{
		Database: "mydb",
		MongoURI: "",
	})
	if err == nil {
		t.Error("expected error for empty Mongo URI, got nil")
	}
}

func TestBackupEngineKeepsURIOutOfArgv(t *testing.T) {
	const uri = "mongodb://admin:topsecret@db.internal:27017/?authSource=admin"
	var (
		capturedArgs []string
		configPath   string
		configData   []byte
		configMode   os.FileMode
	)

	runner := func(_ context.Context, _ string, args ...string) (io.ReadCloser, io.Reader, func() error, error) {
		capturedArgs = args
		for _, a := range args {
			if p, ok := strings.CutPrefix(a, "--config="); ok {
				configPath = p
				if info, err := os.Stat(p); err == nil {
					configMode = info.Mode().Perm()
				}
				configData, _ = os.ReadFile(p)
			}
		}
		return io.NopCloser(bytes.NewReader([]byte("archive"))), strings.NewReader(""), func() error { return nil }, nil
	}

	engine := NewEngine(storage.NewMockStorage(), "mongodb://unused", WithRunner(runner))
	if _, err := engine.Run(context.Background(), models.BackupOptions{Database: "db", MongoURI: uri}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, a := range capturedArgs {
		if strings.Contains(a, "topsecret") || strings.HasPrefix(a, "--uri") {
			t.Fatalf("connection URI leaked into argv: %v", capturedArgs)
		}
	}
	if configPath == "" {
		t.Fatalf("expected --config argument, got %v", capturedArgs)
	}
	if !strings.Contains(string(configData), uri) {
		t.Fatalf("config file does not carry the URI: %q", configData)
	}
	if runtime.GOOS != "windows" && configMode != 0o600 {
		t.Fatalf("expected config file mode 0600, got %v", configMode)
	}
	if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected config file removed after run, stat err = %v", err)
	}
}

func TestBackupEngineRedactsAllURIsInStderr(t *testing.T) {
	runner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		stderr := strings.NewReader("auth failed for mongodb://a:pw1@h1/ and mongodb://b:pw2@h2/?password=pw3")
		return io.NopCloser(bytes.NewReader(nil)), stderr, func() error { return errors.New("exit status 1") }, nil
	}

	engine := NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", WithRunner(runner))
	record, err := engine.Run(context.Background(), models.BackupOptions{Database: "db"})
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, secret := range []string{"pw1", "pw2", "pw3"} {
		if strings.Contains(err.Error(), secret) || strings.Contains(record.ErrorMessage, secret) {
			t.Fatalf("secret %q leaked: err=%q msg=%q", secret, err.Error(), record.ErrorMessage)
		}
	}
}
