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

// processGone polls until pid no longer exists (orphaned zombies are reaped by init).
func processGone(pid int) bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
	}
	return false
}

func TestShutdownKillsDumpAndItsChildren(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pids")
	t.Setenv(helperEnv, "tree")
	t.Setenv(helperPIDFileEnv, pidFile)

	manager := runs.NewManager(nil)
	engine := NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", WithRunner(realToolRunner))
	result := make(chan error, 1)
	if err := manager.Go(runs.BackupKey("conn", "db"), func(ctx context.Context) {
		_, err := engine.Run(ctx, models.BackupOptions{Database: "db"})
		result <- err
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

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := manager.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the backup to be cancelled, got %v", err)
	}
	if !processGone(dumpPID) {
		t.Fatal("mongodump stand-in survived shutdown")
	}
	if !processGone(grandchildPID) {
		_ = syscall.Kill(grandchildPID, syscall.SIGKILL)
		t.Fatal("child of mongodump (ignoring SIGTERM) survived shutdown")
	}
}
