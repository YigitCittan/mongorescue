package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/notify"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// dbFile is the database file name used by the tests.
const dbFile = "mongorescue.db"

// testKey and testBox are the secret key shared by the store tests, so a database can
// be reopened.
var (
	testKey = func() []byte {
		key, err := secretbox.GenerateKey()
		if err != nil {
			panic(err)
		}
		return key
	}()
	testBox = func() *secretbox.Box {
		b, err := secretbox.New(testKey)
		if err != nil {
			panic(err)
		}
		return b
	}()
)

// backend is the contract exercised by the conformance suite: every metadata store
// serves both the store and the notification ports.
type backend interface {
	store.Store
	notify.Repository
	Close() error
}

// opener opens a backend persisted under dir. Opening the same dir again after Close
// must observe everything written before.
type opener func(t *testing.T, dir string) backend

// runConformance runs the behaviour every Store implementation must provide.
func runConformance(t *testing.T, open opener) {
	t.Run("Jobs", func(t *testing.T) { testJobs(t, open(t, t.TempDir())) })
	t.Run("BackupRecords", func(t *testing.T) { testBackupRecords(t, open(t, t.TempDir())) })
	t.Run("RestoreRecords", func(t *testing.T) { testRestoreRecords(t, open(t, t.TempDir())) })
	t.Run("Channels", func(t *testing.T) { testChannels(t, open(t, t.TempDir())) })
	t.Run("Rules", func(t *testing.T) { testRules(t, open(t, t.TempDir())) })
	t.Run("InvalidRecords", func(t *testing.T) { testInvalidRecords(t, open(t, t.TempDir())) })
	t.Run("EmptyListsAreNotNil", func(t *testing.T) { testEmptyLists(t, open(t, t.TempDir())) })
	t.Run("Persistence", func(t *testing.T) { testPersistence(t, open) })
}

func TestSQLiteStoreConformance(t *testing.T) {
	runConformance(t, func(t *testing.T, dir string) backend {
		return storetest.OpenWithBox(t, filepath.Join(dir, dbFile), testBox)
	})
}

// ts returns a fixed UTC timestamp offset by minutes, so values survive JSON exactly.
func ts(minutes int) time.Time {
	return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC).Add(time.Duration(minutes) * time.Minute)
}

