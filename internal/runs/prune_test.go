package runs

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runlog"
)

// TestPruneLogsSkipsActiveRuns keeps the log of a running run, whose writer holds the
// file open (Windows refuses to delete it), however old the file looks.
func TestPruneLogsSkipsActiveRuns(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry(WithLogs(runlog.NewDir(dir)))
	run, err := reg.Register(Meta{Kind: models.RunBackup, ID: "bkp_active_log"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.End)
	run.Printf("still running")
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err = os.Chtimes(filepath.Join(dir, "bkp_active_log.log"), old, old); err != nil {
		t.Fatal(err)
	}
	if n, pruneErr := reg.PruneLogs(time.Hour); pruneErr != nil || n != 0 {
		t.Fatalf("pruned %d, %v; the log of an active run must be kept", n, pruneErr)
	}
	run.End()
	if n, pruneErr := reg.PruneLogs(time.Hour); pruneErr != nil || n != 1 {
		t.Fatalf("pruned %d, %v; want the finished run's old log", n, pruneErr)
	}
}
