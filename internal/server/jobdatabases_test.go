package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestJobDatabaseSelectionAPI(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	h := srv.buildRoutes()

	// Old clients: database only; the response keeps database and shows the selection.
	single := createTestJob(t, h, models.Job{Name: "one", Database: "shop", CronExpression: "@daily", ConnectionID: testConnID})
	if single.Database != "shop" || single.DatabaseSelection.Mode != models.SelectionSingle ||
		!slices.Equal(single.DatabaseSelection.Databases, []string{"shop"}) {
		t.Fatalf("single job = %+v", single)
	}

	multi := createTestJob(t, h, models.Job{Name: "two", CronExpression: "@daily", ConnectionID: testConnID, Parallelism: 2,
		KnownDatabases:    []string{"forged"},
		DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionList, Databases: []string{"shop", "billing"}}})
	if multi.Database != "" || !multi.MultiDatabase() || multi.Parallelism != 2 || multi.KnownDatabases != nil ||
		!strings.HasPrefix(multi.ID, "job_list_") {
		t.Fatalf("multi job = %+v; known databases must never come from the client", multi)
	}

	rec := serve(h, "POST", "/api/v1/jobs", []byte(`{"name":"bad","cron_expression":"@daily","connection_id":"`+testConnID+
		`","collections":["orders"],"database_selection":{"mode":"all"}}`), nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "single-database") {
		t.Errorf("collections on a multi-database job = %d %s; want 400", rec.Code, rec.Body)
	}

	// The test server cannot list databases: a list selection is previewed as named
	// (with a warning), an all selection cannot be resolved (502).
	rec = serve(h, "GET", "/api/v1/jobs/"+multi.ID+"/databases/preview", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d %s", rec.Code, rec.Body)
	}
	var preview struct {
		Data struct {
			Included []string                  `json:"included"`
			Excluded []models.ExcludedDatabase `json:"excluded"`
			New      []string                  `json:"new_since_last_run"`
			Warnings []string                  `json:"warnings"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(preview.Data.Included, []string{"billing", "shop"}) || preview.Data.New == nil || len(preview.Data.Warnings) == 0 {
		t.Errorf("preview = %+v", preview.Data)
	}
	rec = serve(h, "GET", "/api/v1/jobs/"+multi.ID+"/databases/preview?mode=all", nil, nil)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("preview of an all selection without a database list = %d %s; want 502", rec.Code, rec.Body)
	}
	rec = serve(h, "GET", "/api/v1/jobs/databases/preview?connection_id="+testConnID+"&mode=list&databases=a&databases=b", nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"included":["a","b"]`) {
		t.Errorf("preview of an unsaved job = %d %s", rec.Code, rec.Body)
	}
	rec = serve(h, "GET", "/api/v1/jobs/databases/preview?connection_id="+testConnID+"&mode=list&auto_include_new=maybe", nil, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad auto_include_new = %d; want 400", rec.Code)
	}

	rec = serve(h, "GET", "/api/v1/jobs/"+multi.ID+"/runs", nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Errorf("runs of a job that never ran = %d %s", rec.Code, rec.Body)
	}
	rec = serve(h, "POST", "/api/v1/jobs/"+multi.ID+"/cancel", nil, nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("cancel without a run = %d %s; want 409", rec.Code, rec.Body)
	}
	rec = serve(h, "POST", "/api/v1/jobs/job_missing/cancel", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("cancel of an unknown job = %d; want 404", rec.Code)
	}

	// PUT from an old client (no selection, no database) keeps the selection.
	rec = serve(h, "PUT", "/api/v1/jobs/"+multi.ID, []byte(`{"name":"two","cron_expression":"@hourly","connection_id":"`+testConnID+`"}`), nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"mode":"list"`) {
		t.Errorf("PUT without a selection = %d %s", rec.Code, rec.Body)
	}
}
