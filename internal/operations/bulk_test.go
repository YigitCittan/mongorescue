package operations_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// recordingPublisher keeps every published event.
type recordingPublisher struct {
	mu     sync.Mutex
	events []events.Event
}

func (p *recordingPublisher) Publish(_ context.Context, e events.Event) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
	return true
}

func (p *recordingPublisher) bulk() []events.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []events.Event
	for _, e := range p.events {
		if e.Type == events.BulkCompleted {
			out = append(out, e)
		}
	}
	return out
}

// oneConnection resolves only connection "conn_ok".
type oneConnection struct{}

func (oneConnection) Resolve(_ context.Context, id string) (*models.Connection, error) {
	if id != "conn_ok" {
		return nil, connections.ErrNotFound
	}
	return &models.Connection{ID: id, Name: "ok", URI: "mongodb://localhost:27017"}, nil
}

func (c oneConnection) Get(ctx context.Context, id string) (*models.Connection, error) {
	return c.Resolve(ctx, id)
}

func (oneConnection) List(context.Context) ([]*models.Connection, error) { return nil, nil }

// bulkEnv is an operations service with in-memory storage for bulk tests.
type bulkEnv struct {
	svc       *operations.Service
	st        *store.SQLiteStore
	mock      *storage.MockStorage
	pub       *recordingPublisher
	deletedMu sync.Mutex
	deleted   []string
	// brokenTarget names a storage target whose driver cannot be opened.
	brokenTarget string
}

func newBulkEnv(t *testing.T) *bulkEnv {
	t.Helper()
	env := &bulkEnv{st: storetest.New(t), mock: storage.NewMockStorage(), pub: &recordingPublisher{}, brokenTarget: "tgt_broken"}
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	bRunner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
	}
	env.svc = operations.New(operations.Config{
		Store:       env.st,
		Backup:      backup.NewEngine(env.mock, "", backup.WithRunner(bRunner)),
		Restore:     restore.NewEngine(env.mock, ""),
		Runs:        manager,
		Connections: oneConnection{},
		Publisher:   env.pub,
		Storage: func(_ context.Context, id string) (storage.Storage, error) {
			if id == env.brokenTarget {
				return nil, errors.New("target unreachable")
			}
			return env.mock, nil
		},
		OnJobDeleted: func(id string) {
			env.deletedMu.Lock()
			defer env.deletedMu.Unlock()
			env.deleted = append(env.deleted, id)
		},
	})
	return env
}

// admin is a context with an admin API key principal.
func admin() context.Context {
	return auth.WithPrincipal(context.Background(), &auth.Principal{Method: auth.MethodAPIKey, APIKeyID: "key_admin", Scope: auth.ScopeAdmin})
}

// operator is a context with an operator API key principal.
func operator() context.Context {
	return auth.WithPrincipal(context.Background(), &auth.Principal{Method: auth.MethodAPIKey, APIKeyID: "key_op", Scope: auth.ScopeOperator})
}

