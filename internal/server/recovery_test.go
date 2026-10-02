package server

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/metabackup"
	"github.com/yigitcittan/mongorescue/internal/recoverykit"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

const kitPassphrase = "a long kit passphrase"

// kitFixture is a full server with auth, settings, storage targets, the audit log
// and the recovery kit.
type kitFixture struct {
	*authFixture
	key   []byte
	audit *audit.Service
	// mux serves the routes without the auth middleware.
	mux http.Handler
}

func newKitFixture(t *testing.T, staticKey string) *kitFixture {
	t.Helper()
	srv, _, _ := setupTestServer(t)
	st := storetest.New(t)
	authSvc := newTestAuth(t, st, staticKey)
	settingsSvc := newTestSettings(t, st, settings.Defaults().Security)
	targetSvc := targets.NewService(st, nil, t.TempDir())
	key, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	kit, err := recoverykit.New(recoverykit.Config{SecretKey: key, Settings: settingsSvc, Targets: targetSvc, WorkFactor: 10})
	if err != nil {
		t.Fatal(err)
	}
	auditSvc := audit.NewService(st, slog.New(slog.DiscardHandler))
	full := NewServer(bootConfig(), st, srv.backupEngine, srv.restoreEngine, srv.storageDriver, srv.scheduler, nil, nil,
		WithAuth(authSvc), WithSettings(settingsSvc), WithStorageTargets(targetSvc), WithRecoveryKit(kit), WithAudit(auditSvc))
	return &kitFixture{authFixture: &authFixture{h: full.Handler(), auth: authSvc}, key: key, audit: auditSvc, mux: full.mux}
}

func kitWarningActive(t *testing.T, b *browser) bool {
	t.Helper()
	var res settingsResponse
	decodeData(t, b.do("GET", "/api/v1/settings", nil, nil), &res)
	return slices.ContainsFunc(res.Warnings, func(w settings.Warning) bool { return w.ID == settings.WarningRecoveryKit })
}

// TestRecoveryKitDownload checks the download: it needs the current password and a
// long enough passphrase, decrypts with the passphrase to an archive holding
// secret.key, is audited, and resolves the reminder.
func TestRecoveryKitDownload(t *testing.T) {
	f := newKitFixture(t, "")
	b := f.browser(t)
	b.setup(f.authFixture)
	if !kitWarningActive(t, b) {
		t.Fatal("the reminder must be active before the first download")
	}

	rec := b.do("POST", "/api/v1/recovery-kit", map[string]string{"passphrase": kitPassphrase, "current_password": "wrong password"}, nil)
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), kitPassphrase) {
		t.Fatalf("wrong password: %d %s", rec.Code, rec.Body.String())
	}
	rec = b.do("POST", "/api/v1/recovery-kit", map[string]string{"passphrase": "too short", "current_password": testPassword}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("short passphrase: %d %s", rec.Code, rec.Body.String())
	}
	rec = b.do("POST", "/api/v1/recovery-kit", map[string]string{"passphrase": kitPassphrase, "current_password": testPassword}, nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/octet-stream" ||
		!strings.Contains(rec.Header().Get("Content-Disposition"), "mongorescue-recovery-kit-") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("download: %d %v", rec.Code, rec.Header())
	}

	dec, err := encryption.NewDecryptor(encryption.DecryptorConfig{Passphrase: kitPassphrase})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := dec.Decrypt(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decrypt kit: %v", err)
	}
	found := false
	tr := tar.NewReader(plain)
	for {
		hdr, nextErr := tr.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		if hdr.Name == recoverykit.SecretKeyName {
			body, _ := io.ReadAll(tr)
			found = strings.TrimSpace(string(body)) == secretbox.EncodeKey(f.key)
		}
	}
	if !found {
		t.Fatal("the kit does not hold secret.key")
	}
	if kitWarningActive(t, b) {
		t.Fatal("a download must resolve the reminder")
	}

	entries, err := f.audit.List(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var ok, denied int
	for _, e := range entries {
		if e.Tool != recoveryKitRoute {
			continue
		}
		if strings.Contains(string(e.Arguments), kitPassphrase) || strings.Contains(string(e.Arguments), testPassword) {
			t.Fatal("the audit log must not hold the passphrase or the password")
		}
		switch e.Result {
		case audit.ResultOK:
			ok++
		case audit.ResultDenied:
			denied++
		}
	}
	if ok != 1 || denied != 1 {
		t.Fatalf("audit: %d ok, %d denied; want 1 and 1", ok, denied)
	}
}

// kitBody is a valid download request with password.
func kitBody(password string) map[string]string {
	return map[string]string{"passphrase": kitPassphrase, "current_password": password}
}

// TestRecoveryKitRefusesOperatorSessions checks that a signed-in user without the
// admin scope cannot download the kit, even with the right password. The principal is
// attached to the bare routes directly, so the auth service's own check is exercised
// (TestRecoveryKitNeedsTheAdminRole covers real role sessions).
func TestRecoveryKitRefusesOperatorSessions(t *testing.T) {
	f := newKitFixture(t, "")
	b := f.browser(t)
	b.setup(f.authFixture)
	operator := &auth.Principal{User: &auth.User{ID: "usr_operator", Username: "operator"}, Method: auth.MethodSession, Scope: auth.ScopeOperator}
	raw, _ := json.Marshal(kitBody(testPassword))
	rec := httptest.NewRecorder()
	asPrincipal(f.mux, operator).ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/recovery-kit", bytes.NewReader(raw)))
	if rec.Code != http.StatusForbidden || rec.Header().Get("Content-Type") == "application/octet-stream" {
		t.Fatalf("operator session: %d %s", rec.Code, rec.Body.String())
	}
}

