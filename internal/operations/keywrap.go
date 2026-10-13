package operations

import (
	"context"
	"log/slog"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/keywrap"
)

// KeyWrappingChanged publishes security.key_wrapping_changed for a change of the
// KMS wrapping of secret.key (a keywrap.Change from the journal: a key wrapped at
// startup, or the offline rewrap and unwrap commands) and records it in the audit
// log as a system entry. The change carries no key material.
func (s *Service) KeyWrappingChanged(ctx context.Context, c keywrap.Change) {
	ctx = context.WithoutCancel(ctx)
	actor := c.Actor
	if actor == "" {
		actor = keywrap.ActorSystem
	}
	e := events.SecurityEvent(events.SecurityKeyWrapping, c.At, c.Action, actor, "", c.Detail())
	s.alert(ctx, e)
	if s.cfg.Audit != nil {
		// Argument names avoid "key" and "uri", which the audit log masks as
		// secrets; a KMS key URI is not one.
		s.cfg.Audit.Record(ctx, systemAudit(c.At, "keywrap."+c.Action, map[string]any{
			"actor": actor, "kms": keywrap.Spec{Provider: c.Provider, KeyURI: c.KeyURI}.String(),
			"previous_kms": keywrap.Spec{Provider: c.PreviousProvider, KeyURI: c.PreviousKeyURI}.String(),
			"fingerprint":  c.Fingerprint, "files": c.Files,
		}))
	}
	s.logger.Info("secret key wrapping changed", slog.String("action", c.Action), slog.String("actor", actor),
		slog.String("fingerprint", c.Fingerprint))
}
