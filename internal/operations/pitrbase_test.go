package operations_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func TestStartBaseBackupDumpsTheInstanceUnderItsOwnKey(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	_, recipient, err := encryption.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	enc, err := encryption.NewX25519Encryptor([]string{recipient})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var args []string
	release := make(chan struct{})
	runner := func(_ context.Context, _ string, a ...string) (io.ReadCloser, io.Reader, func() error, error) {
		mu.Lock()
		args = a
		mu.Unlock()
		<-release
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
	}
	tick := uint32(100)
	opTime := func(context.Context, string) (pitr.OpTime, pitr.OpTime, error) {
		mu.Lock()
		defer mu.Unlock()
		tick++
		op := pitr.OpTime{TS: pitr.Timestamp{T: tick, I: 1}, Term: 2}
		return op, op, nil
	}
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	mock := storage.NewMockStorage()
	svc := operations.New(operations.Config{
		Store:       st,
		Backup:      backup.NewEngine(mock, "", backup.WithRunner(runner), backup.WithEncryptor(enc), backup.WithOpTimeReader(opTime)),
		Restore:     restore.NewEngine(mock, ""),
		Runs:        manager,
		Connections: fakeConnections{"conn_a": {ID: "conn_a", Name: "rs", URI: "mongodb://u:pw@db.internal/?replicaSet=rs0"}},
		PITR:        st,
	})

	if _, err = svc.StartBaseBackup(ctx, "missing", models.TriggerManual); !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("unknown stream: %v", err)
	}
	if err = st.CreateStream(ctx, &pitr.Stream{ID: "str_a", ConnectionID: "conn_a", ReplicaSet: "rs0", BaseCron: "@daily",
		BaseKeepCount: 7, BaseKeepDays: 14, ChunkSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	rec, err := svc.StartBaseBackup(ctx, "str_a", models.TriggerScheduled)
	if err != nil {
		t.Fatalf("StartBaseBackup: %v", err)
	}
	if !manager.Running(runs.PITRBaseKey("conn_a")) || manager.Running(runs.BackupKey("conn_a", "")) {
		t.Fatal("the base does not hold its own run key")
	}
	if _, err = svc.StartBaseBackup(ctx, "str_a", models.TriggerManual); !errors.Is(err, operations.ErrBusy) {
		t.Fatalf("second base while one runs: %v", err)
	}
	close(release)

	deadline := time.Now().Add(5 * time.Second)
	var final *models.BackupRecord
	for {
		final, err = st.GetBackupRecord(ctx, rec.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.Status != models.StatusInProgress && !manager.Running(runs.PITRBaseKey("conn_a")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("base still running: %+v", final)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if final.Status != models.StatusCompleted || !final.InstanceScope() || final.PITRStreamID != "str_a" ||
		final.TBefore == nil || final.TAfter == nil || final.TBefore.TS.Compare(final.TAfter.TS) >= 0 ||
		!strings.HasPrefix(final.StorageKey, "_mongorescue/base/conn_a/rs0/") || final.Trigger != models.TriggerScheduled {
		t.Fatalf("final record = %+v", final)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(args, "--oplog") {
		t.Fatalf("args = %v", args)
	}

	// A base is not restorable as a database backup.
	if _, err := svc.StartRestore(ctx, models.RestoreRequest{BackupID: final.ID}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("restore of a base: %v", err)
	}
}
