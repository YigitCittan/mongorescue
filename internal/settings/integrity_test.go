package settings

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIntegrityDefaultsAndPatch(t *testing.T) {
	d := Defaults().Integrity
	if !d.VerifyAfterBackup || d.VerifyDecrypt || d.SweepSchedule != SweepOff || !d.StorageScan || d.SweepBytesPerSecond() != 0 {
		t.Fatalf("defaults = %+v", d)
	}
	ctx := context.Background()
	repo := &memRepo{}
	svc, err := NewService(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	off, weekly, limit := false, SweepWeekly, 25
	if _, err = svc.Update(ctx, Patch{Integrity: &IntegrityPatch{VerifyAfterBackup: &off, SweepSchedule: &weekly, SweepBandwidthLimit: &limit}}); err != nil {
		t.Fatal(err)
	}
	got := svc.Current().Integrity
	if got.VerifyAfterBackup || got.SweepSchedule != SweepWeekly || got.SweepBytesPerSecond() != 25<<20 || !got.StorageScan {
		t.Fatalf("after patch = %+v", got)
	}
	// The values survive a reload.
	again, err := NewService(ctx, repo)
	if err != nil || again.Current().Integrity != got {
		t.Fatalf("reloaded = %+v, %v", again.Current().Integrity, err)
	}

	bad := SweepSchedule("hourly")
	if _, err := svc.Update(ctx, Patch{Integrity: &IntegrityPatch{SweepSchedule: &bad}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad schedule = %v", err)
	}
	neg := -1
	if _, err := svc.Update(ctx, Patch{Integrity: &IntegrityPatch{SweepBandwidthLimit: &neg}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative limit = %v", err)
	}
	if SweepMonthly.Interval() != 30*24*time.Hour || SweepOff.Interval() != 0 || !SweepDaily.Valid() {
		t.Fatal("intervals")
	}
}
