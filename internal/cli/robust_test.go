package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/apiclient"
)

func TestKeyFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not checked on Windows")
	}
	dir := t.TempDir()
	for _, tc := range []struct {
		mode os.FileMode
		warn bool
	}{{0o600, false}, {0o400, false}, {0o640, true}, {0o644, true}, {0o606, true}} {
		path := filepath.Join(dir, fmt.Sprintf("key-%o", tc.mode))
		if err := os.WriteFile(path, []byte(testKey), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, tc.mode); err != nil {
			t.Fatal(err)
		}
		code, _, stderr := runCLI(context.Background(), t, listAPI(), map[string]string{EnvAPIKeyFile: path}, "list", "jobs")
		if code != ExitOK {
			t.Fatalf("%o: exit %d: %s", tc.mode, code, stderr)
		}
		if got := strings.Contains(stderr, "readable by other users") && strings.Contains(stderr, "chmod 600"); got != tc.warn {
			t.Errorf("%o: warned %v, want %v: %q", tc.mode, got, tc.warn, stderr)
		}
	}
}

func TestWaitRetriesTransientErrors(t *testing.T) {
	answers := []func(http.ResponseWriter){
		func(w http.ResponseWriter) { reply(w, http.StatusTooManyRequests, "", "slow down") },
		func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "0")
			reply(w, http.StatusServiceUnavailable, "", "shutting down")
		},
		func(w http.ResponseWriter) { reply(w, http.StatusBadGateway, "", "proxy") },
		func(w http.ResponseWriter) {
			reply(w, 200, `[{"id":"bkp_new","database":"shop","status":"completed","size_bytes":1,"duration_seconds":1}]`, "")
		},
	}
	polls := 0
	api, _ := backupSequence("completed")
	api.on("GET /backups", func(w http.ResponseWriter, _ *http.Request) {
		answers[min(polls, len(answers)-1)](w)
		polls++
	})
	code, stdout, stderr := runCLI(context.Background(), t, api, nil, "backup", "--connection", "c", "--database", "shop", "--wait")
	if code != ExitOK || polls != 4 || strings.Count(stderr, "retrying in") != 3 || !strings.Contains(stdout, "completed") {
		t.Fatalf("exit %d after %d polls\nstdout: %s\nstderr: %s", code, polls, stdout, stderr)
	}

	// The budget is 3 failures in a row; the 4th ends the wait with its own code.
	api.on("GET /backups", func(w http.ResponseWriter, _ *http.Request) { reply(w, http.StatusInternalServerError, "", "boom") })
	if code, _, _ = runCLI(context.Background(), t, api, nil, "backup", "--connection", "c", "--database", "shop", "--wait"); code != ExitUnavailable {
		t.Fatalf("exhausted budget: exit %d", code)
	}
	// A refusal that is not transient is not retried.
	api.on("GET /backups", func(w http.ResponseWriter, _ *http.Request) { reply(w, http.StatusForbidden, "", "scope") })
	if code, _, stderr = runCLI(context.Background(), t, api, nil, "backup", "--connection", "c", "--database", "shop", "--wait"); code != ExitAuth || strings.Contains(stderr, "retrying") {
		t.Fatalf("403: exit %d, %s", code, stderr)
	}
}

func TestRetryableDelay(t *testing.T) {
	for _, tc := range []struct {
		err   error
		retry bool
		delay time.Duration
	}{
		{&apiclient.APIError{StatusCode: 429, RetryAfter: 5 * time.Second}, true, 5 * time.Second},
		{&apiclient.APIError{StatusCode: 429, RetryAfter: time.Hour}, true, maxRetryAfter},
		{&apiclient.APIError{StatusCode: 503, RetryAfter: 2 * time.Second}, true, 2 * time.Second},
		{fmt.Errorf("x: %w", apiclient.ErrUnreachable), true, 0},
		{&apiclient.APIError{StatusCode: 404}, false, 0},
		{&apiclient.APIError{StatusCode: 400}, false, 0},
	} {
		if retry, delay := retryable(tc.err); retry != tc.retry || delay != tc.delay {
			t.Errorf("%v: %v %s; want %v %s", tc.err, retry, delay, tc.retry, tc.delay)
		}
	}
}

func TestCancelledBeforeTheRunStarted(t *testing.T) {
	for _, tc := range []struct {
		route, list string
		args        []string
	}{
		{"POST /backups", "mongorescue list backups", []string{"backup", "--connection", "c", "--database", "shop", "--wait"}},
		{"POST /jobs/job_a/run", "mongorescue list backups --job job_a", []string{"backup", "--job", "job_a"}},
		{"POST /restore", "mongorescue list restores --backup bkp_1", []string{"restore", "bkp_1", "--skip-preflight"}},
		{"POST /backups/bkp_1/verify", "mongorescue list backups --id bkp_1", []string{"verify", "bkp_1", "--wait"}},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		api := newFakeAPI().on(tc.route, func(w http.ResponseWriter, _ *http.Request) {
			cancel() // Ctrl-C while the request is in flight
			reply(w, http.StatusAccepted, `{"id":"x"}`, "")
		})
		code, stdout, stderr := runCLI(ctx, t, api, nil, tc.args...)
		cancel()
		if code != ExitFailed || stdout != "" || !strings.Contains(stderr, "cancelled before the request completed; the run may or may not have started") || !strings.Contains(stderr, tc.list) {
			t.Errorf("%s: exit %d\nstdout: %s\nstderr: %s", tc.route, code, stdout, stderr)
		}
	}
	// Interrupted before anything could start (the preflight): exit 1 too.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := newFakeAPI().on("POST /restores/preflight", func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		reply(w, 200, preflightOK, "")
	})
	if code, _, stderr := runCLI(ctx, t, api, nil, "restore", "bkp_1"); code != ExitFailed || !strings.Contains(stderr, "interrupted") || len(api.sent("POST /restore")) != 0 {
		t.Fatalf("preflight interrupted: exit %d, %s", code, stderr)
	}
}

func TestVerifyWaitDefaultTimeout(t *testing.T) {
	old := defaultVerifyTimeout
	defaultVerifyTimeout = 30 * time.Millisecond
	t.Cleanup(func() { defaultVerifyTimeout = old })
	api := newFakeAPI().
		data("POST /backups/bkp_1/verify", http.StatusAccepted, `{"id":"bkp_1","database":"shop","status":"completed"}`).
		data("GET /backups", 200, `[{"id":"bkp_1","database":"shop","status":"completed"}]`)
	code, _, stderr := runCLI(context.Background(), t, api, nil, "verify", "bkp_1", "--wait")
	if code != ExitWaitStopped || !strings.Contains(stderr, "timed out after 30ms") {
		t.Fatalf("exit %d, %s", code, stderr)
	}
	// An explicit --timeout wins.
	code, _, stderr = runCLI(context.Background(), t, api, nil, "verify", "bkp_1", "--wait", "--timeout", "10ms")
	if code != ExitWaitStopped || !strings.Contains(stderr, "timed out after 10ms") {
		t.Fatalf("--timeout: exit %d, %s", code, stderr)
	}
}
