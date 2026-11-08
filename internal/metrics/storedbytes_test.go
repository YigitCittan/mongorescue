package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStoredBytesGauge(t *testing.T) {
	m := New(BuildInfo{Version: "test"})
	const name = "mongorescue_storage_stored_bytes"
	if strings.Contains(scrape(t, m), name) {
		t.Fatal("the gauge is exported without samples")
	}
	m.SetStoredBytes([]StoredBytesSample{{Target: "stg_a", Bytes: 1024}, {Target: "stg_b", Bytes: 0}, {Target: "stg_a", Bytes: 5}})
	out := scrape(t, m)
	for _, want := range []string{name + `{target="stg_a"} 1024`, name + `{target="stg_b"} 0`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Count(out, name+`{target="stg_a"}`) != 1 {
		t.Error("a duplicate target produced two series")
	}

	// A standby exports no usage data.
	rec := httptest.NewRecorder()
	m.StandbyHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if strings.Contains(rec.Body.String(), name) {
		t.Error("a standby exports the stored bytes gauge")
	}

	m.SetStoredBytes(nil)
	if strings.Contains(scrape(t, m), name) {
		t.Error("series remain after the samples were cleared")
	}
}
