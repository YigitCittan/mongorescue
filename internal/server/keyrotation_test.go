package server

import (
	"context"
	"log/slog"
	"net/http"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/keyrotation"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// fakeKeyRotator stands in for *keyrotation.Rotator.
type fakeKeyRotator struct {
	env   bool
	calls int
}

func (f *fakeKeyRotator) FromEnv() bool         { return f.env }
func (f *fakeKeyRotator) Fingerprint() string   { return "fp-current" }
func (f *fakeKeyRotator) PreviousKeyKept() bool { return false }
func (f *fakeKeyRotator) Rotate(context.Context) (*keyrotation.Result, error) {
	f.calls++
	return &keyrotation.Result{OldFingerprint: "fp-current", NewFingerprint: "fp-new"}, nil
}

func newRotationFixture(t *testing.T, r *fakeKeyRotator, staticKey string) *authFixture {
	t.Helper()
	srv, _, _ := setupTestServer(t)
	st := storetest.New(t)
	authSvc := newTestAuth(t, st, staticKey)
	settingsSvc := newTestSettings(t, st, settings.Defaults().Security)
	_ = slog.Default()
	full := NewServer(bootConfig(), st, srv.backupEngine, srv.restoreEngine, srv.storageDriver, srv.scheduler, nil, nil,
		WithAuth(authSvc), WithSettings(settingsSvc), WithKeyRotation(r))
	return &authFixture{h: full.Handler(), auth: authSvc}
}

func TestRotateSecretKeyEndpoint(t *testing.T) {
	r := &fakeKeyRotator{}
	f := newRotationFixture(t, r, "")
	b := f.browser(t)
	b.setup(f)
	if rec := b.do("GET", "/api/v1/security/key-rotation", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body)
	}
	if rec := b.do("POST", "/api/v1/security/rotate-secret-key", map[string]string{"current_password": "wrong password"}, nil); rec.Code != http.StatusForbidden || r.calls != 0 {
		t.Fatalf("wrong password: %d %s", rec.Code, rec.Body)
	}
	rec := b.do("POST", "/api/v1/security/rotate-secret-key", map[string]string{"current_password": testPassword}, nil)
	if rec.Code != http.StatusOK || r.calls != 1 {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body)
	}
}

func TestRotateSecretKeyRefusesEnvKeysAndAPIKeys(t *testing.T) {
	const static = "rotation-static-key-value"
	r := &fakeKeyRotator{env: true}
	f := newRotationFixture(t, r, static)
	b := f.browser(t)
	b.setup(f)
	if rec := b.do("POST", "/api/v1/security/rotate-secret-key", map[string]string{"current_password": testPassword}, nil); rec.Code != http.StatusConflict {
		t.Fatalf("env key: %d %s", rec.Code, rec.Body)
	}
	api := f.browser(t)
	rec := api.do("POST", "/api/v1/security/rotate-secret-key", map[string]string{"current_password": testPassword},
		map[string]string{"Authorization": "Bearer " + static})
	if rec.Code != http.StatusForbidden || r.calls != 0 {
		t.Fatalf("API key: %d %s", rec.Code, rec.Body)
	}
}
