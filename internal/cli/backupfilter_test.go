package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupOfSeveralDatabasesWithCollectionFilters(t *testing.T) {
	api := backupRunAPI(false, "completed")
	code, stdout, stderr := runCLI(context.Background(), t, api, nil, "backup", "--connection", "conn_1",
		"--database", "a:exclude=logs,tmp", "--database", "c:collections=orders", "--databases", "d")
	if code != ExitOK {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	got, _ := json.Marshal(api.sent("POST /backups")[0].body["databases"])
	if want := `[{"exclude_collections":["logs","tmp"],"name":"a"},{"collections":["orders"],"name":"c"},"d"]`; string(got) != want {
		t.Errorf("databases = %s; want %s", got, want)
	}

	// --databases-file takes the API's form; one entry still starts a run.
	file := filepath.Join(t.TempDir(), "dbs.json")
	if err := os.WriteFile(file, []byte(`["a", {"name": "c", "exclude_collections": ["logs"]}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	api = backupRunAPI(false, "completed")
	if code, _, stderr = runCLI(context.Background(), t, api, nil, "backup", "--connection", "conn_1", "--databases-file", file); code != ExitOK {
		t.Fatalf("--databases-file: exit %d: %s", code, stderr)
	}
	got, _ = json.Marshal(api.sent("POST /backups")[0].body["databases"])
	if want := `["a",{"exclude_collections":["logs"],"name":"c"}]`; string(got) != want {
		t.Errorf("databases from the file = %s; want %s", got, want)
	}

	// One filtered --database is a single backup with that filter.
	api, _ = backupSequence("completed")
	if code, _, _ = runCLI(context.Background(), t, api, nil, "backup", "--connection", "c", "--database", "shop:exclude=logs"); code != ExitOK {
		t.Fatalf("single filtered database: exit %d", code)
	}
	body := api.sent("POST /backups")[0].body
	if ex, _ := body["exclude_collections"].([]any); body["database"] != "shop" || len(ex) != 1 || ex[0] != "logs" || body["databases"] != nil {
		t.Errorf("single filtered body = %v", body)
	}
}

func TestBackupDatabaseFilterUsage(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"a": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"backup", "--connection", "c", "--database", "a:exclude="},
		{"backup", "--connection", "c", "--database", ":collections=x"},
		{"backup", "--connection", "c", "--database", "a:collections=x:exclude=y"},
		{"backup", "--connection", "c", "--database", "a:exclude=x", "--collections", "y"},
		{"backup", "--connection", "c", "--database", "a:exclude=x", "--database", "b", "--exclude-collections", "y"},
		{"backup", "--connection", "c", "--databases-file", bad},
		{"backup", "--connection", "c", "--databases-file", filepath.Join(t.TempDir(), "missing.json")},
		{"backup", "--job", "j", "--databases-file", bad},
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
