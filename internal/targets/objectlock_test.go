package targets_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

// lockingDriver is a versioned mock bucket whose Object Lock state is lockErr (nil
// for a lock-enabled bucket). It records the lock mode of every build and the keys
// whose versions it purged.
type lockingDriver struct {
	*storage.MockStorage
	mu       sync.Mutex
	lockErr  error
	checks   int
	modes    []models.ObjectLockMode
	versions []string
}

func (d *lockingDriver) ObjectLockEnabled() bool { return true }

func (d *lockingDriver) PurgeVersions(ctx context.Context, key string, _ time.Time) (*time.Time, error) {
	d.mu.Lock()
	d.versions = append(d.versions, key)
	d.mu.Unlock()
	return nil, d.Delete(ctx, key)
}

func (d *lockingDriver) CheckObjectLock(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.checks++
	return d.lockErr
}

func (d *lockingDriver) SetLegalHold(context.Context, string, string, bool) error { return nil }

func newLockFixture(t *testing.T, lockErr error) (*targets.Service, *lockingDriver) {
	t.Helper()
	d := &lockingDriver{MockStorage: storage.NewMockStorage(), lockErr: lockErr}
	factory := func(_ context.Context, tg *models.StorageTarget, _ string) (storage.Storage, error) {
		d.mu.Lock()
		d.modes = append(d.modes, tg.S3.ObjectLock)
		d.mu.Unlock()
		return d, nil
	}
	return targets.NewService(storetest.New(t), factory, filepath.Join(t.TempDir(), "data"),
		targets.WithTestTimeout(2*time.Second)), d
}

func lockInput(mode models.ObjectLockMode, days int, hold bool) targets.Input {
	in := s3Input("locked", "vault", "secret")
	in.S3.ObjectLock, in.S3.RetentionDays, in.S3.LegalHoldOnPin = mode, days, hold
	return in
}

// TestObjectLockSettingsValidated checks the mode, the retention range and that a
// legal hold on pin needs a lock.
func TestObjectLockSettingsValidated(t *testing.T) {
	svc, _ := newLockFixture(t, nil)
	ctx := context.Background()
	for _, in := range []targets.Input{
		lockInput("legal", 30, false),
		lockInput(models.ObjectLockGovernance, 0, false),
		lockInput(models.ObjectLockCompliance, models.MaxObjectLockRetentionDays+1, false),
		lockInput(models.ObjectLockNone, 0, true),
	} {
		if _, err := svc.Create(ctx, in); !errors.Is(err, targets.ErrInvalid) {
			t.Errorf("%+v = %v; want ErrInvalid", *in.S3, err)
		}
	}
	created, err := svc.Create(ctx, lockInput(" Compliance ", 3650, true))
	if err != nil {
		t.Fatal(err)
	}
	if s3 := created.S3; s3.ObjectLock != models.ObjectLockCompliance || s3.RetentionDays != 3650 || !s3.LegalHoldOnPin {
		t.Fatalf("stored %+v", *s3)
	}
	plain, err := svc.Create(ctx, func() targets.Input {
		in := lockInput("", 9, false)
		in.Name, in.S3.Bucket = "plain", "plain-bucket"
		return in
	}())
	if err != nil {
		t.Fatal(err)
	}
	if plain.S3.ObjectLock != models.ObjectLockNone || plain.S3.RetentionDays != 0 {
		t.Fatalf("an unlocked target shows %q, %d days; want none, 0", plain.S3.ObjectLock, plain.S3.RetentionDays)
	}
}

// TestObjectLockRefusedOnUnlockedBucket checks that saving or testing a locked
// target on a bucket without Object Lock fails with the reason, and that the check
// never runs for an unlocked target.
func TestObjectLockRefusedOnUnlockedBucket(t *testing.T) {
	unavailable := fmt.Errorf("%w: Object Lock is not enabled on bucket vault", storage.ErrObjectLockUnavailable)
	svc, d := newLockFixture(t, unavailable)
	ctx := context.Background()
	_, err := svc.Create(ctx, lockInput(models.ObjectLockGovernance, 7, false))
	if !errors.Is(err, targets.ErrInvalid) || !strings.Contains(err.Error(), "Object Lock is not enabled") {
		t.Fatalf("Create = %v; want ErrInvalid naming the missing Object Lock", err)
	}
	res, err := svc.TestInput(ctx, lockInput(models.ObjectLockGovernance, 7, false), "")
	if err != nil || res.OK || !strings.Contains(res.Error, "Object Lock is not enabled") {
		t.Fatalf("TestInput = %+v, %v; want a failed test naming the missing Object Lock", res, err)
	}

	checks := d.checks
	created, err := svc.Create(ctx, lockInput(models.ObjectLockNone, 0, false))
	if err != nil || d.checks != checks {
		t.Fatalf("unlocked Create = %v (checks %d -> %d); want no lock check", err, checks, d.checks)
	}
	// Turning the lock on later is checked too.
	in := lockInput(models.ObjectLockCompliance, 7, false)
	in.S3.SecretAccessKey = models.SecretMask
	if _, err = svc.Update(ctx, created.ID, in); !errors.Is(err, targets.ErrInvalid) {
		t.Fatalf("Update = %v; want ErrInvalid", err)
	}
}

// TestObjectLockProbeIsUnlockedAndDeletesItsVersion checks that the test of a
// locked target writes its probe without a lock, deletes the probe's version (not
// just its key) and then checks the bucket.
func TestObjectLockProbeIsUnlockedAndDeletesItsVersion(t *testing.T) {
	svc, d := newLockFixture(t, nil)
	ctx := context.Background()
	created, err := svc.Create(ctx, lockInput(models.ObjectLockCompliance, 30, false))
	if err != nil {
		t.Fatal(err)
	}
	d.modes, d.checks = nil, 0
	res, err := svc.Test(ctx, created.ID)
	if err != nil || !res.OK {
		t.Fatalf("Test = %+v, %v", res, err)
	}
	if len(d.modes) != 2 || d.modes[0] != "" || d.modes[1] != models.ObjectLockCompliance {
		t.Errorf("probe driver lock modes = %v; want an unlocked build for the probe and the locked one for its clean-up", d.modes)
	}
	if len(d.versions) != 1 || !strings.HasPrefix(d.versions[0], targets.ProbePrefix) {
		t.Errorf("purged keys = %v; want every version of the probe", d.versions)
	}
	if d.checks != 1 {
		t.Errorf("lock checks = %d; want 1", d.checks)
	}
	if objs, _ := d.List(ctx, ""); len(objs) != 0 {
		t.Errorf("probe objects left: %v", objs)
	}
}
