package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ansiRE matches the ANSI escape sequences the CLI writes.
var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

// fullAPI answers every command the tests run.
func fullAPI() *fakeAPI {
	api := listAPI().
		data("POST /backups", http.StatusAccepted, `{"id":"bkp_new","database":"shop","status":"in_progress"}`).
		data("POST /jobs/job_1/run", http.StatusAccepted, `{"id":"run_1","job_id":"job_1","status":"running","databases":[]}`).
		data("POST /restores/preflight", 200, preflightOK).
		data("POST /restore", http.StatusAccepted, `{"id":"rst_new","backup_id":"bkp_1","source_database":"shop","target_database":"shop_rescue_20261001_000000","status":"in_progress","started_at":"2026-10-01T00:00:00Z","dry_run":false,"source_connection_id":"","source_connection_name":"","target_connection_id":"","target_connection_name":""}`).
		data("POST /backups/bkp_1/verify", http.StatusAccepted, `{"id":"bkp_1","database":"shop","status":"completed","verified_at":"2026-09-01T00:00:00Z","verification":"ok"}`)
	st := statusAPI(`{"user":{"id":"u1","username":"admin"},"csrf_token":"","auth":"api_key","scope":"operator"}`)
	for route, h := range st.routes {
		api.routes[route] = h
	}
	return api
}

// runApp runs the CLI against h, with stdout a terminal or not.
func runApp(t *testing.T, h http.Handler, tty bool, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	env2 := map[string]string{EnvAPIKey: testKey}
	for k, v := range env {
		env2[k] = v
	}
	app := &App{
		Version: "test", PollInterval: time.Millisecond, HTTPClient: &http.Client{Transport: handlerTransport{h}},
		IsTerminal: func(io.Writer) bool { return tty },
	}
	var stdout, stderr bytes.Buffer
	code := app.Run(context.Background(), args, func(k string) string { return env2[k] }, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// TestJSONOutputOfEveryCommand runs every command that calls the API with --json
// against a fake API and asserts valid JSON, never coloured, also on a terminal.
func TestJSONOutputOfEveryCommand(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"backup database", []string{"backup", "--connection", "conn_1", "--database", "shop"}, ExitOK},
		{"backup job", []string{"backup", "--job", "job_1"}, ExitOK},
		{"restore", []string{"restore", "bkp_1"}, ExitOK},
		{"list backups", []string{"list", "backups"}, ExitOK},
		{"list restores", []string{"list", "restores"}, ExitOK},
		{"list jobs", []string{"list", "jobs"}, ExitOK},
		{"list connections", []string{"list", "connections"}, ExitOK},
		{"list targets", []string{"list", "targets"}, ExitOK},
		{"verify", []string{"verify", "bkp_1"}, ExitOK},
		{"status", []string{"status"}, ExitOK},
		{"status readiness", []string{"status", "--readiness"}, ExitFailed},
	}
	covered := map[string]bool{}
	for _, tc := range cases {
		covered[tc.args[0]] = true
		for _, tty := range []bool{false, true} {
			t.Run(tc.name, func(t *testing.T) {
				args := append(append([]string{}, tc.args...), "--json")
				code, stdout, stderr := runApp(t, fullAPI(), tty, nil, args...)
				if code != tc.want {
					t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, tc.want, stdout, stderr)
				}
				if !json.Valid([]byte(stdout)) {
					t.Fatalf("not valid JSON:\n%s", stdout)
				}
				if strings.Contains(stdout, "\x1b") || strings.Contains(stderr, "\x1b") {
					t.Fatalf("escape codes with --json (tty %v):\n%q", tty, stdout)
				}
			})
		}
	}
	for name := range commandFuncs() {
		if !covered[name] {
			t.Errorf("the command %s has no --json case", name)
		}
	}
}

// textCases are commands whose text output has statuses to colour.
var textCases = []struct {
	name string
	args []string
	want int
}{
	{"list backups", []string{"list", "backups"}, ExitOK},
	{"list restores", []string{"list", "restores"}, ExitOK},
	{"status readiness", []string{"status", "--readiness"}, ExitFailed},
	{"backup wait", []string{"backup", "--connection", "c", "--database", "shop", "--wait"}, ExitOK},
}

// waitAPI finishes a backup with a completed status.
func waitAPI() *fakeAPI {
	return newFakeAPI().
		data("POST /backups", http.StatusAccepted, `{"id":"bkp_new","database":"shop","status":"in_progress"}`).
		data("GET /backups", 200, `[{"id":"bkp_new","database":"shop","status":"completed","size_bytes":4096,"duration_seconds":2}]`)
}

