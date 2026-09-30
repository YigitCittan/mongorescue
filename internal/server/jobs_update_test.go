package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// createTestJob creates a job through the API and returns it.
func createTestJob(t *testing.T, h http.Handler, job models.Job) models.Job {
	t.Helper()
	body, _ := json.Marshal(job)
	rec := serve(h, "POST", "/api/v1/jobs", body, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create job = %d %s", rec.Code, rec.Body.String())
	}
	var res struct {
		Data models.Job `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	return res.Data
}

func TestUpdateJobReschedulesAndKeepsIdentity(t *testing.T) {
	srv, metaStore, _ := setupTestServer(t)
	h := srv.buildRoutes()
	ctx := context.Background()

	created := createTestJob(t, h, models.Job{
		Name: "nightly", Database: "shop", CronExpression: "30 4 * * *",
		RetentionDays: 14, RetentionCount: 5, Enabled: true, ConnectionID: testConnID,
	})
	before, err := metaStore.GetJob(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.NextRun == nil || before.NextRun.Minute() != 30 {
		t.Fatalf("next run before the update = %v; want a 04:30 activation", before.NextRun)
	}
	lastRun := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	before.LastRun = &lastRun
	if err = metaStore.UpdateJob(ctx, before); err != nil {
		t.Fatal(err)
	}
	if err = metaStore.SaveBackupRecord(ctx, &models.BackupRecord{ID: "bkp_hist", JobID: created.ID, Database: "shop",
		Status: models.StatusCompleted, StartedAt: lastRun}); err != nil {
		t.Fatal(err)
	}

	body := `{"name":"hourly shop","cron_expression":"@hourly","database":"shop","collections":["orders"],"connection_id":"` + testConnID + `"}`
	rec := serve(h, "PUT", "/api/v1/jobs/"+created.ID, []byte(body), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s; want 200", rec.Code, rec.Body.String())
	}
	var res struct {
		Data models.Job `json:"data"`
	}
	if err = json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if res.Data.ID != created.ID || res.Data.Name != "hourly shop" || res.Data.CronExpression != "@hourly" {
		t.Fatalf("updated job = %+v", res.Data)
	}

	after, err := metaStore.GetJob(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.CreatedAt.Equal(before.CreatedAt) || after.LastRun == nil || !after.LastRun.Equal(lastRun) {
		t.Fatalf("created_at/last_run changed: %v/%v, want %v/%v", after.CreatedAt, after.LastRun, before.CreatedAt, lastRun)
	}
	// Omitted retention, gzip and enabled keep their values.
	if after.RetentionDays != 14 || after.RetentionCount != 5 || !after.Enabled || after.Gzip != created.Gzip {
		t.Fatalf("omitted fields were not kept: %+v", after)
	}
	if len(after.Collections) != 1 || after.Collections[0] != "orders" {
		t.Fatalf("collections = %v", after.Collections)
	}
	// The reschedule takes effect at once: the next run is the next full hour.
	if after.NextRun == nil || after.NextRun.Minute() != 0 || time.Until(*after.NextRun) > time.Hour {
		t.Fatalf("next run after the update = %v; want within the hour at minute 0", after.NextRun)
	}
	if n := srv.scheduler.ActiveJobCount(); n != 1 {
		t.Fatalf("active jobs = %d; want 1", n)
	}

	// The run history stays attached to the job.
	rec = serve(h, "GET", "/api/v1/backups?job_id="+created.ID, nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "bkp_hist") {
		t.Fatalf("backups of the job = %d %s", rec.Code, rec.Body.String())
	}
	rec = serve(h, "GET", "/api/v1/backups?job_id=job_other", nil, nil)
	if strings.Contains(rec.Body.String(), "bkp_hist") {
		t.Fatalf("job_id filter leaked another job's backup: %s", rec.Body.String())
	}
}

func TestUpdateJobDisableUnschedules(t *testing.T) {
	srv, metaStore, _ := setupTestServer(t)
	h := srv.buildRoutes()
	created := createTestJob(t, h, models.Job{Name: "n", Database: "shop", CronExpression: "@daily", Enabled: true, ConnectionID: testConnID})

	body := `{"name":"n","cron_expression":"@daily","database":"shop","enabled":false,"connection_id":"` + testConnID + `"}`
	if rec := serve(h, "PUT", "/api/v1/jobs/"+created.ID, []byte(body), nil); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
	}
	if n := srv.scheduler.ActiveJobCount(); n != 0 {
		t.Fatalf("active jobs = %d; want 0 after disabling", n)
	}
	job, err := metaStore.GetJob(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Enabled || job.NextRun != nil {
		t.Fatalf("disabled job = enabled %v next run %v", job.Enabled, job.NextRun)
	}

	rec := serve(h, "GET", "/api/v1/jobs/"+created.ID, nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"next_runs":[]`) {
		t.Fatalf("details of a disabled job = %d %s; want no next runs", rec.Code, rec.Body.String())
	}
}