// backupAt stores a backup with an archive in the mock storage (when key is set).
func (env *bulkEnv) backupAt(t *testing.T, id, jobID string, status models.BackupStatus, started time.Time, key string, size int64) {
	t.Helper()
	rec := &models.BackupRecord{ID: id, JobID: jobID, Database: "shop", Status: status, StartedAt: started, StorageKey: key, SizeBytes: size}
	if err := env.st.SaveBackupRecord(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if key != "" {
		if _, err := env.mock.Save(context.Background(), key, strings.NewReader("data")); err != nil {
			t.Fatal(err)
		}
	}
}

func (env *bulkEnv) hasObject(key string) bool {
	_, err := env.mock.Stat(context.Background(), key)
	return err == nil
}

func (env *bulkEnv) hasBackup(id string) bool {
	_, err := env.st.GetBackupRecord(context.Background(), id)
	return err == nil
}

func intPtr(n int) *int { return &n }

func TestBulkDeleteBackupsDryRunMatchesRun(t *testing.T) {
	env := newBulkEnv(t)
	ctx := admin()
	now := time.Now().UTC()
	if err := env.st.SaveJob(ctx, &models.Job{ID: "job_a", Name: "Nightly shop", Database: "shop", CronExpression: "@daily"}); err != nil {
		t.Fatal(err)
	}
	env.backupAt(t, "b_old1", "job_a", models.StatusCompleted, now.Add(-72*time.Hour), "shop/old1", 100)
	env.backupAt(t, "b_old2", "job_a", models.StatusCompleted, now.Add(-48*time.Hour), "shop/old2", 200)
	env.backupAt(t, "b_last", "job_a", models.StatusCompleted, now.Add(-24*time.Hour), "shop/last", 300)
	env.backupAt(t, "b_fail", "job_a", models.StatusFailed, now.Add(-time.Hour), "", 0)
	env.backupAt(t, "b_run", "", models.StatusInProgress, now, "", 0)
	env.backupAt(t, "b_manual", "", models.StatusCompleted, now.Add(-2*time.Hour), "shop/manual", 50)

	req := operations.BulkRequest{Action: "delete", IDs: []string{"b_old1", "b_old2", "b_last", "b_fail", "b_run", "b_manual", "b_missing", "b_old1"}, DryRun: true}
	dry, err := env.svc.Bulk(ctx, operations.BulkBackups, req)
	if err != nil {
		t.Fatal(err)
	}
	wantSkipped := []operations.BulkSkip{
		{ID: "b_last", Reason: operations.SkipLastGoodBackup, Params: map[string]string{"job": "Nightly shop"}, Detail: "last successful backup of job Nightly shop"},
		{ID: "b_run", Reason: operations.SkipInProgress, Params: map[string]string{"status": "in_progress"}, Detail: "the backup is still in_progress"},
		{ID: "b_missing", Reason: operations.SkipNotFound, Detail: "no such record"},
	}
	if dry.Matched != 7 || dry.Actionable != 4 || dry.TotalSizeBytes != 350 || !reflect.DeepEqual(dry.Skipped, wantSkipped) {
		t.Fatalf("dry run = %+v", dry)
	}
	if want := []string{"b_old1", "b_old2", "b_fail", "b_manual"}; !reflect.DeepEqual(dry.ActionableIDs, want) {
		t.Fatalf("actionable ids = %v; want %v", dry.ActionableIDs, want)
	}
	if !env.hasBackup("b_old1") || !env.hasObject("shop/old1") || len(env.pub.bulk()) != 0 {
		t.Fatal("a dry run must not change anything or publish a summary")
	}

	req.DryRun = false
	req.ConfirmCount = intPtr(dry.Actionable)
	run, err := env.svc.Bulk(ctx, operations.BulkBackups, req)
	if err != nil {
		t.Fatal(err)
	}
	if run.Matched != dry.Matched || run.Actionable != dry.Actionable || run.TotalSizeBytes != dry.TotalSizeBytes ||
		!reflect.DeepEqual(run.Skipped, dry.Skipped) || run.Succeeded != 4 || run.Failed != 0 || len(run.Results) != 4 {
		t.Fatalf("run = %+v; want the dry run's plan", run)
	}
	for _, id := range dry.ActionableIDs {
		if env.hasBackup(id) {
			t.Errorf("%s still stored", id)
		}
	}
	for _, key := range []string{"shop/old1", "shop/old2", "shop/manual"} {
		if env.hasObject(key) {
			t.Errorf("archive %s still in storage", key)
		}
	}
	if !env.hasBackup("b_last") || !env.hasObject("shop/last") || !env.hasBackup("b_run") {
		t.Fatal("protected backups were deleted")
	}
	summaries := env.pub.bulk()
	if len(summaries) != 1 {
		t.Fatalf("summary events = %+v; want one", summaries)
	}
	if b := summaries[0].Bulk; b == nil || b.Resource != "backups" || b.Action != "delete" || b.Succeeded != 4 || b.Skipped != 3 || b.Failed != 0 || b.Actor != "api_key:key_admin" {
		t.Fatalf("summary = %+v", summaries[0].Bulk)
	}
}

func TestBulkConfirmCount(t *testing.T) {
	env := newBulkEnv(t)
	ctx := admin()
	now := time.Now().UTC()
	var ids []string
	for i := range 12 {
		id := fmt.Sprintf("b_%02d", i)
		env.backupAt(t, id, "", models.StatusFailed, now.Add(-time.Duration(i)*time.Minute), "", 0)
		ids = append(ids, id)
	}
	for _, c := range []struct {
		name    string
		confirm *int
	}{{"missing", nil}, {"too low", intPtr(11)}, {"too high", intPtr(13)}} {
		_, err := env.svc.Bulk(ctx, operations.BulkBackups, operations.BulkRequest{Action: "delete", IDs: ids, ConfirmCount: c.confirm})
		if !errors.Is(err, operations.ErrBulkConfirm) {
			t.Fatalf("%s confirm_count: %v; want ErrBulkConfirm", c.name, err)
		}
	}
	if !env.hasBackup("b_00") {
		t.Fatal("a refused run deleted backups")
	}
	// Ten items or fewer need no count, but a wrong one is still refused.
	if _, err := env.svc.Bulk(ctx, operations.BulkBackups, operations.BulkRequest{Action: "delete", IDs: ids[:2], ConfirmCount: intPtr(3)}); !errors.Is(err, operations.ErrBulkConfirm) {
		t.Fatalf("wrong count for two items: %v", err)
	}
	res, err := env.svc.Bulk(ctx, operations.BulkBackups, operations.BulkRequest{Action: "delete", IDs: ids[:10]})
	if err != nil || res.Succeeded != 10 {
		t.Fatalf("ten items without a count = %+v, %v", res, err)
	}
	res, err = env.svc.Bulk(ctx, operations.BulkBackups, operations.BulkRequest{Action: "delete", IDs: ids, ConfirmCount: intPtr(2)})
	if err != nil || res.Succeeded != 2 || res.Matched != 12 || len(res.Skipped) != 10 {
		t.Fatalf("remaining two = %+v, %v", res, err)
	}
}

func TestBulkFilterMode(t *testing.T) {
	env := newBulkEnv(t)
	ctx := admin()
	now := time.Now().UTC()
	for i := range 5 {
		env.backupAt(t, fmt.Sprintf("f_%d", i), "", models.StatusFailed, now.Add(-time.Duration(i)*time.Hour), "", 0)
	}
	env.backupAt(t, "ok_1", "", models.StatusCompleted, now, "shop/ok1", 10)
	env.backupAt(t, "old_fail", "", models.StatusFailed, now.Add(-30*24*time.Hour), "", 0)

	from := now.Add(-24 * time.Hour).Format(time.RFC3339)
	filter := &operations.BulkFilter{Status: "failed", From: from}
	dry, err := env.svc.Bulk(ctx, operations.BulkBackups, operations.BulkRequest{Action: "delete", Filter: filter, DryRun: true})
	if err != nil || dry.Matched != 5 || dry.Actionable != 5 {
		t.Fatalf("filter dry run = %+v, %v", dry, err)
	}
	res, err := env.svc.Bulk(ctx, operations.BulkBackups, operations.BulkRequest{Action: "delete", Filter: filter, ConfirmCount: intPtr(5)})
	if err != nil || res.Succeeded != 5 {
		t.Fatalf("filter run = %+v, %v", res, err)
	}
	if !env.hasBackup("ok_1") || !env.hasBackup("old_fail") {
		t.Fatal("records outside the filter were deleted")
	}

	// The cap applies to filters and to ID lists.
	operations.SetBulkCap(env.svc, 1)
	if _, err = env.svc.Bulk(ctx, operations.BulkBackups, operations.BulkRequest{Action: "delete", Filter: &operations.BulkFilter{}, DryRun: true}); !errors.Is(err, operations.ErrBulkTooLarge) {
		t.Fatalf("filter over the cap: %v; want ErrBulkTooLarge", err)
	}
	if _, err = env.svc.Bulk(ctx, operations.BulkBackups, operations.BulkRequest{Action: "delete", IDs: []string{"a", "b"}, DryRun: true}); !errors.Is(err, operations.ErrBulkTooLarge) {
		t.Fatalf("ids over the cap: %v; want ErrBulkTooLarge", err)
	}
	if res, err = env.svc.Bulk(ctx, operations.BulkBackups, operations.BulkRequest{Action: "delete", Filter: &operations.BulkFilter{Status: "completed"}, DryRun: true}); err != nil || res.Matched != 1 {
		t.Fatalf("filter at the cap = %+v, %v", res, err)
	}
}

func TestBulkValidation(t *testing.T) {
	env := newBulkEnv(t)
	ctx := admin()
	for _, c := range []struct {
		name     string
		resource operations.BulkResource
		req      operations.BulkRequest
		msg      string
	}{
		{"unknown action", operations.BulkBackups, operations.BulkRequest{Action: "explode", IDs: []string{"a"}}, "unknown action"},
		{"no selection", operations.BulkBackups, operations.BulkRequest{Action: "delete"}, "either ids or filter"},
		{"both selections", operations.BulkBackups, operations.BulkRequest{Action: "delete", IDs: []string{"a"}, Filter: &operations.BulkFilter{}}, "either ids or filter"},
		{"empty id", operations.BulkBackups, operations.BulkRequest{Action: "delete", IDs: []string{""}}, "ids must be"},
		{"foreign filter field", operations.BulkRestores, operations.BulkRequest{Action: "delete", Filter: &operations.BulkFilter{Trigger: "manual"}}, "do not apply"},
		{"bad status", operations.BulkBackups, operations.BulkRequest{Action: "delete", Filter: &operations.BulkFilter{Status: "zombie"}}, "status must be"},
		{"bad time", operations.BulkBackups, operations.BulkRequest{Action: "delete", Filter: &operations.BulkFilter{From: "yesterday"}}, "RFC 3339"},
		{"run_now without a scheduler", operations.BulkJobs, operations.BulkRequest{Action: "run_now", IDs: []string{"j"}}, "unknown action"},
	} {
		_, err := env.svc.Bulk(ctx, c.resource, c.req)
		if !errors.Is(err, operations.ErrInvalid) || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: %v; want ErrInvalid mentioning %q", c.name, err, c.msg)
		}
	}
}

