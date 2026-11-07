package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestConnectionTLSExpiryGauge(t *testing.T) {
	m := New(BuildInfo{Version: "test"})
	const name = "mongorescue_connection_tls_cert_expiry_timestamp_seconds"
	if strings.Contains(scrape(t, m), name) {
		t.Fatal("the gauge is exported without samples")
	}
	at := time.Unix(1893456000, 0)
	m.SetConnectionTLSExpiries([]TLSCertSample{
		{Connection: "conn_1", Kind: "client", NotAfter: at},
		{Connection: "conn_1", Kind: "ca", NotAfter: at.Add(time.Hour)},
	})
	out := scrape(t, m)
	for _, want := range []string{
		name + `{connection="conn_1",kind="client"} 1.893456e+09`,
		name + `{connection="conn_1",kind="ca"} 1.8934596e+09`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}

	// A standby exposes no connection data.
	rec := httptest.NewRecorder()
	m.StandbyHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if strings.Contains(rec.Body.String(), name) {
		t.Error("a standby exports the certificate expiry gauge")
	}

	// A deleted connection leaves no series behind.
	m.SetConnectionTLSExpiries(nil)
	if strings.Contains(scrape(t, m), name) {
		t.Error("series remain after the samples were cleared")
	}
}
