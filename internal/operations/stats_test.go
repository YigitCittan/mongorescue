package operations_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// brokenStatsStore fails the job list and the backup aggregates.
type brokenStatsStore struct {
	store.Store
}

var errBroken = errors.New("database is locked")

func (brokenStatsStore) ListJobs(context.Context) ([]*models.Job, error) { return nil, errBroken }

func (brokenStatsStore) BackupStats(context.Context, time.Time) (*store.BackupStats, error) {
	return nil, errBroken
}

func newServiceWith(t *testing.T, st store.Store) *operations.Service {
	t.Helper()
	mock := storage.NewMockStorage()
	bRunner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
	}
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	return operations.New(operations.Config{
		Store:   st,
		Backup:  backup.NewEngine(mock, "", backup.WithRunner(bRunner)),
		Restore: restore.NewEngine(mock, ""),
		Runs:    manager,
		Version: "v-test",
	})
}

func TestStatsReportsDegradedInsteadOfSwallowingErrors(t *testing.T) {
	svc := newServiceWith(t, brokenStatsStore{Store: storetest.New(t)})
	st := svc.Stats(context.Background())
	if !st.Degraded {
		t.Fatal("Stats is not degraded although the store failed")
	}
	for _, want := range []string{"list jobs", "backup stats", "database is locked"} {
		if !strings.Contains(st.DegradedReason, want) {
			t.Errorf("degraded reason %q does not mention %q", st.DegradedReason, want)
		}
	}
	if st.CorruptRecords != nil {
		t.Errorf("Stats filled corrupt records: %+v", st.CorruptRecords)
	}
}

func TestStatsCountReadableJobsWhenOneRowIsBad(t *testing.T) {
	svc, st := newService(t)
	ctx := context.Background()
	for _, id := range []string{"job_a", "job_bad", "job_c"} {
		if err := st.SaveJob(ctx, &models.Job{ID: id, Name: id, Database: "shop", Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	storetest.CorruptRow(t, st, "jobs", "job_bad", `{"id":"job_bad","retention_count":"many"}`)

	stats := svc.Stats(ctx)
	if stats.Degraded || stats.ActiveJobs != 2 {
		t.Fatalf("stats = degraded %v (%s), active jobs %d; want healthy with 2", stats.Degraded, stats.DegradedReason, stats.ActiveJobs)
	}
	jobs, err := svc.ListJobs(ctx)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("ListJobs = %d, %v; want the 2 readable jobs", len(jobs), err)
	}
	bad, err := svc.CorruptRecords(ctx)
	if err != nil || len(bad) != 1 || bad[0] != (store.CorruptRecord{Table: "jobs", ID: "job_bad", Error: "field retention_count: a JSON string does not fit type int"}) {
		t.Fatalf("CorruptRecords = %+v, %v", bad, err)
	}
}