func TestBulkScopes(t *testing.T) {
	env := newBulkEnv(t)
	env.backupAt(t, "b1", "", models.StatusFailed, time.Now(), "", 0)
	for _, c := range []struct {
		resource operations.BulkResource
		action   string
	}{{operations.BulkBackups, "delete"}, {operations.BulkRestores, "delete"}, {operations.BulkJobs, "delete"}, {operations.BulkJobs, "enable"}, {operations.BulkJobs, "disable"}} {
		for _, dry := range []bool{true, false} {
			_, err := env.svc.Bulk(operator(), c.resource, operations.BulkRequest{Action: c.action, IDs: []string{"b1"}, DryRun: dry})
			if !errors.Is(err, auth.ErrForbidden) {
				t.Errorf("operator %s %s (dry run %v): %v; want ErrForbidden", c.resource, c.action, dry, err)
			}
		}
	}
	if !env.hasBackup("b1") {
		t.Fatal("a forbidden run deleted a backup")
	}
	var listed []string
	for _, a := range env.svc.BulkActions(operator()) {
		listed = append(listed, fmt.Sprintf("%s/%s:%v", a.Resource, a.Name, a.Allowed))
	}
	want := []string{"backups/cancel:true", "backups/delete:false", "backups/pin:true", "backups/unpin:false", "jobs/delete:false",
		"jobs/disable:false", "jobs/enable:false", "restores/cancel:true", "restores/delete:false"}
	if !reflect.DeepEqual(listed, want) {
		t.Fatalf("actions for an operator = %v; want %v (run_now needs a scheduler, verify a verifier)", listed, want)
	}
}

