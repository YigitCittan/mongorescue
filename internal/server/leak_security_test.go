package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// echoingProber fails like a MongoDB driver that repeats the connection string in its
// errors, raw and percent-decoded.
type echoingProber struct{}

func echoError(uri string) error {
	decoded, _ := url.PathUnescape(uri)
	return errors.New("server selection error: context deadline exceeded, current topology: { uri: " + uri + ", decoded: " + decoded + " }")
}

func (echoingProber) Ping(_ context.Context, uri string) (connections.ServerInfo, error) {
	return connections.ServerInfo{}, echoError(uri)
}

func (echoingProber) ListDatabases(_ context.Context, uri string) ([]connections.Database, error) {
	return nil, echoError(uri)
}

func (echoingProber) ListCollections(_ context.Context, uri, _ string) ([]connections.Collection, error) {
	return nil, echoError(uri)
}

// TestFailedConnectionTestsAreRedacted runs every endpoint that contacts a MongoDB
// server against a driver whose errors repeat the connection string, with passwords
// that need percent-encoding, and checks responses and logs for the passwords.
func TestFailedConnectionTestsAreRedacted(t *testing.T) {
	const saved, unsaved = "p@ss:w/rd-LEAK1", "LEAK2 pw%?&"
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv, _, _ := setupTestServer(t)
	st := storetest.New(t)
	now := time.Now().UTC()
	savedURI := "mongodb://admin:" + url.QueryEscape(saved) + "@db.internal:27017/?authSource=admin"
	if err := st.SaveConnection(context.Background(), &models.Connection{ID: "conn_leak", Name: "leak", URI: savedURI, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	conns := connections.NewService(st, echoingProber{}, connections.WithLogger(logger), connections.WithTestTimeout(time.Second))
	h := keyed{NewServer(bootConfig(), st, srv.backupEngine, srv.restoreEngine, srv.storageDriver, srv.scheduler, nil, logger,
		WithAuth(newTestAuth(t, st, testAPIKey)), WithConnections(conns)).Handler()}

	unsavedURI := "mongodb://tester:" + url.QueryEscape(unsaved) + "@other.internal:27017/"
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/v1/connections/conn_leak/test", nil},
		{"POST", "/api/v1/connections/test", map[string]string{"uri": unsavedURI}},
		{"POST", "/api/v1/connections", map[string]string{"name": "bad", "uri": "mongodb://x:" + url.QueryEscape(unsaved) + "@h:27017/\x00"}},
		{"GET", "/api/v1/connections/conn_leak/databases", nil},
		{"GET", "/api/v1/connections/conn_leak/databases/shop/collections", nil},
		{"GET", "/api/v1/connections/conn_leak", nil},
		{"GET", "/api/v1/connections", nil},
	} {
		var raw []byte
		if tc.body != nil {
			raw, _ = json.Marshal(tc.body)
		}
		rec := serve(h, tc.method, tc.path, raw, map[string]string{"Content-Type": "application/json"})
		for _, secret := range []string{saved, url.QueryEscape(saved), unsaved, url.QueryEscape(unsaved), "LEAK1", "LEAK2"} {
			if strings.Contains(rec.Body.String(), secret) {
				t.Errorf("%s %s (%d) leaks %q: %s", tc.method, tc.path, rec.Code, secret, rec.Body)
			}
		}
	}
	for _, secret := range []string{"LEAK1", "LEAK2"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs leak %q:\n%s", secret, logs.String())
		}
	}
}
