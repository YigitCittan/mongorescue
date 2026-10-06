package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/yigitcittan/mongorescue/internal/keyrotation"
	"github.com/yigitcittan/mongorescue/internal/settings"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/auth/oidc"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/metabackup"
	"github.com/yigitcittan/mongorescue/internal/recoverykit"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

// previousKeyCheck runs once the metadata snapshots sealed with every rotated-away
// secret key were pruned (metabackup.Config.OnRetiredPruned): secret.key.previous is
// then only needed by an operator who kept nothing else, so it is deleted when a
// recovery kit (which carries the current key) was downloaded after the last
// rotation, and settings.WarningPreviousKey asks the administrator otherwise.
func previousKeyCheck(settingsSvc *settings.Service, rotator func() *keyrotation.Rotator, logger *slog.Logger) func(context.Context, time.Time) {
	return func(_ context.Context, lastRotation time.Time) {
		r := rotator()
		if r == nil || !r.PreviousKeyKept() {
			settingsSvc.SetPreviousKeyWarning(false)
			return
		}
		kit := settingsSvc.RecoveryKitStatus()
		if kit.DownloadedAt == nil || !kit.DownloadedAt.After(lastRotation) {
			settingsSvc.SetPreviousKeyWarning(true)
			return
		}
		if err := r.ForgetPreviousKey(); err != nil {
			logger.Warn("could not delete secret.key.previous", logsafe.Error(err))
			settingsSvc.SetPreviousKeyWarning(true)
			return
		}
		settingsSvc.SetPreviousKeyWarning(false)
		logger.Info("deleted secret.key.previous: its metadata snapshots were pruned and a recovery kit was downloaded since the rotation")
	}
}

// keyHolders are the components that keep secret.key, or a subkey of it, in memory.
type keyHolders struct {
	auth       *auth.Service
	oidcFlow   *secretbox.Ref
	metaBackup *metabackup.Service
	kit        *recoverykit.Service
	logger     *slog.Logger
}

// apply refreshes what depends on the rotated key and needs the store (the recovery
// kit fingerprint) once the rotation committed (keyrotation.ApplyFunc).
func (h *keyHolders) apply(_, _ []byte) {
	if err := h.kit.Refresh(context.Background()); err != nil {
		h.logger.Warn("could not check whether the recovery kit is current", logsafe.Error(err))
	}
}

// onCommit hands a rotated secret.key to every in-memory holder
// (keyrotation.Config.OnCommit). It runs under the store's key lock, so a metadata
// snapshot never pairs the new database with the old install ID; it must not call
// the store. A failure is logged and fixed by a restart, which derives everything
// from secret.key again.
func (h *keyHolders) onCommit(next, _ []byte) {
	fail := func(what string, err error) {
		h.logger.Error("secret key rotation: "+what+" keeps the old key until a restart", logsafe.Error(err))
	}
	if sub, err := secretbox.DeriveSubkey(next, auth.ImportedKeySubkeyPurpose); err != nil {
		fail("authentication", err)
	} else {
		h.auth.SetImportedKeySecret(sub)
	}
	if sub, err := secretbox.DeriveSubkey(next, oidc.FlowSubkeyPurpose); err != nil {
		fail("single sign-on", err)
	} else if box, boxErr := secretbox.New(sub); boxErr != nil {
		fail("single sign-on", boxErr)
	} else {
		h.oidcFlow.Store(box)
	}
	if id, err := metabackup.InstallID(next); err != nil {
		fail("metadata backups", err)
	} else if err = h.metaBackup.SetInstallID(id); err != nil {
		fail("metadata backups", err)
	}
	if err := h.kit.SetSecretKey(next, h.metaBackup.Prefix()); err != nil {
		fail("the recovery kit", err)
	}
}
