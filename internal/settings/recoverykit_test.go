package settings

import (
	"context"
	"errors"
	"testing"
	"time"
)

func hasWarning(s *Service, id string) bool {
	for _, w := range s.Warnings() {
		if w.ID == id && w.Message != "" && w.Setting != "" {
			return true
		}
	}
	return false
}

// TestRecoveryKitWarningLifecycle checks that the reminder is active until a kit is
// downloaded, survives a restart, comes back when the material changes and can be
// dismissed for the current material only.
func TestRecoveryKitWarningLifecycle(t *testing.T) {
	ctx := context.Background()
	repo := &memRepo{}
	svc := newSvc(t, repo)
	if hasWarning(svc, WarningRecoveryKit) {
		t.Fatal("no reminder before a fingerprint is known")
	}
	svc.NoteRecoveryKitFingerprint("fp1")
	if !hasWarning(svc, WarningRecoveryKit) {
		t.Fatal("the reminder must be active while no kit was downloaded")
	}
	if st := svc.RecoveryKitStatus(); st.DownloadedAt != nil || st.UpToDate {
		t.Fatalf("status before download = %+v", st)
	}
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := svc.MarkRecoveryKitDownloaded(ctx, "fp1", at); err != nil {
		t.Fatal(err)
	}
	if hasWarning(svc, WarningRecoveryKit) {
		t.Fatal("a download must resolve the reminder")
	}
	if st := svc.RecoveryKitStatus(); st.DownloadedAt == nil || !st.DownloadedAt.Equal(at) || !st.UpToDate {
		t.Fatalf("status after download = %+v", st)
	}

	// A restart keeps the download; the same material keeps the reminder off.
	again := newSvc(t, repo)
	again.NoteRecoveryKitFingerprint("fp1")
	if hasWarning(again, WarningRecoveryKit) {
		t.Fatal("the download must survive a restart")
	}
	// New material (a rotated key or a changed target) brings it back.
	again.NoteRecoveryKitFingerprint("fp2")
	if !hasWarning(again, WarningRecoveryKit) || again.RecoveryKitStatus().UpToDate {
		t.Fatal("changed material must bring the reminder back")
	}
	// Dismissing hides it for this material only.
	if err := again.DismissWarning(ctx, WarningRecoveryKit); err != nil {
		t.Fatal(err)
	}
	if hasWarning(again, WarningRecoveryKit) {
		t.Fatal("dismissed reminder still shown")
	}
	third := newSvc(t, repo)
	third.NoteRecoveryKitFingerprint("fp2")
	if hasWarning(third, WarningRecoveryKit) {
		t.Fatal("the dismissal must survive a restart")
	}
	third.NoteRecoveryKitFingerprint("fp3")
	if !hasWarning(third, WarningRecoveryKit) {
		t.Fatal("a dismissal must not hide the reminder for new material")
	}
}

// TestMetadataBackupUnencryptedWarning checks that the warning follows the settings
// and cannot be dismissed.
func TestMetadataBackupUnencryptedWarning(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t, &memRepo{})
	if hasWarning(svc, WarningMetadataBackupUnencrypted) {
		t.Fatal("metadata backups are off by default")
	}
	on := true
	if _, err := svc.Update(ctx, Patch{MetadataBackup: &MetadataBackupPatch{Enabled: &on}}); err != nil {
		t.Fatal(err)
	}
	if !hasWarning(svc, WarningMetadataBackupUnencrypted) {
		t.Fatal("metadata backups without encryption must warn")
	}
	if err := svc.DismissWarning(ctx, WarningMetadataBackupUnencrypted); !errors.Is(err, ErrInvalid) {
		t.Fatalf("dismiss = %v, want ErrInvalid", err)
	}
}

// TestMetadataBackupSettings checks the defaults, a patch and validation.
func TestMetadataBackupSettings(t *testing.T) {
	ctx := context.Background()
	d := Defaults().MetadataBackup
	if d.Enabled || d.Interval.Std() != 24*time.Hour || d.RetentionCount != 14 || d.TargetID != "" {
		t.Fatalf("defaults = %+v", d)
	}
	repo := &memRepo{}
	svc := newSvc(t, repo)
	on, every, keep, target := true, Duration(6*time.Hour), 3, " stg_1 "
	// A lower snapshot count is a lowered protection: set directly it is refused, the
	// operations service applies it after the grace period.
	if _, err := svc.Update(ctx, Patch{MetadataBackup: &MetadataBackupPatch{RetentionCount: &keep}}); !errors.Is(err, ErrProtectionLowered) {
		t.Fatalf("lower retention_count directly = %v; want ErrProtectionLowered", err)
	}
	if _, err := svc.Update(WithLoweredProtection(ctx), Patch{MetadataBackup: &MetadataBackupPatch{Enabled: &on, Interval: &every, RetentionCount: &keep, TargetID: &target}}); err != nil {
		t.Fatal(err)
	}
	got := newSvc(t, repo).Current().MetadataBackup
	if !got.Enabled || got.Interval != every || got.RetentionCount != 3 || got.TargetID != "stg_1" {
		t.Fatalf("stored = %+v", got)
	}
	for _, p := range []MetadataBackupPatch{
		{Interval: new(Duration(time.Minute))},
		{Interval: new(Duration(31 * 24 * time.Hour))},
		{RetentionCount: new(0)},
		{RetentionCount: new(MaxMetadataBackupRetention + 1)},
		{TargetID: new("bad\nid")},
	} {
		if _, err := svc.Update(ctx, Patch{MetadataBackup: &p}); !errors.Is(err, ErrInvalid) {
			t.Errorf("Update(%+v) = %v, want ErrInvalid", p, err)
		}
	}
}
