package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

const testKey = "mr_test_secretkey"

// handlerTransport serves requests with a handler in memory: no network.
type handlerTransport struct{ h http.Handler }

// RoundTrip implements http.RoundTripper.
func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	if r.Body == nil {
		r.Body = http.NoBody
	}
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, r)
	if err := r.Context().Err(); err != nil {
		return nil, err // cancelled while the server was answering
	}
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

// recorded is one request the fake API received.
type recorded struct {
	method, path, query, auth, transport, agent string
	body                                        map[string]any
}

// fakeAPI is a scripted MongoRescue API.
type fakeAPI struct {
	mu       sync.Mutex
	requests []recorded
	routes   map[string]http.HandlerFunc
}

func newFakeAPI() *fakeAPI { return &fakeAPI{routes: map[string]http.HandlerFunc{}} }

// on answers "METHOD /api/v1/path" with h.
func (f *fakeAPI) on(route string, h http.HandlerFunc) *fakeAPI {
	f.routes[route] = h
	return f
}

// data answers route with a success envelope around data (a JSON string).
func (f *fakeAPI) data(route string, status int, data string) *fakeAPI {
	return f.on(route, func(w http.ResponseWriter, _ *http.Request) { reply(w, status, data, "") })
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := recorded{
		method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization"),
		transport: r.Header.Get("X-MongoRescue-Transport"), agent: r.Header.Get("User-Agent"),
	}
	if b, _ := io.ReadAll(r.Body); len(b) > 0 {
		_ = json.Unmarshal(b, &rec.body)
	}
	f.mu.Lock()
	f.requests = append(f.requests, rec)
	h := f.routes[r.Method+" "+strings.TrimPrefix(r.URL.Path, "/api/v1")]
	f.mu.Unlock()
	if h == nil {
		reply(w, http.StatusNotFound, "", "no route "+r.Method+" "+r.URL.Path)
		return
	}
	h(w, r)
}

// sent returns the requests received for "METHOD /path" (under /api/v1).
func (f *fakeAPI) sent(route string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, r := range f.requests {
		if r.method+" "+strings.TrimPrefix(r.path, "/api/v1") == route {
			out = append(out, r)
		}
	}
	return out
}

func reply(w http.ResponseWriter, status int, data, errMsg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	env := map[string]any{"success": status < 400}
	if data != "" {
		env["data"] = json.RawMessage(data)
	}
	if errMsg != "" {
		env["error"] = errMsg
	}
	_ = json.NewEncoder(w).Encode(env)
}