func TestBulkSharedArchives(t *testing.T) {
	env := newBulkEnv(t)
	ctx := admin()
	now := time.Now().UTC()
	env.backupAt(t, "s1", "", models.StatusCompleted, now.Add(-2*time.Hour), "shop/shared", 10)
	env.backupAt(t, "s2", "", models.StatusFailed, now.Add(-time.Hour), "shop/shared", 0)
	env.backupAt(t, "s3", "", models.StatusCompleted, now, "shop/shared", 10)

	res, err := env.svc.Bulk(ctx, operations.BulkBackups, operations.BulkRequest{Action: "delete", IDs: []string{"s1"}})
	if err != nil || res.Succeeded != 1 || !strings.Contains(res.Results[0].Detail, "archive is kept") {
		t.Fatalf("deleting one sharer = %+v, %v", res, err)
	}
	if !env.hasObject("shop/shared") {
		t.Fatal("an archive other records name was deleted")
	}
	// The last record naming it takes the archive with it, also within one run.
	res, err = env.svc.Bulk(ctx, operations.BulkBackups, operations.BulkRequest{Action: "delete", IDs: []string{"s2", "s3"}})
	if err != nil || res.Succeeded != 2 || res.Results[0].Detail == "" || res.Results[1].Detail != "" {
		t.Fatalf("deleting the rest = %+v, %v", res, err)
	}
	if env.hasObject("shop/shared") {
		t.Fatal("the archive outlived its last record")
	}

	// The single-item delete follows the same rule.
	env.backupAt(t, "u1", "", models.StatusCompleted, now, "shop/u", 1)
	env.backupAt(t, "u2", "", models.StatusCompleted, now, "shop/u", 1)
	single, err := env.svc.DeleteBackup(ctx, "u1")
	if err != nil || single.ArchiveDeleted || single.ArchiveKept == "" || !env.hasObject("shop/u") {
		t.Fatalf("single delete of a sharer = %+v, %v", single, err)
	}
	if single, err = env.svc.DeleteBackup(ctx, "u2"); err != nil || !single.ArchiveDeleted || env.hasObject("shop/u") {
		t.Fatalf("single delete of the last sharer = %+v, %v", single, err)
	}
	if _, err = env.svc.DeleteBackup(ctx, "u2"); !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("deleting a deleted backup: %v; want ErrNotFound", err)
	}
}

