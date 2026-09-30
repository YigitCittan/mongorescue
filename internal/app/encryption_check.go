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
// The previous configuration counts as "enabled" only when the deprecated switch is
// still present and set to true (environment or config.json) and the recipients or
// the passphrase were imported. The import markers alone are not enough: they were
// also written for MONGORESCUE_ENCRYPTION_ENABLED=false, and a false alarm reaches
// every notification channel.
//
// The warning is raised once (persistently, see settings.WarningEncryptionOff) and
// logged on every start while it is active. Its alert is published until the event
// bus has handed it to the notification service (see watchEncryptionOffAlert), so a
// process that stops before the bus runs sends it on the next start. Encryption is
// never turned on automatically: the imported recipients may no longer be right, so
// the admin decides. An admin who turned encryption off, or dismissed the warning, is
// not asked again.
func checkEncryptionAfterUpgrade(ctx context.Context, logger *slog.Logger, legacy *config.Legacy, svc *settings.Service, pub events.Publisher) error {
	if !svc.Current().Encryption.Enabled && encryptionWasEnabled(legacy, svc) {
		if _, err := svc.RaiseEncryptionOffWarning(ctx); err != nil {
			return err
		}
	}
	for _, w := range svc.Warnings() {
		if w.ID == settings.WarningEncryptionOff {
			logger.Warn(w.Message)
		}
	}
	if pub != nil && svc.EncryptionOffAlertPending() {
		pub.Publish(ctx, events.Event{Type: events.EncryptionOffAfterUpgrade, Time: time.Now().UTC(), Status: "warning"})
	}
	return nil
}

// watchEncryptionOffAlert returns a bus handler that records the encryption-off
// alert as sent. Subscribed after the notification service, it runs once that
// service has enqueued the alert for every channel.
func watchEncryptionOffAlert(logger *slog.Logger, svc *settings.Service) events.Handler {
	return func(ctx context.Context, e events.Event) {
		if e.Type != events.EncryptionOffAfterUpgrade {
			return
		}
		if err := svc.MarkEncryptionOffAlerted(ctx); err != nil {
			logger.Error("could not record the encryption alert as sent", slog.Any("error", err))
		}
	}
}

// encryptionWasEnabled reports whether the previous configuration encrypted backups.
func encryptionWasEnabled(legacy *config.Legacy, svc *settings.Service) bool {
	return legacy.EncryptionWasEnabled() && slices.ContainsFunc(config.LegacyEncryptionKeySources(), svc.WasImported)
}
