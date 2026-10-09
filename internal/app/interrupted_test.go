package app

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func TestFailInterruptedRuns(t *testing.T) {
	ctx := context.Background()
	fs := storetest.New(t)
	_ = fs.SaveBackupRecord(ctx, &models.BackupRecord{ID: "running", Database: "d", Status: models.StatusInProgress, SHA256: "e3b0"})
	_ = fs.SaveBackupRecord(ctx, &models.BackupRecord{ID: "done", Database: "d", Status: models.StatusCompleted, SHA256: "abcd"})
	_ = fs.SaveRestoreRecord(ctx, &models.RestoreRecord{ID: "rst", Status: models.RestoreStatusInProgress})

	a := &App{metaStore: fs, logger: slog.Default()}
	a.failInterruptedRuns(ctx)

	if b, _ := fs.GetBackupRecord(ctx, "running"); b.Status != models.StatusFailed || b.SHA256 != "" || b.ErrorMessage == "" {
		t.Fatalf("interrupted backup not failed: %+v", b)
	}
	if b, _ := fs.GetBackupRecord(ctx, "done"); b.Status != models.StatusCompleted || b.SHA256 != "abcd" {
		t.Fatalf("completed backup modified: %+v", b)
	}
	restores, _ := fs.ListRestoreRecords(ctx)
	if len(restores) != 1 || restores[0].Status != models.RestoreStatusFailed {
		t.Fatalf("interrupted restore not failed: %+v", restores)
	}
}

func TestFailInterruptedJobRuns(t *testing.T) {
	ctx := context.Background()
	fs := storetest.New(t)
	run := &models.JobRun{ID: "run_1", JobID: "job", Status: models.JobRunRunning, StartedAt: time.Now().Add(-time.Hour),
		Databases: []models.JobRunDatabase{
			{Database: "a", BackupID: "bkp_a", Status: models.StatusCompleted},
			{Database: "b", BackupID: "bkp_b", Status: models.StatusInProgress},
		}}
	if err := fs.SaveJobRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	done := &models.JobRun{ID: "run_0", JobID: "job", Status: models.JobRunOK, StartedAt: time.Now().Add(-2 * time.Hour)}
	if err := fs.SaveJobRun(ctx, done); err != nil {
		t.Fatal(err)
	}
	a := &App{metaStore: fs, logger: slog.Default()}
	a.failInterruptedRuns(ctx)

	got, err := fs.GetJobRun(ctx, "run_1")
	if err != nil || got.Status != models.JobRunPartial || got.Databases[1].Status != models.StatusFailed || got.CompletedAt == nil {
		t.Fatalf("interrupted run = %+v, %v; want partial with b failed", got, err)
	}
	if kept, _ := fs.GetJobRun(ctx, "run_0"); kept.Status != models.JobRunOK {
		t.Errorf("a finished run was changed: %+v", kept)
	}
}

