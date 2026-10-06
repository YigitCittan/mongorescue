package operations_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// holdDriver records the S3 legal holds set on a mock bucket; fail makes them fail.
type holdDriver struct {
	*storage.MockStorage
	mu    sync.Mutex
	holds []string
	fail  error
}

func (d *holdDriver) CheckObjectLock(context.Context) error { return nil }

func (d *holdDriver) SetLegalHold(_ context.Context, key, versionID string, on bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fail != nil {
		return d.fail
	}
	d.holds = append(d.holds, fmt.Sprintf("%s@%s=%v", key, versionID, on))
	return nil
}

// lockTargets resolves tgt_lock, an S3 target with compliance lock and
// legal_hold_on_pin, and tgt_plain, one without.
type lockTargets struct{}

func (lockTargets) Resolve(_ context.Context, id string) (*models.StorageTarget, error) {
	s3 := &models.S3Target{Bucket: "vault", ObjectLock: models.ObjectLockCompliance, RetentionDays: 30, LegalHoldOnPin: true}
	if id == "tgt_plain" {
		s3 = &models.S3Target{Bucket: "plain"}
	}
	return &models.StorageTarget{ID: id, Name: id, Type: models.StorageS3, S3: s3}, nil
}

func (lockTargets) List(context.Context) ([]*models.StorageTarget, error) { return nil, nil }

func newLegalHoldEnv(t *testing.T) (*protEnv, *holdDriver) {
	t.Helper()
	env := newProtEnv(t)
	d := &holdDriver{MockStorage: env.mock}
	env.cfg.Targets = lockTargets{}
	env.cfg.Storage = func(context.Context, string) (storage.Storage, error) { return d, nil }
	env.svc = operations.New(env.cfg)
	operations.SetNow(env.svc, env.clock.Now)
	for id, target := range map[string]string{"bkp_lock": "tgt_lock", "bkp_plain": "tgt_plain"} {
		rec := &models.BackupRecord{ID: id, Database: "shop", Status: models.StatusCompleted, StartedAt: env.clock.Now(),
			StorageKey: "shop/" + id, StorageTargetID: target, StorageVersionID: "v-" + id, ConnectionID: "conn_ok"}
		if err := env.st.SaveBackupRecord(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
	}
	return env, d
}

// TestLegalHoldFollowsPinAndUnpin proves that on a target with legal_hold_on_pin a
// pin sets an S3 legal hold on the archive's version and an admin's unpin lifts
// it, that a non-admin cannot lift it, and that targets without the option are
// left alone.
func TestLegalHoldFollowsPinAndUnpin(t *testing.T) {
	env, d := newLegalHoldEnv(t)
	alice := asUser("alice", auth.ScopeAdmin)
	rec, err := env.svc.PinBackup(asUser("olga", auth.ScopeOperator), "bkp_lock", "litigation")
	if err != nil || !rec.Pinned || !rec.LegalHold {
		t.Fatalf("pin = %+v, %v; want pinned with a legal hold", rec, err)
	}
	if _, err = env.svc.PinBackup(alice, "bkp_plain", ""); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(d.holds) != "[shop/bkp_lock@v-bkp_lock=true]" {
		t.Fatalf("holds after the pins = %v; want only bkp_lock's version on", d.holds)
	}
	if _, err = env.svc.UnpinBackup(asUser("olga", auth.ScopeOperator), "bkp_lock"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("operator unpin = %v; want ErrForbidden", err)
	}
	if rec, err = env.svc.UnpinBackup(alice, "bkp_lock"); err != nil || rec.Pinned || rec.LegalHold {
		t.Fatalf("unpin = %+v, %v; want unpinned without a hold", rec, err)
	}
	if _, err = env.svc.UnpinBackup(alice, "bkp_plain"); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(d.holds) != "[shop/bkp_lock@v-bkp_lock=true shop/bkp_lock@v-bkp_lock=false]" {
		t.Fatalf("holds = %v; want bkp_lock on then off", d.holds)
	}
}

// TestLegalHoldFailureLeavesThePin proves a pin or unpin whose legal hold change
// fails is refused and changes nothing.
func TestLegalHoldFailureLeavesThePin(t *testing.T) {
	env, d := newLegalHoldEnv(t)
	alice := asUser("alice", auth.ScopeAdmin)
	d.fail = errors.New("AccessDenied: s3:PutObjectLegalHold")
	if _, err := env.svc.PinBackup(alice, "bkp_lock", ""); !errors.Is(err, operations.ErrLegalHold) {
		t.Fatalf("pin with a failing hold = %v; want ErrLegalHold", err)
	}
	if rec, _ := env.st.GetBackupRecord(context.Background(), "bkp_lock"); rec.Pinned || rec.LegalHold {
		t.Fatalf("record after a failed pin = %+v", rec)
	}
	d.fail = nil
	if _, err := env.svc.PinBackup(alice, "bkp_lock", ""); err != nil {
		t.Fatal(err)
	}
	d.fail = errors.New("AccessDenied")
	if _, err := env.svc.UnpinBackup(alice, "bkp_lock"); !errors.Is(err, operations.ErrLegalHold) {
		t.Fatalf("unpin with a failing hold = %v; want ErrLegalHold", err)
	}
	if rec, _ := env.st.GetBackupRecord(context.Background(), "bkp_lock"); !rec.Pinned || !rec.LegalHold {
		t.Fatalf("record after a failed unpin = %+v; want still pinned and held", rec)
	}
}

// TestLegalHoldUnpinWaitsForTheSecondApprover proves that with the two-person rule
// the hold is lifted only when a second administrator approves the unpin.
func TestLegalHoldUnpinWaitsForTheSecondApprover(t *testing.T) {
	env, d := newLegalHoldEnv(t)
	alice := asUser("alice", auth.ScopeAdmin)
	if _, err := env.svc.PinBackup(alice, "bkp_lock", ""); err != nil {
		t.Fatal(err)
	}
	env.enableTwoPerson(t)
	_, err := env.svc.UnpinBackup(alice, "bkp_lock")
	a := pendingApproval(t, err)
	if len(d.holds) != 1 {
		t.Fatalf("holds before the approval = %v; want only the pin's", d.holds)
	}
	if _, err = env.svc.Approve(asUser("bob", auth.ScopeAdmin), a.ID); err != nil {
		t.Fatal(err)
	}
	rec, _ := env.st.GetBackupRecord(context.Background(), "bkp_lock")
	if rec.Pinned || rec.LegalHold || len(d.holds) != 2 || d.holds[1] != "shop/bkp_lock@v-bkp_lock=false" {
		t.Fatalf("after the approval: %+v, holds %v; want unpinned and the hold lifted", rec, d.holds)
	}
}

// TestJobWarningsForARetentionShorterThanTheLock checks the warning a job save
// carries when its retention deletes backups before their object lock ends.
func TestJobWarningsForARetentionShorterThanTheLock(t *testing.T) {
	env, _ := newLegalHoldEnv(t)
	ctx := context.Background()
	if w := env.svc.JobWarnings(ctx, &models.Job{StorageTargetID: "tgt_lock", RetentionDays: 7}); len(w) != 1 {
		t.Fatalf("warnings = %v; want one for 7 days under a 30-day lock", w)
	}
	for _, j := range []*models.Job{
		{StorageTargetID: "tgt_lock", RetentionDays: 30},
		{StorageTargetID: "tgt_plain", RetentionDays: 1},
	} {
		if w := env.svc.JobWarnings(ctx, j); len(w) != 0 {
			t.Errorf("warnings for %+v = %v; want none", j, w)
		}
	}
}
