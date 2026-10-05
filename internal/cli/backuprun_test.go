package cli

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
)

// backupRunAPI starts a run of a and c (b busy when busy is set) and answers the
// list of its backups with in_progress first, then the final statuses.
func backupRunAPI(busy bool, finalC string) *fakeAPI {
	var mu sync.Mutex
	polls := 0
	busyJSON := `[]`
	if busy {
		busyJSON = `[{"database":"b","busy":true,"error":"another backup of database b is already running"}]`
	}
	return newFakeAPI().
		data("POST /backups", http.StatusAccepted, `{"run_id":"run_1","backups":[`+
			`{"id":"bkp_a","database":"a","status":"in_progress","run_id":"run_1"},`+
			`{"id":"bkp_c","database":"c","status":"in_progress","run_id":"run_1"}],"busy":`+busyJSON+`}`).
		on("GET /backups", func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			polls++
			n := polls
			mu.Unlock()
			if n < 2 {
				reply(w, 200, `[{"id":"bkp_a","database":"a","status":"in_progress"},{"id":"bkp_c","database":"c","status":"in_progress"}]`, "")
				return
			}
			c := `{"id":"bkp_c","database":"c","status":"` + finalC + `","size_bytes":2048`
			if finalC == "failed" {
				c += `,"error_message":"mongodump exited 1"`
			}
			reply(w, 200, `[{"id":"bkp_a","database":"a","status":"completed","size_bytes":1024},`+c+`}]`, "")
		})
}

func TestBackupOfSeveralDatabases(t *testing.T) {
	api := backupRunAPI(false, "completed")
	code, stdout, stderr := runCLI(context.Background(), t, api, nil, "backup", "--connection", "conn_1", "--database", "a", "--database", "c", "--wait", "--parallelism", "2")
	if code != ExitOK || !strings.Contains(stdout, "Run run_1: 2 of 2 databases backed up.") || !strings.Contains(stderr, "run run_1: 0 of 2 databases done") {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	body := api.sent("POST /backups")[0].body
	dbs, _ := body["databases"].([]any)
	if body["connection_id"] != "conn_1" || len(dbs) != 2 || dbs[0] != "a" || dbs[1] != "c" || body["parallelism"] != float64(2) || body["database"] != "" {
		t.Fatalf("body = %v", body)
	}
	if q := api.sent("GET /backups")[0].query; !strings.Contains(q, "run_id=run_1") {
		t.Errorf("poll query = %q; want the run's backups", q)
	}

	// --databases, with one database failing and another busy: exit 1 and a summary.
	api = backupRunAPI(true, "failed")
	code, stdout, stderr = runCLI(context.Background(), t, api, nil, "backup", "--connection", "conn_1", "--databases", "a,b,c", "--wait")
	if code != ExitFailed {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	golden(t, "backup_run_partial", stdout)
	if !strings.Contains(stderr, "skipped b: another backup of database b is already running") {
		t.Errorf("stderr = %s; want the busy database reported", stderr)
	}
	body = api.sent("POST /backups")[0].body
	if dbs, _ = body["databases"].([]any); len(dbs) != 3 {
		t.Errorf("--databases body = %v", body)
	}

	// Without --wait: the run and how to follow it; --quiet prints the run ID.
	code, stdout, _ = runCLI(context.Background(), t, backupRunAPI(false, "completed"), nil, "backup", "--connection", "c", "--databases", "a,c")
	if code != ExitOK || !strings.Contains(stdout, "mongorescue list backups --run run_1") {
		t.Fatalf("no wait: %d %s", code, stdout)
	}
	code, stdout, _ = runCLI(context.Background(), t, backupRunAPI(false, "completed"), nil, "backup", "--connection", "c", "--databases", "a,c", "--quiet")
	if code != ExitOK || stdout != "run_1\n" {
		t.Fatalf("quiet: %d %q", code, stdout)
	}
	// One --database stays a single backup.
	api, _ = backupSequence("completed")
	if code, _, _ = runCLI(context.Background(), t, api, nil, "backup", "--connection", "c", "--database", "shop"); code != ExitOK {
		t.Fatalf("single database: exit %d", code)
	}
	if body = api.sent("POST /backups")[0].body; body["database"] != "shop" || body["databases"] != nil {
		t.Errorf("single body = %v", body)
	}
}

func TestBackupOfSeveralDatabasesUsage(t *testing.T) {
	for _, args := range [][]string{
		{"backup", "--connection", "c", "--database", "a", "--database", "b", "--collections", "x"},
		{"backup", "--connection", "c", "--database", "a", "--parallelism", "2"},
		{"backup", "--job", "j", "--databases", "a,b"},
		{"backup", "--connection", "c", "--databases", " , "},
	} {
		api := newFakeAPI()
		if code, _, _ := runCLI(context.Background(), t, api, nil, args...); code != ExitUsage {
			t.Errorf("%v: exit %d; want usage", args, code)
		}
		if len(api.sent("POST /backups")) != 0 {
			t.Errorf("%v: a backup was started", args)
		}
	}
}

func TestListBackupsOfARun(t *testing.T) {
	api := listAPI()
	if code, _, stderr := runCLI(context.Background(), t, api, nil, "list", "backups", "--run", "run_1"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	sent := api.sent("GET /backups")
	if len(sent) != 1 || !slices.Contains(strings.Split(sent[0].query, "&"), "run_id=run_1") {
		t.Fatalf("query = %+v; want run_id=run_1", sent)
	}
}