func TestGetJobDetailsListsNextRuns(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	h := srv.buildRoutes()
	created := createTestJob(t, h, models.Job{Name: "n", Database: "shop", CronExpression: "@hourly", Enabled: true, ConnectionID: testConnID})

	rec := serve(h, "GET", "/api/v1/jobs/"+created.ID, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body.String())
	}
	var res struct {
		Data struct {
			ID       string      `json:"id"`
			NextRuns []time.Time `json:"next_runs"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if res.Data.ID != created.ID || len(res.Data.NextRuns) != 3 {
		t.Fatalf("details = %+v; want the job with 3 next runs", res.Data)
	}
	for i := 1; i < len(res.Data.NextRuns); i++ {
		if got := res.Data.NextRuns[i].Sub(res.Data.NextRuns[i-1]); got != time.Hour {
			t.Fatalf("next runs %v are not an hour apart", res.Data.NextRuns)
		}
	}
	if rec = serve(h, "GET", "/api/v1/jobs/job_missing", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("GET unknown job = %d; want 404", rec.Code)
	}
}

func TestUpdateJobRejectsInvalidInput(t *testing.T) {
	srv, metaStore, _ := setupTestServer(t)
	h := srv.buildRoutes()
	created := createTestJob(t, h, models.Job{Name: "n", Database: "shop", CronExpression: "@daily", Enabled: true, ConnectionID: testConnID})

	conn := `"connection_id":"` + testConnID + `"`
	for name, body := range map[string]string{
		"invalid json":       `{`,
		"invalid cron":       `{"cron_expression":"every tuesday","database":"shop",` + conn + `}`,
		"missing database":   `{"cron_expression":"@daily","database":" ",` + conn + `}`,
		"negative retention": `{"cron_expression":"@daily","database":"shop","retention_days":-1,` + conn + `}`,
		"no connection":      `{"cron_expression":"@daily","database":"shop"}`,
		"unknown connection": `{"cron_expression":"@daily","database":"shop","connection_id":"conn_missing"}`,
	} {
		if rec := serve(h, "PUT", "/api/v1/jobs/"+created.ID, []byte(body), nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: PUT = %d %s; want 400", name, rec.Code, rec.Body.String())
		}
	}
	job, err := metaStore.GetJob(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.CronExpression != "@daily" || job.Database != "shop" {
		t.Fatalf("a rejected update was stored: %+v", job)
	}

	body := `{"cron_expression":"@daily","database":"shop",` + conn + `}`
	if rec := serve(h, "PUT", "/api/v1/jobs/job_missing", []byte(body), nil); rec.Code != http.StatusNotFound {
		t.Fatalf("PUT unknown job = %d %s; want 404", rec.Code, rec.Body.String())
	}
	// Create applies the same validation.
	if rec := serve(h, "POST", "/api/v1/jobs", []byte(`{"cron_expression":"61 * * * *","database":"shop",`+conn+`}`), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("POST with an invalid cron = %d %s; want 400", rec.Code, rec.Body.String())
	}
}

func TestUpdateJobNeedsAdmin(t *testing.T) {
	f := newScopeFixture(t)
	body := []byte(`{"cron_expression":"@daily","database":"shop","connection_id":"` + testConnID + `"}`)
	for _, scope := range []auth.Scope{auth.ScopeRead, auth.ScopeOperator} {
		rec := serve(f.h, "PUT", "/api/v1/jobs/job_1", body, map[string]string{"X-API-Key": f.keys[scope], "Content-Type": "application/json"})
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s key PUT = %d %s; want 403", scope, rec.Code, rec.Body.String())
		}
	}
	rec := serve(f.h, "PUT", "/api/v1/jobs/job_1", body, map[string]string{"X-API-Key": f.keys[auth.ScopeAdmin], "Content-Type": "application/json"})
	if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
		t.Fatalf("admin key PUT = %d %s; want it past the scope check", rec.Code, rec.Body.String())
	}
}

func TestUpdateJobRefusesAStaleUpdatedAt(t *testing.T) {
	srv, metaStore, _ := setupTestServer(t)
	h := srv.buildRoutes()
	created := createTestJob(t, h, models.Job{Name: "n", Database: "shop", CronExpression: "@daily", Enabled: true, ConnectionID: testConnID})
	stored, err := metaStore.GetJob(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	body := func(name string, at time.Time) []byte {
		b, _ := json.Marshal(map[string]any{"name": name, "cron_expression": "@daily", "database": "shop", "connection_id": testConnID, "updated_at": at})
		return b
	}
	if rec := serve(h, "PUT", "/api/v1/jobs/"+created.ID, body("stale", stored.UpdatedAt.Add(-time.Second)), nil); rec.Code != http.StatusConflict {
		t.Fatalf("PUT with a stale updated_at = %d %s; want 409", rec.Code, rec.Body.String())
	}
	if rec := serve(h, "PUT", "/api/v1/jobs/"+created.ID, body("fresh", stored.UpdatedAt), nil); rec.Code != http.StatusOK {
		t.Fatalf("PUT with the current updated_at = %d %s; want 200", rec.Code, rec.Body.String())
	}
	if got, _ := metaStore.GetJob(context.Background(), created.ID); got.Name != "fresh" {
		t.Fatalf("name = %q; want fresh (and never stale)", got.Name)
	}
}

// Concurrent updates are stored and scheduled in the same order: the stored next run
// always belongs to the stored schedule.
func TestConcurrentJobUpdatesStayConsistent(t *testing.T) {
	srv, metaStore, _ := setupTestServer(t)
	h := srv.buildRoutes()
	created := createTestJob(t, h, models.Job{Name: "n", Database: "shop", CronExpression: "@daily", Enabled: true, ConnectionID: testConnID})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		cron := "@daily"
		if i%2 == 1 {
			cron = "@hourly"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := []byte(`{"name":"n","cron_expression":"` + cron + `","database":"shop","connection_id":"` + testConnID + `"}`)
			if rec := serve(h, "PUT", "/api/v1/jobs/"+created.ID, b, nil); rec.Code != http.StatusOK {
				t.Errorf("PUT = %d %s", rec.Code, rec.Body.String())
			}
		}()
	}
	wg.Wait()
	job, err := metaStore.GetJob(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := scheduler.NextRuns(job.CronExpression, time.Now().Add(-time.Minute), 1)
	if job.NextRun == nil || len(want) != 1 || !job.NextRun.Equal(want[0]) {
		t.Fatalf("stored next run %v does not belong to the stored schedule %q (want %v)", job.NextRun, job.CronExpression, want)
	}
}

// failingJobStore fails every job read with an unexpected error.
type failingJobStore struct {
	store.Store
}

func (failingJobStore) GetJob(context.Context, string) (*models.Job, error) {
	return nil, errors.New("disk I/O error")
}

func TestJobStoreFailuresAreInternalErrors(t *testing.T) {
	base := storetest.New(t)
	failing := failingJobStore{Store: base}
	srv := NewServer(bootConfig(), failing, nil, nil, storage.NewMockStorage(), nil, nil, nil, withTestConnection(t, base, nil))
	h := srv.buildRoutes()
	body := []byte(`{"id":"job_x","name":"n","cron_expression":"@daily","database":"shop","connection_id":"` + testConnID + `"}`)
	for _, method := range []string{"PUT", "POST"} {
		path := "/api/v1/jobs"
		if method == "PUT" {
			path += "/job_x"
		}
		rec := serve(h, method, path, body, nil)
		if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "disk") {
			t.Errorf("%s with a failing store = %d %s; want a generic 500", method, rec.Code, rec.Body.String())
		}
	}
}
