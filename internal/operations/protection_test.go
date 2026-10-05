package operations_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// protEnv is an operations service with live settings, a clock and a count of
// administrators, for the delete protection.
type protEnv struct {
	svc      *operations.Service
	st       *store.SQLiteStore
	mock     *storage.MockStorage
	settings *settings.Service
	targets  *targets.Service
	clock    *fakeClock
	pub      *recordingPublisher
	admins   int
}

func newProtEnv(t *testing.T) *protEnv {
	t.Helper()
	env := &protEnv{st: storetest.New(t), mock: storage.NewMockStorage(), pub: &recordingPublisher{}, admins: 2,
		clock: &fakeClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}}
	var err error
	if env.settings, err = settings.NewService(context.Background(), env.st, settings.WithClock(env.clock.Now)); err != nil {
		t.Fatal(err)
	}
	env.targets = targets.NewService(env.st, func(context.Context, *models.StorageTarget, string) (storage.Storage, error) { return env.mock, nil }, t.TempDir())
	if _, err = env.targets.Create(context.Background(), targets.Input{Name: "Local", Type: models.StorageLocal,
		Local: &models.LocalTarget{Path: t.TempDir()}, IsDefault: true}); err != nil {
		t.Fatal(err)
	}
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	bRunner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
	}
	env.svc = operations.New(operations.Config{
		Store:           env.st,
		Backup:          backup.NewEngine(env.mock, "", backup.WithRunner(bRunner)),
		Restore:         restore.NewEngine(env.mock, ""),
		Runs:            manager,
		Connections:     oneConnection{},
		Targets:         env.targets,
		Publisher:       env.pub,
		Storage:         func(context.Context, string) (storage.Storage, error) { return env.mock, nil },
		Settings:        env.settings.Current,
		SettingsUpdater: env.settings,
		SecondApproverCheck: func(context.Context) error {
			if env.admins < auth.MinApprovalAdmins {
				return auth.ErrTooFewAdmins
			}
			return nil
		},
	})
	operations.SetNow(env.svc, env.clock.Now)
	return env
}