// runCLI runs the CLI against h with env (the API key in MONGORESCUE_CLI_API_KEY
// unless env sets a key source).
func runCLI(ctx context.Context, t *testing.T, h http.Handler, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	if env == nil {
		env = map[string]string{EnvAPIKey: testKey}
	}
	app := &App{Version: "test", PollInterval: time.Millisecond, HTTPClient: &http.Client{Transport: handlerTransport{h}}}
	var stdout, stderr bytes.Buffer
	code := app.Run(ctx, args, func(k string) string { return env[k] }, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: a test fixture.
	if err != nil {
		t.Fatalf("%v (run go test -run %s -update)", err, t.Name())
	}
	if got != string(want) {
		t.Errorf("%s differs:\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}

const (
	backupsJSON = `[
 {"id":"bkp_shop_1","database":"shop","status":"completed","trigger":"scheduled","job_id":"job_a","storage_type":"local","storage_key":"k","size_bytes":1572864,"started_at":"2026-10-01T03:00:00Z","duration_seconds":12.4,"retried_by":{"id":"x"}},
 {"id":"bkp_crm_2","database":"crm","status":"failed","storage_type":"local","storage_key":"","size_bytes":0,"started_at":"2026-10-01T02:00:00Z","error_message":"dial mongodb://admin:hunter2@db:27017 refused"}]`
	restoresJSON = `[{"id":"rst_1","backup_id":"bkp_shop_1","source_database":"shop","target_database":"shop_rescue_20261001_040000","status":"completed","started_at":"2026-10-01T04:00:00Z","duration_seconds":3,"dry_run":false,"source_connection_id":"","source_connection_name":"","target_connection_id":"","target_connection_name":""}]`
	jobsJSON     = `[{"id":"job_a","name":"Shop nightly","cron_expression":"0 3 * * *","database":"shop","database_selection":{"mode":"single","databases":["shop"],"auto_include_new":false},"enabled":true,"next_run":"2026-10-02T03:00:00Z","storage_type":"local","storage_target_id":"","retention_days":7,"retention_count":0,"gzip":true,"include_users_and_roles":false,"connection_id":"conn_1","created_at":"2026-09-01T00:00:00Z","updated_at":"2026-09-01T00:00:00Z"},
 {"id":"job_b","name":"All","cron_expression":"@hourly","database":"","database_selection":{"mode":"all","auto_include_new":true},"enabled":false,"storage_type":"local","storage_target_id":"","retention_days":0,"retention_count":5,"gzip":true,"include_users_and_roles":false,"connection_id":"conn_1","created_at":"2026-09-01T00:00:00Z","updated_at":"2026-09-01T00:00:00Z"}]`
	connectionsJSON = `[{"id":"conn_1","name":"prod","uri":"mongodb://backup:s3cret@db.example:27017/?authSource=admin","created_at":"2026-09-01T00:00:00Z","updated_at":"2026-09-01T00:00:00Z","last_test_at":"2026-10-01T00:00:00Z","last_test_ok":true}]`
	targetsJSON     = `[{"id":"tgt_1","name":"Local","type":"local","is_default":true,"local":{"path":"/var/lib/mongorescue/backups"},"created_at":"2026-09-01T00:00:00Z","updated_at":"2026-09-01T00:00:00Z","last_test_ok":false},
 {"id":"tgt_2","name":"Offsite","type":"s3","is_default":false,"s3":{"bucket":"dr","prefix":"mr/","endpoint":"https://minio.example","use_path_style":true},"created_at":"2026-09-01T00:00:00Z","updated_at":"2026-09-01T00:00:00Z","last_test_ok":true}]`
)

func listAPI() *fakeAPI {
	return newFakeAPI().
		on("GET /backups", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"success":true,"data":`+backupsJSON+`,"meta":{"total":12,"limit":50,"offset":0}}`)
		}).
		data("GET /restores", 200, restoresJSON).
		data("GET /jobs", 200, jobsJSON).
		data("GET /connections", 200, connectionsJSON).
		data("GET /storage-targets", 200, targetsJSON)
}

func TestListGolden(t *testing.T) {
	api := listAPI()
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"list_backups", []string{"list", "backups"}},
		{"list_backups_json", []string{"list", "backups", "--json"}},
		{"list_backups_quiet", []string{"list", "backups", "--quiet"}},
		{"list_restores", []string{"list", "restores"}},
		{"list_jobs", []string{"list", "jobs"}},
		{"list_connections", []string{"list", "connections"}},
		{"list_targets", []string{"list", "targets"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(context.Background(), t, api, nil, tc.args...)
			if code != ExitOK {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			golden(t, tc.name, stdout)
			if strings.Contains(stdout+stderr, "hunter2") || strings.Contains(stdout+stderr, "s3cret") {
				t.Fatalf("a password is printed:\n%s%s", stdout, stderr)
			}
		})
	}
	// The page note goes to stderr.
	if _, _, stderr := runCLI(context.Background(), t, api, nil, "list", "backups"); !strings.Contains(stderr, "Showing 1-2 of 12") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestListFiltersAndFlagOrder(t *testing.T) {
	api := listAPI()
	for _, args := range [][]string{
		{"list", "backups", "--status", "failed", "--database=shop", "--limit", "5"},
		{"list", "--status", "failed", "backups", "--limit=5", "--database", "shop"},
		{"list", "--status=failed", "--database", "shop", "--limit", "5", "backups"},
	} {
		if code, _, stderr := runCLI(context.Background(), t, api, nil, args...); code != ExitOK {
			t.Fatalf("%v: exit %d: %s", args, code, stderr)
		}
	}
	for _, r := range api.sent("GET /backups") {
		if r.query != "database=shop&limit=5&status=failed" {
			t.Fatalf("query = %q", r.query)
		}
	}
	if code, _, _ := runCLI(context.Background(), t, api, nil, "list", "restores", "--id", "rst_1,rst_2", "--limit", "0"); code != ExitOK {
		t.Fatal("restores by id failed")
	}
	if got := api.sent("GET /restores"); len(got) != 1 || got[0].query != "id=rst_1%2Crst_2" {
		t.Fatalf("restores query = %+v", got)
	}
	for _, args := range [][]string{
		{"list"},
		{"list", "things"},
		{"list", "jobs", "--status", "failed"},
		{"list", "backups", "extra"},
		{"list", "connections", "--limit", "5"},
		{"list", "backups", "--json", "--quiet"},
		{"list", "backups", "--limit", "0", "--offset", "5"},
	} {
		if code, _, stderr := runCLI(context.Background(), t, api, nil, args...); code != ExitUsage || !strings.Contains(stderr, "mongorescue:") {
			t.Errorf("%v: exit %d, %q; want 2", args, code, stderr)
		}
	}
}

func TestExitCodes(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   int
	}{
		{http.StatusBadRequest, ExitFailed},
		{http.StatusUnauthorized, ExitAuth},
		{http.StatusForbidden, ExitAuth},
		{http.StatusNotFound, ExitNotFound},
		{http.StatusConflict, ExitConflict},
		{http.StatusUnprocessableEntity, ExitFailed},
		{http.StatusInternalServerError, ExitUnavailable},
		{http.StatusServiceUnavailable, ExitUnavailable},
		{http.StatusFound, ExitUnavailable},
	} {
		api := newFakeAPI().on("POST /backups/bkp_1/verify", func(w http.ResponseWriter, _ *http.Request) {
			reply(w, tc.status, "", "refused with mongodb://u:pw@h")
		})
		code, _, stderr := runCLI(context.Background(), t, api, nil, "verify", "bkp_1")
		if code != tc.want || strings.Contains(stderr, ":pw@") {
			t.Errorf("%d: exit %d (%s); want %d", tc.status, code, stderr, tc.want)
		}
	}
	// Unreachable: a closed server.
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	app := &App{}
	var stdout, stderr bytes.Buffer
	env := map[string]string{EnvAPIKey: testKey, EnvURL: srv.URL}
	if code := app.Run(context.Background(), []string{"status"}, func(k string) string { return env[k] }, &stdout, &stderr); code != ExitUnavailable || !strings.Contains(stderr.String(), "Is MongoRescue running at") {
		t.Fatalf("unreachable: exit %d, %s", code, stderr.String())
	}
	// Usage.
	for _, args := range [][]string{{}, {"bogus"}, {"verify"}, {"verify", "a", "b"}, {"backup"}, {"backup", "--job", "j", "--database", "d"}, {"status", "--wait"}, {"verify", "x", "--timeout", "1s"}} {
		if code, _, _ := runCLI(context.Background(), t, newFakeAPI(), nil, args...); code != ExitUsage {
			t.Errorf("%v: exit %d; want 2", args, code)
		}
	}
	// Help.
	if code, stdout, _ := runCLI(context.Background(), t, newFakeAPI(), nil, "help", "restore"); code != ExitOK || !strings.Contains(stdout, "--in-place") {
		t.Fatalf("help restore = %d %q", code, stdout)
	}
	if code, _, stderr := runCLI(context.Background(), t, newFakeAPI(), nil, "backup", "-h"); code != ExitOK || !strings.Contains(stderr, "Usage: mongorescue backup") {
		t.Fatalf("backup -h = %d %q", code, stderr)
	}
}

// backupSequence answers GET /backups?id= with each status in turn (the last one
// repeats) and counts the polls.
func backupSequence(statuses ...string) (*fakeAPI, *int) {
	polls := 0
	var mu sync.Mutex
	api := newFakeAPI().
		data("POST /backups", http.StatusAccepted, `{"id":"bkp_new","database":"shop","status":"in_progress"}`).
		on("GET /backups", func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			st := statuses[min(polls, len(statuses)-1)]
			polls++
			mu.Unlock()
			extra := `,"progress":{"id":"bkp_new","kind":"backup","phase":"dumping","percent":40,"bytes":2048,"documents":1,"collections_done":1,"collections_total":2,"bytes_per_second":1,"started_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}`
			if st != "in_progress" {
				extra = `,"size_bytes":4096,"duration_seconds":2`
			}
			if st == "failed" {
				extra += `,"error_message":"mongodump exited 1"`
			}
			reply(w, 200, `[{"id":"bkp_new","database":"shop","status":"`+st+`"`+extra+`}]`, "")
		})
	return api, &polls
}

func TestBackupWait(t *testing.T) {
	api, _ := backupSequence("in_progress", "in_progress", "completed")
	code, stdout, stderr := runCLI(context.Background(), t, api, nil, "backup", "--connection", "conn_1", "--database", "shop", "--wait", "--gzip=false")
	if code != ExitOK || !strings.Contains(stdout, "Backup bkp_new of shop completed: 4.0 KiB in 2s.") || !strings.Contains(stderr, "backup bkp_new: in_progress (dumping, 40%, 2.0 KiB)") {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	body := api.sent("POST /backups")[0].body
	if body["connection_id"] != "conn_1" || body["database"] != "shop" || body["gzip"] != false {
		t.Fatalf("body = %v", body)
	}

	api, _ = backupSequence("in_progress", "failed")
	code, stdout, stderr = runCLI(context.Background(), t, api, nil, "backup", "--connection", "conn_1", "--database", "shop", "--wait")
	if code != ExitFailed || !strings.Contains(stdout, "failed: mongodump exited 1") || strings.Contains(stderr, "mongorescue: failed") {
		t.Fatalf("failed backup: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	api, _ = backupSequence("failed")
	if code, _, stderr = runCLI(context.Background(), t, api, nil, "backup", "--connection", "c", "--database", "shop", "--wait", "--json"); code != ExitFailed || !strings.Contains(stderr, "backup bkp_new failed") {
		t.Fatalf("failed backup --json: exit %d, %s", code, stderr)
	}
}

func TestWaitTimeoutAndInterrupt(t *testing.T) {
	api, _ := backupSequence("in_progress")
	code, stdout, stderr := runCLI(context.Background(), t, api, nil, "backup", "--connection", "c", "--database", "shop", "--wait", "--timeout", "30ms")
	if code != ExitWaitStopped || !strings.Contains(stderr, "timed out after 30ms") || !strings.Contains(stderr, "mongorescue list backups --id bkp_new") || stdout != "" {
		t.Fatalf("timeout: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	api, polls := backupSequence("in_progress")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api.on("GET /backups", func(w http.ResponseWriter, _ *http.Request) {
		*polls++
		if *polls == 3 {
			cancel() // Ctrl-C while waiting
		}
		reply(w, 200, `[{"id":"bkp_new","database":"shop","status":"in_progress"}]`, "")
	})
	code, _, stderr = runCLI(ctx, t, api, nil, "backup", "--connection", "c", "--database", "shop", "--wait")
	if code != ExitWaitStopped || !strings.Contains(stderr, "interrupted") || !strings.Contains(stderr, "keeps running on the server") {
		t.Fatalf("interrupt: exit %d\nstderr: %s", code, stderr)
	}
	if len(api.sent("POST /backups/bkp_new/cancel")) != 0 {
		t.Fatal("stopping the wait cancelled the backup")
	}
}

func TestBackupJob(t *testing.T) {
	runs := 0
	api := newFakeAPI().
		data("POST /jobs/job_multi/run", http.StatusAccepted, `{"id":"run_1","job_id":"job_multi","status":"running","databases":[]}`).
		on("GET /jobs/job_multi/runs", func(w http.ResponseWriter, _ *http.Request) {
			runs++
			if runs < 2 {
				reply(w, 200, `[{"id":"run_1","job_id":"job_multi","status":"running","databases":[{"database":"a","status":"in_progress"}]}]`, "")
				return
			}
			reply(w, 200, `[{"id":"run_1","job_id":"job_multi","status":"partial","duration_seconds":5,"databases":[{"database":"a","backup_id":"bkp_a","status":"completed"},{"database":"b","status":"failed","error":"boom"}]}]`, "")
		}).
		data("POST /jobs/job_single/run", http.StatusAccepted, `{"id":"bkp_s","database":"shop","status":"in_progress"}`)
	code, stdout, stderr := runCLI(context.Background(), t, api, nil, "backup", "--job", "job_multi", "--wait")
	if code != ExitFailed {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	golden(t, "backup_job_partial", stdout)
	code, stdout, _ = runCLI(context.Background(), t, api, nil, "backup", "--job", "job_single", "--quiet")
	if code != ExitOK || stdout != "bkp_s\n" {
		t.Fatalf("single job: %d %q", code, stdout)
	}
}

// restoreAPI answers the preflight with preflight and starts restores.
func restoreAPI(preflight string) *fakeAPI {
	return newFakeAPI().
		data("POST /restores/preflight", 200, preflight).
		data("POST /restore", http.StatusAccepted, `{"id":"rst_new","backup_id":"bkp_1","source_database":"shop","target_database":"shop_rescue_20261001_000000","status":"in_progress","started_at":"2026-10-01T00:00:00Z","dry_run":false,"source_connection_id":"","source_connection_name":"","target_connection_id":"","target_connection_name":""}`).
		data("GET /restores", 200, `[{"id":"rst_new","backup_id":"bkp_1","source_database":"shop","target_database":"shop_rescue_20261001_000000","status":"completed","started_at":"2026-10-01T00:00:00Z","duration_seconds":4,"dry_run":false,"source_connection_id":"","source_connection_name":"","target_connection_id":"","target_connection_name":"","verification":{"status":"passed","collections":2,"checked_at":"2026-10-01T00:00:05Z"}}]`)
}

const preflightOK = `{"ok":true,"checks":[{"id":"connection","status":"pass","message":"connected"},{"id":"server_version","status":"warn","message":"unknown version"}]}`

func TestRestoreSafeCloneBody(t *testing.T) {
	api := restoreAPI(preflightOK)
	code, stdout, stderr := runCLI(context.Background(), t, api, nil, "restore", "bkp_1", "--wait", "--verify-restore", "--collections", "orders, users")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	golden(t, "restore_safe_clone", stdout)
	if !strings.Contains(stderr, "preflight warning: server_version: unknown version") {
		t.Fatalf("stderr = %s", stderr)
	}
	for _, route := range []string{"POST /restores/preflight", "POST /restore"} {
		got := api.sent(route)
		if len(got) != 1 {
			t.Fatalf("%s sent %d times", route, len(got))
		}
		body := got[0].body
		for _, k := range []string{"safe_clone", "confirm_in_place", "target_database", "force", "verify"} {
			if _, ok := body[k]; ok {
				t.Errorf("%s: a safe-clone body has %s: %v", route, k, body)
			}
		}
		if body["backup_id"] != "bkp_1" || body["drop_target"] != false || body["dry_run"] != false || body["verify_restore"] != true {
			t.Errorf("%s: body = %v", route, body)
		}
		if cols, _ := body["selected_collections"].([]any); len(cols) != 2 || cols[0] != "orders" || cols[1] != "users" {
			t.Errorf("%s: collections = %v", route, body["selected_collections"])
		}
	}
}

func TestRestoreInPlaceGuard(t *testing.T) {
	for _, args := range [][]string{
		{"restore", "bkp_1", "--in-place"},
		{"restore", "--in-place", "bkp_1", "--drop"},
		{"restore", "bkp_1", "--confirm"},
		{"restore", "bkp_1", "--target-database", "other"},
		{"restore", "bkp_1", "--drop"},
		{"restore", "bkp_1", "--verify-archive", "--no-verify-archive"},
		{"restore"},
	} {
		api := restoreAPI(preflightOK)
		code, _, stderr := runCLI(context.Background(), t, api, nil, args...)
		if code != ExitUsage {
			t.Errorf("%v: exit %d; want 2 (%s)", args, code, stderr)
		}
		api.mu.Lock()
		n := len(api.requests)
		api.mu.Unlock()
		if n != 0 {
			t.Errorf("%v: %d requests sent before the guard", args, n)
		}
	}
	api := restoreAPI(preflightOK)
	code, _, stderr := runCLI(context.Background(), t, api, nil, "restore", "--in-place", "--confirm", "bkp_1", "--target-database", "shop_copy", "--drop", "--no-verify-archive", "--quiet")
	if code != ExitOK {
		t.Fatalf("in place: exit %d, %s", code, stderr)
	}
	body := api.sent("POST /restore")[0].body
	if body["safe_clone"] != false || body["confirm_in_place"] != true || body["target_database"] != "shop_copy" || body["drop_target"] != true || body["verify"] != false {
		t.Fatalf("in-place body = %v", body)
	}
}

func TestRestorePreflightFailure(t *testing.T) {
	failed := `{"ok":false,"checks":[{"id":"connection","status":"pass","message":"connected"},{"id":"disk_space","status":"fail","message":"the target has 1 MiB free"}]}`
	api := restoreAPI(failed)
	code, stdout, stderr := runCLI(context.Background(), t, api, nil, "restore", "bkp_1")
	if code != ExitFailed || stdout != "" || !strings.Contains(stderr, "disk_space  fail") || !strings.Contains(stderr, "preflight failed (disk_space)") {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if len(api.sent("POST /restore")) != 0 {
		t.Fatal("the restore started after a failed preflight")
	}
	code, stdout, _ = runCLI(context.Background(), t, api, nil, "restore", "bkp_1", "--json")
	if code != ExitFailed || !strings.Contains(stdout, `"disk_space"`) {
		t.Fatalf("--json: exit %d, %s", code, stdout)
	}
	code, _, stderr = runCLI(context.Background(), t, api, nil, "restore", "bkp_1", "--force")
	if code != ExitOK || !strings.Contains(stderr, "restoring anyway") {
		t.Fatalf("--force: exit %d, %s", code, stderr)
	}
	if got := api.sent("POST /restore"); len(got) != 1 || got[0].body["force"] != true {
		t.Fatalf("--force body = %+v", got)
	}
	// The server's own preflight refusal (409 with the checks) is a failure too.
	api = restoreAPI(preflightOK).on("POST /restore", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusConflict, failed, "restore preflight failed: disk_space")
	})
	code, _, stderr = runCLI(context.Background(), t, api, nil, "restore", "bkp_1", "--skip-preflight")
	if code != ExitFailed || !strings.Contains(stderr, "disk_space") || len(api.sent("POST /restores/preflight")) != 0 {
		t.Fatalf("server refusal: exit %d, %s", code, stderr)
	}
	// A plain conflict stays a conflict.
	api = restoreAPI(preflightOK).on("POST /restore", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusConflict, "", "a restore into shop is already running")
	})
	if code, _, _ = runCLI(context.Background(), t, api, nil, "restore", "bkp_1"); code != ExitConflict {
		t.Fatalf("conflict: exit %d", code)
	}
}

func TestKeySourcePriority(t *testing.T) {
	dir := t.TempDir()
	flagFile, envFile := filepath.Join(dir, "flag.key"), filepath.Join(dir, "env.key")
	if err := os.WriteFile(flagFile, []byte("key-from-flag-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envFile, []byte("  key-from-env-file  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	all := map[string]string{EnvAPIKeyFile: envFile, EnvAPIKey: "key-from-env", "MONGORESCUE_API_KEY": "server-admin-key", "MONGORESCUE_API_KEY_FILE": envFile}
	without := func(keys ...string) map[string]string {
		m := map[string]string{}
		for k, v := range all {
			m[k] = v
		}
		for _, k := range keys {
			delete(m, k)
		}
		return m
	}
	for _, tc := range []struct {
		name     string
		env      map[string]string
		args     []string
		want     string
		warnings string
	}{
		{"flag file first", all, []string{"--api-key-file", flagFile, "--api-key", "key-from-flag"}, "key-from-flag-file", "--api-key is ignored"},
		{"env file next", all, nil, "key-from-env-file", ""},
		{"env key next", without(EnvAPIKeyFile), []string{"--api-key", "key-from-flag"}, "key-from-env", "--api-key is ignored"},
		{"flag key last", without(EnvAPIKeyFile, EnvAPIKey), []string{"--api-key", "key-from-flag"}, "key-from-flag", "visible to other users"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := listAPI()
			code, _, stderr := runCLI(context.Background(), t, api, tc.env, append([]string{"list", "connections"}, tc.args...)...)
			if code != ExitOK {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			got := api.sent("GET /connections")[0]
			if got.auth != "Bearer "+tc.want || got.transport != "cli" || got.agent != "mongorescue-cli/test" {
				t.Fatalf("request = %+v; want key %s", got, tc.want)
			}
			if !strings.Contains(stderr, tc.warnings) {
				t.Fatalf("stderr = %q; want %q", stderr, tc.warnings)
			}
		})
	}
	// The server's own variables are never read.
	code, _, stderr := runCLI(context.Background(), t, listAPI(), map[string]string{"MONGORESCUE_API_KEY": "server-admin-key", "MONGORESCUE_API_KEY_FILE": envFile}, "list", "jobs")
	if code != ExitUsage || !strings.Contains(stderr, "no API key") {
		t.Fatalf("server key variables: exit %d, %s", code, stderr)
	}
	// An empty or missing key file is a usage error.
	empty := filepath.Join(dir, "empty.key")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{empty, filepath.Join(dir, "missing.key")} {
		if code, _, _ := runCLI(context.Background(), t, listAPI(), map[string]string{EnvAPIKeyFile: path}, "list", "jobs"); code != ExitUsage {
			t.Errorf("%s: exit %d; want 2", path, code)
		}
	}
}

func TestPlainHTTPWarning(t *testing.T) {
	env := map[string]string{EnvAPIKey: testKey, EnvURL: "http://backup.example:8080"}
	_, _, stderr := runCLI(context.Background(), t, listAPI(), env, "list", "jobs")
	if !strings.Contains(stderr, "plain HTTP to backup.example:8080") {
		t.Fatalf("stderr = %q", stderr)
	}
	_, _, stderr = runCLI(context.Background(), t, listAPI(), env, "list", "jobs", "--url", "https://backup.example")
	if strings.Contains(stderr, "plain HTTP") {
		t.Fatalf("https warned: %q", stderr)
	}
	if code, _, stderr := runCLI(context.Background(), t, listAPI(), nil, "list", "jobs", "--url", "http://u:secretpw@host"); code != ExitUsage || strings.Contains(stderr, "secretpw") {
		t.Fatalf("credentials in URL: exit %d, %s", code, stderr)
	}
}

func statusAPI(me string) *fakeAPI {
	return newFakeAPI().
		data("GET /health", 200, `{"status":"healthy","version":"0.17.0","time":"2026-10-01T00:00:00Z"}`).
		data("GET /auth/me", 200, me).
		data("GET /stats", 200, `{"total_backups":12,"completed_backups":10,"failed_backups":2,"failed_backups_24h":1,"active_backups":1,"total_bytes":1073741824,"total_restores":3,"active_restores":0,"active_jobs":2,"last_backup":{"id":"bkp_shop_1","database":"shop","status":"completed","started_at":"2026-10-01T03:00:00Z"},"job_last_backups":{}}`).
		data("GET /runs/active", 200, `[{"id":"bkp_x","kind":"backup","database":"crm","phase":"dumping","percent":12.5,"bytes":1048576,"documents":10,"collections_done":0,"collections_total":3,"bytes_per_second":1,"started_at":"2026-10-01T05:00:00Z","updated_at":"2026-10-01T05:00:01Z"}]`).
		data("GET /readiness", 200, `{"generated_at":"2026-10-01T06:00:00Z","keys_escrowed":true,"summary":{"ok":1,"warn":0,"fail":1},"rows":[{"connection_id":"conn_1","connection_name":"prod","database":"crm","jobs":[],"rpo":{"target_seconds":86400,"age_seconds":172800,"met":false},"keys_escrowed":true,"encrypted":false,"status":"fail","reasons":["rpo_missed"]},{"connection_id":"conn_1","connection_name":"prod","database":"shop","jobs":[],"last_good_backup":{"id":"bkp_shop_1","at":"2026-10-01T03:00:00Z"},"rpo":{"target_seconds":86400,"age_seconds":10800,"met":true},"keys_escrowed":true,"encrypted":false,"status":"ok","reasons":[]}]}`)
}

func TestStatus(t *testing.T) {
	// A server of v0.16 reports no scope.
	code, stdout, stderr := runCLI(context.Background(), t, statusAPI(`{"user":{"id":"u1","username":"admin"},"csrf_token":"","auth":"api_key"}`), nil, "status")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	golden(t, "status_no_scope", stdout)

	code, stdout, _ = runCLI(context.Background(), t, statusAPI(`{"user":{"id":"u1","username":"admin"},"csrf_token":"","auth":"api_key","scope":"operator"}`), nil, "status", "--readiness")
	if code != ExitFailed {
		t.Fatalf("--readiness with a failing database: exit %d", code)
	}
	golden(t, "status_readiness", stdout)

	code, stdout, _ = runCLI(context.Background(), t, statusAPI(`{"user":null,"csrf_token":"","auth":"api_key","scope":"read"}`), nil, "status", "--json")
	if code != ExitOK {
		t.Fatalf("--json: exit %d", code)
	}
	golden(t, "status_json", stdout)

	if code, stdout, _ = runCLI(context.Background(), t, statusAPI(`{"user":null,"csrf_token":"","auth":""}`), nil, "status"); code != ExitAuth || stdout != "" {
		t.Fatalf("signed out: exit %d, %q", code, stdout)
	}
	if code, stdout, _ = runCLI(context.Background(), t, statusAPI(`{"user":null,"auth":"api_key"}`), nil, "status", "--quiet"); code != ExitOK || stdout != "" {
		t.Fatalf("--quiet: exit %d, %q", code, stdout)
	}
}

func TestVerify(t *testing.T) {
	polls := 0
	api := newFakeAPI().
		data("POST /backups/bkp_1/verify", http.StatusAccepted, `{"id":"bkp_1","database":"shop","status":"completed","verified_at":"2026-09-01T00:00:00Z","verification":"ok"}`).
		on("GET /backups", func(w http.ResponseWriter, _ *http.Request) {
			polls++
			if polls < 3 {
				reply(w, 200, `[{"id":"bkp_1","database":"shop","status":"completed","verified_at":"2026-09-01T00:00:00Z","verification":"ok"}]`, "")
				return
			}
			reply(w, 200, `[{"id":"bkp_1","database":"shop","status":"completed","verified_at":"2026-10-01T00:00:00Z","verification":"mismatch","verification_error":"checksum mismatch"}]`, "")
		})
	code, stdout, stderr := runCLI(context.Background(), t, api, nil, "verify", "--wait", "bkp_1")
	if code != ExitFailed || polls != 3 || !strings.Contains(stdout, "verification mismatch at 2026-10-01T00:00:00Z: checksum mismatch") {
		t.Fatalf("exit %d after %d polls\nstdout: %s\nstderr: %s", code, polls, stdout, stderr)
	}
	code, stdout, _ = runCLI(context.Background(), t, api, nil, "verify", "bkp_1")
	if code != ExitOK || !strings.Contains(stdout, "Started the verification of backup bkp_1") {
		t.Fatalf("no wait: exit %d, %s", code, stdout)
	}
}

func TestParseInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	a := fs.Bool("a", false, "")
	b := fs.String("b", "", "")
	pos, err := parseInterspersed(fs, []string{"x", "-a", "y", "--b", "v", "--", "-z", "w"})
	if err != nil || !*a || *b != "v" || strings.Join(pos, " ") != "x y -z w" {
		t.Fatalf("pos = %v, a %v, b %q, %v", pos, *a, *b, err)
	}
	if _, err := parseInterspersed(fs, []string{"x", "--nope"}); err == nil {
		t.Fatal("unknown flag accepted")
	}
}
