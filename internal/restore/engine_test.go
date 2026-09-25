package restore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

func TestRestoreEngineSafeClone(t *testing.T) {
	mockStore := storage.NewMockStorage()
	ctx := context.Background()

	backupPayload := []byte("fake-compressed-archive-bytes")
	storageKey := "mydb/2026/09/bkp_mydb_20260924_120000.archive.gz"
	_, _ = mockStore.Save(ctx, storageKey, bytes.NewReader(backupPayload))

	sourceRecord := &models.BackupRecord{
		ID:         "bkp_mydb_20260924_120000",
		Database:   "ecommerce_prod",
		StorageKey: storageKey,
	}

	var capturedArgs []string
	customRunner := func(_ context.Context, _ string, stdin io.Reader, args ...string) (io.Reader, func() error, error) {
		capturedArgs = args
		// Ensure stream can be read
		data, _ := io.ReadAll(stdin)
		if !bytes.Equal(data, backupPayload) {
			t.Errorf("expected stream data %q, got %q", backupPayload, data)
		}
		stderr := strings.NewReader("restoring into safe clone namespace\n")
		wait := func() error { return nil }
		return stderr, wait, nil
	}

	engine := NewEngine(mockStore, "mongodb://localhost:27017", WithRunner(customRunner))

	record, err := engine.Run(ctx, models.RestoreRequest{
		BackupID: sourceRecord.ID,
	}, sourceRecord)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if record.Status != models.RestoreStatusCompleted {
		t.Errorf("expected completed, got %s", record.Status)
	}

	// Verify safe clone namespace format
	if !strings.HasPrefix(record.TargetDatabase, "ecommerce_prod_rescue_") {
		t.Errorf("expected target database to start with ecommerce_prod_rescue_, got %s", record.TargetDatabase)
	}

	// Verify mongorestore args contained namespace rewrite
	hasFrom := false
	hasTo := false
	for _, arg := range capturedArgs {
		if strings.HasPrefix(arg, "--nsFrom=ecommerce_prod.*") {
			hasFrom = true
		}
		if strings.HasPrefix(arg, "--nsTo="+record.TargetDatabase+".*") {
			hasTo = true
		}
	}

	if !hasFrom || !hasTo {
		t.Errorf("expected namespace rewrite args in %v", capturedArgs)
	}
}

func TestRestoreEngineDryRunAndDrop(t *testing.T) {
	mockStore := storage.NewMockStorage()
	ctx := context.Background()

	storageKey := "mydb/archive.gz"
	_, _ = mockStore.Save(ctx, storageKey, strings.NewReader("archive-content"))

	sourceRecord := &models.BackupRecord{
		ID:         "bkp_1",
		Database:   "mydb",
		StorageKey: storageKey,
		SHA256:     sha256Hex("archive-content"), // non-safe-clone restores verify by default
	}

	var capturedArgs []string
	customRunner := func(_ context.Context, _ string, _ io.Reader, args ...string) (io.Reader, func() error, error) {
		capturedArgs = args
		return strings.NewReader("dry run complete\n"), func() error { return nil }, nil
	}

	engine := NewEngine(mockStore, "mongodb://localhost:27017", WithRunner(customRunner))

	inPlace := false
	req := models.RestoreRequest{
		BackupID:            sourceRecord.ID,
		TargetDatabase:      "mydb",
		SafeClone:           &inPlace,
		ConfirmInPlace:      true,
		DropTarget:          true,
		DryRun:              true,
		SelectedCollections: []string{"users", "orders"},
	}

	record, err := engine.Run(ctx, req, sourceRecord)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !record.DryRun {
		t.Error("expected DryRun true in record")
	}

	argMap := make(map[string]bool)
	for _, arg := range capturedArgs {
		argMap[arg] = true
	}

	if !argMap["--dryRun"] {
		t.Error("expected --dryRun flag")
	}
	if !argMap["--drop"] {
		t.Error("expected --drop flag")
	}
	if !argMap["--nsInclude=mydb.users"] {
		t.Error("expected --nsInclude=mydb.users")
	}
	if !argMap["--nsInclude=mydb.orders"] {
		t.Error("expected --nsInclude=mydb.orders")
	}
}

