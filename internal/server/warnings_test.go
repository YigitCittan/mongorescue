package server

import (
	"context"
	"net/http"
	"testing"

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
