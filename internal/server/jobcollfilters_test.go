package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestJobSelectionTakesPerDatabaseCollectionFilters(t *testing.T) {
	srv, metaStore, _ := setupTestServer(t)
	h := srv.buildRoutes()

	body := `{"name":"filtered","cron_expression":"@daily","connection_id":"` + testConnID + `",` +
		`"database_selection":{"mode":"list","databases":["shop",{"name":"crm","exclude_collections":["logs","tmp"]},{"name":"billing","collections":["invoices"]}]}}`
	rec := serve(h, "POST", "/api/v1/jobs", []byte(body), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	var res struct {
		Data models.Job `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	want := []models.DatabaseFilter{
		{Name: "billing", Collections: []string{"invoices"}},
		{Name: "crm", ExcludeCollections: []string{"logs", "tmp"}},
	}
	sel := res.Data.DatabaseSelection
	if !slices.Equal(sel.Databases, []string{"shop", "crm", "billing"}) || !reflect.DeepEqual(sel.CollectionFilters, want) {
		t.Fatalf("selection = %+v", sel)
	}
	// Stored in the job's data, and read back unchanged.
	stored, err := metaStore.GetJob(context.Background(), res.Data.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored.DatabaseSelection.CollectionFilters, want) {
		t.Errorf("stored filters = %+v", stored.DatabaseSelection.CollectionFilters)
	}

	for name, sel := range map[string]string{
		"pattern":    `{"mode":"pattern","include":["s*"],"collection_filters":[{"name":"shop","collections":["x"]}]}`,
		"not listed": `{"mode":"list","databases":["shop"],"collection_filters":[{"name":"crm","collections":["x"]}]}`,
		"bad name":   `{"mode":"list","databases":[{"name":"shop","exclude_collections":["a$b"]}]}`,
		"bad entry":  `{"mode":"list","databases":[42]}`,
		"both":       `{"mode":"list","databases":[{"name":"shop","collections":["a"],"exclude_collections":["b"]}]}`,
		"excluded":   `{"mode":"all","exclude":["sh*"],"databases":[{"name":"shop","collections":["a"]}]}`,
	} {
		rec = serve(h, "POST", "/api/v1/jobs", []byte(`{"name":"bad","cron_expression":"@daily","connection_id":"`+testConnID+`","database_selection":`+sel+`}`), nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s; want 400", name, rec.Code, rec.Body)
		}
	}

	// A job stored before filters existed (names only) keeps working unchanged.
	old := createTestJob(t, h, models.Job{Name: "old", CronExpression: "@daily", ConnectionID: testConnID,
		DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionList, Databases: []string{"shop"}}})
	if old.DatabaseSelection.CollectionFilters != nil || !strings.HasPrefix(old.ID, "job_list_") {
		t.Errorf("old job = %+v", old)
	}
}
