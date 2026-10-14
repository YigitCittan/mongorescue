package vault

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParseAddr(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		insecure bool
		err      error
	}{
		{in: "https://vault.example.com:8200", want: "https://vault.example.com:8200"},
		{in: " https://vault:8200/ ", want: "https://vault:8200"},
		{in: "http://127.0.0.1:8200", err: ErrInsecureAddr},
		{in: "http://127.0.0.1:8200", insecure: true, want: "http://127.0.0.1:8200"},
		{in: "https://vault:8200/v1/secret", err: ErrInvalidAddr},
		{in: "https://user:pass@vault:8200", err: ErrInvalidAddr},
		{in: "https://vault:8200?x=1", err: ErrInvalidAddr},
		{in: "ftp://vault", err: ErrInvalidAddr},
		{in: "vault:8200", err: ErrInvalidAddr},
	} {
		got, err := ParseAddr(tc.in, tc.insecure)
		if !errors.Is(err, tc.err) || got != tc.want {
			t.Errorf("ParseAddr(%q, %v) = %q, %v; want %q, %v", tc.in, tc.insecure, got, err, tc.want, tc.err)
		}
	}
}

func TestAuthFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if _, err := AuthFromEnv(env(nil)); !errors.Is(err, ErrNoAuth) {
		t.Errorf("no auth = %v", err)
	}
	a, err := AuthFromEnv(env(map[string]string{EnvToken: " s.tok ", EnvRoleID: "r", EnvSecretID: "s"}))
	if err != nil || a.Method != AuthToken || a.Token != "s.tok" {
		t.Errorf("token wins = %+v, %v", a, err)
	}
	a, _ = AuthFromEnv(env(map[string]string{EnvRoleID: "r", EnvSecretID: "s"}))
	if a.Method != AuthAppRole || a.Mount != "approle" {
		t.Errorf("approle = %+v", a)
	}
	a, _ = AuthFromEnv(env(map[string]string{EnvK8sRole: "mr", EnvK8sMount: "k8s"}))
	if a.Method != AuthKubernetes || a.Mount != "k8s" || a.TokenPath != defaultK8sTokenPath {
		t.Errorf("kubernetes = %+v", a)
	}
}

func TestClientNeverFollowsRedirects(t *testing.T) {
	var elsewhere atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1/steal", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c, err := New(Config{Addr: srv.URL, AllowInsecure: true, Auth: Auth{Method: AuthToken, Token: "s.secret-token"}, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Do(context.Background(), http.MethodGet, "/v1/secret/data/x", nil, nil)
	var se *StatusError
	if !errors.As(err, &se) || !se.Permanent() || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("redirect = %v", err)
	}
	if elsewhere.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
	if strings.Contains(err.Error(), "s.secret-token") {
		t.Fatal("the error holds the token")
	}
}

func TestClientLogsInAgainWhenTheTokenExpired(t *testing.T) {
	var logins atomic.Int64
	valid := atomic.Value{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/auth/kubernetes/login":
			n := logins.Add(1)
			tok := "s.login-" + string(rune('0'+n))
			valid.Store(tok)
			_, _ = w.Write([]byte(`{"auth":{"client_token":"` + tok + `"}}`))
		case r.Header.Get("X-Vault-Token") != valid.Load():
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
		default:
			_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
		}
	}))
	defer srv.Close()
	jwt := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(jwt, []byte("jwt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{Addr: srv.URL, AllowInsecure: true, HTTPClient: srv.Client(),
		Auth: Auth{Method: AuthKubernetes, Mount: "kubernetes", Role: "mr", TokenPath: jwt}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = c.Do(ctx, http.MethodGet, "/v1/x", nil, nil); err != nil || logins.Load() != 1 {
		t.Fatalf("first call = %v, logins %d", err, logins.Load())
	}
	valid.Store("s.other")
	if err = c.Do(ctx, http.MethodGet, "/v1/x", nil, nil); err != nil || logins.Load() != 2 {
		t.Fatalf("call after expiry = %v, logins %d", err, logins.Load())
	}
	missing, _ := New(Config{Addr: srv.URL, AllowInsecure: true, HTTPClient: srv.Client(),
		Auth: Auth{Method: AuthKubernetes, Mount: "kubernetes", Role: "mr", TokenPath: filepath.Join(t.TempDir(), "none")}})
	if err = missing.Do(ctx, http.MethodGet, "/v1/x", nil, nil); !errors.Is(err, ErrAuthConfig) {
		t.Fatalf("missing service account token = %v", err)
	}
}

func TestNewRefusesInsecureAndUnauthenticated(t *testing.T) {
	if _, err := New(Config{Addr: "http://vault:8200", Auth: Auth{Method: AuthToken, Token: "t"}}); !errors.Is(err, ErrInsecureAddr) {
		t.Errorf("plain http = %v", err)
	}
	if _, err := New(Config{Addr: "https://vault:8200"}); !errors.Is(err, ErrNoAuth) {
		t.Errorf("no auth = %v", err)
	}
	if _, err := New(Config{Addr: "https://vault:8200", Auth: Auth{Method: AuthToken, Token: "t"}, CACert: filepath.Join(t.TempDir(), "none.pem")}); err == nil {
		t.Error("a missing CA file was accepted")
	}
}