// TestRecoveryKitNeedsTheCSRFToken checks that a session request without
// X-CSRF-Token is refused before the password is checked.
func TestRecoveryKitNeedsTheCSRFToken(t *testing.T) {
	f := newKitFixture(t, "")
	b := f.browser(t)
	b.setup(f.authFixture)
	b.csrf = ""
	if rec := b.do("POST", "/api/v1/recovery-kit", kitBody(testPassword), nil); rec.Code != http.StatusForbidden || rec.Header().Get("Content-Type") == "application/octet-stream" {
		t.Fatalf("without CSRF: %d %s", rec.Code, rec.Body.String())
	}
}

// TestRecoveryKitThrottlesWrongPasswords checks that repeated wrong passwords get
// 429 with Retry-After, that the right password is then refused too, and that the
// audit log records the throttled attempts as rate limited with status 429.
func TestRecoveryKitThrottlesWrongPasswords(t *testing.T) {
	f := newKitFixture(t, "")
	b := f.browser(t)
	b.setup(f.authFixture)
	throttled := false
	for range 20 {
		rec := b.do("POST", "/api/v1/recovery-kit", kitBody("wrong password"), nil)
		if rec.Code == http.StatusTooManyRequests {
			if rec.Header().Get("Retry-After") == "" {
				t.Fatal("429 without Retry-After")
			}
			throttled = true
			break
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("wrong password: %d %s", rec.Code, rec.Body.String())
		}
	}
	if !throttled {
		t.Fatal("repeated wrong passwords were never throttled")
	}
	if rec := b.do("POST", "/api/v1/recovery-kit", kitBody(testPassword), nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("right password while throttled: %d", rec.Code)
	}
	entries, err := f.audit.List(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(entries, func(e *audit.Entry) bool {
		return e.Tool == recoveryKitRoute && e.Result == audit.ResultRateLimited && e.HTTPStatus == http.StatusTooManyRequests
	}) {
		t.Fatalf("no rate-limited audit entry with status 429: %+v", entries)
	}
	if slices.ContainsFunc(entries, func(e *audit.Entry) bool {
		return e.Tool == recoveryKitRoute && e.Result == audit.ResultDenied && e.HTTPStatus == http.StatusTooManyRequests
	}) {
		t.Fatal("throttled attempts must not be audited as denied")
	}
}

// TestMetadataBackupEndpoints checks that a snapshot can be started and its status
// read, and that a stopped service answers 503.
func TestMetadataBackupEndpoints(t *testing.T) {
	ctx := context.Background()
	srv, _, _ := setupTestServer(t)
	st := storetest.New(t)
	dataDir := t.TempDir()
	targetSvc := targets.NewService(st, storage.NewForTarget, dataDir)
	if _, _, err := targetSvc.EnsureDefault(ctx, filepath.Join(t.TempDir(), "backups")); err != nil {
		t.Fatal(err)
	}
	mb := metabackup.New(metabackup.Config{InstallID: "0123456789abcdef", Store: st, Targets: targetSvc, DataDir: dataDir, Logger: slog.New(slog.DiscardHandler)})
	h := keyed{NewServer(bootConfig(), st, srv.backupEngine, srv.restoreEngine, srv.storageDriver, srv.scheduler, nil, nil,
		WithAuth(newTestAuth(t, st, testAPIKey)), WithMetadataBackup(mb)).Handler()}
	do := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}
	if rec := do("POST", "/api/v1/metadata-backup/run"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("run before start: %d", rec.Code)
	}
	mb.Start(ctx)
	defer mb.Stop()
	if rec := do("POST", "/api/v1/metadata-backup/run"); rec.Code != http.StatusAccepted {
		t.Fatalf("run: %d %s", rec.Code, rec.Body.String())
	}
	var status metabackup.Status
	for deadline := time.Now().Add(10 * time.Second); status.Last == nil; {
		if time.Now().After(deadline) {
			t.Fatalf("no snapshot recorded: %+v", status)
		}
		time.Sleep(10 * time.Millisecond)
		decodeData(t, do("GET", "/api/v1/metadata-backup"), &status)
	}
	if !strings.HasPrefix(status.Last.Key, metabackup.Prefix+"0123456789abcdef/") || status.Prefix != metabackup.Prefix+"0123456789abcdef/" || status.LastError != "" {
		t.Fatalf("status = %+v", status)
	}
}

// TestRecoveryKitRefusesAPIKeys checks that an admin API key cannot download the
// kit, even with a password.
func TestRecoveryKitRefusesAPIKeys(t *testing.T) {
	const static = "recovery-kit-static-key"
	f := newKitFixture(t, static)
	b := f.browser(t)
	b.setup(f.authFixture)
	api := f.browser(t)
	rec := api.do("POST", "/api/v1/recovery-kit", map[string]string{"passphrase": kitPassphrase, "current_password": testPassword},
		map[string]string{"Authorization": "Bearer " + static})
	if rec.Code != http.StatusForbidden || rec.Header().Get("Content-Type") == "application/octet-stream" {
		t.Fatalf("API key: %d %s", rec.Code, rec.Body.String())
	}
	if rec := api.do("GET", "/api/v1/recovery-kit", nil, map[string]string{"X-API-Key": static}); rec.Code != http.StatusOK {
		t.Fatalf("status with an API key: %d %s", rec.Code, rec.Body.String())
	}
}
