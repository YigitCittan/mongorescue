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
// found, and so is a restore of A's stream into B; A's own restore passes the
// scope checks (operator) and is refused only by its plan.
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
			// A's stream: an operator may restore it into safe clones, so only the
			// plan refuses it (the stream has no chunks yet).
			rec := serve(f.h, http.MethodPost, path, body(accessPstA), h)
			want := http.StatusUnprocessableEntity
			if strings.HasSuffix(path, "/preflight") {
				want = http.StatusOK
			}
			if rec.Code != want || !strings.Contains(rec.Body.String(), "no oplog chunks") {
				t.Errorf("POST %s for A's stream: %d %s; want %d and the plan's refusal", path, rec.Code, rec.Body.String(), want)
			}
			// Into B's server: not found, like B's stream.
			into := []byte(`{"pitr":{"stream_id":"` + accessPstA + `","at":"2026-10-05T12:00:00Z"},"target_connection_id":"` + accessConnB + `"}`)
			rec = serve(f.h, http.MethodPost, path, into, h)
			if !hiddenNotFound(rec.Code, rec.Body.String()) || leaked(rec.Body.String(), accessConnB) != "" {
				t.Errorf("POST %s for A's stream into B: %d %s; want 404", path, rec.Code, rec.Body.String())
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
