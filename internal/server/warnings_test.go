package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

// TestSettingsWarningsEndpoints checks the persistent warnings in GET
// /api/v1/settings and dismissing them.
func TestSettingsWarningsEndpoints(t *testing.T) {
	f := newTargetsFixture(t)
	warnings := func() []settings.Warning {
		t.Helper()
		rec := f.do("GET", "/api/v1/settings", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET settings: %d", rec.Code)
		}
		var got struct {
			Warnings []settings.Warning `json:"warnings"`
		}
		decodeData(t, rec, &got)
		if got.Warnings == nil {
			t.Fatal("warnings must be a list, never null")
		}
		return got.Warnings
	}
	if w := warnings(); len(w) != 0 {
		t.Fatalf("warnings = %+v; want none", w)
	}
	if _, err := f.settings.RaiseEncryptionOffWarning(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := warnings()
	if len(w) != 1 || w[0].ID != settings.WarningEncryptionOff || w[0].Setting != "encryption" {
		t.Fatalf("warnings = %+v; want the encryption warning", w)
	}

	if rec := f.do("POST", "/api/v1/settings/warnings/nope/dismiss", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("dismiss unknown: %d", rec.Code)
	}
	if rec := f.do("POST", "/api/v1/settings/warnings/"+settings.WarningEncryptionOff+"/dismiss", nil); rec.Code != http.StatusOK {
		t.Fatalf("dismiss: %d %s", rec.Code, rec.Body)
	}
	if w := warnings(); len(w) != 0 {
		t.Fatalf("warnings after dismiss = %+v", w)
	}
}

// TestConnectionTLSWarning checks that GET /api/v1/settings lists the connections
// with loosened TLS checks in a warning while any has one on.
func TestConnectionTLSWarning(t *testing.T) {
	f := newTargetsFixture(t)
	tlsWarning := func() *settings.Warning {
		t.Helper()
		rec := f.do("GET", "/api/v1/settings", nil)
		var got struct {
			Warnings []settings.Warning `json:"warnings"`
		}
		decodeData(t, rec, &got)
		for i := range got.Warnings {
			if got.Warnings[i].ID == settings.WarningConnectionTLSLoosened {
				return &got.Warnings[i]
			}
		}
		return nil
	}
	if w := tlsWarning(); w != nil {
		t.Fatalf("warning without loosened connections: %+v", w)
	}
	now := time.Now().UTC()
	for _, c := range []*models.Connection{
		{ID: "conn_insecure", Name: "lab", URI: "mongodb://lab/?tls=true", CreatedAt: now, UpdatedAt: now,
			ConnectionTLS: models.ConnectionTLS{Insecure: true}},
		{ID: "conn_hosts", Name: "staging", URI: "mongodb://stg/?tls=true", CreatedAt: now, UpdatedAt: now,
			ConnectionTLS: models.ConnectionTLS{AllowInvalidHostnames: true}},
	} {
		if err := f.store.SaveConnection(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
	w := tlsWarning()
	if w == nil || len(w.Connections) != 2 || w.Setting != "connections" {
		t.Fatalf("warning = %+v", w)
	}
	byID := map[string]settings.WarningConnection{}
	for _, c := range w.Connections {
		byID[c.ID] = c
	}
	if !byID["conn_insecure"].Insecure || !byID["conn_hosts"].AllowInvalidHostnames || byID["conn_hosts"].Insecure {
		t.Fatalf("connections = %+v", w.Connections)
	}
}
