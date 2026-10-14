package vaultcreds

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/vault"
)

// fakeVault is a hermetic Vault: kv-v2 secrets, the database and AWS secrets
// engines with leases, and lease renewal and revocation.
type fakeVault struct {
	t *testing.T

	mu sync.Mutex
	// kv maps "<mount>/<path>" to the secret's data (nil: deleted version).
	kv map[string]map[string]any
	// ttl, renewable and maxTTL (seconds) shape issued leases.
	ttl       int
	renewable bool
	maxTTL    int
	// down answers every request with 503; failRenew only renewals.
	down      bool
	failRenew bool
	// issued counts leases; reads counts kv reads; renewals counts renewals.
	issued, reads, renewals int
	revoked                 []string
	// leases maps lease IDs to the seconds granted so far.
	leases map[string]int
}

func newFakeVault(t *testing.T) (*fakeVault, *httptest.Server) {
	t.Helper()
	f := &fakeVault{t: t, kv: map[string]map[string]any{}, ttl: 30, renewable: true, maxTTL: 1000, leases: map[string]int{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeVault) reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (f *fakeVault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		f.reply(w, http.StatusServiceUnavailable, map[string]any{"errors": []string{"Vault is sealed"}})
		return
	}
	if r.Header.Get("X-Vault-Token") != "s.test" {
		f.reply(w, http.StatusForbidden, map[string]any{"errors": []string{"permission denied"}})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/")
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch {
	case path == "auth/token/lookup-self":
		f.reply(w, http.StatusOK, map[string]any{"data": map[string]any{"ttl": 0}})
	case path == "sys/leases/renew":
		f.renewals++
		id, _ := body["lease_id"].(string)
		granted, ok := f.leases[id]
		if f.failRenew || !ok {
			f.reply(w, http.StatusServiceUnavailable, map[string]any{"errors": []string{"renewal unavailable"}})
			return
		}
		inc := int(body["increment"].(float64))
		if granted+inc > f.maxTTL {
			inc = max(f.maxTTL-granted, 0)
		}
		f.leases[id] = granted + inc
		f.reply(w, http.StatusOK, map[string]any{"lease_id": id, "lease_duration": inc, "renewable": true})
	case path == "sys/leases/revoke":
		id, _ := body["lease_id"].(string)
		f.revoked = append(f.revoked, id)
		delete(f.leases, id)
		f.reply(w, http.StatusNoContent, nil)
	case strings.Contains(path, "/data/"):
		f.reads++
		data, ok := f.kv[strings.Replace(path, "/data/", "/", 1)]
		if !ok {
			f.reply(w, http.StatusNotFound, map[string]any{"errors": []string{}})
			return
		}
		f.reply(w, http.StatusOK, map[string]any{"data": map[string]any{"data": data, "metadata": map[string]any{"version": 3}}})
	case strings.HasPrefix(path, "database/creds/"), strings.HasPrefix(path, "aws/creds/"):
		f.issued++
		id := fmt.Sprintf("%s/lease-%d", path, f.issued)
		f.leases[id] = f.ttl
		data := map[string]any{"username": fmt.Sprintf("v-user-%d", f.issued), "password": fmt.Sprintf("dyn-pass-%d", f.issued)}
		if strings.HasPrefix(path, "aws/") {
			data = map[string]any{"access_key": fmt.Sprintf("AKIA%d", f.issued), "secret_key": fmt.Sprintf("aws-secret-%d", f.issued),
				"security_token": "sts-token"}
		}
		f.reply(w, http.StatusOK, map[string]any{"lease_id": id, "lease_duration": f.ttl, "renewable": f.renewable, "data": data})
	default:
		f.reply(w, http.StatusNotFound, map[string]any{"errors": []string{}})
	}
}

// set changes the fake under its lock.
func (f *fakeVault) set(fn func(f *fakeVault)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// get reads the fake under its lock.
func get[T any](f *fakeVault, fn func(f *fakeVault) T) T {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fn(f)
}

// newTestResolver returns a resolver of the fake whose lease seconds last unit.
func newTestResolver(t *testing.T, srv *httptest.Server, unit time.Duration, alerts *[]string) *Resolver {
	t.Helper()
	client, err := vault.New(vault.Config{Addr: srv.URL, AllowInsecure: true, HTTPClient: srv.Client(),
		Auth: vault.Auth{Method: vault.AuthToken, Token: "s.test"}})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	r := New(Config{Client: client, Alert: func(msg string) {
		if alerts != nil {
			mu.Lock()
			*alerts = append(*alerts, msg)
			mu.Unlock()
		}
	}})
	r.unit = unit
	t.Cleanup(r.Close)
	return r
}

// eventually waits up to 5s for cond.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