func apiFor(name string) *fakeAPI {
	if strings.HasPrefix(name, "backup wait") {
		return waitAPI()
	}
	return fullAPI()
}

func TestColorOnATerminalOnly(t *testing.T) {
	for _, tc := range textCases[:4] {
		t.Run(tc.name, func(t *testing.T) {
			code, colored, stderr := runApp(t, apiFor(tc.name), true, nil, tc.args...)
			if code != tc.want {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if !ansiRE.MatchString(colored) {
				t.Fatalf("no colour on a terminal:\n%s", colored)
			}
			_, plain, _ := runApp(t, apiFor(tc.name), false, nil, tc.args...)
			if ansiRE.MatchString(plain) || strings.Contains(plain, "\x1b") {
				t.Fatalf("escape codes when piped:\n%q", plain)
			}
			if got := ansiRE.ReplaceAllString(colored, ""); got != plain {
				t.Errorf("colour changes the layout:\n--- coloured, stripped\n%s\n--- plain\n%s", got, plain)
			}
		})
	}
}

func TestColorOffSwitches(t *testing.T) {
	switches := []struct {
		name string
		env  map[string]string
		args []string
	}{
		{"NO_COLOR=1", map[string]string{"NO_COLOR": "1"}, nil},
		{"NO_COLOR with any value", map[string]string{"NO_COLOR": "false"}, nil},
		{"TERM=dumb", map[string]string{"TERM": "dumb"}, nil},
		{"--no-color", nil, []string{"--no-color"}},
	}
	for _, tc := range textCases[:4] {
		for _, sw := range switches {
			t.Run(tc.name+"/"+sw.name, func(t *testing.T) {
				args := append(append([]string{}, tc.args...), sw.args...)
				code, stdout, stderr := runApp(t, apiFor(tc.name), true, sw.env, args...)
				if code != tc.want {
					t.Fatalf("exit %d: %s", code, stderr)
				}
				if strings.Contains(stdout, "\x1b") || strings.Contains(stderr, "\x1b") {
					t.Fatalf("escape codes:\n%q", stdout)
				}
			})
		}
	}
	// An empty NO_COLOR does not count (https://no-color.org), TERM=xterm does.
	_, stdout, _ := runApp(t, fullAPI(), true, map[string]string{"NO_COLOR": "", "TERM": "xterm-256color"}, "list", "backups")
	if !ansiRE.MatchString(stdout) {
		t.Errorf("an empty NO_COLOR turned the colour off:\n%q", stdout)
	}
	// --quiet prints IDs only, never colour.
	_, stdout, _ = runApp(t, fullAPI(), true, nil, "list", "backups", "--quiet")
	if strings.Contains(stdout, "\x1b") {
		t.Errorf("--quiet coloured: %q", stdout)
	}
}

func TestPaintAndWidth(t *testing.T) {
	on, off := &session{color: true}, &session{}
	for status, want := range map[string]string{
		"completed": ansiGreen + "completed" + ansiReset,
		"failed":    ansiRed + "failed" + ansiReset,
		"running":   ansiYellow + "running" + ansiReset,
		"Pass":      ansiGreen + "Pass" + ansiReset,
		"unknown":   "unknown",
		"":          "",
	} {
		if got := on.paint(status); got != want {
			t.Errorf("paint(%q) = %q, want %q", status, got, want)
		}
		if got := off.paint(status); got != status {
			t.Errorf("uncoloured paint(%q) = %q", status, got)
		}
	}
	if w := visibleWidth(on.paint("completed")); w != len("completed") {
		t.Errorf("visibleWidth = %d", w)
	}
	if w := visibleWidth("héllo"); w != 5 {
		t.Errorf("visibleWidth(héllo) = %d", w)
	}
}

func TestTableAlignment(t *testing.T) {
	var buf bytes.Buffer
	tb := newTable(&buf, "ID", "STATUS", "SIZE")
	tb.row("a", "\x1b[32mok\x1b[0m", "1")
	tb.row("longer", "failed", "")
	if err := tb.flush(); err != nil {
		t.Fatal(err)
	}
	want := "ID      STATUS  SIZE\na       ok      1\nlonger  failed  -\n"
	if got := ansiRE.ReplaceAllString(buf.String(), ""); got != want {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
}

func TestIsTerminalOfNonConsoles(t *testing.T) {
	if isTerminal(&bytes.Buffer{}) {
		t.Error("a buffer is not a terminal")
	}
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if isTerminal(f) {
		t.Error("a file is not a terminal")
	}
}
