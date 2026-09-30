package app

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

// checkEncryptionAfterUpgrade warns when the previous (deprecated) configuration had
// backup encryption enabled but it is off now. Releases up to v0.7.1 imported the
// encryption settings without the switch, so such an installation has been writing
// unencrypted backups since the upgrade.
//
// The previous configuration counts as "enabled" when the deprecated switch is still
// set to true (environment or config.json), or when the import recorded both the
// switch and the keys new backups were encrypted with. The warning is raised once
// (persistently, see settings.WarningEncryptionOff) and publishes one
// EncryptionOffAfterUpgrade event; it is logged on every start while it is active.
// Encryption is never turned on automatically: the imported recipients may no
// longer be right, so the admin decides. An admin who turned encryption off, or
// dismissed the warning, is not asked again.
func checkEncryptionAfterUpgrade(ctx context.Context, logger *slog.Logger, legacy *config.Legacy, svc *settings.Service, pub events.Publisher) error {
	if svc.Current().Encryption.Enabled || !encryptionWasEnabled(legacy, svc) {
		return nil
	}
	raised, err := svc.RaiseEncryptionOffWarning(ctx)
	if err != nil {
		return err
	}
	for _, w := range svc.Warnings() {
		if w.ID == settings.WarningEncryptionOff {
			logger.Warn(w.Message)
		}
	}
	if raised && pub != nil {
		pub.Publish(ctx, events.Event{Type: events.EncryptionOffAfterUpgrade, Time: time.Now().UTC(), Status: "warning"})
	}
	return nil
}

// encryptionWasEnabled reports whether the previous configuration encrypted backups.
func encryptionWasEnabled(legacy *config.Legacy, svc *settings.Service) bool {
	if legacy.EncryptionWasEnabled() {
		return true
	}
	if legacy.EncryptionDisabled() {
		return false
	}
	return slices.ContainsFunc(config.LegacyEncryptionSwitchSources(), svc.WasImported) &&
		slices.ContainsFunc(config.LegacyEncryptionKeySources(), svc.WasImported)
}
