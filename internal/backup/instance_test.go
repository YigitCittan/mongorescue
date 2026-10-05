package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

func instanceOptions() models.BackupOptions {
	return models.BackupOptions{
		Scope: models.ScopeInstance, ConnectionID: "conn1", ReplicaSet: "rs0", PITRStreamID: "str1",
		MongoURI: "mongodb://localhost:27017/?replicaSet=rs0", Trigger: models.TriggerScheduled,
	}
}

func testEncryptor(t *testing.T) *encryption.Encryptor {
	t.Helper()
	_, r := newKeyPair(t)
	enc, err := encryption.NewX25519Encryptor([]string{r})
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

// opTimes returns an OpTimeReader that hands out the given optimes in order.
func opTimes(ops ...pitr.OpTime) (OpTimeReader, *int) {
	var mu sync.Mutex
	calls := 0
	return func(context.Context, string) (pitr.OpTime, error) {
		mu.Lock()
		defer mu.Unlock()
		if calls >= len(ops) {
			return pitr.OpTime{}, errors.New("no more optimes")
		}
		calls++
		return ops[calls-1], nil
	}, &calls
}

func TestInstanceBackupDumpsTheWholeInstanceWithTheOplog(t *testing.T) {
	var gotArgs []string
	runner := func(_ context.Context, _ string, args ...string) (io.ReadCloser, io.Reader, func() error, error) {
		gotArgs = args
		return io.NopCloser(bytes.NewReader([]byte("archive"))), strings.NewReader(""), func() error { return nil }, nil
	}
	before := pitr.OpTime{TS: pitr.Timestamp{T: 100, I: 1}, Term: 3}
	after := pitr.OpTime{TS: pitr.Timestamp{T: 105, I: 2}, Term: 3}
	reader, calls := opTimes(before, after)
	store := storage.NewMockStorage()
	manifests := 0
	engine := NewEngine(store, "", WithRunner(runner), WithEncryptor(testEncryptor(t)), WithOpTimeReader(reader),
		WithManifestCapturer(func(context.Context, string, string) (*models.Manifest, error) {
			manifests++
			return &models.Manifest{}, nil
		}))

	opts := instanceOptions()
	opts.Gzip = false // a base is always gzipped
	rec, err := engine.Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.Status != models.StatusCompleted || !rec.InstanceScope() || rec.Database != "" || rec.PITRStreamID != "str1" || rec.ReplicaSet != "rs0" {
		t.Fatalf("record = %+v", rec)
	}
	if !slices.Contains(gotArgs, "--oplog") || !slices.Contains(gotArgs, "--gzip") || !slices.Contains(gotArgs, "--archive") {
		t.Fatalf("args = %v", gotArgs)
	}
	for _, a := range gotArgs {
		if strings.HasPrefix(a, "--db") || strings.HasPrefix(a, "--collection") || strings.HasPrefix(a, "--uri") {
			t.Fatalf("instance dump passes %s", a)
		}
	}
	if *calls != 2 || rec.TBefore == nil || *rec.TBefore != before || rec.TAfter == nil || *rec.TAfter != after {
		t.Fatalf("T_before %v, T_after %v after %d reads", rec.TBefore, rec.TAfter, *calls)
	}
	wantPrefix := "_mongorescue/base/conn1/rs0/" + rec.StartedAt.Format("2006/01") + "/" + rec.ID + ".archive.gz.age"
	if rec.StorageKey != wantPrefix || !rec.Encrypted {
		t.Fatalf("key %q (encrypted %v), want %q", rec.StorageKey, rec.Encrypted, wantPrefix)
	}
	if err := models.ValidateID(rec.ID); err != nil {
		t.Fatalf("ID %q: %v", rec.ID, err)
	}
	if manifests != 0 || rec.HasManifest {
		t.Fatalf("a base captured %d manifests", manifests)
	}
	if _, err := store.Stat(context.Background(), rec.StorageKey); err != nil {
		t.Fatalf("archive not stored: %v", err)
	}
}

func TestInstanceBackupRequiresEncryption(t *testing.T) {
	reader, _ := opTimes(pitr.OpTime{TS: pitr.Timestamp{T: 1}})
	engine := NewEngine(storage.NewMockStorage(), "", WithRunner(staticRunner([]byte("x"))), WithOpTimeReader(reader))
	if _, err := engine.Prepare(instanceOptions()); !errors.Is(err, ErrEncryptionRequired) {
		t.Fatalf("Prepare without encryption: %v", err)
	}
}

func TestInstanceBackupRefusesDatabaseOptions(t *testing.T) {
	engine := NewEngine(storage.NewMockStorage(), "", WithRunner(staticRunner([]byte("x"))), WithEncryptor(testEncryptor(t)))
	for name, mutate := range map[string]func(*models.BackupOptions){
		"database":    func(o *models.BackupOptions) { o.Database = "shop" },
		"collections": func(o *models.BackupOptions) { o.Collections = []string{"a"} },
		"users":       func(o *models.BackupOptions) { o.IncludeUsersAndRoles = true },
	} {
		opts := instanceOptions()
		mutate(&opts)
		if _, err := engine.Prepare(opts); !errors.Is(err, ErrInstanceScope) {
			t.Errorf("%s: %v", name, err)
		}
	}
	opts := instanceOptions()
	opts.ReplicaSet = ""
	if _, err := engine.Prepare(opts); err == nil {
		t.Error("no replica set: accepted")
	}
}

func TestInstanceBackupFailsWithoutOpTimes(t *testing.T) {
	store := storage.NewMockStorage()
	for name, reader := range map[string]OpTimeReader{
		"no reader": nil,
		"after fails": func() OpTimeReader {
			r, _ := opTimes(pitr.OpTime{TS: pitr.Timestamp{T: 5}})
			return r
		}(),
	} {
		opts := []Option{WithRunner(staticRunner([]byte("archive"))), WithEncryptor(testEncryptor(t))}
		if reader != nil {
			opts = append(opts, WithOpTimeReader(reader))
		}
		engine := NewEngine(store, "", opts...)
		rec, err := engine.Run(context.Background(), instanceOptions())
		if !errors.Is(err, ErrOpTime) || rec.Status != models.StatusFailed {
			t.Fatalf("%s: %v, status %s", name, err, rec.Status)
		}
		if _, openErr := store.Stat(context.Background(), rec.StorageKey); !errors.Is(openErr, storage.ErrNotFound) {
			t.Fatalf("%s: the archive of a failed base stays: %v", name, openErr)
		}
	}
}

func TestDatabaseBackupKeepsItsArguments(t *testing.T) {
	args := NewEngine(nil, "").buildDumpArgs("--config=x", models.BackupOptions{Database: "shop"})
	if slices.Contains(args, "--oplog") || !slices.Contains(args, "--db=shop") {
		t.Fatalf("args = %v", args)
	}
}
