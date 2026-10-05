package operations_test

import (
	"context"
	"io"
	"strings"
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

// TestBaseBackupSharesTheConnectionSlotsAndReadPreference checks that a PITR base
// backup honours its connection's max_concurrent_backups (it waits, in progress,
// for a slot another backup holds, and is never failed for it) and records the
// connection's read preference.
func TestBaseBackupSharesTheConnectionSlotsAndReadPreference(t *testing.T) {
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
	runner := func(context.Context, string, ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
	}
	op := pitr.OpTime{TS: pitr.Timestamp{T: 100, I: 1}, Term: 1}
	opTime := func(context.Context, string) (pitr.OpTime, pitr.OpTime, error) { return op, op, nil }
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	mock := storage.NewMockStorage()
	svc := operations.New(operations.Config{
		Store: st,
		Backup: backup.NewEngine(mock, "", backup.WithRunner(runner), backup.WithEncryptor(enc), backup.WithOpTimeReader(opTime),
			backup.WithConnectionSlots(manager)),
		Restore: restore.NewEngine(mock, ""),
		Runs:    manager,
		Connections: fakeConnections{"conn_a": {ID: "conn_a", Name: "rs", URI: "mongodb://db.internal/?replicaSet=rs0",
			ReadPreference: models.ReadSecondaryPreferred, MaxConcurrentBackups: 1}},
		PITR: st,
	})
	if err = st.CreateStream(ctx, &pitr.Stream{ID: "str_a", ConnectionID: "conn_a", ReplicaSet: "rs0", BaseCron: "@daily",
		BaseKeepCount: 7, BaseKeepDays: 14, ChunkSeconds: 60}); err != nil {
		t.Fatal(err)
	}

	// Another backup of the connection holds its only slot.
	release, err := manager.AcquireSlot(ctx, runs.ConnectionKey("conn_a"), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := svc.StartBaseBackup(ctx, "str_a", models.TriggerScheduled)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	waiting, err := st.GetBackupRecord(ctx, rec.ID)
	if err != nil || waiting.Status != models.StatusInProgress {
		t.Fatalf("a base waiting for a slot: %+v, %v", waiting, err)
	}

	release()
	deadline := time.Now().Add(5 * time.Second)
	for {
		final, getErr := st.GetBackupRecord(ctx, rec.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if final.Status != models.StatusInProgress {
			if final.Status != models.StatusCompleted || final.ReadPreference != models.ReadSecondaryPreferred {
				t.Fatalf("base after the slot freed: %+v", final)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the base did not run once the slot was free")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
