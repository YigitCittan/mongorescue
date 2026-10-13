package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/keywrap"
)

// fakeVault answers the Transit encrypt and decrypt endpoints and the AppRole and
// Kubernetes logins; tokens it issued can be revoked to force a new login.
type fakeVault struct {
	mu        sync.Mutex
	requests  []string
	tokens    map[string]bool
	logins    []map[string]string
	namespace string
	failNext  int
}

func (f *fakeVault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.URL.Path)
	f.namespace = r.Header.Get("X-Vault-Namespace")
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	reply := func(status int, v any) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	if f.failNext != 0 {
		status := f.failNext
		f.failNext = 0
		reply(status, map[string]any{"errors": []string{"injected"}})
		return
	}
	switch r.URL.Path {
	case "/v1/auth/approle/login", "/v1/auth/k8s/login":
		f.logins = append(f.logins, body)
		token := "s.issued-" + string(rune('a'+len(f.logins)))
		f.tokens[token] = true
		reply(http.StatusOK, map[string]any{"auth": map[string]any{"client_token": token}})
		return
	}
	if !f.tokens[r.Header.Get("X-Vault-Token")] {
		reply(http.StatusForbidden, map[string]any{"errors": []string{"permission denied"}})
		return
	}
	switch r.URL.Path {
	case "/v1/team/transit/encrypt/mongorescue":
		reply(http.StatusOK, map[string]any{"data": map[string]string{"ciphertext": "vault:v1:" + body["plaintext"]}})
	case "/v1/team/transit/decrypt/mongorescue":
		pt, ok := strings.CutPrefix(body["ciphertext"], "vault:v1:")
		if !ok {
			reply(http.StatusBadRequest, map[string]any{"errors": []string{"invalid ciphertext: no prefix"}})
			return
		}
		reply(http.StatusOK, map[string]any{"data": map[string]string{"plaintext": pt}})
	default:
		reply(http.StatusNotFound, map[string]any{"errors": []string{}})
	}
}

