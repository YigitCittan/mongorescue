package server

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

// Shared fixtures of the server tests.
const (
	// testConnID is the connection seeded by withTestConnection.
	testConnID = "conn_test"
	// testConnURI is its connection string (the password must never be served).
	testConnURI = "mongodb://admin:s3cret-pw@db.internal:27017/?authSource=admin"
	// testAPIKey is the static API key configured by keyed test servers.
	testAPIKey = "test-static-api-key"
)

// fakeProber is a connections.Prober that never touches the network.
type fakeProber struct {
	mu      sync.Mutex
	pingErr error
	dbs     []connections.Database
	cols    []connections.Collection
	delay   time.Duration
	lastURI string
}

func (p *fakeProber) Ping(ctx context.Context, uri string) (connections.ServerInfo, error) {
	p.mu.Lock()
	p.lastURI = uri
	delay, err := p.delay, p.pingErr
	p.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return connections.ServerInfo{}, ctx.Err()
		}
	}
	if err != nil {
		return connections.ServerInfo{}, err
	}
	return connections.ServerInfo{Version: "7.0.14"}, nil
}

func (p *fakeProber) ListDatabases(_ context.Context, uri string) ([]connections.Database, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastURI = uri
	if p.pingErr != nil {
		return nil, p.pingErr
	}
	return p.dbs, nil
}

func (p *fakeProber) ListCollections(_ context.Context, uri, _ string) ([]connections.Collection, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastURI = uri
	if p.pingErr != nil {
		return nil, p.pingErr
	}
	return p.cols, nil
}

func (p *fakeProber) uri() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastURI
}

// seedTestConnection stores the testConnID connection.
func seedTestConnection(t *testing.T, st connections.Repository) {
	t.Helper()
	now := time.Now().UTC()
	if err := st.SaveConnection(context.Background(), &models.Connection{
		ID: testConnID, Name: "test server", URI: testConnURI, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
}

// withTestConnection seeds testConnID and wires a connections service backed by prober
// (a fresh fakeProber when nil).
func withTestConnection(t *testing.T, st connections.Repository, prober connections.Prober) Option {
	t.Helper()
	seedTestConnection(t, st)
	if prober == nil {
		prober = &fakeProber{}
	}
	return WithConnections(connections.NewService(st, prober, connections.WithTestTimeout(2*time.Second)))
}

// newTestAuth returns an auth service with cheap bcrypt. A non-empty staticKey is
// imported as an API key without a user (like a key from MONGORESCUE_API_KEY).
func newTestAuth(t *testing.T, repo auth.Repository, staticKey string, opts ...auth.Option) *auth.Service {
	t.Helper()
	opts = append([]auth.Option{auth.WithBcryptCost(bcrypt.MinCost)}, opts...)
	svc, err := auth.NewService(repo, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if staticKey != "" {
		if _, err := svc.ImportAPIKey(context.Background(), staticKey); err != nil {
			t.Fatal(err)
		}
	}
	return svc
}

// testConfig is what server tests configure: the API key imported into the test auth
// service and the security settings the server reads.
type testConfig struct {
	APIKey   string
	Security settings.Security
}

// newTestConfig returns the defaults (no API key).
func newTestConfig() *testConfig {
	return &testConfig{Security: settings.Defaults().Security}
}

// newTestSettings returns a settings service on repo with sec applied.
func newTestSettings(t *testing.T, repo settings.Repository, sec settings.Security) *settings.Service {
	t.Helper()
	svc, err := settings.NewService(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	origins := sec.CORSOrigins
	if _, err := svc.Update(context.Background(), settings.Patch{Security: &settings.SecurityPatch{
		SessionIdleTimeout: &sec.SessionIdleTimeout, SessionAbsoluteTimeout: &sec.SessionAbsoluteTimeout,
		SecureCookies: &sec.SecureCookies, TrustProxyHeaders: &sec.TrustProxyHeaders,
		CORSOrigins: &origins, MetricsPublic: &sec.MetricsPublic,
	}}); err != nil {
		t.Fatal(err)
	}
	return svc
}

// keyed authenticates requests that carry no credentials with testAPIKey, so tests of
// business endpoints can use the full middleware chain without repeating headers.
type keyed struct{ h http.Handler }

func (k keyed) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") == "" && r.Header.Get("X-API-Key") == "" && r.Header.Get("Cookie") == "" {
		r.Header.Set("X-API-Key", testAPIKey)
	}
	k.h.ServeHTTP(w, r)
}

// bootConfig returns the bootstrap configuration of test servers.
func bootConfig() *config.Config {
	return config.Default()
}

// errUnreachable simulates a server that cannot be reached.
var errUnreachable = errors.New("server selection error: dial tcp 10.0.0.1:27017: i/o timeout, uri mongodb://admin:s3cret-pw@db.internal:27017")
