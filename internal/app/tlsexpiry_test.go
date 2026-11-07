package app

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/mongotls/mongotlstest"
)

// TestTLSExpiryMonitorSetsTheGauge proves the wiring of the certificate expiry
// check: a stored client certificate shows up in the gauge, labelled with the
// connection ID, and a deleted connection disappears.
func TestTLSExpiryMonitorSetsTheGauge(t *testing.T) {
	application, err := New(testConfig(t), nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	ctx := context.Background()
	ca, err := mongotlstest.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	notAfter := time.Now().Add(5 * 24 * time.Hour).Truncate(time.Second)
	client, err := ca.ClientExpiring("backup", notAfter)
	if err != nil {
		t.Fatal(err)
	}
	conns := application.tlsExpiry.conns
	created, err := conns.Create(ctx, connections.Input{Name: "x509", URI: "mongodb://db.internal/?tls=true&authMechanism=MONGODB-X509",
		TLSInput: connections.TLSInput{ClientCertPEM: &client.CertPEM, ClientKeyPEM: &client.KeyPEM}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-application.tlsExpiry.kick:
	default:
		t.Fatal("creating a connection did not kick the monitor")
	}
	scrape := func() string {
		rec := httptest.NewRecorder()
		application.metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
		return rec.Body.String()
	}
	application.tlsExpiry.check(ctx)
	if body := scrape(); !strings.Contains(body, `mongorescue_connection_tls_cert_expiry_timestamp_seconds{connection="`+created.ID+`",kind="client"}`) {
		t.Fatalf("gauge missing:\n%s", body)
	}
	if err = conns.Delete(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	application.tlsExpiry.check(ctx)
	if body := scrape(); strings.Contains(body, "mongorescue_connection_tls_cert_expiry_timestamp_seconds{") {
		t.Fatalf("gauge remains for a deleted connection:\n%s", body)
	}
}
