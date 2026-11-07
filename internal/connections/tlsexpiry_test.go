package connections_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotls"
	"github.com/yigitcittan/mongorescue/internal/mongotls/mongotlstest"
)

// expiringClient returns a CA, and a client certificate and key that expire at
// notAfter.
func expiringClient(t *testing.T, notAfter time.Time) (ca, cert, key string) {
	t.Helper()
	authority, err := mongotlstest.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	client, err := authority.ClientExpiring("backup", notAfter)
	if err != nil {
		t.Fatal(err)
	}
	return authority.CertPEM, client.CertPEM, client.KeyPEM
}

// TestTLSExpiryIsAnnouncedOncePerThreshold proves that a short-lived client
// certificate is reported once at 30 days, once at 7 days and once when it has
// expired, however often the check runs, and that a renewed certificate starts over.
func TestTLSExpiryIsAnnouncedOncePerThreshold(t *testing.T) {
	var changes atomic.Int32
	svc, _, _ := newService(t, connections.WithOnChange(func() { changes.Add(1) }))
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Second)
	_, cert, key := expiringClient(t, start.Add(20*24*time.Hour))
	c, err := svc.Create(ctx, connections.Input{Name: "x509", URI: x509URI, TLSInput: connections.TLSInput{
		ClientCertPEM: &cert, ClientKeyPEM: &key}})
	if err != nil {
		t.Fatal(err)
	}
	if changes.Load() != 1 {
		t.Fatalf("OnChange calls after create = %d, want 1", changes.Load())
	}
	if len(c.TLSCertExpiry) != 1 || c.TLSCertExpiry[0].Kind != mongotls.KindClient {
		t.Fatalf("create result expiries = %+v", c.TLSCertExpiry)
	}

	tracker := connections.NewTLSExpiryTracker()
	announce := func(at time.Time) []connections.TLSCertStatus {
		t.Helper()
		statuses, statusErr := svc.TLSCertStatuses(ctx, at)
		if statusErr != nil {
			t.Fatal(statusErr)
		}
		return tracker.Announce(statuses)
	}
	steps := []struct {
		name string
		at   time.Time
		want models.TLSExpiryLevel
	}{
		{"in the 30 day window", start, models.TLSExpiryWarn},
		{"an hour later", start.Add(time.Hour), models.TLSExpiryOK},
		{"in the 7 day window", start.Add(14 * 24 * time.Hour), models.TLSExpiryCritical},
		{"and again", start.Add(15 * 24 * time.Hour), models.TLSExpiryOK},
		{"expired", start.Add(21 * 24 * time.Hour), models.TLSExpiryExpired},
		{"expired, checked again", start.Add(22 * 24 * time.Hour), models.TLSExpiryOK},
	}
	for _, s := range steps {
		got := announce(s.at)
		if s.want == models.TLSExpiryOK {
			if len(got) != 0 {
				t.Fatalf("%s: announced %+v, want nothing", s.name, got)
			}
			continue
		}
		if len(got) != 1 || got[0].Level != s.want || got[0].ConnectionID != c.ID || got[0].Kind != mongotls.KindClient {
			t.Fatalf("%s: announced %+v, want one %v of %s", s.name, got, s.want, c.ID)
		}
	}

	// Renewing the certificate (another fingerprint) starts over.
	_, cert2, key2 := expiringClient(t, start.Add(60*24*time.Hour))
	if _, err = svc.Update(ctx, c.ID, connections.Input{Name: "x509", URI: x509URI, TLSInput: connections.TLSInput{
		ClientCertPEM: &cert2, ClientKeyPEM: &key2}}); err != nil {
		t.Fatal(err)
	}
	if got := announce(start.Add(35 * 24 * time.Hour)); len(got) != 1 || got[0].Level != models.TLSExpiryWarn {
		t.Fatalf("renewed certificate: announced %+v, want one warn", got)
	}
	if err = svc.Delete(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if got := announce(start.Add(23 * 24 * time.Hour)); len(got) != 0 {
		t.Fatalf("after delete: announced %+v", got)
	}
	if changes.Load() != 3 {
		t.Fatalf("OnChange calls = %d, want 3", changes.Load())
	}
}

// TestTLSExpiryInAPIResponsesNeverCarriesKeys checks that List and Get report the
// earliest client and CA expiry and nothing sensitive.
func TestTLSExpiryInAPIResponsesNeverCarriesKeys(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	notAfter := time.Now().UTC().Add(10 * 24 * time.Hour).Truncate(time.Second)
	ca, cert, key := expiringClient(t, notAfter)
	c, err := svc.Create(ctx, connections.Input{Name: "x509", URI: x509URI, TLSInput: connections.TLSInput{
		CAPEM: &ca, ClientCertPEM: &cert, ClientKeyPEM: &key}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.TLSCertExpiry) != 2 || got.TLSCertExpiry[0].Kind != "client" || got.TLSCertExpiry[1].Kind != "ca" {
		t.Fatalf("expiries = %+v", got.TLSCertExpiry)
	}
	if !got.TLSCertExpiry[0].NotAfter.Equal(notAfter) {
		t.Errorf("client expiry = %v, want %v", got.TLSCertExpiry[0].NotAfter, notAfter)
	}
	// The test CA is valid for a day: it expires first and is critical.
	if lvl := got.TLSCertExpiry[1].Level(time.Now()); lvl != models.TLSExpiryCritical {
		t.Errorf("CA level = %v, want critical", lvl)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "PRIVATE KEY") {
		t.Fatalf("response leaks the key: %s", raw)
	}
}