func TestBulkPartialFailures(t *testing.T) {
	env := newBulkEnv(t)
	ctx := admin()
	now := time.Now().UTC()
	// An unreachable storage target still lets the record go, with a warning.
	if err := env.st.SaveBackupRecord(ctx, &models.BackupRecord{ID: "broken", Database: "shop", Status: models.StatusCompleted,
		StartedAt: now, StorageKey: "shop/broken", StorageTargetID: env.brokenTarget}); err != nil {
		t.Fatal(err)
	}
	env.backupAt(t, "fine", "", models.StatusCompleted, now, "shop/fine", 1)
	res, err := env.svc.Bulk(ctx, operations.BulkBackups, operations.BulkRequest{Action: "delete", IDs: []string{"broken", "fine"}})
	if err != nil || res.Succeeded != 2 || res.Results[0].Warning == "" || res.Results[1].Warning != "" {
		t.Fatalf("delete with a broken target = %+v, %v", res, err)
	}

	// Enabling a job whose connection is gone fails for that job only.
	for _, j := range []*models.Job{
		{ID: "j_good", Name: "good", Database: "shop", CronExpression: "@daily", ConnectionID: "conn_ok"},
		{ID: "j_orphan", Name: "orphan", Database: "shop", CronExpression: "@daily", ConnectionID: "conn_gone"},
		{ID: "j_on", Name: "on", Database: "shop", CronExpression: "@daily", ConnectionID: "conn_ok", Enabled: true},
	} {
		if err = env.st.SaveJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	res, err = env.svc.Bulk(ctx, operations.BulkJobs, operations.BulkRequest{Action: "enable", IDs: []string{"j_good", "j_orphan", "j_on"}})
	if err != nil || res.Succeeded != 1 || res.Failed != 1 || len(res.Skipped) != 1 || res.Skipped[0].Reason != operations.SkipAlreadyEnabled {
		t.Fatalf("enable = %+v, %v", res, err)
	}
	if r := res.Results[1]; r.ID != "j_orphan" || r.OK || !strings.Contains(r.Error, "connection") {
		t.Fatalf("orphan result = %+v", r)
	}
	if j, _ := env.st.GetJob(ctx, "j_good"); !j.Enabled {
		t.Fatal("j_good not enabled")
	}
	if s := env.pub.bulk(); len(s) != 2 || s[1].Bulk.Failed != 1 {
		t.Fatalf("summaries = %+v", s)
	}

	// Disabling always works; deleting calls the hook.
	res, err = env.svc.Bulk(ctx, operations.BulkJobs, operations.BulkRequest{Action: "disable", Filter: &operations.BulkFilter{}})
	if err != nil || res.Succeeded != 2 || len(res.Skipped) != 1 {
		t.Fatalf("disable all = %+v, %v", res, err)
	}
	res, err = env.svc.Bulk(ctx, operations.BulkJobs, operations.BulkRequest{Action: "delete", Filter: &operations.BulkFilter{Q: "o"}})
	if err != nil || res.Succeeded != 3 {
		t.Fatalf("delete jobs = %+v, %v", res, err)
	}
	if len(env.deleted) != 3 {
		t.Fatalf("job deleted hook calls = %v", env.deleted)
	}
}

func TestBulkDeleteRestores(t *testing.T) {
	env := newBulkEnv(t)
	ctx := admin()
	now := time.Now().UTC()
	for _, r := range []*models.RestoreRecord{
		{ID: "r_done", BackupID: "b", TargetDatabase: "shop_rescue", Status: models.RestoreStatusCompleted, StartedAt: now},
		{ID: "r_fail", BackupID: "b", TargetDatabase: "shop_rescue", Status: models.RestoreStatusFailed, StartedAt: now},
		{ID: "r_run", BackupID: "b", TargetDatabase: "shop_rescue", Status: models.RestoreStatusInProgress, StartedAt: now},
	} {
		if err := env.st.SaveRestoreRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	res, err := env.svc.Bulk(ctx, operations.BulkRestores, operations.BulkRequest{Action: "delete", Filter: &operations.BulkFilter{Database: "shop_rescue"}})
	if err != nil || res.Succeeded != 2 || len(res.Skipped) != 1 || res.Skipped[0].Reason != operations.SkipInProgress {
		t.Fatalf("delete restores = %+v, %v", res, err)
	}
	list, _ := env.st.ListRestoreRecords(ctx)
	if len(list) != 1 || list[0].ID != "r_run" {
		t.Fatalf("remaining restores = %+v", list)
	}
}

func TestBulkStopsWhenTheRequestEnds(t *testing.T) {
	env := newBulkEnv(t)
	ctx, cancel := context.WithCancel(admin())
	defer cancel()
	now := time.Now().UTC()
	env.backupAt(t, "c1", "", models.StatusCompleted, now, "", 0)
	env.backupAt(t, "c2", "", models.StatusCompleted, now, "", 0)
	if err := env.st.SaveBackupRecord(ctx, &models.BackupRecord{ID: "c0", Database: "shop", Status: models.StatusCompleted,
		StartedAt: now, StorageKey: "k", StorageTargetID: "tgt_cancel"}); err != nil {
		t.Fatal(err)
	}
	// The first item's storage lookup ends the request; that item is still finished.
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	svc := operations.New(operations.Config{
		Store: env.st, Backup: backup.NewEngine(env.mock, ""), Restore: restore.NewEngine(env.mock, ""), Runs: manager,
		Storage: func(context.Context, string) (storage.Storage, error) {
			cancel()
			return nil, errors.New("gone")
		},
	})
	res, err := svc.Bulk(ctx, operations.BulkBackups, operations.BulkRequest{Action: "delete", IDs: []string{"c0", "c1", "c2"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Succeeded != 1 || res.Failed != 2 || res.Results[0].Warning == "" || !strings.Contains(res.Results[2].Error, "not processed") {
		t.Fatalf("cancelled run = %+v", res)
	}
	if env.hasBackup("c0") {
		t.Fatal("the started item was left half done")
	}
	if !env.hasBackup("c1") || !env.hasBackup("c2") {
		t.Fatal("items after the end of the request were processed")
	}
}

// blockingRunner runs jobs until release is closed.
type blockingRunner struct {
	st      store.Store
	release chan struct{}
}

func (r blockingRunner) PrepareJobRun(ctx context.Context, jobID string, trigger models.BackupTrigger) (*models.Job, *models.BackupRecord, error) {
	job, err := r.st.GetJob(ctx, jobID)
	if err != nil {
		return nil, nil, err
	}
	return job, &models.BackupRecord{ID: "run_" + jobID, JobID: jobID, Database: job.Database, ConnectionID: job.ConnectionID,
		Status: models.StatusInProgress, Trigger: trigger, StartedAt: time.Now().UTC()}, nil
}

func (r blockingRunner) ExecuteJobRun(ctx context.Context, _ *models.Job, record *models.BackupRecord) (*models.BackupRecord, error) {
	select {
	case <-r.release:
	case <-ctx.Done():
	}
	return record, nil
}

func TestBulkRunNow(t *testing.T) {
	st := storetest.New(t)
	mock := storage.NewMockStorage()
	manager := runs.NewManager(nil)
	runner := blockingRunner{st: st, release: make(chan struct{})}
	t.Cleanup(func() {
		close(runner.release)
		_ = manager.Shutdown(context.Background())
	})
	svc := operations.New(operations.Config{Store: st, Backup: backup.NewEngine(mock, ""), Restore: restore.NewEngine(mock, ""), Runs: manager, Jobs: runner})
	ctx := operator()
	for _, j := range []*models.Job{
		{ID: "j1", Name: "one", Database: "shop", ConnectionID: "c", CronExpression: "@daily"},
		{ID: "j2", Name: "two", Database: "shop", ConnectionID: "c", CronExpression: "@daily"},
		{ID: "j3", Name: "three", Database: "crm", ConnectionID: "c", CronExpression: "@daily"},
	} {
		if err := st.SaveJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	res, err := svc.Bulk(ctx, operations.BulkJobs, operations.BulkRequest{Action: "run_now", IDs: []string{"j1", "j2", "j3", "nope"}})
	if err != nil {
		t.Fatal(err)
	}
	// j2 backs up the database j1 is already backing up: it fails alone.
	if res.Succeeded != 2 || res.Failed != 1 || len(res.Skipped) != 1 || res.Results[0].Detail != "run_j1" ||
		res.Results[1].OK || !strings.Contains(res.Results[1].Error, "already running") {
		t.Fatalf("run_now = %+v", res)
	}
}

func TestBulkTrustProtections(t *testing.T) {
	env := newBulkEnv(t)
	ctx := admin()
	now := time.Now().UTC()
	if err := env.st.SaveJob(ctx, &models.Job{ID: "job_v", Name: "Verified", Database: "shop", CronExpression: "@daily"}); err != nil {
		t.Fatal(err)
	}
	save := func(rec *models.BackupRecord) {
		t.Helper()
		rec.Database = "shop"
		if err := env.st.SaveBackupRecord(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	save(&models.BackupRecord{ID: "v_old", JobID: "job_v", Status: models.StatusCompleted, Trigger: models.TriggerScheduled, StartedAt: now.Add(-72 * time.Hour)})
	save(&models.BackupRecord{ID: "v_verified", JobID: "job_v", Status: models.StatusCompleted, Trigger: models.TriggerScheduled,
		StartedAt: now.Add(-48 * time.Hour), Verification: models.VerificationOK})
	save(&models.BackupRecord{ID: "v_last", JobID: "job_v", Status: models.StatusCompleted, Trigger: models.TriggerScheduled, StartedAt: now.Add(-24 * time.Hour)})
	save(&models.BackupRecord{ID: "pinned", Status: models.StatusCompleted, StartedAt: now, Pinned: true})

	ids := []string{"v_old", "v_verified", "v_last", "pinned"}
	dry, err := env.svc.Bulk(ctx, operations.BulkBackups, operations.BulkRequest{Action: "delete", IDs: ids, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, sk := range dry.Skipped {
		reasons[sk.ID] = sk.Reason
	}
	want := map[string]string{"v_verified": operations.SkipLastVerified, "v_last": operations.SkipLastGoodBackup, "pinned": operations.SkipPinned}
	if !reflect.DeepEqual(reasons, want) || dry.Actionable != 1 {
		t.Fatalf("skips = %v (actionable %d); want %v", reasons, dry.Actionable, want)
	}

	// A pin set after the plan still protects the backup.
	if _, err = env.svc.PinBackup(ctx, "v_old", "audit"); err != nil {
		t.Fatal(err)
	}
	res, err := env.svc.Bulk(ctx, operations.BulkBackups, operations.BulkRequest{Action: "delete", IDs: []string{"v_old"}, ConfirmCount: intPtr(1)})
	if !errors.Is(err, operations.ErrBulkConfirm) {
		t.Fatalf("stale count after a pin: %+v, %v; want ErrBulkConfirm", res, err)
	}
	if _, err = env.svc.DeleteBackup(ctx, "v_old"); !errors.Is(err, operations.ErrPinned) {
		t.Fatalf("single delete of a pinned backup: %v; want ErrPinned", err)
	}

	// Pin and unpin in bulk; the note only goes with pin.
	if _, err = env.svc.Bulk(ctx, operations.BulkBackups, operations.BulkRequest{Action: "unpin", IDs: ids, Note: "x"}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("note on unpin: %v; want ErrInvalid", err)
	}
	res, err = env.svc.Bulk(ctx, operations.BulkBackups, operations.BulkRequest{Action: "pin", IDs: ids, Note: "legal hold"})
	if err != nil || res.Succeeded != 2 || len(res.Skipped) != 2 {
		t.Fatalf("pin = %+v, %v", res, err)
	}
	if b, _ := env.st.GetBackupRecord(ctx, "v_last"); !b.Pinned || b.PinNote != "legal hold" || b.PinnedBy == "" {
		t.Fatalf("pinned record = %+v", b)
	}
	if _, err = env.svc.Bulk(operator(), operations.BulkBackups, operations.BulkRequest{Action: "unpin", IDs: ids}); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("operator unpin: %v; want ErrForbidden", err)
	}
	res, err = env.svc.Bulk(ctx, operations.BulkBackups, operations.BulkRequest{Action: "unpin", IDs: ids})
	if err != nil || res.Succeeded != 4 {
		t.Fatalf("unpin = %+v, %v", res, err)
	}

	// Cancel skips what is not running.
	res, err = env.svc.Bulk(operator(), operations.BulkBackups, operations.BulkRequest{Action: "cancel", IDs: ids, DryRun: true})
	if err != nil || res.Actionable != 0 || len(res.Skipped) != 4 || res.Skipped[0].Reason != operations.SkipNotRunning {
		t.Fatalf("cancel dry run = %+v, %v", res, err)
	}
}
