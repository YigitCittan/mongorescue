package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// raceStore runs after once, right after the first read of job id: a run that
// records known databases between a save's first read and its write.
type raceStore struct {
	*store.SQLiteStore
	mu    sync.Mutex
	id    string
	after func()
}

func (s *raceStore) GetJob(ctx context.Context, id string) (*models.Job, error) {
	job, err := s.SQLiteStore.GetJob(ctx, id)
	s.mu.Lock()
	after := s.after
	if id == s.id {
		s.after = nil
	}
	s.mu.Unlock()
	if id == s.id && after != nil {
		after()
	}
	return job, err
}

func TestFullSaveKeepsKnownDatabasesARunRecorded(t *testing.T) {
	base := storetest.New(t)
	st := &raceStore{SQLiteStore: base}
	mock := storage.NewMockStorage()
	bEngine := backup.NewEngine(mock, "mongodb://localhost:27017")
	sched := scheduler.NewScheduler(st, bEngine, mock, nil)
	srv := NewServer(bootConfig(), st, bEngine, restore.NewEngine(mock, "mongodb://localhost:27017"), mock, sched, nil, nil,
		withTestConnection(t, st, nil))
	h := srv.buildRoutes()
	ctx := context.Background()

	sel := models.DatabaseSelection{Mode: models.SelectionPattern, Include: []string{"app_*"}, AutoIncludeNew: true}
	job := &models.Job{ID: "job_app", Name: "apps", CronExpression: "@daily", ConnectionID: testConnID,
		DatabaseSelection: sel, KnownDatabases: []string{"app_a"}, Parallelism: 1}
	if err := base.SaveJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	// A run adds app_b while the save is in flight.
	st.mu.Lock()
	st.id = job.ID
	st.after = func() {
		if err := base.UpdateJobKnownDatabases(ctx, job.ID, func(*models.Job) ([]string, bool) {
			return []string{"app_a", "app_b"}, true
		}); err != nil {
			t.Error(err)
		}
	}
	st.mu.Unlock()

	body, _ := json.Marshal(map[string]any{
		"id": job.ID, "name": "apps renamed", "cron_expression": "@hourly", "connection_id": testConnID,
		"database_selection": sel, "known_databases": []string{"forged"},
	})
	if rec := serve(h, "POST", "/api/v1/jobs", body, nil); rec.Code != http.StatusCreated {
		t.Fatalf("save = %d %s", rec.Code, rec.Body)
	}
	stored, err := base.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != "apps renamed" || !slices.Equal(stored.KnownDatabases, []string{"app_a", "app_b"}) {
		t.Fatalf("stored job = %q known %v; want the renamed job keeping app_b", stored.Name, stored.KnownDatabases)
	}
	// app_b is known, so the next run does not announce it as added again.
	res := models.ResolveSelection(stored.Selection(), []string{"app_a", "app_b"}, stored.KnownDatabases)
	if len(res.New) != 0 {
		t.Errorf("databases new for the next run: %v; want none", res.New)
	}
}
