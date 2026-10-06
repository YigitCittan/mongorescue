package models

import (
	"strings"
	"testing"
	"time"
)

func s3Target(mode ObjectLockMode, days int) *StorageTarget {
	return &StorageTarget{ID: "stg_1", Name: "vault", Type: StorageS3, S3: &S3Target{Bucket: "b", ObjectLock: mode, RetentionDays: days}}
}

// TestImmutabilityHints checks the hint of every kind of target: info without a
// lock (S3 or local), a warning for governance mode, none for compliance mode.
func TestImmutabilityHints(t *testing.T) {
	for _, tc := range []struct {
		target *StorageTarget
		code   string
		level  string
	}{
		{s3Target("", 0), HintNoObjectLock, HintInfo},
		{s3Target(ObjectLockGovernance, 30), HintGovernanceBypass, HintWarn},
		{s3Target(ObjectLockCompliance, 30), "", ""},
		{&StorageTarget{Type: StorageLocal, Local: &LocalTarget{Path: "/b"}}, HintLocalNotImmutable, HintInfo},
	} {
		h := tc.target.ImmutabilityHint()
		switch {
		case tc.code == "" && h != nil:
			t.Errorf("%v: hint %+v; want none", tc.target.S3, h)
		case tc.code != "" && (h == nil || h.Code != tc.code || h.Level != tc.level):
			t.Errorf("%+v: hint %+v; want %s/%s", tc.target, h, tc.level, tc.code)
		}
		red := tc.target.Redacted()
		if (len(red.Hints) == 1) != (tc.code != "") {
			t.Errorf("Redacted hints = %+v", red.Hints)
		}
		if len(tc.target.Hints) != 0 {
			t.Error("Redacted changed the original")
		}
	}
	if s3Target("", 0).Redacted().S3.ObjectLock != ObjectLockNone {
		t.Error("an unlocked target must show object_lock none")
	}
}

// TestRetentionLockWarning warns about a job retention that deletes backups before
// their lock ends.
func TestRetentionLockWarning(t *testing.T) {
	locked := s3Target(ObjectLockCompliance, 30)
	for _, tc := range []struct {
		days, count int
		warn        bool
	}{
		{7, 0, true}, {30, 0, false}, {60, 5, false}, {0, 5, true}, {0, 0, false},
	} {
		if got := locked.RetentionLockWarning(tc.days, tc.count); (got != "") != tc.warn || tc.warn && !strings.Contains(got, "30-day object lock") {
			t.Errorf("retention %d days, %d count: %q; want warning=%v", tc.days, tc.count, got, tc.warn)
		}
	}
	if s3Target("", 0).RetentionLockWarning(1, 0) != "" {
		t.Error("an unlocked target never warns")
	}
}

// TestPurgeDueWaitsForTheLock checks that a deleted record is due only once both
// its grace period and its retain-until date have passed, and that the version and
// retention of a stored object are recorded.
func TestPurgeDueWaitsForTheLock(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	until := t0.Add(30 * 24 * time.Hour)
	r := &BackupRecord{ID: "bkp", Status: StatusCompleted}
	r.SetStorageObject(&StorageObject{VersionID: "v1", RetainUntil: &until, ObjectLockMode: ObjectLockGovernance})
	if r.StorageVersionID != "v1" || r.RetainUntil == nil || !r.RetainUntil.Equal(until) || r.ObjectLockMode != ObjectLockGovernance {
		t.Fatalf("record = %+v", r)
	}
	r.MarkDeleted(SoftDelete{At: t0, PurgeAfter: t0.Add(7 * 24 * time.Hour)})
	grace := 7 * 24 * time.Hour
	if r.PurgeDue(t0.Add(grace), grace) || !r.LockedAt(t0.Add(grace)) {
		t.Fatal("due while locked")
	}
	if !r.PurgeDue(until, grace) || r.LockedAt(until) {
		t.Fatal("not due once the lock ended")
	}
	plain := &BackupRecord{ID: "p", Status: StatusCompleted}
	plain.SetStorageObject(&StorageObject{})
	plain.SetStorageObject(nil)
	if plain.RetainUntil != nil || plain.StorageVersionID != "" {
		t.Fatalf("plain = %+v", plain)
	}
}
