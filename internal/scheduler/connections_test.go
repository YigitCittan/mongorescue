package scheduler

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// resolver serves fixed connections.
type resolver map[string]*models.Connection

func (r resolver) Resolve(_ context.Context, id string) (*models.Connection, error) {
	c, ok := r[id]
	if !ok {
		return nil, connections.ErrNotFound
	}
	return c, nil
}

// uriRecorder is a mongodump stand-in that records the connection string it was given.
type uriRecorder struct {
	mu   sync.Mutex
	args []string
}

func (u *uriRecorder) run(_ context.Context, _ string, args ...string) (io.ReadCloser, io.Reader, func() error, error) {
	u.mu.Lock()
	u.args = append(u.args, strings.Join(args, " "))
	u.mu.Unlock()
	return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
}

func TestJobRunsResolveTheirConnection(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	mock := storage.NewMockStorage()
	rec := &uriRecorder{}
	engine := backup.NewEngine(mock, "", backup.WithRunner(rec.run))
	conns := resolver{"conn_a": {ID: "conn_a", Name: "server a", URI: "mongodb://a.internal:27017/"}}
	s := NewScheduler(st, engine, mock, nil, WithConnectionResolver(conns))

	job := &models.Job{ID: "job_a", Name: "a", Database: "shop", CronExpression: "@daily", ConnectionID: "conn_a",
		ExcludeCollections: []string{"logs"}}
	if err := st.SaveJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	record, err := s.TriggerJob(ctx, "job_a")
	if err != nil {
		t.Fatalf("TriggerJob: %v", err)
	}
	if record.ConnectionID != "conn_a" || record.ConnectionName != "server a" || record.Status != models.StatusCompleted {
		t.Fatalf("record = %+v", record)
	}
	if len(rec.args) != 1 || !strings.Contains(rec.args[0], "--excludeCollection=logs") {
		t.Fatalf("dump args = %v; want the job's exclusions", rec.args)
	}

	// Jobs without a connection, or with a deleted one, fail clearly.
	for id, conn := range map[string]string{"job_none": "", "job_gone": "conn_gone"} {
		if err := st.SaveJob(ctx, &models.Job{ID: id, Name: id, Database: "shop", ConnectionID: conn}); err != nil {
			t.Fatal(err)
		}
		_, err := s.TriggerJob(ctx, id)
		if conn == "" && !errors.Is(err, ErrNoConnection) {
			t.Errorf("%s: %v; want ErrNoConnection", id, err)
		}
		if conn != "" && !errors.Is(err, connections.ErrNotFound) {
			t.Errorf("%s: %v; want connections.ErrNotFound", id, err)
		}
	}
}

func TestRetentionOnlyPrunesTheJobsConnection(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	mock := storage.NewMockStorage()
	engine := backup.NewEngine(mock, "", backup.WithRunner((&uriRecorder{}).run))
	conns := resolver{
		"conn_a": {ID: "conn_a", Name: "a", URI: "mongodb://a/"},
		"conn_b": {ID: "conn_b", Name: "b", URI: "mongodb://b/"},
	}
	s := NewScheduler(st, engine, mock, nil, WithConnectionResolver(conns))

	old := time.Now().UTC().AddDate(0, 0, -30)
	for _, r := range []*models.BackupRecord{
		{ID: "bkp_a_old", Database: "shop", ConnectionID: "conn_a", Status: models.StatusCompleted, StorageKey: "a/old", StartedAt: old},
		{ID: "bkp_b_old", Database: "shop", ConnectionID: "conn_b", Status: models.StatusCompleted, StorageKey: "b/old", StartedAt: old},
	} {
		if err := st.SaveBackupRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
		_, _ = mock.Save(ctx, r.StorageKey, strings.NewReader("x"))
	}
	job := &models.Job{ID: "job_a", Name: "a", Database: "shop", ConnectionID: "conn_a", RetentionDays: 7}
	if err := st.SaveJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	s.executeJob(ctx, "job_a") // a scheduled run: only those apply retention
	a, _ := st.GetBackupRecord(ctx, "bkp_a_old")
	b, _ := st.GetBackupRecord(ctx, "bkp_b_old")
	if a.Status != models.StatusPruned || b.Status != models.StatusCompleted {
		t.Fatalf("pruned across servers: a=%s b=%s; want only a pruned", a.Status, b.Status)
	}
}