// TestInterruptedJobRunKeepsCompletedDatabases checks that recovery rebuilds a run's
// databases from its backup records: a database that completed before the crash
// stays completed even when the stored run still lists it as running.
func TestInterruptedJobRunKeepsCompletedDatabases(t *testing.T) {
	ctx := context.Background()
	fs := storetest.New(t)
	at := time.Now().Add(-time.Hour)
	for _, b := range []*models.BackupRecord{
		{ID: "bkp_a", RunID: "run_2", JobID: "job", Database: "a", Status: models.StatusCompleted, StartedAt: at, SHA256: "aa"},
		{ID: "bkp_b", RunID: "run_2", JobID: "job", Database: "b", Status: models.StatusInProgress, StartedAt: at},
		{ID: "bkp_c", RunID: "run_2", JobID: "job", Database: "c", Status: models.StatusInProgress, StartedAt: at},
	} {
		if err := fs.SaveBackupRecord(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	run := &models.JobRun{ID: "run_2", JobID: "job", Status: models.JobRunRunning, StartedAt: at, Databases: []models.JobRunDatabase{
		{Database: "a", BackupID: "bkp_a", Status: models.StatusInProgress},
		{Database: "b", BackupID: "bkp_b", Status: models.StatusInProgress},
		{Database: "c", BackupID: "bkp_c", Status: models.StatusInProgress},
	}}
	if err := fs.SaveJobRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	a := &App{metaStore: fs, logger: slog.Default()}
	a.failInterruptedRuns(ctx)

	got, err := fs.GetJobRun(ctx, "run_2")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != models.JobRunPartial || got.Databases[0].Status != models.StatusCompleted ||
		got.Databases[1].Status != models.StatusFailed || got.Databases[2].Status != models.StatusFailed ||
		got.Databases[1].Error == "" {
		t.Fatalf("recovered run = %+v; want a completed, b and c failed (partial)", got)
	}
	if b, _ := fs.GetBackupRecord(ctx, "bkp_a"); b.Status != models.StatusCompleted {
		t.Errorf("completed backup changed: %+v", b)
	}
}

// TestFailInterruptedRunsPublishesFailures pins that runs a killed process left in
// progress are reported: each interrupted backup and restore publishes its failure
// event, and a run of several databases its summary (its databases' events are
// InRun, for the metrics only). The chaos suite found that a SIGKILL during a backup
// marked it failed at the next start without any alert.
func TestFailInterruptedRunsPublishesFailures(t *testing.T) {
	ctx := context.Background()
	fs := storetest.New(t)
	_ = fs.SaveBackupRecord(ctx, &models.BackupRecord{ID: "single", Database: "d", JobID: "job1", Status: models.StatusInProgress})
	_ = fs.SaveBackupRecord(ctx, &models.BackupRecord{ID: "bkp_a", Database: "a", JobID: "job2", RunID: "run_1", Status: models.StatusCompleted})
	_ = fs.SaveBackupRecord(ctx, &models.BackupRecord{ID: "bkp_b", Database: "b", JobID: "job2", RunID: "run_1", Status: models.StatusInProgress})
	_ = fs.SaveRestoreRecord(ctx, &models.RestoreRecord{ID: "rst", BackupID: "single", Status: models.RestoreStatusInProgress})
	run := &models.JobRun{ID: "run_1", JobID: "job2", Status: models.JobRunRunning, StartedAt: time.Now().Add(-time.Hour),
		Databases: []models.JobRunDatabase{
			{Database: "a", BackupID: "bkp_a", Status: models.StatusCompleted},
			{Database: "b", BackupID: "bkp_b", Status: models.StatusInProgress},
		}}
	if err := fs.SaveJobRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	bus := events.NewBus()
	var mu sync.Mutex
	var got []events.Event
	bus.Subscribe(func(_ context.Context, e events.Event) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, e)
	})
	a := &App{metaStore: fs, logger: slog.Default(), bus: bus}
	// Published before the bus runs, as at startup: the bus queues them.
	a.failInterruptedRuns(ctx)
	runCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { _ = bus.Run(runCtx) })
	cancel()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	find := func(match func(events.Event) bool) *events.Event {
		for i := range got {
			if match(got[i]) {
				return &got[i]
			}
		}
		return nil
	}
	if e := find(func(e events.Event) bool { return e.Type == events.BackupFailed && e.BackupID == "single" }); e == nil || e.InRun || e.JobID != "job1" || e.Error == "" {
		t.Fatalf("no backup.failed for the interrupted backup: %+v", got)
	}
	if e := find(func(e events.Event) bool { return e.Type == events.BackupFailed && e.BackupID == "bkp_b" }); e == nil || !e.InRun || e.RunID != "run_1" {
		t.Fatalf("the interrupted database of a multi-database run must publish an InRun event: %+v", got)
	}
	if e := find(func(e events.Event) bool { return e.Type == events.BackupFailed && e.RunID == "run_1" && e.Run != nil }); e == nil {
		t.Fatalf("no summary event for the interrupted run: %+v", got)
	}
	if e := find(func(e events.Event) bool { return e.Type == events.RestoreFailed && e.RestoreID == "rst" }); e == nil || e.Error == "" {
		t.Fatalf("no restore.failed for the interrupted restore: %+v", got)
	}
	if e := find(func(e events.Event) bool { return e.BackupID == "bkp_a" }); e != nil {
		t.Fatalf("a completed backup published an event: %+v", e)
	}
}

// A backup left in progress whose archive exists in storage (its final record
// was never saved: a full data disk or a save still being retried, then a
// restart) is failed at the next start, but its archive is kept, without
// archive_cleanup_pending, so the storage scan offers it for import. Only an
// archive smaller than its recorded size goes to the purge; a missing one is
// left alone.
func TestFailInterruptedRunsKeepsAnExistingArchive(t *testing.T) {
	ctx := context.Background()
	fs := storetest.New(t)
	mem := storage.NewMockStorage()
	for key, data := range map[string]string{
		"d/2026/10/uploaded.archive.gz": "a complete archive",
		"d/2026/10/partial.archive.gz":  "short",
	} {
		if _, err := mem.Save(ctx, key, strings.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []*models.BackupRecord{
		{ID: "uploaded", Database: "d", Status: models.StatusInProgress, StorageTargetID: "local", StorageKey: "d/2026/10/uploaded.archive.gz"},
		{ID: "partial", Database: "d", Status: models.StatusInProgress, StorageTargetID: "local", StorageKey: "d/2026/10/partial.archive.gz", SizeBytes: 1000},
		{ID: "never", Database: "d", Status: models.StatusInProgress, StorageTargetID: "local", StorageKey: "d/2026/10/never.archive.gz"},
	} {
		if err := fs.SaveBackupRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	a := &App{metaStore: fs, logger: slog.Default(),
		storages: func(context.Context, string) (storage.Storage, error) { return mem, nil }}
	a.failInterruptedRuns(ctx)

	kept, err := fs.GetBackupRecord(ctx, "uploaded")
	if err != nil || kept.Status != models.StatusFailed || kept.ArchiveCleanupPending ||
		kept.ErrorMessage != "interrupted before the record was saved; the archive was kept" {
		t.Fatalf("interrupted backup with its archive = %+v, %v", kept, err)
	}
	if _, err = mem.Stat(ctx, kept.StorageKey); err != nil {
		t.Fatalf("the archive was not kept: %v", err)
	}
	if p, _ := fs.GetBackupRecord(ctx, "partial"); p.Status != models.StatusFailed || !p.ArchiveCleanupPending {
		t.Fatalf("partial archive = %+v; want it sent to the purge", p)
	}
	if n, _ := fs.GetBackupRecord(ctx, "never"); n.Status != models.StatusFailed || n.ArchiveCleanupPending ||
		strings.Contains(n.ErrorMessage, "kept") {
		t.Fatalf("backup without an archive = %+v", n)
	}
	pending, err := fs.PendingArchiveCleanups(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != "partial" {
		t.Fatalf("pending cleanups %+v, %v; want only the partial archive", pending, err)
	}
}
