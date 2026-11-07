package server

import (
	"context"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotls/mongotlstest"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

// TestConnectionTLSCertExpiringWarning checks that GET /api/v1/settings lists the
// connections with a certificate that expires within 30 days, and that the
// connection API carries the expiry for the badge.
func TestConnectionTLSCertExpiringWarning(t *testing.T) {
	f := newTargetsFixture(t)
	find := func() *settings.Warning {
		t.Helper()
		rec := f.do("GET", "/api/v1/settings", nil)
		var got struct {
			Warnings []settings.Warning `json:"warnings"`
		}
		decodeData(t, rec, &got)
		for i := range got.Warnings {
			if got.Warnings[i].ID == settings.WarningConnectionTLSCertExpiring {
				return &got.Warnings[i]
			}
		}
		return nil
	}
	if w := find(); w != nil {
		t.Fatalf("warning without certificates: %+v", w)
	}
	ca, err := mongotlstest.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	healthy, err := ca.ClientExpiring("healthy", time.Now().Add(90*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	soon, err := ca.ClientExpiring("soon", time.Now().Add(5*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, c := range []*models.Connection{
		{ID: "conn_ok", Name: "fine", URI: "mongodb://ok/?tls=true", CreatedAt: now, UpdatedAt: now,
			ConnectionTLS: models.ConnectionTLS{ClientCertPEM: healthy.CertPEM, ClientKeyPEM: healthy.KeyPEM}},
		{ID: "conn_soon", Name: "renew me", URI: "mongodb://soon/?tls=true", CreatedAt: now, UpdatedAt: now,
			ConnectionTLS: models.ConnectionTLS{ClientCertPEM: soon.CertPEM, ClientKeyPEM: soon.KeyPEM}},
	} {
		if err = f.store.SaveConnection(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
	w := find()
	if w == nil || w.Setting != "connections" || len(w.Connections) != 1 || w.Connections[0].ID != "conn_soon" {
		t.Fatalf("warning = %+v", w)
	}
	if c := w.Connections[0]; len(c.Certs) != 1 || c.Certs[0].Kind != "client" || c.Certs[0].Expired {
		t.Fatalf("certs = %+v", c.Certs)
	}
}
