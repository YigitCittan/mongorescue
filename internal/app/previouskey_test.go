package app

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/keyrotation"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

func hasWarning(s *settings.Service, id string) bool {
	return slices.ContainsFunc(s.Warnings(), func(w settings.Warning) bool { return w.ID == id })
}

// TestPreviousKeyIsDeletedOnlyAfterANewRecoveryKit checks what happens once the
// snapshots sealed with the old key were pruned: without a recovery kit downloaded
// after the rotation the administrator is warned and secret.key.previous stays;
// with one, the file is deleted and the warning goes.
func TestPreviousKeyIsDeletedOnlyAfterANewRecoveryKit(t *testing.T) {
	ctx := context.Background()
	files := keyrotation.FilesIn(t.TempDir())
	old, _ := secretbox.GenerateKey()
	if err := secretbox.WriteKeyFile(files.Previous, old); err != nil {
		t.Fatal(err)
	}
	r := keyrotation.New(keyrotation.Config{Files: files})
	svc, err := settings.NewService(ctx, &memSettingsRepo{})
	if err != nil {
		t.Fatal(err)
	}
	rotation := time.Now().Add(-time.Hour)
	check := previousKeyCheck(svc, func() *keyrotation.Rotator { return r }, slog.New(slog.DiscardHandler))

	check(ctx, rotation)
	if !hasWarning(svc, settings.WarningPreviousKey) || !r.PreviousKeyKept() {
		t.Fatal("without a new recovery kit: want the warning and the file kept")
	}
	if err = svc.MarkRecoveryKitDownloaded(ctx, "fp", rotation.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	check(ctx, rotation)
	if !r.PreviousKeyKept() {
		t.Fatal("a kit downloaded before the rotation must not delete the previous key")
	}
	if err = svc.MarkRecoveryKitDownloaded(ctx, "fp", time.Now()); err != nil {
		t.Fatal(err)
	}
	check(ctx, rotation)
	if hasWarning(svc, settings.WarningPreviousKey) || r.PreviousKeyKept() {
		t.Fatal("with a kit downloaded after the rotation: want the file deleted and no warning")
	}
}
