package operations_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

// TestRequireLockedCopiesRefusesUnlockedCopyTargets proves that a job requiring
// locked copies is refused (ErrInvalid, ErrUnlockedCopyTarget) while a copy target
// has no Object Lock, and accepted with a locked one.
func TestRequireLockedCopiesRefusesUnlockedCopyTargets(t *testing.T) {
	env, locked := newLockTargetEnv(t, models.ObjectLockCompliance, 30)
	ctx := context.Background()
	in := lockTargetInput("", 0, false)
	in.Name, in.S3.Bucket, in.S3.SecretAccessKey = "plain", "plain", "secret"
	plain, err := env.targets.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	job := &models.Job{Name: "j", Database: "shop", ConnectionID: "conn_ok", CopyTargets: []string{plain.ID}, RequireLockedCopies: true}
	err = env.svc.ValidateJob(ctx, job)
	if !errors.Is(err, operations.ErrInvalid) || !errors.Is(err, models.ErrUnlockedCopyTarget) {
		t.Fatalf("unlocked copy target = %v; want ErrInvalid wrapping ErrUnlockedCopyTarget", err)
	}
	job.CopyTargets = []string{locked}
	if err = env.svc.ValidateJob(ctx, job); err != nil {
		t.Fatalf("locked copy target = %v", err)
	}
	job.CopyTargets, job.RequireLockedCopies = []string{plain.ID}, false
	if err = env.svc.ValidateJob(ctx, job); err != nil {
		t.Fatalf("unlocked copy target without the policy = %v", err)
	}
	// With security.require_locked_copies on the policy applies to every job.
	on := true
	if _, err = env.svc.UpdateSettings(asUser("alice", auth.ScopeAdmin), settings.Patch{Security: &settings.SecurityPatch{RequireLockedCopies: &on}}); err != nil {
		t.Fatal(err)
	}
	if err = env.svc.ValidateJob(ctx, job); !errors.Is(err, models.ErrUnlockedCopyTarget) {
		t.Fatalf("unlocked copy target under the setting = %v; want ErrUnlockedCopyTarget", err)
	}
}

// saveLockedJob stores job_l requiring locked copies, created an hour ago.
func saveLockedJob(t *testing.T, env *protEnv) *models.Job {
	t.Helper()
	job := &models.Job{ID: "job_l", Name: "l", Database: "shop", ConnectionID: "conn_ok", CronExpression: "@daily",
		RequireLockedCopies: true, CreatedAt: env.clock.Now().Add(-time.Hour)}
	if err := env.st.SaveJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	return job
}

// lockedUpdate is the update of job_l with require_locked_copies set to v.
func lockedUpdate(v bool) operations.JobUpdate {
	return operations.JobUpdate{Name: "l", Database: "shop", ConnectionID: "conn_ok", CronExpression: "@daily", RequireLockedCopies: &v}
}

