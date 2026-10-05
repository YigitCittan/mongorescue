package app

import (
	"context"
	"log/slog"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/auth/oidc"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/metabackup"
	"github.com/yigitcittan/mongorescue/internal/recoverykit"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

// keyHolders are the components that keep secret.key, or a subkey of it, in memory.
type keyHolders struct {
	auth       *auth.Service
	oidcFlow   *secretbox.Ref
	metaBackup *metabackup.Service
	kit        *recoverykit.Service
	logger     *slog.Logger
}

// apply hands a rotated secret.key to every holder (keyrotation.ApplyFunc). The
// store already uses it; a failure here is logged and fixed by a restart, which
// derives everything from secret.key again.
func (h *keyHolders) apply(next, _ []byte) {
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
		return
	}
	if err := h.kit.Refresh(context.Background()); err != nil {
		h.logger.Warn("could not check whether the recovery kit is current", logsafe.Error(err))
	}
}