func newVaultTest(t *testing.T, env map[string]string) (*fakeVault, keywrap.Wrapper, string) {
	t.Helper()
	fake := &fakeVault{tokens: map[string]bool{"s.root": true}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	uri := srv.URL + "/team/transit/keys/mongorescue"
	w, err := OpenWith(context.Background(), keywrap.Spec{Provider: keywrap.ProviderVaultTransit, KeyURI: uri},
		Options{HTTPClient: srv.Client(), Getenv: func(k string) string { return env[k] }, AllowInsecure: true})
	if err != nil {
		t.Fatal(err)
	}
	return fake, w, uri
}

func roundTrip(t *testing.T, w keywrap.Wrapper) []byte {
	t.Helper()
	key := bytes.Repeat([]byte{3}, 32)
	ct, err := w.Wrap(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(ct, []byte("vault:v1:")) || bytes.Contains(ct, key) {
		t.Fatalf("ciphertext = %q", ct)
	}
	got, err := w.Unwrap(context.Background(), ct)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("Unwrap = %v", err)
	}
	return ct
}

func TestVaultTransitWithToken(t *testing.T) {
	fake, w, uri := newVaultTest(t, map[string]string{EnvVaultToken: "s.root", EnvVaultNamespace: "admin/team"})
	if w.Provider() != keywrap.ProviderVaultTransit || w.KeyID() != strings.TrimSuffix(uri, "") {
		t.Fatalf("Provider/KeyID = %s %s", w.Provider(), w.KeyID())
	}
	ct := roundTrip(t, w)
	if strings.Join(fake.requests, ",") != "/v1/team/transit/encrypt/mongorescue,/v1/team/transit/decrypt/mongorescue" {
		t.Errorf("requests = %v", fake.requests)
	}
	if fake.namespace != "admin/team" {
		t.Errorf("namespace header = %q", fake.namespace)
	}
	if _, err := w.Unwrap(context.Background(), []byte("vault:v9")); !keywrap.IsPermanent(err) || !strings.Contains(err.Error(), "invalid ciphertext") {
		t.Errorf("bad ciphertext = %v; want a permanent error", err)
	}
	if _, err := w.Unwrap(context.Background(), []byte("azkv1:x")); !keywrap.IsPermanent(err) {
		t.Errorf("foreign ciphertext = %v; want a permanent error", err)
	}
	// A static token that is refused is not retried with a login.
	delete(fake.tokens, "s.root")
	if _, err := w.Unwrap(context.Background(), ct); !keywrap.IsPermanent(err) || len(fake.logins) != 0 {
		t.Errorf("revoked token = %v (logins %d)", err, len(fake.logins))
	}
	fake.tokens["s.root"] = true
	fake.failNext = http.StatusServiceUnavailable // sealed
	if _, err := w.Unwrap(context.Background(), ct); err == nil || keywrap.IsPermanent(err) {
		t.Errorf("sealed Vault = %v; want a transient error", err)
	}
}

func TestVaultTransitWithAppRoleLogsInAgain(t *testing.T) {
	fake, w, _ := newVaultTest(t, map[string]string{EnvVaultRoleID: "role-1", EnvVaultSecretID: "secret-1"})
	ct := roundTrip(t, w)
	if len(fake.logins) != 1 || fake.logins[0]["role_id"] != "role-1" || fake.logins[0]["secret_id"] != "secret-1" {
		t.Fatalf("logins = %v", fake.logins)
	}
	// The token expired: one new login, then the call succeeds.
	for tok := range fake.tokens {
		if tok != "s.root" {
			delete(fake.tokens, tok)
		}
	}
	if _, err := w.Unwrap(context.Background(), ct); err != nil {
		t.Fatalf("Unwrap after the token expired = %v", err)
	}
	if len(fake.logins) != 2 {
		t.Errorf("logins = %d; want 2", len(fake.logins))
	}
}

func TestVaultTransitWithKubernetes(t *testing.T) {
	jwt := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(jwt, []byte("eyJhbGciOi.sa-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake, w, _ := newVaultTest(t, map[string]string{EnvVaultK8sRole: "mongorescue", EnvVaultK8sMount: "k8s", EnvVaultK8sTokenPath: jwt})
	roundTrip(t, w)
	if len(fake.logins) != 1 || fake.logins[0]["role"] != "mongorescue" || fake.logins[0]["jwt"] != "eyJhbGciOi.sa-token" {
		t.Fatalf("logins = %v", fake.logins)
	}

	_, missing, _ := newVaultTest(t, map[string]string{EnvVaultK8sRole: "r", EnvVaultK8sTokenPath: filepath.Join(t.TempDir(), "none")})
	if _, err := missing.Wrap(context.Background(), make([]byte, 32)); !keywrap.IsPermanent(err) {
		t.Errorf("missing service account token = %v; want permanent", err)
	}
}

func TestVaultTransitConfiguration(t *testing.T) {
	spec := keywrap.Spec{Provider: keywrap.ProviderVaultTransit, KeyURI: "https://vault:8200/transit/keys/k"}
	if _, err := OpenWith(context.Background(), spec, Options{Getenv: func(string) string { return "" }}); err == nil ||
		!strings.Contains(err.Error(), EnvVaultToken) {
		t.Errorf("no authentication = %v", err)
	}
	bad := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{EnvVaultToken: "t", EnvVaultCACert: bad}
	if _, err := OpenWith(context.Background(), spec, Options{Getenv: func(k string) string { return env[k] }}); err == nil {
		t.Error("an invalid VAULT_CACERT was accepted")
	}
	env[EnvVaultCACert] = filepath.Join(t.TempDir(), "missing.pem")
	if _, err := OpenWith(context.Background(), spec, Options{Getenv: func(k string) string { return env[k] }}); err == nil {
		t.Error("a missing VAULT_CACERT was accepted")
	}
}

func TestVaultTransitTrustsVaultCACert(t *testing.T) {
	srv := httptest.NewTLSServer(&fakeVault{tokens: map[string]bool{"s.root": true}})
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := "-----BEGIN CERTIFICATE-----\n" + base64.StdEncoding.EncodeToString(srv.Certificate().Raw) + "\n-----END CERTIFICATE-----\n"
	if err := os.WriteFile(ca, []byte(pemBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{EnvVaultToken: "s.root", EnvVaultCACert: ca}
	w, err := OpenWith(context.Background(), keywrap.Spec{Provider: keywrap.ProviderVaultTransit, KeyURI: srv.URL + "/team/transit/keys/mongorescue"},
		Options{Getenv: func(k string) string { return env[k] }})
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, w)
}
