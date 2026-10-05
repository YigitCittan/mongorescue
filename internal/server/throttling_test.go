package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestJobThrottlingAndWindowFields(t *testing.T) {
	srv, metaStore, _ := setupTestServer(t)
	h := srv.buildRoutes()
	ctx := context.Background()
	base := `"name":"w","database":"shop","cron_expression":"0 * * * *","connection_id":"` + testConnID + `"`

	// Tags without a read preference are refused, not ignored.
	for _, body := range []string{`{` + base + `,"read_preference_tags":[{"dc":"east"}]}`, `{` + base + `,"read_preference":"","read_preference_tags":[{"dc":"east"}]}`} {
		rec := serve(h, "POST", "/api/v1/jobs", []byte(body), nil)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "need a read_preference other than primary") {
			t.Errorf("POST %s = %d %s; want 400 about the missing mode", body, rec.Code, rec.Body.String())
		}
	}
	for field, bad := range map[string]string{
		"read_preference":          `"read_preference":"secondary_preferred"`,
		"read_preference_tags":     `"read_preference":"primary","read_preference_tags":[{"dc":"east"}]`,
		"max_upload_mbps":          `"max_upload_mbps":-1`,
		"num_parallel_collections": `"num_parallel_collections":17`,
		"backup_window":            `"backup_window":{"start":"22:00","end":"22:00"}`,
		"timezone":                 `"backup_window":{"start":"22:00","end":"02:00","timezone":"Mars/Olympus"}`,
	} {
		rec := serve(h, "POST", "/api/v1/jobs", []byte(`{`+base+`,`+bad+`}`), nil)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), field) {
			t.Errorf("POST with a bad %s = %d %s; want 400 naming it", field, rec.Code, rec.Body.String())
		}
	}

	created := createTestJob(t, h, models.Job{Name: "w", Database: "shop", CronExpression: "0 * * * *", ConnectionID: testConnID, Enabled: true,
		ReadPreference: models.ReadSecondary, ReadPreferenceTags: []map[string]string{{"dc": "east"}},
		MaxUploadMbps: 200, NumParallelCollections: 2,
		BackupWindow: &models.BackupWindow{Timezone: "Europe/Istanbul", Days: []string{"SAT", "sun"}, Start: "22:00", End: "02:00", CancelAtWindowEnd: true}})
	if created.BackupWindow == nil || created.BackupWindow.Days[0] != "sat" || created.ReadPreference != models.ReadSecondary {
		t.Fatalf("created %+v", created)
	}
	var d struct {
		BackupWindow *models.BackupWindow `json:"backup_window"`
		WindowOpen   *bool                `json:"window_open"`
		NextRuns     []string             `json:"next_runs"`
		MaxUpload    float64              `json:"max_upload_mbps"`
	}
	decodeData(t, serve(h, "GET", "/api/v1/jobs/"+created.ID, nil, nil), &d)
	if d.WindowOpen == nil || d.BackupWindow == nil || d.MaxUpload != 200 || len(d.NextRuns) != 3 {
		t.Fatalf("details %+v", d)
	}

	put := func(body string) int {
		return serve(h, "PUT", "/api/v1/jobs/"+created.ID, []byte(body), nil).Code
	}
	// Omitted fields keep their values.
	if code := put(`{` + base + `}`); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	stored, err := metaStore.GetJob(ctx, created.ID)
	if err != nil || stored.BackupWindow == nil || stored.ReadPreference != models.ReadSecondary || stored.NumParallelCollections != 2 || stored.MaxUploadMbps != 200 {
		t.Fatalf("after a PUT without the fields: %+v, %v", stored, err)
	}
	// "" uses the connection's read preference, {} removes the window.
	if code := put(`{` + base + `,"read_preference":"","backup_window":{},"max_upload_mbps":0,"num_parallel_collections":0}`); code != http.StatusOK {
		t.Fatalf("PUT reset = %d", code)
	}
	stored, _ = metaStore.GetJob(ctx, created.ID)
	if stored.BackupWindow != nil || stored.ReadPreference != "" || stored.ReadPreferenceTags != nil || stored.MaxUploadMbps != 0 || stored.NumParallelCollections != 0 {
		t.Fatalf("after the reset: %+v", stored)
	}
	rec := serve(h, "PUT", "/api/v1/jobs/"+created.ID, []byte(`{`+base+`,"read_preference_tags":[{"dc":"east"}]}`), nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "need a read_preference other than primary") {
		t.Fatalf("PUT tags without read_preference = %d %s; want 400", rec.Code, rec.Body.String())
	}
}

func TestConnectionReadPreferenceAndLimit(t *testing.T) {
	srv, metaStore, _ := setupTestServer(t)
	h := srv.buildRoutes()
	ctx := context.Background()
	for _, bad := range []string{`"read_preference":"fastest"`, `"max_concurrent_backups":-1`, `"max_concurrent_backups":65`, `"read_preference_tags":[{"dc":"east"}]`} {
		rec := serve(h, "POST", "/api/v1/connections", []byte(`{"name":"bad","uri":"mongodb://db1:27017",`+bad+`}`), nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s = %d %s; want 400", bad, rec.Code, rec.Body.String())
		}
	}
	rec := serve(h, "POST", "/api/v1/connections", []byte(`{"name":"rs","uri":"mongodb://u:pw@db1:27017,db2:27017/?replicaSet=rs0",`+
		`"read_preference":"secondaryPreferred","read_preference_tags":[{"use":"backup"}],"max_concurrent_backups":2}`), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body.String())
	}
	var c models.Connection
	decodeData(t, rec, &c)
	if c.ReadPreference != models.ReadSecondaryPreferred || c.MaxConcurrentBackups != 2 || len(c.ReadPreferenceTags) != 1 || strings.Contains(c.URI, "pw") {
		t.Fatalf("created %+v", c)
	}
	// An update without the fields keeps them.
	if rec = serve(h, "PUT", "/api/v1/connections/"+c.ID, []byte(`{"name":"rs2","uri":"`+c.URI+`"}`), nil); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
	}
	repo, ok := metaStore.(interface {
		GetConnection(context.Context, string) (*models.Connection, error)
	})
	if !ok {
		t.Fatal("the test store holds no connections")
	}
	stored, err := repo.GetConnection(ctx, c.ID)
	if err != nil || stored.Name != "rs2" || stored.ReadPreference != models.ReadSecondaryPreferred || stored.MaxConcurrentBackups != 2 {
		t.Fatalf("stored %+v, %v", stored, err)
	}
}
