package settings

import (
	"context"
	"errors"
	"testing"
)

func hasEncryptionOffWarning(s *Service) bool {
	for _, w := range s.Warnings() {
		if w.ID == WarningEncryptionOff && w.Setting == "encryption" && w.Message != "" {
			return true
		}
	}
	return false
}

// TestEncryptionOffWarningIsRaisedOnceAndPersists checks that the warning is raised
// once, survives a restart, and is not raised again.
func TestEncryptionOffWarningIsRaisedOnceAndPersists(t *testing.T) {
	ctx := context.Background()
	repo := &memRepo{}
	svc := newSvc(t, repo)
	if hasEncryptionOffWarning(svc) {
		t.Fatal("no warning before it is raised")
	}
	raised, err := svc.RaiseEncryptionOffWarning(ctx)
	if err != nil || !raised || !hasEncryptionOffWarning(svc) {
		t.Fatalf("first raise = %v, %v; want the warning", raised, err)
	}
	if raised, err = svc.RaiseEncryptionOffWarning(ctx); err != nil || raised {
		t.Fatalf("second raise = %v, %v; want no new warning", raised, err)
	}
	again := newSvc(t, repo)
	if !hasEncryptionOffWarning(again) {
		t.Fatal("the warning must survive a restart")
	}
	if raised, _ = again.RaiseEncryptionOffWarning(ctx); raised {
		t.Fatal("a restart must not raise the warning (and send the alert) again")
	}
	if again.Current().Encryption.Enabled {
		t.Fatal("raising the warning must never enable encryption")
	}
}

// TestEncryptionOffWarningClears covers the three ways the warning ends: turning
// encryption on, dismissing it, and turning encryption off explicitly before it was
// raised. None of them lets it come back.
func TestEncryptionOffWarningClears(t *testing.T) {
	ctx := context.Background()
	key, _ := GenerateKey()

	t.Run("encryption turned on", func(t *testing.T) {
		repo := &memRepo{}
		svc := newSvc(t, repo)
		if _, err := svc.RaiseEncryptionOffWarning(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Update(ctx, Patch{Encryption: &EncryptionPatch{Enabled: ptr(true), Recipients: ptr([]string{key.Recipient})}}); err != nil {
			t.Fatal(err)
		}
		if hasEncryptionOffWarning(svc) {
			t.Fatal("enabling encryption must clear the warning")
		}
		// Turning it off later is the admin's choice: no warning again.
		if _, err := svc.Update(ctx, Patch{Encryption: &EncryptionPatch{Enabled: ptr(false)}}); err != nil {
			t.Fatal(err)
		}
		again := newSvc(t, repo)
		if raised, _ := again.RaiseEncryptionOffWarning(ctx); raised || hasEncryptionOffWarning(again) {
			t.Fatal("a resolved warning must not come back")
		}
	})

	t.Run("dismissed", func(t *testing.T) {
		repo := &memRepo{}
		svc := newSvc(t, repo)
		if _, err := svc.RaiseEncryptionOffWarning(ctx); err != nil {
			t.Fatal(err)
		}
		if err := svc.DismissWarning(ctx, WarningEncryptionOff); err != nil {
			t.Fatal(err)
		}
		if hasEncryptionOffWarning(svc) || hasEncryptionOffWarning(newSvc(t, repo)) {
			t.Fatal("a dismissed warning must stay hidden, also after a restart")
		}
		if raised, _ := newSvc(t, repo).RaiseEncryptionOffWarning(ctx); raised {
			t.Fatal("a dismissed warning must not be raised again")
		}
		if err := svc.DismissWarning(ctx, "no_such_warning"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("dismiss unknown = %v; want ErrInvalid", err)
		}
	})

	t.Run("explicitly turned off first", func(t *testing.T) {
		svc := newSvc(t, &memRepo{})
		if _, err := svc.Update(ctx, Patch{Encryption: &EncryptionPatch{Enabled: ptr(false)}}); err != nil {
			t.Fatal(err)
		}
		if raised, _ := svc.RaiseEncryptionOffWarning(ctx); raised || hasEncryptionOffWarning(svc) {
			t.Fatal("an explicit decision to keep encryption off must not be overridden")
		}
	})

	t.Run("already on", func(t *testing.T) {
		svc := newSvc(t, &memRepo{})
		if _, err := svc.Update(ctx, Patch{Encryption: &EncryptionPatch{Enabled: ptr(true), Recipients: ptr([]string{key.Recipient})}}); err != nil {
			t.Fatal(err)
		}
		if raised, _ := svc.RaiseEncryptionOffWarning(ctx); raised || hasEncryptionOffWarning(svc) {
			t.Fatal("no warning while encryption is on")
		}
	})

	t.Run("unrelated update keeps it", func(t *testing.T) {
		svc := newSvc(t, &memRepo{})
		if _, err := svc.RaiseEncryptionOffWarning(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Update(ctx, Patch{General: &GeneralPatch{DefaultRetentionDays: ptr(3)}}); err != nil {
			t.Fatal(err)
		}
		if !hasEncryptionOffWarning(svc) {
			t.Fatal("an unrelated update must not clear the warning")
		}
	})
}
