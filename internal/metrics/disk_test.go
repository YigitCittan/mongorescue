package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiskSeries(t *testing.T) {
	m := New(BuildInfo{Version: "1", Commit: "c", GoVersion: "go"})
	if strings.Contains(scrape(t, m), "mongorescue_data_dir_free_bytes") {
		t.Fatal("disk gauges are exported without a source")
	}
	m.SetDiskSource(func(context.Context) DiskUsage {
		return DiskUsage{DataDirFree: 1024, DataDirKnown: true, Targets: []DiskTarget{{Target: "stg_a", FreeBytes: 2048}, {Target: "stg_a", FreeBytes: 1}}}
	})
	body := scrape(t, m)
	for _, want := range []string{"mongorescue_data_dir_free_bytes 1024", `mongorescue_storage_free_bytes{target="stg_a"} 2048`} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape lacks %q", want)
		}
	}
	rec := httptest.NewRecorder()
	m.StandbyHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if strings.Contains(rec.Body.String(), "mongorescue_data_dir_free_bytes") || strings.Contains(rec.Body.String(), "mongorescue_storage_free_bytes") {
		t.Error("a standby exports the disk gauges")
	}
}
