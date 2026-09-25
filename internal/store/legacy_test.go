package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/notify"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// legacyStateWithoutNotifications is a state file written before notifications existed.
const legacyStateWithoutNotifications = `{
  "jobs": {"job_legacy": {"id": "job_legacy", "name": "Legacy", "cron_expression": "@daily", "database": "shop", "enabled": true}},
  "backups": {},
  "restores": {}
}`

// fixtureJob is a job as written by the JSON store, with its own connection string.
type fixtureJob struct {
	models.Job
	MongoURI string `json:"mongo_uri,omitempty"`
}

// legacyFixture is a complete state.json as written by the JSON store.
type legacyFixture struct {
	Jobs     map[string]*fixtureJob           `json:"jobs"`
	Backups  map[string]*models.BackupRecord  `json:"backups"`
	Restores map[string]*models.RestoreRecord `json:"restores"`
	Channels map[string]*notify.Channel       `json:"notification_channels,omitempty"`
	Rules    map[string]*notify.Rule          `json:"notification_rules,omitempty"`
}

func fullFixture() legacyFixture {
	done := ts(30)
	return legacyFixture{
		Jobs: map[string]*fixtureJob{
			"job_1": {Job: models.Job{ID: "job_1", Name: "Nightly", CronExpression: "0 2 * * *", Database: "shop", Enabled: true,
				CreatedAt: ts(0), UpdatedAt: ts(1)}, MongoURI: "mongodb://user:pw@db/shop"},
			"job_2": {Job: models.Job{ID: "job_2", Name: "Hourly", CronExpression: "@hourly", Database: "crm", CreatedAt: ts(2), UpdatedAt: ts(2)}},
		},
		Backups: map[string]*models.BackupRecord{
			"bkp_1": {ID: "bkp_1", JobID: "job_1", Database: "shop", Status: models.StatusCompleted, StorageKey: "k1",
				SizeBytes: 42, SHA256: "ff", StartedAt: ts(10), CompletedAt: &done},
			"bkp_2": {ID: "bkp_2", Database: "shop", Status: models.StatusInProgress, StartedAt: ts(20)},
			// Records without an ID take their map key; null entries are skipped.
			"bkp_3": {Database: "crm", Status: models.StatusFailed, StartedAt: ts(15)},
			"bkp_4": nil,
		},
		Restores: map[string]*models.RestoreRecord{
			"rst_1": {ID: "rst_1", BackupID: "bkp_1", SourceDatabase: "shop", TargetDatabase: "shop_rescue_1",
				Status: models.RestoreStatusCompleted, StartedAt: ts(40)},
		},
		Channels: map[string]*notify.Channel{
			"ch_1": {ID: "ch_1", Name: "Ops", Type: notify.ChannelTelegram, Enabled: true,
				Telegram:     &notify.TelegramConfig{BotToken: "1:secret", ChatID: "42"},
				LastDelivery: &notify.DeliveryStatus{Time: ts(50), Success: true, Attempts: 1, Event: events.BackupFailed}},
			"ch_2": {ID: "ch_2", Name: "Hook", Type: notify.ChannelWebhook,
				Webhook: &notify.WebhookConfig{URL: "https://example.com", Secret: "hmac"}},
		},
		Rules: map[string]*notify.Rule{
			"rule_1": {ID: "rule_1", Name: "failures", Enabled: true, Events: []events.EventType{events.BackupFailed},
				ChannelIDs: []string{"ch_1", "ch_2"}},
		},
	}
}

