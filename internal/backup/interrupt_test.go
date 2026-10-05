package backup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// A backup waiting for a slot ends as cancelled by the system, never as failed,
// when the runs manager shuts down or the run's context ends.
func TestWaitingBackupIsInterruptedNotFailed(t *testing.T) {
	for _, how := range []string{"manager shutdown", "context ended"} {
		t.Run(how, func(t *testing.T) {
			m := runs.NewManager(nil)
			defer func() { _ = m.Shutdown(context.Background()) }()
			held, err := m.AcquireSlot(context.Background(), runs.ConnectionKey("conn_a"), 1, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer held()
			engine := NewEngine(storage.NewMockStorage(), "", WithConnectionSlots(m), WithRunner((&argsRunner{}).run))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				deadline := time.Now().Add(5 * time.Second)
				for m.SlotWaiters(runs.ConnectionKey("conn_a")) != 1 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if how == "manager shutdown" {
					_ = m.Shutdown(context.Background())
				} else {
					cancel()
				}
			}()
			rec, err := engine.Run(ctx, models.BackupOptions{Database: "shop", MongoURI: "mongodb://db1", ConnectionID: "conn_a", MaxConcurrentBackups: 1})
			if !errors.Is(err, ErrInterrupted) {
				t.Fatalf("err %v, want ErrInterrupted", err)
			}
			if rec.Status != models.StatusCancelled || rec.CancelledBy != runs.SystemActor || rec.CancelledAt == nil {
				t.Fatalf("record %s by %q", rec.Status, rec.CancelledBy)
			}
		})
	}
}