// TestNoJobOptsOutWhileTheSettingIsOn proves that with
// security.require_locked_copies on, a new job without the policy and a job turning
// it off are refused.
func TestNoJobOptsOutWhileTheSettingIsOn(t *testing.T) {
	env := newProtEnv(t)
	alice := asUser("alice", auth.ScopeAdmin)
	saveLockedJob(t, env)
	on := true
	if _, err := env.svc.UpdateSettings(alice, settings.Patch{Security: &settings.SecurityPatch{RequireLockedCopies: &on}}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.UpdateJob(alice, "job_l", lockedUpdate(false)); !errors.Is(err, operations.ErrInvalid) || !errors.Is(err, operations.ErrLockedCopiesRequired) {
		t.Fatalf("opting out under the setting = %v; want ErrLockedCopiesRequired", err)
	}
	if _, err := env.svc.HoldLockedCopies(context.Background(), nil, &models.Job{ID: "job_new"}); !errors.Is(err, operations.ErrLockedCopiesRequired) {
		t.Fatalf("a new job without the policy = %v; want ErrLockedCopiesRequired", err)
	}
}

// TestTurningAJobsLockedCopiesOffIsDelayed proves that a job turning
// require_locked_copies off keeps it until the grace period ended.
func TestTurningAJobsLockedCopiesOffIsDelayed(t *testing.T) {
	env := newProtEnv(t)
	alice := asUser("alice", auth.ScopeAdmin)
	saveLockedJob(t, env)
	res, err := env.svc.UpdateJob(alice, "job_l", lockedUpdate(false))
	if err != nil || !res.RequireLockedCopies || res.PendingLockedCopies == nil || res.PendingLockedCopies.Kind != models.PendingJobLockedCopies {
		t.Fatalf("update = %+v, %v; want the policy kept and a pending change", res, err)
	}
	env.clock.Advance(models.GraceDuration(models.DefaultDeleteGraceDays))
	env.svc.ApplyDueChanges(context.Background())
	if job, _ := env.st.GetJob(context.Background(), "job_l"); job.RequireLockedCopies {
		t.Fatal("the job still requires locked copies after the grace period")
	}
}

// TestTurningAJobsLockedCopiesOffNeedsApproval proves that under the two-person
// rule a job turning require_locked_copies off waits for a second administrator,
// then for the grace period.
func TestTurningAJobsLockedCopiesOffNeedsApproval(t *testing.T) {
	env := newAdminEnv(t)
	aliceCtx := env.ctxOf(t, "alice")
	saveLockedJob(t, env.protEnv)
	res, err := env.svc.UpdateJob(aliceCtx, "job_l", lockedUpdate(false))
	if err != nil || res.LockedCopiesApproval == nil || res.LockedCopiesApproval.Action != models.ApprovalJobLockedCopies || !res.RequireLockedCopies {
		t.Fatalf("update = %+v, %v; want an approval request", res, err)
	}
	if _, err = env.svc.Approve(env.ctxOf(t, "bob"), res.LockedCopiesApproval.ID); err != nil {
		t.Fatal(err)
	}
	env.clock.Advance(models.GraceDuration(models.DefaultDeleteGraceDays))
	env.svc.ApplyDueChanges(context.Background())
	if job, _ := env.st.GetJob(context.Background(), "job_l"); job.RequireLockedCopies {
		t.Fatal("the job still requires locked copies after the approval and the grace period")
	}
}

// TestDrillSourceMustBeACopyTarget proves that a restore test may only read from
// one of the job's copy targets.
func TestDrillSourceMustBeACopyTarget(t *testing.T) {
	env, locked := newLockTargetEnv(t, models.ObjectLockCompliance, 30)
	ctx := context.Background()
	job := &models.Job{Name: "j", Database: "shop", ConnectionID: "conn_ok",
		RestoreTest: &models.RestoreTestPolicy{Enabled: true, SourceTargetID: locked}}
	if err := env.svc.ValidateJob(ctx, job); !errors.Is(err, operations.ErrInvalid) || !errors.Is(err, models.ErrInvalidRestoreTest) {
		t.Fatalf("drill from a target that is no copy target = %v; want ErrInvalid", err)
	}
	job.CopyTargets = []string{locked}
	if err := env.svc.ValidateJob(ctx, job); err != nil {
		t.Fatalf("drill from a copy target = %v", err)
	}
}

// TestTurningRequireLockedCopiesOffIsDelayed proves that turning
// security.require_locked_copies on applies at once, while turning it off waits
// for the grace period.
func TestTurningRequireLockedCopiesOffIsDelayed(t *testing.T) {
	env := newProtEnv(t)
	alice := asUser("alice", auth.ScopeAdmin)
	on, off := true, false
	if _, err := env.svc.UpdateSettings(alice, settings.Patch{Security: &settings.SecurityPatch{RequireLockedCopies: &on}}); err != nil {
		t.Fatal(err)
	}
	if !env.settings.Current().Security.RequireLockedCopies {
		t.Fatal("turning it on did not apply at once")
	}
	if _, _, err := env.settings.UpdateChanged(context.Background(), settings.Patch{Security: &settings.SecurityPatch{RequireLockedCopies: &off}}); !errors.Is(err, settings.ErrProtectionLowered) {
		t.Fatalf("direct lowering = %v; want ErrProtectionLowered", err)
	}
	res, err := env.svc.UpdateSettings(alice, settings.Patch{Security: &settings.SecurityPatch{RequireLockedCopies: &off}})
	if err != nil || len(res.Pending) != 1 || res.Pending[0].Kind != models.PendingDisableLockedCopies {
		t.Fatalf("turning it off = %+v, %v; want a pending change", res, err)
	}
	env.svc.ApplyDueChanges(context.Background())
	if !env.settings.Current().Security.RequireLockedCopies {
		t.Fatal("turned off before the grace period")
	}
	env.clock.Advance(models.GraceDuration(models.DefaultDeleteGraceDays))
	env.svc.ApplyDueChanges(context.Background())
	if env.settings.Current().Security.RequireLockedCopies {
		t.Fatal("still on after the grace period")
	}
}

// TestTurningRequireLockedCopiesOffNeedsApproval proves that under the two-person
// rule turning security.require_locked_copies off waits for a second
// administrator, then for the grace period.
func TestTurningRequireLockedCopiesOffNeedsApproval(t *testing.T) {
	env := newAdminEnv(t)
	aliceCtx := env.ctxOf(t, "alice")
	on, off := true, false
	if _, err := env.svc.UpdateSettings(aliceCtx, settings.Patch{Security: &settings.SecurityPatch{RequireLockedCopies: &on}}); err != nil {
		t.Fatal(err)
	}
	res, err := env.svc.UpdateSettings(aliceCtx, settings.Patch{Security: &settings.SecurityPatch{RequireLockedCopies: &off}})
	if err != nil || len(res.Approvals) != 1 || res.Approvals[0].Action != models.ApprovalDisableLockedCopies {
		t.Fatalf("turning it off = %+v, %v; want an approval request", res, err)
	}
	if _, err = env.svc.Approve(env.ctxOf(t, "bob"), res.Approvals[0].ID); err != nil {
		t.Fatal(err)
	}
	if !env.settings.Current().Security.RequireLockedCopies {
		t.Fatal("turned off before the grace period")
	}
	env.clock.Advance(models.GraceDuration(models.DefaultDeleteGraceDays))
	env.svc.ApplyDueChanges(context.Background())
	if env.settings.Current().Security.RequireLockedCopies {
		t.Fatal("still on after the approval and the grace period")
	}
}
