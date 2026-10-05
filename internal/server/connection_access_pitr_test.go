package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/cli"
)

// TestPITRRestoresFollowConnectionAccessOverHTTP checks the point-in-time restore
// bodies of POST /api/v1/restore and its preflight, and CLI restore --pitr, for a
// caller limited to connection A: B's stream (by stream or connection ID) is not
// found, A's gets the usual admin refusal.
func TestPITRRestoresFollowConnectionAccessOverHTTP(t *testing.T) {
	f := newAccessFixture(t)
	body := func(stream string) []byte {
		return []byte(`{"pitr":{"stream_id":"` + stream + `","at":"2026-10-05T12:00:00Z"}}`)
	}
	for _, path := range []string{"/api/v1/restore", "/api/v1/restores/preflight"} {
		for _, h := range []map[string]string{f.keyHeaders(), f.session(t)} {
			for _, b := range []string{accessPstB, accessConnB} {
				rec := serve(f.h, http.MethodPost, path, body(b), h)
				if !hiddenNotFound(rec.Code, rec.Body.String()) || leaked(rec.Body.String(), b) != "" {
					t.Errorf("POST %s for B's stream %s: %d %s; want 404", path, b, rec.Code, rec.Body.String())
				}
			}
			rec := serve(f.h, http.MethodPost, path, body(accessPstA), h)
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "admin") {
				t.Errorf("POST %s for A's stream: %d %s; want the admin refusal", path, rec.Code, rec.Body.String())
			}
		}
	}
	// The CLI is a client of the same route.
	ts := httptest.NewServer(f.h)
	t.Cleanup(ts.Close)
	app := &cli.App{Version: "test", PollInterval: time.Millisecond, HTTPClient: ts.Client()}
	var stdout, stderr bytes.Buffer
	env := map[string]string{cli.EnvAPIKey: f.key, cli.EnvURL: ts.URL}
	code := app.Run(context.Background(), []string{"restore", "--pitr", accessPstB, "--at", "2026-10-05T12:00:00Z"},
		func(k string) string { return env[k] }, &stdout, &stderr)
	if code != cli.ExitNotFound {
		t.Errorf("restore --pitr of B's stream: exit %d %s%s; want %d", code, stdout.String(), stderr.String(), cli.ExitNotFound)
	}
}