func testJobs(t *testing.T, s backend) {
	ctx := context.Background()
	job := &models.Job{
		ID: "job_b", Name: "Beta", CronExpression: "0 2 * * *", Database: "ecommerce",
		Collections: []string{"orders"}, StorageType: models.StorageLocal,
		RetentionDays: 14, RetentionCount: 10, Gzip: true, Enabled: true,
	}
	if err := s.SaveJob(ctx, job); err != nil {
		t.Fatalf("SaveJob: %v", err)
	}
	if job.CreatedAt.IsZero() || job.UpdatedAt.IsZero() {
		t.Fatalf("SaveJob must set CreatedAt and UpdatedAt on the job: %+v", job)
	}
	created := job.CreatedAt
	if err := s.SaveJob(ctx, &models.Job{ID: "job_a", Name: "Alpha", Database: "crm"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetJob(ctx, "job_b")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if !reflect.DeepEqual(got.Collections, job.Collections) || got.Name != job.Name || !got.CreatedAt.Equal(created) {
		t.Fatalf("GetJob = %+v; want %+v", got, job)
	}

	// Updating keeps CreatedAt and moves UpdatedAt.
	got.Name = "Beta renamed"
	if err = s.SaveJob(ctx, got); err != nil {
		t.Fatal(err)
	}
	again, _ := s.GetJob(ctx, "job_b")
	if again.Name != "Beta renamed" || !again.CreatedAt.Equal(created) || again.UpdatedAt.Before(job.UpdatedAt) {
		t.Fatalf("updated job = %+v", again)
	}

	jobs, err := s.ListJobs(ctx)
	if err != nil || len(jobs) != 2 || jobs[0].ID != "job_a" || jobs[1].ID != "job_b" {
		t.Fatalf("ListJobs = %v, %v; want sorted by name", jobs, err)
	}

	// Returned values are copies.
	jobs[0].Name = "mutated"
	if j, _ := s.GetJob(ctx, "job_a"); j.Name != "Alpha" {
		t.Fatal("ListJobs must return copies")
	}

	if err := s.DeleteJob(ctx, "job_b"); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	if _, err := s.GetJob(ctx, "job_b"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetJob after delete = %v; want ErrNotFound", err)
	}
	if err := s.DeleteJob(ctx, "job_b"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second DeleteJob = %v; want ErrNotFound", err)
	}
}

func testBackupRecords(t *testing.T, s backend) {
	ctx := context.Background()
	completed := ts(5)
	full := &models.BackupRecord{
		ID: "bkp_2", JobID: "job_1", Database: "ecommerce", Status: models.StatusCompleted,
		StorageType: models.StorageS3, StorageKey: "ecommerce/bkp_2.archive.gz", SizeBytes: 1024,
		SHA256: "abcd", Encrypted: true, EncryptionMode: "x25519", Collections: []string{"a", "b"},
		StartedAt: ts(2), CompletedAt: &completed, DurationSeconds: 3.5, ErrorMessage: "",
	}
	records := []*models.BackupRecord{
		{ID: "bkp_1", Database: "ecommerce", Status: models.StatusCompleted, StartedAt: ts(1)},
		full,
		{ID: "bkp_3", Database: "crm", Status: models.StatusFailed, StartedAt: ts(3)},
		{ID: "bkp_0", Database: "crm", Status: models.StatusInProgress}, // zero StartedAt sorts last
	}
	for _, r := range records {
		if err := s.SaveBackupRecord(ctx, r); err != nil {
			t.Fatalf("SaveBackupRecord(%s): %v", r.ID, err)
		}
	}

	got, err := s.GetBackupRecord(ctx, "bkp_2")
	if err != nil {
		t.Fatalf("GetBackupRecord: %v", err)
	}
	if !reflect.DeepEqual(got, full) {
		t.Fatalf("GetBackupRecord = %+v\nwant %+v", got, full)
	}

	assertIDs(t, "all backups", backupIDs(t, s, ""), "bkp_3", "bkp_2", "bkp_1", "bkp_0")
	assertIDs(t, "ecommerce backups", backupIDs(t, s, "ecommerce"), "bkp_2", "bkp_1")
	assertIDs(t, "unknown database", backupIDs(t, s, "nope"))

	// Saving again replaces the record and its indexed columns.
	got.Database, got.StartedAt = "crm", ts(10)
	if err := s.SaveBackupRecord(ctx, got); err != nil {
		t.Fatal(err)
	}
	assertIDs(t, "crm backups after move", backupIDs(t, s, "crm"), "bkp_2", "bkp_3", "bkp_0")
	assertIDs(t, "ecommerce backups after move", backupIDs(t, s, "ecommerce"), "bkp_1")

	if err := s.DeleteBackupRecord(ctx, "bkp_1"); err != nil {
		t.Fatalf("DeleteBackupRecord: %v", err)
	}
	if _, err := s.GetBackupRecord(ctx, "bkp_1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetBackupRecord after delete = %v", err)
	}
	if err := s.DeleteBackupRecord(ctx, "bkp_1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second DeleteBackupRecord = %v", err)
	}
}

func testRestoreRecords(t *testing.T, s backend) {
	ctx := context.Background()
	for _, r := range []*models.RestoreRecord{
		{ID: "rst_1", BackupID: "bkp_1", SourceDatabase: "shop", TargetDatabase: "shop_rescue_1",
			Status: models.RestoreStatusCompleted, StartedAt: ts(1), Verified: true},
		{ID: "rst_2", BackupID: "bkp_1", SourceDatabase: "shop", TargetDatabase: "shop_rescue_2",
			Status: models.RestoreStatusInProgress, StartedAt: ts(2), DryRun: true},
	} {
		if err := s.SaveRestoreRecord(ctx, r); err != nil {
			t.Fatalf("SaveRestoreRecord: %v", err)
		}
	}
	list, err := s.ListRestoreRecords(ctx)
	if err != nil || len(list) != 2 || list[0].ID != "rst_2" || list[1].ID != "rst_1" {
		t.Fatalf("ListRestoreRecords = %v, %v; want newest first", list, err)
	}
	if !list[0].DryRun || !list[1].Verified {
		t.Fatalf("restore fields lost: %+v %+v", list[0], list[1])
	}

	list[0].Status = models.RestoreStatusFailed
	if err := s.SaveRestoreRecord(ctx, list[0]); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListRestoreRecords(ctx)
	if len(list) != 2 || list[0].Status != models.RestoreStatusFailed {
		t.Fatalf("restore update lost: %+v", list)
	}
}