func writeState(t *testing.T, dir string, v any) string {
	t.Helper()
	var content []byte
	switch s := v.(type) {
	case string:
		content = []byte(s)
	default:
		var err error
		if content, err = json.MarshalIndent(v, "", "  "); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, store.LegacyStateFileName)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func archives(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, store.LegacyStateFileName+".migrated-*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMigrateLegacyStateImportsEverything(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	fixture := fullFixture()
	statePath := writeState(t, dir, fixture)
	s, logs := openLogged(t, filepath.Join(dir, dbFile))

	res, err := s.MigrateLegacyState(ctx, statePath)
	if err != nil {
		t.Fatalf("MigrateLegacyState: %v", err)
	}
	if res == nil || res.Jobs != 2 || res.Backups != 3 || res.Restores != 1 || res.Channels != 2 || res.Rules != 1 {
		t.Fatalf("import counts = %+v", res)
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state.json must be renamed after import: %v", err)
	}
	if a := archives(t, dir); len(a) != 1 || a[0] != res.ArchivedAs {
		t.Fatalf("archives = %v; ArchivedAs = %q", a, res.ArchivedAs)
	}
	if !strings.Contains(logs.String(), "migrated legacy state.json") || !strings.Contains(logs.String(), "backups=3") {
		t.Fatalf("import must be logged with counts; logs:\n%s", logs)
	}

	// Every record round-trips unchanged, including timestamps and channel secrets.
	for id, want := range fixture.Jobs {
		if got, err := s.GetJob(ctx, id); err != nil || !reflect.DeepEqual(got, &want.Job) {
			t.Errorf("job %s = %+v, %v\nwant %+v", id, got, err, want.Job)
		}
	}
	// The legacy job connection string is kept for MigrateLegacyJobURIs, but encrypted.
	var raw string
	if err := rawDB(t, filepath.Join(dir, dbFile)).QueryRow("SELECT data FROM jobs WHERE id = 'job_1'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "user:pw") || !strings.Contains(raw, `"mongo_uri":"sb2:`) {
		t.Fatalf("legacy job uri must be stored encrypted: %s", raw)
	}
	for id, want := range fixture.Backups {
		if want == nil {
			if _, err := s.GetBackupRecord(ctx, id); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("null backup %s imported: %v", id, err)
			}
			continue
		}
		want.ID = id
		if got, err := s.GetBackupRecord(ctx, id); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("backup %s = %+v, %v\nwant %+v", id, got, err, want)
		}
	}
	assertIDs(t, "imported backups", backupIDs(t, s, ""), "bkp_2", "bkp_3", "bkp_1")
	if r, err := s.ListRestoreRecords(ctx); err != nil || len(r) != 1 || !reflect.DeepEqual(r[0], fixture.Restores["rst_1"]) {
		t.Errorf("restores = %+v, %v", r, err)
	}
	for id, want := range fixture.Channels {
		if got, err := s.GetChannel(ctx, id); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("channel %s = %+v, %v\nwant %+v", id, got, err, want)
		}
	}
	if got, err := s.GetRule(ctx, "rule_1"); err != nil || !reflect.DeepEqual(got, fixture.Rules["rule_1"]) {
		t.Errorf("rule = %+v, %v", got, err)
	}

	// A second call finds nothing to do.
	if res, err := s.MigrateLegacyState(ctx, statePath); res != nil || err != nil {
		t.Fatalf("second MigrateLegacyState = %+v, %v; want nil, nil", res, err)
	}
}

func TestMigrateLegacyStateWithoutNotifications(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	statePath := writeState(t, dir, legacyStateWithoutNotifications)
	s, _ := openLogged(t, filepath.Join(dir, dbFile))

	res, err := s.MigrateLegacyState(ctx, statePath)
	if err != nil || res.Jobs != 1 || res.Channels != 0 || res.Rules != 0 {
		t.Fatalf("MigrateLegacyState = %+v, %v", res, err)
	}
	if j, err := s.GetJob(ctx, "job_legacy"); err != nil || !j.Enabled || j.Database != "shop" {
		t.Fatalf("legacy job = %+v, %v", j, err)
	}
}

func TestMigrateLegacyStateEmptyFile(t *testing.T) {
	dir := t.TempDir()
	statePath := writeState(t, dir, "")
	s, _ := openLogged(t, filepath.Join(dir, dbFile))

	res, err := s.MigrateLegacyState(context.Background(), statePath)
	if err != nil || res == nil || res.Jobs+res.Backups+res.Restores+res.Channels+res.Rules != 0 {
		t.Fatalf("MigrateLegacyState(empty) = %+v, %v", res, err)
	}
	if len(archives(t, dir)) != 1 {
		t.Fatal("empty state file must be archived")
	}
}

func TestMigrateLegacyStateNoFile(t *testing.T) {
	dir := t.TempDir()
	s, _ := openLogged(t, filepath.Join(dir, dbFile))
	res, err := s.MigrateLegacyState(context.Background(), filepath.Join(dir, store.LegacyStateFileName))
	if res != nil || err != nil {
		t.Fatalf("MigrateLegacyState without file = %+v, %v; want nil, nil", res, err)
	}
}

func TestMigrateLegacyStateFailureLeavesFileUntouched(t *testing.T) {
	duplicate := fullFixture()
	// Two keys holding the same record ID: the count check must reject the import
	// after rows were already written, proving the transaction rolls back.
	duplicate.Jobs["job_copy"] = duplicate.Jobs["job_1"]

	cases := map[string]any{
		"corrupt json":  `{"jobs": {`,
		"wrong types":   `{"jobs": []}`,
		"duplicate ids": duplicate,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			statePath := writeState(t, dir, content)
			before, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			s, _ := openLogged(t, filepath.Join(dir, dbFile))

			res, err := s.MigrateLegacyState(ctx, statePath)
			if !errors.Is(err, store.ErrLegacyImport) || res != nil {
				t.Fatalf("MigrateLegacyState = %+v, %v; want ErrLegacyImport", res, err)
			}
			after, err := os.ReadFile(statePath)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("state.json must stay untouched (err %v)", err)
			}
			if len(archives(t, dir)) != 0 {
				t.Fatal("failed import must not archive the file")
			}
			if jobs, _ := s.ListJobs(ctx); len(jobs) != 0 {
				t.Fatalf("failed import left %d jobs behind", len(jobs))
			}
		})
	}
}

func TestMigrateLegacyStateNamesConflictingIDs(t *testing.T) {
	cases := map[string]struct {
		state any
		want  []string
	}{
		"same record under two keys": {
			state: func() legacyFixture {
				f := fullFixture()
				f.Jobs["job_copy"] = f.Jobs["job_1"]
				return f
			}(),
			want: []string{"jobs", `"job_1"`, `"job_copy"`},
		},
		"inner id disagrees with key": {
			state: `{"backups": {"bkp_a": {"id": "bkp_b", "database": "d"}, "bkp_b": {"database": "d"}}}`,
			want:  []string{"backups", `duplicate id "bkp_b"`, `"bkp_a"`},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			statePath := writeState(t, dir, tc.state)
			s, _ := openLogged(t, filepath.Join(dir, dbFile))
			_, err := s.MigrateLegacyState(context.Background(), statePath)
			if !errors.Is(err, store.ErrLegacyImport) {
				t.Fatalf("err = %v; want ErrLegacyImport", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q must mention %s", err, w)
				}
			}
		})
	}
}

func TestMigrateLegacyStateIgnoredWhenDatabaseHasData(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	statePath := writeState(t, dir, fullFixture())
	s, logs := openLogged(t, filepath.Join(dir, dbFile))
	if err := s.SaveRule(ctx, &notify.Rule{ID: "existing", Name: "existing"}); err != nil {
		t.Fatal(err)
	}

	res, err := s.MigrateLegacyState(ctx, statePath)
	if res != nil || err != nil {
		t.Fatalf("MigrateLegacyState = %+v, %v; want nil, nil", res, err)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "ignoring legacy state file") {
		t.Fatalf("want a warning; logs:\n%s", logs)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state.json must be left in place: %v", err)
	}
	if jobs, _ := s.ListJobs(ctx); len(jobs) != 0 {
		t.Fatal("state.json must not be merged into a non-empty database")
	}
}
