//go:build integration

package integration

import (
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/app"
	"github.com/yigitcittan/mongorescue/internal/config"
)

// The Keycloak realm of testdata/keycloak-realm.json, imported by
// scripts/test-integration-docker.sh (IT_PROVIDERS containing keycloak).
const (
	keycloakRealm    = "mongorescue"
	keycloakClient   = "mongorescue"
	keycloakSecret   = "mongorescue-it-client-secret" //nolint:gosec // G101: the test realm's client secret.
	keycloakRedirect = "https://mongorescue.test/auth/oidc/callback"
)

// requireKeycloak returns the base URL of the test Keycloak or skips.
func requireKeycloak(t *testing.T) string {
	t.Helper()
	base := strings.TrimRight(os.Getenv("MONGORESCUE_TEST_KEYCLOAK_URL"), "/")
	if base == "" {
		t.Skip("MONGORESCUE_TEST_KEYCLOAK_URL not set (run scripts/test-integration-docker.sh with IT_PROVIDERS containing keycloak)")
	}
	return base
}

// ssoServer is a full MongoRescue instance (internal/app, as the binary wires it)
// served over HTTP, with its first administrator signed in.
type ssoServer struct {
	url   string
	admin *apiClient
	logs  *syncBuffer
}

func newSSOServer(t *testing.T) *ssoServer {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	logger, logs := captureLogger()
	a, err := app.New(cfg, logger, app.WithGetenv(func(string) string { return "" }))
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)

	admin := newAPIClient(t, srv.URL)
	if code, body := admin.do("POST", "/api/v1/setup", map[string]string{
		"setup_code": a.SetupCode(), "username": "breakglass", "password": "a long local admin password",
	}); code != http.StatusCreated {
		t.Fatalf("setup: %d %s", code, body)
	}
	admin.csrf = admin.me()
	return &ssoServer{url: srv.URL, admin: admin, logs: logs}
}