// backup stores a completed backup with an archive.
func (env *protEnv) backup(t *testing.T, id, jobID string, age time.Duration) {
	t.Helper()
	rec := &models.BackupRecord{ID: id, JobID: jobID, Database: "shop", Status: models.StatusCompleted, Trigger: models.TriggerScheduled,
		StartedAt: env.clock.Now().Add(-age), StorageKey: "shop/" + id, ConnectionID: "conn_ok"}
	if err := env.st.SaveBackupRecord(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if _, err := env.mock.Save(context.Background(), rec.StorageKey, strings.NewReader("data")); err != nil {
		t.Fatal(err)
	}
}

func (env *protEnv) status(t *testing.T, id string) models.BackupStatus {
	t.Helper()
	rec, err := env.st.GetBackupRecord(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return rec.Status
}

// enableTwoPerson turns the two-person rule on.
func (env *protEnv) enableTwoPerson(t *testing.T) {
	t.Helper()
	on := true
	if _, err := env.svc.UpdateSettings(asUser("alice", auth.ScopeAdmin), settings.Patch{Security: &settings.SecurityPatch{RequireSecondApprover: &on}}); err != nil {
		t.Fatal(err)
	}
}

// pendingApproval returns the approval request err carries.
func pendingApproval(t *testing.T, err error) *models.Approval {
	t.Helper()
	var pending *operations.ApprovalPendingError
	if !errors.As(err, &pending) || !errors.Is(err, operations.ErrApprovalRequired) {
		t.Fatalf("err = %v; want an approval request", err)
	}
	return pending.Approval
}

// keyOf is an admin API key created by user name.
func keyOf(name string) context.Context {
	return auth.WithPrincipal(context.Background(), &auth.Principal{Method: auth.MethodAPIKey, APIKeyID: "key_" + name, APIKeyName: name + "-key",
		User: &auth.User{ID: "usr_" + name, Username: name, Role: auth.RoleAdmin}, Scope: auth.ScopeAdmin})
}

func TestTwoPersonRuleNeedsTwoAdmins(t *testing.T) {
	env := newProtEnv(t)
	env.admins = 1
	on := true
	_, err := env.svc.UpdateSettings(asUser("alice", auth.ScopeAdmin), settings.Patch{Security: &settings.SecurityPatch{RequireSecondApprover: &on}})
	if !errors.Is(err, operations.ErrTooFewAdmins) {
		t.Fatalf("enable with one admin = %v; want ErrTooFewAdmins", err)
	}
	if env.settings.Current().Security.RequireSecondApprover {
		t.Fatal("the two-person rule was turned on with one administrator")
	}
	// Lowering it directly, bypassing the operations service, is refused too.
	env.admins = 2
	env.enableTwoPerson(t)
	off := false
	if _, err = env.settings.Update(context.Background(), settings.Patch{Security: &settings.SecurityPatch{RequireSecondApprover: &off}}); !errors.Is(err, settings.ErrProtectionLowered) {
		t.Fatalf("direct disable = %v; want ErrProtectionLowered", err)
	}
}

// TestTwoPersonRuleBlocksASingleAdmin proves that with the rule on, a stolen admin
// key (or one admin) cannot finish a destructive action: the request waits, the
// requester cannot approve it, an API key never can, and only another signed-in
// administrator can.
func TestTwoPersonRuleBlocksASingleAdmin(t *testing.T) {
	env := newProtEnv(t)
	env.enableTwoPerson(t)
	env.backup(t, "b1", "", time.Hour)

	a := pendingApproval(t, func() error { _, err := env.svc.DeleteBackup(keyOf("alice"), "b1", "ransom"); return err }())
	if env.status(t, "b1") != models.StatusCompleted || a.Action != models.ApprovalDeleteBackup || a.RequestedByUserID != "usr_alice" ||
		a.RequestedVia != string(auth.MethodAPIKey) || !a.ExpiresAt.Equal(env.clock.Now().Add(models.ApprovalTTL)) {
		t.Fatalf("approval = %+v; the backup must stay", a)
	}
	requested := false
	for _, e := range env.pub.events {
		requested = requested || (e.Type == events.SecurityApprovalRequested && e.ApprovalID == a.ID)
	}
	if !requested {
		t.Fatal("no security.approval_requested event")
	}

	if _, err := env.svc.Approve(asUser("alice", auth.ScopeAdmin), a.ID); !errors.Is(err, operations.ErrSelfApproval) {
		t.Fatalf("self-approval = %v; want ErrSelfApproval", err)
	}
	if _, err := env.svc.Approve(keyOf("bob"), a.ID); !errors.Is(err, operations.ErrApprovalNeedsSession) {
		t.Fatalf("approval by an API key = %v; want ErrApprovalNeedsSession", err)
	}
	if _, err := env.svc.Approve(asUser("carol", auth.ScopeOperator), a.ID); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("approval by an operator = %v; want ErrForbidden", err)
	}
	if env.status(t, "b1") != models.StatusCompleted {
		t.Fatal("a refused approval deleted the backup")
	}

	done, err := env.svc.Approve(asUser("bob", auth.ScopeAdmin), a.ID)
	if err != nil || done.Status != models.ApprovalApproved || done.DecidedByUserID != "usr_bob" || !strings.Contains(done.Result, "recoverable until") {
		t.Fatalf("approval = %+v, %v", done, err)
	}
	rec, _ := env.st.GetBackupRecord(context.Background(), "b1")
	if rec.Status != models.StatusDeleted || rec.DeletedBy != "alice" || rec.DeleteApprovedBy != "bob" || rec.DeleteReason != "ransom" {
		t.Fatalf("approved deletion = %+v", rec)
	}
	if _, err = env.mock.Stat(context.Background(), "shop/b1"); err != nil {
		t.Fatalf("an approved delete is soft too: %v", err)
	}
	if _, err = env.svc.Approve(asUser("dave", auth.ScopeAdmin), a.ID); !errors.Is(err, operations.ErrApprovalClosed) {
		t.Fatalf("second approval = %v; want ErrApprovalClosed", err)
	}
}

func TestApprovalsExpire(t *testing.T) {
	env := newProtEnv(t)
	env.enableTwoPerson(t)
	env.backup(t, "b1", "", time.Hour)
	_, err := env.svc.DeleteBackup(asUser("alice", auth.ScopeAdmin), "b1", "")
	a := pendingApproval(t, err)
	env.clock.Advance(models.ApprovalTTL)
	if _, err = env.svc.Approve(asUser("bob", auth.ScopeAdmin), a.ID); !errors.Is(err, operations.ErrApprovalClosed) {
		t.Fatalf("approval after expiry = %v; want ErrApprovalClosed", err)
	}
	got, err := env.svc.GetApproval(asUser("bob", auth.ScopeAdmin), a.ID)
	if err != nil || got.Status != models.ApprovalExpired {
		t.Fatalf("expired request = %+v, %v", got, err)
	}
	if env.status(t, "b1") != models.StatusCompleted {
		t.Fatal("an expired request deleted the backup")
	}
	list, err := env.svc.ListApprovals(asUser("bob", auth.ScopeAdmin), models.ApprovalPending)
	if err != nil || len(list) != 0 {
		t.Fatalf("pending = %+v, %v; want none", list, err)
	}
}

func TestRejectedApprovalsDoNothing(t *testing.T) {
	env := newProtEnv(t)
	env.enableTwoPerson(t)
	env.backup(t, "b1", "", time.Hour)
	_, err := env.svc.DeleteBackup(asUser("alice", auth.ScopeAdmin), "b1", "")
	a := pendingApproval(t, err)
	got, err := env.svc.Reject(keyOf("bob"), a.ID, "not now")
	if err != nil || got.Status != models.ApprovalRejected {
		t.Fatalf("reject = %+v, %v", got, err)
	}
	if _, err = env.svc.Approve(asUser("bob", auth.ScopeAdmin), a.ID); !errors.Is(err, operations.ErrApprovalClosed) {
		t.Fatalf("approve after reject = %v; want ErrApprovalClosed", err)
	}
	if env.status(t, "b1") != models.StatusCompleted {
		t.Fatal("a rejected request deleted the backup")
	}
}

// TestTargetDeletionIsProtected proves that a storage target holding any backup
// record but purged ones (a deleted backup waiting for its purge included) cannot be
// deleted, that an unused one goes at once, and that the two-person rule covers it.
func TestTargetDeletionIsProtected(t *testing.T) {
	env := newProtEnv(t)
	ctx := keyOf("alice")
	mk := func(name string, def bool) *models.StorageTarget {
		t.Helper()
		tg, err := env.targets.Create(ctx, targets.Input{Name: name, Type: models.StorageLocal, Local: &models.LocalTarget{Path: t.TempDir()}, IsDefault: def})
		if err != nil {
			t.Fatal(err)
		}
		return tg
	}
	mk("default", true)
	used, spare, other := mk("used", false), mk("spare", false), mk("other", false)
	rec := &models.BackupRecord{ID: "b1", Database: "shop", Status: models.StatusCompleted, StorageTargetID: used.ID, StorageKey: "shop/b1", StartedAt: env.clock.Now()}
	if err := env.st.SaveBackupRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.DeleteBackup(ctx, "b1", ""); err != nil {
		t.Fatal(err)
	}
	if err := env.svc.DeleteTarget(ctx, used.ID); !errors.Is(err, targets.ErrInUse) {
		t.Fatalf("delete of a target with a deleted backup = %v; want ErrInUse", err)
	}
	if err := env.svc.DeleteTarget(ctx, spare.ID); err != nil {
		t.Fatalf("delete of an unused target = %v", err)
	}
	env.enableTwoPerson(t)
	a := pendingApproval(t, env.svc.DeleteTarget(ctx, other.ID))
	if _, err := env.targets.Get(ctx, other.ID); err != nil {
		t.Fatalf("target deleted without approval: %v", err)
	}
	if _, err := env.svc.Approve(asUser("bob", auth.ScopeAdmin), a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.targets.Get(ctx, other.ID); !errors.Is(err, targets.ErrNotFound) {
		t.Fatalf("approved target delete = %v; want gone", err)
	}
}

func TestTwoPersonRuleCoversBulkUnpinAndTurningItOff(t *testing.T) {
	env := newProtEnv(t)
	env.enableTwoPerson(t)
	alice, bob := asUser("alice", auth.ScopeAdmin), asUser("bob", auth.ScopeAdmin)
	for i, id := range []string{"b1", "b2", "b3"} {
		env.backup(t, id, "", time.Duration(i+1)*time.Hour)
	}

	_, err := env.svc.Bulk(alice, operations.BulkBackups, operations.BulkRequest{Action: "delete", IDs: []string{"b1", "b2"}})
	bulk := pendingApproval(t, err)
	if bulk.Action != models.ApprovalBulkDeleteBackups || len(bulk.IDs) != 2 || env.status(t, "b1") != models.StatusCompleted {
		t.Fatalf("bulk request = %+v", bulk)
	}
	if dry, dryErr := env.svc.Bulk(alice, operations.BulkBackups, operations.BulkRequest{Action: "delete", IDs: []string{"b1"}, DryRun: true}); dryErr != nil || dry.Actionable != 1 {
		t.Fatalf("a dry run needs no approval: %+v, %v", dry, dryErr)
	}
	if done, approveErr := env.svc.Approve(bob, bulk.ID); approveErr != nil || done.Status != models.ApprovalApproved {
		t.Fatalf("bulk approval = %+v, %v", done, approveErr)
	}
	if env.status(t, "b1") != models.StatusDeleted || env.status(t, "b2") != models.StatusDeleted {
		t.Fatal("approved bulk delete did not delete")
	}

	if _, err = env.svc.PinBackup(alice, "b3", "hold"); err != nil {
		t.Fatal(err)
	}
	_, err = env.svc.UnpinBackup(alice, "b3")
	unpin := pendingApproval(t, err)
	if rec, _ := env.st.GetBackupRecord(context.Background(), "b3"); !rec.Pinned {
		t.Fatal("unpinned without approval")
	}
	if _, err = env.svc.Approve(bob, unpin.ID); err != nil {
		t.Fatal(err)
	}
	if rec, _ := env.st.GetBackupRecord(context.Background(), "b3"); rec.Pinned {
		t.Fatal("approved unpin did not unpin")
	}

	off := false
	res, err := env.svc.UpdateSettings(alice, settings.Patch{Security: &settings.SecurityPatch{RequireSecondApprover: &off}})
	if err != nil || len(res.Approvals) != 1 || !env.settings.Current().Security.RequireSecondApprover {
		t.Fatalf("disable = %+v, %v; want an approval request and the rule still on", res, err)
	}
	if _, err = env.svc.Approve(bob, res.Approvals[0].ID); err != nil {
		t.Fatal(err)
	}
	if env.settings.Current().Security.RequireSecondApprover {
		t.Fatal("approved disable left the rule on")
	}
}

// TestRetentionShorteningIsDelayed proves that a shorter retention takes effect only
// after the grace period, while a longer one applies at once and cancels it.
func TestRetentionShorteningIsDelayed(t *testing.T) {
	env := newProtEnv(t)
	ctx := keyOf("alice")
	job := &models.Job{ID: "job_r", Name: "r", Database: "shop", ConnectionID: "conn_ok", CronExpression: "@daily", RetentionDays: 30, RetentionCount: 10}
	if err := env.st.SaveJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	one, two := 1, 2
	update := func(days, count *int) *operations.JobSaveResult {
		t.Helper()
		res, err := env.svc.UpdateJob(ctx, "job_r", operations.JobUpdate{Name: "r", Database: "shop", ConnectionID: "conn_ok",
			CronExpression: "@daily", RetentionDays: days, RetentionCount: count})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := update(&one, &two)
	if res.RetentionDays != 30 || res.RetentionCount != 10 || res.PendingRetention == nil ||
		!res.PendingRetention.EffectiveAt.Equal(env.clock.Now().Add(models.GraceDuration(models.DefaultDeleteGraceDays))) {
		t.Fatalf("update = %+v / %+v; want the retention kept and a pending change", res.Job, res.PendingRetention)
	}
	stored, _ := env.st.GetJob(ctx, "job_r")
	if stored.RetentionDays != 30 || stored.RetentionCount != 10 {
		t.Fatalf("stored retention = %d/%d; want 30/10 during the grace period", stored.RetentionDays, stored.RetentionCount)
	}
	details, err := env.svc.GetJobDetails(ctx, "job_r")
	if err != nil || details.PendingRetention == nil {
		t.Fatalf("details = %+v, %v; want the pending change", details, err)
	}
	env.clock.Advance(6 * 24 * time.Hour)
	env.svc.ApplyDueChanges(context.Background())
	if stored, _ = env.st.GetJob(ctx, "job_r"); stored.RetentionDays != 30 {
		t.Fatal("the shortening applied before the grace period")
	}
	env.clock.Advance(24 * time.Hour)
	env.svc.ApplyDueChanges(context.Background())
	if stored, _ = env.st.GetJob(ctx, "job_r"); stored.RetentionDays != 1 || stored.RetentionCount != 2 {
		t.Fatalf("stored retention = %d/%d; want 1/2 after the grace period", stored.RetentionDays, stored.RetentionCount)
	}

	// Lengthening applies at once and drops a pending shortening.
	five := 5
	if res = update(&five, &two); res.RetentionDays != 5 || res.PendingRetention != nil {
		t.Fatalf("lengthen = %+v / %+v", res.Job, res.PendingRetention)
	}
	if res = update(&one, &two); res.PendingRetention == nil {
		t.Fatal("no pending change")
	}
	forever := 0
	if res = update(&forever, &two); res.RetentionDays != 0 || res.PendingRetention != nil {
		t.Fatalf("keep forever = %+v / %+v; want applied and the pending change gone", res.Job, res.PendingRetention)
	}
	list, _ := env.svc.PendingChanges(ctx)
	if len(list) != 0 {
		t.Fatalf("pending = %+v", list)
	}
}

// TestNewJobCannotShortenTheRetentionOfAnOldID proves that recreating a deleted job
// under its ID counts as keeping its backups forever.
func TestNewJobCannotShortenTheRetentionOfAnOldID(t *testing.T) {
	env := newProtEnv(t)
	env.backup(t, "b_old", "job_gone", 48*time.Hour)
	job := &models.Job{ID: "job_gone", RetentionDays: 1, RetentionCount: 1}
	hold, err := env.svc.HoldRetention(context.Background(), nil, job)
	if err != nil || hold == nil || job.RetentionDays != 0 || job.RetentionCount != 0 {
		t.Fatalf("hold = %+v, %v, job %d/%d; want the shortening held", hold, err, job.RetentionDays, job.RetentionCount)
	}
	fresh := &models.Job{ID: "job_new", RetentionDays: 1}
	if hold, err = env.svc.HoldRetention(context.Background(), nil, fresh); err != nil || hold != nil || fresh.RetentionDays != 1 {
		t.Fatalf("new job hold = %+v, %v", hold, err)
	}
}

// TestLoweringTheGracePeriodIsDelayed proves that a shorter grace period takes effect
// only after the current one, and a longer one at once.
func TestLoweringTheGracePeriodIsDelayed(t *testing.T) {
	env := newProtEnv(t)
	ctx := keyOf("alice")
	one := 1
	res, err := env.svc.UpdateSettings(ctx, settings.Patch{Security: &settings.SecurityPatch{DeleteGraceDays: &one}})
	if err != nil || len(res.Pending) != 1 || env.settings.Current().Security.DeleteGraceDays != models.DefaultDeleteGraceDays {
		t.Fatalf("lower = %+v, %v; want a pending change and the grace period kept", res, err)
	}
	bad := 0
	if _, err = env.svc.UpdateSettings(ctx, settings.Patch{Security: &settings.SecurityPatch{DeleteGraceDays: &bad}}); !errors.Is(err, settings.ErrInvalid) {
		t.Fatalf("grace 0 = %v; want ErrInvalid", err)
	}
	env.clock.Advance(models.GraceDuration(models.DefaultDeleteGraceDays) - time.Minute)
	env.svc.ApplyDueChanges(context.Background())
	if got := env.settings.Current().Security.DeleteGraceDays; got != models.DefaultDeleteGraceDays {
		t.Fatalf("grace = %d before the current period ended", got)
	}
	env.clock.Advance(time.Minute)
	env.svc.ApplyDueChanges(context.Background())
	if got := env.settings.Current().Security.DeleteGraceDays; got != 1 {
		t.Fatalf("grace = %d; want 1 after the current period", got)
	}
	// Raising applies at once and cancels a pending lowering.
	thirty, ten := 30, 10
	if _, err = env.svc.UpdateSettings(ctx, settings.Patch{Security: &settings.SecurityPatch{DeleteGraceDays: &ten}}); err != nil {
		t.Fatal(err)
	}
	if _, err = env.svc.UpdateSettings(ctx, settings.Patch{Security: &settings.SecurityPatch{DeleteGraceDays: &one}}); err != nil {
		t.Fatal(err)
	}
	if _, err = env.svc.UpdateSettings(ctx, settings.Patch{Security: &settings.SecurityPatch{DeleteGraceDays: &thirty}}); err != nil {
		t.Fatal(err)
	}
	if list, _ := env.svc.PendingChanges(ctx); len(list) != 0 || env.settings.Current().Security.DeleteGraceDays != 30 {
		t.Fatalf("pending = %+v, grace %d; want 30 at once and nothing pending", list, env.settings.Current().Security.DeleteGraceDays)
	}
	// A delete now uses the raised grace period.
	env.backup(t, "b1", "", time.Hour)
	del, err := env.svc.DeleteBackup(ctx, "b1", "")
	if err != nil || del.PurgeAfter.Sub(del.DeletedAt) != 30*24*time.Hour {
		t.Fatalf("delete = %+v, %v; want 30 days of grace", del, err)
	}
}

func TestDeletedBackupsCannotBeRestoredOrPinned(t *testing.T) {
	env := newProtEnv(t)
	ctx := keyOf("alice")
	env.backup(t, "b1", "", time.Hour)
	if _, err := env.svc.DeleteBackup(ctx, "b1", ""); err != nil {
		t.Fatal(err)
	}
	req := models.RestoreRequest{BackupID: "b1", TargetConnectionID: "conn_ok"}
	if _, err := env.svc.StartRestore(ctx, req); !errors.Is(err, operations.ErrBackupDeleted) {
		t.Fatalf("restore of a deleted backup = %v; want ErrBackupDeleted", err)
	}
	if _, err := env.svc.PreflightRestore(ctx, req); !errors.Is(err, operations.ErrBackupDeleted) {
		t.Fatalf("preflight of a deleted backup = %v; want ErrBackupDeleted", err)
	}
	if _, err := env.svc.PinBackup(ctx, "b1", ""); !errors.Is(err, operations.ErrBackupDeleted) {
		t.Fatalf("pin of a deleted backup = %v; want ErrBackupDeleted", err)
	}
	if _, err := env.svc.UndeleteBackup(ctx, "b1"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.PinBackup(ctx, "b1", ""); err != nil {
		t.Fatalf("pin after undelete = %v", err)
	}
}

func TestRunningBackupsCannotBeDeleted(t *testing.T) {
	env := newProtEnv(t)
	if err := env.st.SaveBackupRecord(context.Background(), &models.BackupRecord{ID: "run", Database: "shop", Status: models.StatusInProgress, StartedAt: env.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.DeleteBackup(keyOf("alice"), "run", ""); !errors.Is(err, operations.ErrBackupRunning) {
		t.Fatalf("delete of a running backup = %v; want ErrBackupRunning", err)
	}
}
