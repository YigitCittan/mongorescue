package backup

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runlog"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// TestBackupCancelThroughRegistry cancels a backup of a slow fake mongodump through
// the run registry mid-stream: the tool is killed and reaped, the partial artifact is
// deleted, the record is cancelled with who cancelled it, the progress was parsed
// and the run log holds the redacted tool output and the phase lines.
func TestBackupCancelThroughRegistry(t *testing.T) {
	for name, enc := range encryptorModes(t) {
		t.Run(name, func(t *testing.T) {
			logDir := t.TempDir()
			reg := runs.NewRegistry(runs.WithLogs(runlog.NewDir(logDir)))
			runner := &helperRunner{mode: "progress"}
			engine := NewEngine(nil, "mongodb://localhost:27017", WithRunner(runner.run), WithEncryptor(enc))
			opts := models.BackupOptions{Database: "shop"}
			record, err := engine.Prepare(opts)
			if err != nil {
				t.Fatal(err)
			}
			run, err := reg.Register(runs.Meta{Kind: models.RunBackup, ID: record.ID, Database: "shop"})
			if err != nil {
				t.Fatal(err)
			}
			var progress models.RunProgress
			var once sync.Once
			store := &cancelAfterStorage{MockStorage: storage.NewMockStorage(), n: 512 << 10, cancel: func() {
				once.Do(func() {
					// stderr is parsed concurrently with the archive stream.
					for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
						if progress = run.Snapshot(); progress.Documents == 2502 {
							break
						}
					}
					_ = reg.Cancel(record.ID, runs.Cancellation{By: "alice"})
				})
			}}
			engine.storage = store

			got, err := runWithDeadline(t, func() (*models.BackupRecord, error) {
				defer run.End()
				return engine.Execute(run.Bind(context.Background()), opts, record)
			})
			if !errors.Is(err, runs.ErrCancelled) {
				t.Fatalf("expected a cancellation, got %v", err)
			}
			if got.Status != models.StatusCancelled || got.CancelledBy != "alice" || got.CancelledAt == nil {
				t.Fatalf("record = %s by %q at %v", got.Status, got.CancelledBy, got.CancelledAt)
			}
			if got.ErrorMessage != "backup cancelled by alice" || got.SizeBytes != 0 || got.SHA256 != "" {
				t.Fatalf("record message %q size %d sha %q", got.ErrorMessage, got.SizeBytes, got.SHA256)
			}
			if got.Phases.Started == nil || got.Phases.Finished == nil || got.Phases.UploadDone != nil {
				t.Fatalf("phases = %+v", got.Phases)
			}
			runner.assertReaped(t)
			store.mu.Lock()
			deleted := slices.Contains(store.deleted, got.StorageKey)
			store.mu.Unlock()
			if !deleted {
				t.Fatal("the partial artifact of a cancelled backup was not deleted")
			}

			if progress.Bytes <= 0 || progress.Phase != models.PhaseDumping || progress.CollectionsTotal != 2 ||
				progress.CollectionsDone != 1 || progress.Documents != 2502 || progress.CurrentCollection != "shop.orders" {
				t.Fatalf("progress mid-run = %+v", progress)
			}
			if progress.Percent == nil || *progress.Percent < 25 || *progress.Percent > 26 {
				t.Fatalf("percent = %v, want about 25%% (2502 of 10002 documents)", progress.Percent)
			}

			raw, err := os.ReadFile(filepath.Join(logDir, record.ID+".log"))
			if err != nil {
				t.Fatal(err)
			}
			log := string(raw)
			for _, want := range []string{
				"[mongorescue] backup " + record.ID + " of database shop started",
				"[mongorescue] mongodump started: --archive",
				"writing shop.orders to archive on stdout",
				"[######..................]  shop.orders  2500/10000  (25.0%)",
				"cancellation requested (cancelled by alice)",
				"[mongorescue] backup cancelled: backup cancelled by alice",
				"dup key: { ****** }",
			} {
				if !strings.Contains(log, want) {
					t.Errorf("run log misses %q:\n%s", want, log)
				}
			}
			for _, secret := range []string{"hunter2", "alice@example.com", "--config="} {
				if strings.Contains(log, secret) {
					t.Errorf("run log leaks %q:\n%s", secret, log)
				}
			}
		})
	}
}

// TestBackupCancelBeforeStart records a backup cancelled before mongodump started as
// cancelled without starting the tool.
func TestBackupCancelBeforeStart(t *testing.T) {
	reg := runs.NewRegistry()
	started := false
	engine := NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", WithRunner(
		func(ctx context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
			started = true
			return nil, nil, nil, ctx.Err()
		}))
	opts := models.BackupOptions{Database: "shop"}
	record, err := engine.Prepare(opts)
	if err != nil {
		t.Fatal(err)
	}
	run, _ := reg.Register(runs.Meta{Kind: models.RunBackup, ID: record.ID})
	if err := reg.Cancel(record.ID, runs.Cancellation{By: "API key ci", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	got, err := engine.Execute(run.Bind(context.Background()), opts, record)
	run.End()
	if !errors.Is(err, runs.ErrCancelled) || got.Status != models.StatusCancelled || got.CancelledBy != "API key ci" {
		t.Fatalf("got %s by %q, %v", got.Status, got.CancelledBy, err)
	}
	if !started {
		// The runner sees the cancelled context and refuses to start.
		t.Log("runner was not called")
	}
	if err := reg.Cancel(record.ID, runs.Cancellation{By: "bob"}); !errors.Is(err, runs.ErrNotRunning) {
		t.Fatalf("cancelling an ended run = %v, want ErrNotRunning", err)
	}
}
