package operations_test

import (
	"context"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

// newLockTargetEnv is a protection environment whose storage targets are served by
// a lock-enabled mock bucket, with one S3 target "vault" locked in mode for days.
func newLockTargetEnv(t *testing.T, mode models.ObjectLockMode, days int) (*protEnv, string) {
	t.Helper()
	env := newProtEnv(t)
	d := &holdDriver{MockStorage: storage.NewMockStorage()}
	env.targets = targets.NewService(env.st, func(context.Context, *models.StorageTarget, string) (storage.Storage, error) { return d, nil },
		t.TempDir(), targets.WithClock(env.clock.Now))
	env.cfg.Targets = env.targets
	env.svc = operations.New(env.cfg)
	operations.SetNow(env.svc, env.clock.Now)
	in := lockTargetInput(mode, days, false)
	in.S3.SecretAccessKey = "secret" // updates send the mask back
	created, err := env.targets.Create(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return env, created.ID
}

func lockTargetInput(mode models.ObjectLockMode, days int, hold bool) targets.Input {
	return targets.Input{Name: "vault", Type: models.StorageS3, S3: &models.S3Target{
		Endpoint: "https://s3.example.com", Bucket: "vault", AccessKeyID: "AKID", SecretAccessKey: models.SecretMask,
		ObjectLock: mode, RetentionDays: days, LegalHoldOnPin: hold,
	}}
}

// lockOf returns the stored object lock of target id.
func lockOf(t *testing.T, env *protEnv, id string) models.ObjectLockSettings {
	t.Helper()
	got, err := env.targets.Resolve(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got.S3.LockSettings()
}

// TestLoweringTheObjectLockWaitsForTheGracePeriod proves that a weaker mode or a
// shorter retention is stored only after the delete grace period, while raising the
// lock applies at once and drops a pending lowering.
func TestLoweringTheObjectLockWaitsForTheGracePeriod(t *testing.T) {
	env, id := newLockTargetEnv(t, models.ObjectLockCompliance, 30)
	alice := asUser("alice", auth.ScopeAdmin)
	compliance30 := models.ObjectLockSettings{Mode: models.ObjectLockCompliance, RetentionDays: 30}

	res, err := env.svc.UpdateTarget(alice, id, lockTargetInput(models.ObjectLockGovernance, 10, false))
	if err != nil {
		t.Fatal(err)
	}
	if res.PendingObjectLock == nil || res.PendingObjectLock.ObjectLock == nil ||
		*res.PendingObjectLock.ObjectLock != (models.ObjectLockSettings{Mode: models.ObjectLockGovernance, RetentionDays: 10}) {
		t.Fatalf("pending = %+v; want governance, 10 days", res.PendingObjectLock)
	}
	if got := lockOf(t, env, id); got != compliance30 {
		t.Fatalf("lock right after the lowering = %v; want compliance, 30 days kept", got)
	}
	env.clock.Advance(models.GraceDuration(models.DefaultDeleteGraceDays) - time.Minute)
	env.svc.ApplyDueChanges(context.Background())
	if got := lockOf(t, env, id); got != compliance30 {
		t.Fatalf("lock before the grace period ended = %v", got)
	}
	env.clock.Advance(time.Minute)
	env.svc.ApplyDueChanges(context.Background())
	if got := lockOf(t, env, id); got != (models.ObjectLockSettings{Mode: models.ObjectLockGovernance, RetentionDays: 10}) {
		t.Fatalf("lock after the grace period = %v; want governance, 10 days", got)
	}

	// Raising applies at once and drops a pending lowering.
	if _, err = env.svc.UpdateTarget(alice, id, lockTargetInput(models.ObjectLockGovernance, 5, false)); err != nil {
		t.Fatal(err)
	}
	res, err = env.svc.UpdateTarget(alice, id, lockTargetInput(models.ObjectLockCompliance, 60, true))
	if err != nil {
		t.Fatal(err)
	}
	if res.PendingObjectLock != nil || res.Approval != nil {
		t.Fatalf("raising the lock deferred %+v / %+v", res.PendingObjectLock, res.Approval)
	}
	if got := lockOf(t, env, id); got != (models.ObjectLockSettings{Mode: models.ObjectLockCompliance, RetentionDays: 60, LegalHoldOnPin: true}) {
		t.Fatalf("lock after raising = %v; want compliance, 60 days, legal hold", got)
	}
	env.clock.Advance(models.GraceDuration(models.DefaultDeleteGraceDays))
	env.svc.ApplyDueChanges(context.Background())
	if got := lockOf(t, env, id); got.RetentionDays != 60 {
		t.Fatalf("a dropped lowering still applied: %v", got)
	}

	// A mixed change raises the mode at once and holds the shorter retention.
	env2, id2 := newLockTargetEnv(t, models.ObjectLockGovernance, 30)
	res, err = env2.svc.UpdateTarget(alice, id2, lockTargetInput(models.ObjectLockCompliance, 10, false))
	if err != nil {
		t.Fatal(err)
	}
	if got := lockOf(t, env2, id2); got != compliance30 || res.PendingObjectLock == nil {
		t.Fatalf("mixed change = %v, pending %+v; want compliance 30 now and 10 days pending", got, res.PendingObjectLock)
	}
	// Turning the lock off is a lowering too.
	if res, err = env2.svc.UpdateTarget(alice, id2, lockTargetInput(models.ObjectLockNone, 0, false)); err != nil || res.PendingObjectLock == nil {
		t.Fatalf("turning the lock off = %+v, %v; want a pending change", res, err)
	}
	if got := lockOf(t, env2, id2); got != compliance30 {
		t.Fatalf("lock right after turning it off = %v", got)
	}
}

// TestLoweringTheObjectLockNeedsASecondApprover proves that with the two-person
// rule a lowering waits for a second administrator, and then for the grace period.
func TestLoweringTheObjectLockNeedsASecondApprover(t *testing.T) {
	env, id := newLockTargetEnv(t, models.ObjectLockCompliance, 30)
	env.enableTwoPerson(t)
	alice := asUser("alice", auth.ScopeAdmin)
	res, err := env.svc.UpdateTarget(alice, id, lockTargetInput(models.ObjectLockNone, 0, false))
	if err != nil {
		t.Fatal(err)
	}
	if res.Approval == nil || res.Approval.Action != models.ApprovalLowerObjectLock || res.PendingObjectLock != nil {
		t.Fatalf("result = approval %+v, pending %+v; want an approval request only", res.Approval, res.PendingObjectLock)
	}
	if _, err = env.svc.Approve(asUser("bob", auth.ScopeAdmin), res.Approval.ID); err != nil {
		t.Fatal(err)
	}
	if got := lockOf(t, env, id); got.Mode != models.ObjectLockCompliance {
		t.Fatalf("lock right after the approval = %v; want it kept for the grace period", got)
	}
	env.clock.Advance(models.GraceDuration(models.DefaultDeleteGraceDays))
	env.svc.ApplyDueChanges(context.Background())
	if got := lockOf(t, env, id); got.Locked() {
		t.Fatalf("lock after the approval and the grace period = %v; want none", got)
	}
}