// me reads the session's CSRF token.
func (c *apiClient) me() string {
	c.t.Helper()
	code, body := c.do("GET", "/api/v1/auth/me", nil)
	if code != http.StatusOK {
		c.t.Fatalf("me: %d %s", code, body)
	}
	var env struct {
		Data struct {
			CSRF string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		c.t.Fatal(err)
	}
	return env.Data.CSRF
}

// ssoBrowser follows the single sign-on like a browser: a cookie jar for both
// MongoRescue and Keycloak, redirects followed by hand, and Keycloak's login form
// filled in.
type ssoBrowser struct {
	t      *testing.T
	server string
	client *http.Client
}

func newSSOBrowser(t *testing.T, server string) *ssoBrowser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &ssoBrowser{t: t, server: server, client: &http.Client{
		Jar: jar, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (b *ssoBrowser) send(req *http.Request) (*http.Response, string) {
	b.t.Helper()
	resp, err := b.client.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", req.Method, req.URL.Redacted(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		b.t.Fatal(err)
	}
	return resp, string(raw)
}

func (b *ssoBrowser) get(target string) (*http.Response, string) {
	b.t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		b.t.Fatal(err)
	}
	return b.send(req)
}

var loginFormAction = regexp.MustCompile(`<form[^>]+id="kc-form-login"[^>]+action="([^"]+)"`)

// signIn runs the whole flow for a Keycloak user and returns the final redirect of
// MongoRescue's callback (the return path, or /?oidc_error=...).
func (b *ssoBrowser) signIn(username, password string) *url.URL {
	b.t.Helper()
	resp, body := b.get(b.server + "/auth/oidc/start?return_to=" + url.QueryEscape("/#/backups"))
	if resp.StatusCode != http.StatusFound {
		b.t.Fatalf("start: %d %s", resp.StatusCode, body)
	}
	authURL := resp.Header.Get("Location")
	resp, body = b.get(authURL)
	if resp.StatusCode != http.StatusOK {
		b.t.Fatalf("keycloak authorize: %d", resp.StatusCode)
	}
	m := loginFormAction.FindStringSubmatch(body)
	if m == nil {
		b.t.Fatalf("no login form on the keycloak page")
	}
	form := url.Values{"username": {username}, "password": {password}, "credentialId": {""}}
	req, err := http.NewRequest(http.MethodPost, html.UnescapeString(m[1]), strings.NewReader(form.Encode()))
	if err != nil {
		b.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, _ = b.send(req)
	if resp.StatusCode != http.StatusFound {
		return nil // Keycloak showed the form again: wrong credentials
	}
	cb, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || cb.String() == "" || !strings.HasPrefix(cb.String(), keycloakRedirect) {
		b.t.Fatalf("keycloak redirected to %q; want the registered callback", resp.Header.Get("Location"))
	}
	// The registered redirect URI names the public host; the browser would reach
	// MongoRescue there. Send the same path and query to the test server.
	resp, body = b.get(b.server + cb.Path + "?" + cb.RawQuery)
	if resp.StatusCode != http.StatusFound {
		b.t.Fatalf("callback: %d %s", resp.StatusCode, body)
	}
	final, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		b.t.Fatal(err)
	}
	return final
}

// identity returns the signed-in user's name, provider and role ("" when signed
// out).
func (b *ssoBrowser) identity() (name, provider, role string) {
	b.t.Helper()
	_, body := b.get(b.server + "/api/v1/auth/me")
	var env struct {
		Data struct {
			User *struct {
				Username     string `json:"username"`
				AuthProvider string `json:"auth_provider"`
			} `json:"user"`
			Role string `json:"role"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		b.t.Fatal(err)
	}
	if env.Data.User == nil {
		return "", "", ""
	}
	return env.Data.User.Username, env.Data.User.AuthProvider, env.Data.Role
}

// TestKeycloakSingleSignOn signs in through a real Keycloak (dev mode, imported
// realm): discovery test, the authorization code flow with PKCE through
// Keycloak's login form, group mappings to roles, the domain filter, provider
// logout and the audit log.
func TestKeycloakSingleSignOn(t *testing.T) {
	kc := requireKeycloak(t)
	issuer := kc + "/realms/" + keycloakRealm
	s := newSSOServer(t)

	code, body := s.admin.do("POST", "/api/v1/settings/oidc/test", map[string]string{"issuer": issuer})
	if code != http.StatusOK || !strings.Contains(string(body), `"RS256"`) {
		t.Fatalf("provider test: %d %s", code, body)
	}
	// Turning single sign-on on runs discovery against Keycloak.
	code, body = s.admin.do("PUT", "/api/v1/settings", map[string]any{"oidc": map[string]any{
		"enabled": true, "display_name": "Keycloak", "issuer": issuer, "client_id": keycloakClient,
		"client_secret": keycloakSecret, "redirect_url": keycloakRedirect, "rp_logout": true,
		"role_mappings": []map[string]string{{"group": "backup-admins", "role": "admin"}, {"group": "backup-ops", "role": "operator"}},
		"allowed_email_domains": []string{"corp.test"},
	}})
	if code != http.StatusOK || strings.Contains(string(body), keycloakSecret) {
		t.Fatalf("enable single sign-on: %d %s", code, body)
	}

	jane := newSSOBrowser(t, s.url)
	if final := jane.signIn("jane", "jane-it-password-1"); final == nil || final.String() != "/#/backups" {
		t.Fatalf("jane's sign-in ended at %v; want /#/backups", final)
	}
	if name, provider, role := jane.identity(); name != "jane" || provider != "oidc" || role != "admin" {
		t.Fatalf("jane = %q %q %q; want an oidc admin", name, provider, role)
	}

	bob := newSSOBrowser(t, s.url)
	if final := bob.signIn("bob", "bob-it-password-1"); final == nil || final.Query().Get("oidc_error") != "" {
		t.Fatalf("bob's sign-in ended at %v", final)
	}
	if name, _, role := bob.identity(); name != "bob" || role != "operator" {
		t.Fatalf("bob = %q %q; want an operator", name, role)
	}

	// eve's verified email is outside the allowed domains.
	eve := newSSOBrowser(t, s.url)
	if final := eve.signIn("eve", "eve-it-password-1"); final == nil || final.Query().Get("oidc_error") != "domain_not_allowed" {
		t.Fatalf("eve's sign-in ended at %v; want domain_not_allowed", final)
	}
	// Wrong credentials never reach MongoRescue.
	if final := newSSOBrowser(t, s.url).signIn("jane", "not her password"); final != nil {
		t.Fatalf("a wrong Keycloak password ended at %v", final)
	}

	// Provider logout.
	req, err := http.NewRequest(http.MethodPost, s.url+"/api/v1/auth/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, meBody := jane.get(s.url + "/api/v1/auth/me")
	var me struct {
		Data struct {
			CSRF string `json:"csrf_token"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(meBody), &me)
	req.Header.Set("X-CSRF-Token", me.Data.CSRF)
	resp, logoutBody := jane.send(req)
	if resp.StatusCode != http.StatusOK || !strings.Contains(logoutBody, issuer+"/protocol/openid-connect/logout?") {
		t.Fatalf("logout: %d %s", resp.StatusCode, logoutBody)
	}

	// The audit log has every callback, and no token or code anywhere.
	code, body = s.admin.do("GET", "/api/v1/audit/events?action="+url.QueryEscape("/auth/oidc/callback"), nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"provider":"oidc"`) || !strings.Contains(string(body), `"reason":"domain_not_allowed"`) {
		t.Fatalf("audit log: %d %s", code, body)
	}
	// "eyJ" starts every JWT (an encoded '{"').
	for _, secret := range []string{keycloakSecret, "jane-it-password-1", "eyJ"} {
		for where, text := range map[string]string{"server logs": s.logs.String(), "audit log": string(body)} {
			if strings.Contains(text, secret) {
				t.Errorf("the %s contain %q", where, secret)
			}
		}
	}
}