func TestRestoreEngineStorageNotFound(t *testing.T) {
	mockStore := storage.NewMockStorage()
	engine := NewEngine(mockStore, "mongodb://localhost:27017")

	ctx := context.Background()
	sourceRecord := &models.BackupRecord{
		ID:         "nonexistent",
		Database:   "mydb",
		StorageKey: "missing.archive",
		SHA256:     sha256Hex("irrelevant"),
	}

	_, err := engine.Run(ctx, models.RestoreRequest{
		BackupID: "nonexistent",
	}, sourceRecord)

	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestRestoreEngineSubprocessError(t *testing.T) {
	mockStore := storage.NewMockStorage()
	ctx := context.Background()

	storageKey := "mydb/archive.gz"
	_, _ = mockStore.Save(ctx, storageKey, strings.NewReader("archive-content"))

	sourceRecord := &models.BackupRecord{
		ID:         "bkp_1",
		Database:   "mydb",
		StorageKey: storageKey,
		SHA256:     sha256Hex("archive-content"),
	}

	customRunner := func(_ context.Context, _ string, _ io.Reader, _ ...string) (io.Reader, func() error, error) {
		stderr := strings.NewReader("error connecting to host: connection refused\n")
		wait := func() error { return errors.New("exit status 1") }
		return stderr, wait, nil
	}

	engine := NewEngine(mockStore, "mongodb://localhost:27017", WithRunner(customRunner))

	record, err := engine.Run(ctx, models.RestoreRequest{
		BackupID: sourceRecord.ID,
	}, sourceRecord)

	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if record.Status != models.RestoreStatusFailed {
		t.Errorf("expected status failed, got %s", record.Status)
	}
}

func TestRestoreEngineKeepsURIOutOfArgvAndRedactsStderr(t *testing.T) {
	const uri = "mongodb://admin:topsecret@db.internal:27017/"
	mockStore := storage.NewMockStorage()
	ctx := context.Background()
	_, _ = mockStore.Save(ctx, "mydb/archive.gz", strings.NewReader("archive"))
	sourceRecord := &models.BackupRecord{ID: "bkp_1", Database: "mydb", StorageKey: "mydb/archive.gz", SHA256: sha256Hex("archive")}

	var (
		capturedArgs []string
		configPath   string
		configData   []byte
	)
	runner := func(_ context.Context, _ string, _ io.Reader, args ...string) (io.Reader, func() error, error) {
		capturedArgs = args
		for _, a := range args {
			if p, ok := strings.CutPrefix(a, "--config="); ok {
				configPath = p
				configData, _ = os.ReadFile(p)
			}
		}
		stderr := strings.NewReader("failed mongodb://a:pw1@h1/ then mongodb://b:pw2@h2/")
		return stderr, func() error { return errors.New("exit status 1") }, nil
	}

	engine := NewEngine(mockStore, "mongodb://unused", WithRunner(runner))
	record, err := engine.Run(ctx, models.RestoreRequest{BackupID: "bkp_1", MongoURI: uri}, sourceRecord)
	if err == nil {
		t.Fatal("expected failure")
	}

	for _, a := range capturedArgs {
		if strings.Contains(a, "topsecret") || strings.HasPrefix(a, "--uri") {
			t.Fatalf("connection URI leaked into argv: %v", capturedArgs)
		}
	}
	if configPath == "" || !strings.Contains(string(configData), uri) {
		t.Fatalf("expected --config file carrying the URI, args=%v data=%q", capturedArgs, configData)
	}
	if _, statErr := os.Stat(configPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("expected config file removed after run, stat err = %v", statErr)
	}
	for _, secret := range []string{"pw1", "pw2"} {
		if strings.Contains(err.Error(), secret) || strings.Contains(record.ErrorMessage, secret) {
			t.Fatalf("secret %q leaked: err=%q msg=%q", secret, err.Error(), record.ErrorMessage)
		}
	}
}

func TestRestoreIDsAreUniqueWithinASecond(t *testing.T) {
	e := NewEngine(storage.NewMockStorage(), "mongodb://h")
	seen := map[string]bool{}
	src := &models.BackupRecord{ID: "bkp_1", Database: strings.Repeat("d", 80)}
	for range 50 {
		rec, err := e.Prepare(models.RestoreRequest{BackupID: "bkp_1"}, src)
		if err != nil {
			t.Fatal(err)
		}
		if err := models.ValidateID(rec.ID); err != nil || seen[rec.ID] {
			t.Fatalf("restore id %q invalid or reused (%v)", rec.ID, err)
		}
		seen[rec.ID] = true
	}
}
