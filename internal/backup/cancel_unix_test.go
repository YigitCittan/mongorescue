//go:build unix

package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// TestRegistryCancelKillsDumpAndItsChildren cancels a running backup through the run
// registry (as POST /api/v1/backups/{id}/cancel does) while the application keeps
// running: the fake mongodump and its SIGTERM-ignoring child are both gone afterwards.
func TestRegistryCancelKillsDumpAndItsChildren(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pids")
	t.Setenv(helperEnv, "tree")
	t.Setenv(helperPIDFileEnv, pidFile)

	reg := runs.NewRegistry()
	manager := runs.NewManager(nil)
	defer func() { _ = manager.Shutdown(context.Background()) }()
	engine := NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", WithRunner(realToolRunner))
	opts := models.BackupOptions{Database: "db"}
	record, err := engine.Prepare(opts)
	if err != nil {
		t.Fatal(err)
	}
	run, err := reg.Register(runs.Meta{Kind: models.RunBackup, ID: record.ID})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		rec *models.BackupRecord
		err error
	}
	done := make(chan result, 1)
	if err := manager.Go(runs.BackupKey("conn", "db"), func(ctx context.Context) {
		// End before reporting, like the operations service does before it releases
		// the database: the test checks the registry right after the result.
		rec, err := engine.Execute(run.Bind(ctx), opts, record)
		run.End()
		done <- result{rec, err}
	}); err != nil {
		t.Fatal(err)
	}

	var dumpPID, grandchildPID int
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if data, err := os.ReadFile(pidFile); err == nil {
			if _, err := fmt.Sscanf(string(data), "%d %d", &dumpPID, &grandchildPID); err == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("helper never reported its PIDs")
		}
	}

	if err := reg.Cancel(record.ID, runs.Cancellation{By: "alice"}); err != nil {
		t.Fatal(err)
	}
	var res result
	select {
	case res = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the cancelled backup did not return")
	}
	if !errors.Is(res.err, runs.ErrCancelled) || res.rec.Status != models.StatusCancelled {
		t.Fatalf("got %v, %v; want a cancelled backup", res.rec.Status, res.err)
	}
	if !processGone(dumpPID) {
		t.Fatal("mongodump stand-in survived the cancellation")
	}
	if !processGone(grandchildPID) {
		_ = syscall.Kill(grandchildPID, syscall.SIGKILL)
		t.Fatal("child of mongodump (ignoring SIGTERM) survived the cancellation")
	}
	if reg.Get(record.ID) != nil {
		t.Fatal("the run is still registered after it ended")
	}
}