func testChannels(t *testing.T, s backend) {
	ctx := context.Background()
	ch := &notify.Channel{ID: "c1", Name: "Ops", Type: notify.ChannelTelegram, Enabled: true,
		Telegram: &notify.TelegramConfig{BotToken: "1:secret", ChatID: "42"}}
	ch2 := &notify.Channel{ID: "c2", Name: "Mail", Type: notify.ChannelEmail,
		Email: &notify.EmailConfig{Host: "h", Port: 25, To: []string{"a@b.co"}}}
	ch3 := &notify.Channel{ID: "c0", Name: "Mail", Type: notify.ChannelWebhook,
		Webhook: &notify.WebhookConfig{URL: "https://example.com/hook", Secret: "s3cr3t"}}
	for _, c := range []*notify.Channel{ch, ch2, ch3} {
		if err := s.SaveChannel(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	rule := &notify.Rule{ID: "r1", Name: "failures", Enabled: true,
		Events: []events.EventType{events.BackupFailed}, ChannelIDs: []string{"c1", "c2"}}
	other := &notify.Rule{ID: "r2", Name: "all", ChannelIDs: []string{"c2"}}
	for _, r := range []*notify.Rule{rule, other} {
		if err := s.SaveRule(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	chans, err := s.ListChannels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, "channels by name then id", channelIDs(chans), "c0", "c2", "c1")

	// Secrets are stored as-is; masking is not the store's job.
	if c0, getErr := s.GetChannel(ctx, "c0"); getErr != nil || c0.Webhook.Secret != "s3cr3t" {
		t.Fatalf("GetChannel(c0) = %+v, %v", c0, getErr)
	}

	status := notify.DeliveryStatus{Time: ts(7), Success: true, Attempts: 2, Event: events.BackupFailed}
	if err = s.SaveDeliveryStatus(ctx, "c1", status); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveDeliveryStatus(ctx, "ghost", notify.DeliveryStatus{}); !errors.Is(err, notify.ErrChannelNotFound) {
		t.Fatalf("status for missing channel = %v", err)
	}
	c1, err := s.GetChannel(ctx, "c1")
	if err != nil || c1.Telegram.BotToken != "1:secret" || c1.LastDelivery == nil || !reflect.DeepEqual(*c1.LastDelivery, status) {
		t.Fatalf("channel after delivery status = %+v, %v", c1, err)
	}

	// Returned values are deep copies.
	got, _ := s.GetChannel(ctx, "c2")
	got.Email.To[0] = "mutated@x.co"
	if again, _ := s.GetChannel(ctx, "c2"); again.Email.To[0] != "a@b.co" {
		t.Fatal("GetChannel must return a deep copy")
	}

	// Deleting a channel removes it from every referencing rule atomically.
	if err := s.DeleteChannel(ctx, "c2"); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.GetRule(ctx, "r1"); len(r.ChannelIDs) != 1 || r.ChannelIDs[0] != "c1" {
		t.Fatalf("r1 channels after delete = %v", r.ChannelIDs)
	}
	if r, _ := s.GetRule(ctx, "r2"); len(r.ChannelIDs) != 0 {
		t.Fatalf("r2 channels after delete = %v", r.ChannelIDs)
	}
	if _, err := s.GetChannel(ctx, "c2"); !errors.Is(err, notify.ErrChannelNotFound) {
		t.Fatalf("GetChannel after delete = %v", err)
	}
	if err := s.DeleteChannel(ctx, "c2"); !errors.Is(err, notify.ErrChannelNotFound) {
		t.Fatalf("second delete = %v", err)
	}
}

func testRules(t *testing.T, s backend) {
	ctx := context.Background()
	for _, r := range []*notify.Rule{
		{ID: "r2", Name: "b", Enabled: true, Events: []events.EventType{events.BackupFailed}, JobIDs: []string{"j1"}, ChannelIDs: []string{"c1"}},
		{ID: "r1", Name: "a", ChannelIDs: []string{}},
		{ID: "r0", Name: "b"},
	} {
		if err := s.SaveRule(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	rules, err := s.ListRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(rules))
	for _, r := range rules {
		ids = append(ids, r.ID)
	}
	assertIDs(t, "rules by name then id", ids, "r1", "r0", "r2")

	r2, err := s.GetRule(ctx, "r2")
	if err != nil || !r2.Enabled || len(r2.JobIDs) != 1 || r2.Events[0] != events.BackupFailed {
		t.Fatalf("GetRule = %+v, %v", r2, err)
	}
	r2.Name = "renamed"
	if err := s.SaveRule(ctx, r2); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.GetRule(ctx, "r2"); r.Name != "renamed" {
		t.Fatalf("rule update lost: %+v", r)
	}

	if err := s.DeleteRule(ctx, "r2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRule(ctx, "r2"); !errors.Is(err, notify.ErrRuleNotFound) {
		t.Fatalf("GetRule after delete = %v", err)
	}
	if err := s.DeleteRule(ctx, "r2"); !errors.Is(err, notify.ErrRuleNotFound) {
		t.Fatalf("second DeleteRule = %v", err)
	}
}

func testInvalidRecords(t *testing.T, s backend) {
	ctx := context.Background()
	for name, err := range map[string]error{
		"nil job":        s.SaveJob(ctx, nil),
		"job without id": s.SaveJob(ctx, &models.Job{Name: "x"}),
		"backup":         s.SaveBackupRecord(ctx, &models.BackupRecord{}),
		"restore":        s.SaveRestoreRecord(ctx, nil),
		"channel":        s.SaveChannel(ctx, &notify.Channel{Name: "x"}),
		"rule":           s.SaveRule(ctx, nil),
	} {
		if !errors.Is(err, store.ErrInvalidRecord) {
			t.Errorf("%s: err = %v; want ErrInvalidRecord", name, err)
		}
	}
}

func testEmptyLists(t *testing.T, s backend) {
	ctx := context.Background()
	jobs, err1 := s.ListJobs(ctx)
	backups, err2 := s.ListBackupRecords(ctx, "")
	restores, err3 := s.ListRestoreRecords(ctx)
	chans, err4 := s.ListChannels(ctx)
	rules, err5 := s.ListRules(ctx)
	if err := errors.Join(err1, err2, err3, err4, err5); err != nil {
		t.Fatal(err)
	}
	if jobs == nil || backups == nil || restores == nil || chans == nil || rules == nil {
		t.Fatal("empty lists must be non-nil so they serialise as []")
	}
}

func testPersistence(t *testing.T, open opener) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	if err := s.SaveJob(ctx, &models.Job{ID: "j", Name: "J", Database: "d", ConnectionID: "conn_1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveBackupRecord(ctx, &models.BackupRecord{ID: "b", Database: "d", StartedAt: ts(1)}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRestoreRecord(ctx, &models.RestoreRecord{ID: "r", StartedAt: ts(1)}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveChannel(ctx, &notify.Channel{ID: "c", Name: "C", Type: notify.ChannelWebhook,
		Webhook: &notify.WebhookConfig{URL: "https://example.com", Secret: "x"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRule(ctx, &notify.Rule{ID: "ru", Name: "R", ChannelIDs: []string{"c"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	re := open(t, dir)
	if j, err := re.GetJob(ctx, "j"); err != nil || j.ConnectionID != "conn_1" {
		t.Fatalf("job after reopen = %+v, %v", j, err)
	}
	if _, err := re.GetBackupRecord(ctx, "b"); err != nil {
		t.Fatalf("backup after reopen: %v", err)
	}
	if l, err := re.ListRestoreRecords(ctx); err != nil || len(l) != 1 {
		t.Fatalf("restores after reopen = %v, %v", l, err)
	}
	if c, err := re.GetChannel(ctx, "c"); err != nil || c.Webhook.Secret != "x" {
		t.Fatalf("channel after reopen = %+v, %v", c, err)
	}
	if r, err := re.GetRule(ctx, "ru"); err != nil || len(r.ChannelIDs) != 1 {
		t.Fatalf("rule after reopen = %+v, %v", r, err)
	}
}

func backupIDs(t *testing.T, s backend, database string) []string {
	t.Helper()
	list, err := s.ListBackupRecords(context.Background(), database)
	if err != nil {
		t.Fatalf("ListBackupRecords(%q): %v", database, err)
	}
	ids := make([]string, 0, len(list))
	for _, r := range list {
		ids = append(ids, r.ID)
	}
	return ids
}

func channelIDs(list []*notify.Channel) []string {
	ids := make([]string, 0, len(list))
	for _, c := range list {
		ids = append(ids, c.ID)
	}
	return ids
}

func assertIDs(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %v; want %v", what, got, want)
	}
}

func TestCreateAndUpdateJobNeverOverwriteOrResurrect(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	job := &models.Job{ID: "job_x", Name: "first", Database: "db"}
	if err := s.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateJob(ctx, &models.Job{ID: "job_x", Name: "second"}); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("duplicate create = %v; want ErrAlreadyExists", err)
	}
	if got, _ := s.GetJob(ctx, "job_x"); got.Name != "first" {
		t.Fatalf("job overwritten: %+v", got)
	}
	job.Name = "renamed"
	if err := s.UpdateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteJob(ctx, "job_x"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateJob(ctx, job); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("update of a deleted job = %v; want ErrNotFound", err)
	}
	if _, err := s.GetJob(ctx, "job_x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a deleted job was recreated")
	}
}
