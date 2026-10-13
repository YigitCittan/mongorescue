package operations

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/events"
)

// maxNamedKeys bounds the keys an api_key.deadline_set event names.
const maxNamedKeys = 20

// APIKeyExpiryChecked announces the outcome of auth.Service.CheckAPIKeyExpiry: an
// api_key.deadline_set event (stored for the notification channels before it
// returns, and audited as a system entry) when keys got a deadline from the
// maximum lifetime, and an api_key.expiring event per key that newly came within
// the warning window. Events name keys by name, prefix and ID, never by secret.
func (s *Service) APIKeyExpiryChecked(ctx context.Context, c *auth.ExpiryCheck) {
	if c == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	now := s.now().UTC()
	if len(c.Capped) > 0 {
		detail := fmt.Sprintf("security.api_key_max_lifetime_days is %d: %d API key(s) without an earlier expiry now expire at %s: %s",
			int(c.MaxLifetime/(24*time.Hour)), len(c.Capped), c.Deadline.UTC().Format(time.RFC3339), keyList(c.Capped))
		e := events.SecurityEvent(events.APIKeyDeadlineSet, now, "deadline_set", "system", "", detail)
		s.alert(ctx, e)
		if s.cfg.Audit != nil {
			ids := make([]string, 0, len(c.Capped))
			for _, k := range c.Capped {
				ids = append(ids, k.ID)
			}
			// Argument names avoid "key", which the audit log masks as a secret.
			entry := systemAudit(now, "api_key.deadline_set", map[string]any{
				"deadline": c.Deadline.UTC(), "max_lifetime_days": int(c.MaxLifetime / (24 * time.Hour)), "ids": ids,
			})
			entry.APIKeyName = "api_key_expiry"
			s.cfg.Audit.Record(ctx, entry)
		}
		s.logger.Warn("API keys got an expiry from the maximum lifetime",
			slog.Int("keys", len(c.Capped)), slog.Time("expires_at", c.Deadline.UTC()))
	}
	for _, k := range c.Announce {
		detail := fmt.Sprintf("API key %q (%s, %s) expires at %s; create a replacement and update the clients that use it",
			k.Name, keyPrefix(k), k.ID, k.ExpiresAt.UTC().Format(time.RFC3339))
		s.publish(ctx, events.SecurityEvent(events.APIKeyExpiring, now, "expiring", "system", "", detail))
	}
}

// keyList names up to maxNamedKeys keys ("name (mr_prefix_…)").
func keyList(keys []*auth.APIKey) string {
	names := make([]string, 0, min(len(keys), maxNamedKeys))
	for i, k := range keys {
		if i == maxNamedKeys {
			names = append(names, fmt.Sprintf("and %d more", len(keys)-maxNamedKeys))
			break
		}
		names = append(names, fmt.Sprintf("%q (%s)", k.Name, keyPrefix(k)))
	}
	return strings.Join(names, ", ")
}

// keyPrefix is the public display form of a key's prefix.
func keyPrefix(k *auth.APIKey) string {
	return "mr_" + k.Prefix + "_…"
}
