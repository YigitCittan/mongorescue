package auth

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"
)

// API key lifetimes.
const (
	// MaxAPIKeyLifetime bounds the expiry of a new key when no maximum lifetime is
	// set (about ten years), so expiry times stay within what the store can hold.
	MaxAPIKeyLifetime = 3650 * 24 * time.Hour
	// DefaultAPIKeyExpiryWarning is how long before its expiry a key counts as
	// expiring when the policy says nothing.
	DefaultAPIKeyExpiryWarning = 14 * 24 * time.Hour
)

// APIKeyPolicy is the expiry policy of API keys (the security settings
// api_key_max_lifetime_days and api_key_expiry_warning_days).
type APIKeyPolicy struct {
	// MaxLifetime caps new keys, which then need an expiry within it, and gives
	// existing keys a deadline of now + MaxLifetime (see CheckAPIKeyExpiry). Zero
	// is unlimited: keys may have no expiry.
	MaxLifetime time.Duration
	// WarnBefore is how long before its expiry a key is reported as expiring
	// (DefaultAPIKeyExpiryWarning when zero).
	WarnBefore time.Duration
}

// WithAPIKeyPolicy reads the API key expiry policy from fn whenever a key is
// created or the expiries are checked, so changed settings apply immediately.
func WithAPIKeyPolicy(fn func() APIKeyPolicy) Option {
	return func(s *Service) { s.keyPolicy = fn }
}

// APIKeyPolicy returns the current API key expiry policy.
func (s *Service) APIKeyPolicy() APIKeyPolicy {
	var p APIKeyPolicy
	if s.keyPolicy != nil {
		p = s.keyPolicy()
	}
	if p.MaxLifetime < 0 {
		p.MaxLifetime = 0
	}
	if p.WarnBefore <= 0 {
		p.WarnBefore = DefaultAPIKeyExpiryWarning
	}
	return p
}

// checkExpiry validates the expiry of a new key against the policy and returns it
// in UTC (nil for none).
func (s *Service) checkExpiry(at *time.Time) (*time.Time, error) {
	policy := s.APIKeyPolicy()
	now := s.now().UTC()
	if at == nil {
		if policy.MaxLifetime > 0 {
			return nil, fmt.Errorf("%w: an expiry is required, at most %s from now (security.api_key_max_lifetime_days)",
				ErrInvalidExpiry, lifetimeText(policy.MaxLifetime))
		}
		return nil, nil
	}
	exp := at.UTC()
	limit, setting := MaxAPIKeyLifetime, ""
	if policy.MaxLifetime > 0 && policy.MaxLifetime < limit {
		limit, setting = policy.MaxLifetime, " (security.api_key_max_lifetime_days)"
	}
	switch {
	case !exp.After(now):
		return nil, fmt.Errorf("%w: the expiry must be in the future", ErrInvalidExpiry)
	case exp.After(now.Add(limit)):
		return nil, fmt.Errorf("%w: the expiry may be at most %s from now%s", ErrInvalidExpiry, lifetimeText(limit), setting)
	}
	return &exp, nil
}

// lifetimeText renders a lifetime in whole days ("365 days").
func lifetimeText(d time.Duration) string {
	days := int(d / (24 * time.Hour))
	if days == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", days)
}

// ExpiryStatus describes the API keys that need attention: those that expire
// within the warning window or have expired, and, while a maximum lifetime is set,
// the keys imported from MONGORESCUE_API_KEY, which never expire.
type ExpiryStatus struct {
	// Expiring lists the keys that expire within the warning window or have
	// expired, soonest first.
	Expiring []*APIKey
	// ExpiringCount counts the keys that expire within the warning window and have
	// not expired yet; ExpiredCount those past their expiry.
	ExpiringCount, ExpiredCount int
	// NeverExpiring lists the imported keys without an expiry while a maximum
	// lifetime is set.
	NeverExpiring []*APIKey
}

// ExpiryCheck is the outcome of CheckAPIKeyExpiry.
type ExpiryCheck struct {
	ExpiryStatus
	// Deadline is the expiry given to the keys in Capped (zero without a maximum
	// lifetime).
	Deadline time.Time
	// MaxLifetime is the maximum lifetime the deadline comes from.
	MaxLifetime time.Duration
	// Capped lists the keys that had no expiry, or a later one, and now expire at
	// Deadline.
	Capped []*APIKey
	// Announce lists the keys of Expiring (not expired yet) whose expiry was not
	// announced before; each expiry is announced once (an api_key.expiring event).
	Announce []*APIKey
}

// APIKeyExpiryStatus reports the keys actor may see (as ListAPIKeys) that expire
// within the warning window or have expired, and the imported keys that never
// expire while a maximum lifetime is set. It changes nothing.
func (s *Service) APIKeyExpiryStatus(ctx context.Context, actor *Principal) (*ExpiryStatus, error) {
	if err := actor.Require(ScopeRead); err != nil {
		return nil, err
	}
	keys, err := s.repo.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	admin := actor.Allows(ScopeAdmin)
	keys = slices.DeleteFunc(keys, func(k *APIKey) bool {
		return !admin && (actor.UserID() == "" || k.CreatedBy != actor.UserID())
	})
	st := s.expiryStatus(keys)
	return &st, nil
}

// expiryStatus classifies keys by the current policy.
func (s *Service) expiryStatus(keys []*APIKey) ExpiryStatus {
	policy := s.APIKeyPolicy()
	now := s.now().UTC()
	var st ExpiryStatus
	for _, k := range keys {
		switch {
		case k.ExpiresAt == nil:
			if policy.MaxLifetime > 0 && k.CreatedBy == "" {
				st.NeverExpiring = append(st.NeverExpiring, k)
			}
			continue
		case !now.Before(*k.ExpiresAt):
			st.ExpiredCount++
		case k.ExpiresAt.Sub(now) <= policy.WarnBefore:
			st.ExpiringCount++
		default:
			continue
		}
		st.Expiring = append(st.Expiring, k)
	}
	slices.SortStableFunc(st.Expiring, func(a, b *APIKey) int { return a.ExpiresAt.Compare(*b.ExpiresAt) })
	return st
}

// CheckAPIKeyExpiry applies the expiry policy to the stored keys. While a maximum
// lifetime is set, every key with a creator that has no expiry, or a later one
// than now + the maximum lifetime, gets that deadline: the change is reported in
// Capped (and logged), so the caller can announce it. Keys imported from
// MONGORESCUE_API_KEY keep no expiry and are reported in NeverExpiring. Keys
// that newly entered the warning window are returned in Announce and recorded as
// announced, so each expiry is announced once. It is a system task: the caller
// publishes the events and runs it on the active instance only.
func (s *Service) CheckAPIKeyExpiry(ctx context.Context) (*ExpiryCheck, error) {
	policy := s.APIKeyPolicy()
	out := &ExpiryCheck{MaxLifetime: policy.MaxLifetime}
	if policy.MaxLifetime > 0 {
		out.Deadline = s.now().UTC().Add(policy.MaxLifetime)
		capped, err := s.repo.CapAPIKeyExpiry(ctx, out.Deadline)
		if err != nil {
			return nil, err
		}
		out.Capped = capped
		for _, k := range capped {
			s.logger.Warn("api key now expires at the maximum lifetime", slog.String("api_key_id", k.ID),
				slog.String("prefix", k.Prefix), slog.Time("expires_at", out.Deadline))
		}
	}
	keys, err := s.repo.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	out.ExpiryStatus = s.expiryStatus(keys)
	now := s.now().UTC()
	for _, k := range out.Expiring {
		if !now.Before(*k.ExpiresAt) || (k.ExpiryWarnedFor != nil && k.ExpiryWarnedFor.Equal(*k.ExpiresAt)) {
			continue
		}
		if err := s.repo.MarkAPIKeyExpiryWarned(ctx, k.ID, *k.ExpiresAt); err != nil {
			return nil, err
		}
		warned := *k.ExpiresAt
		k.ExpiryWarnedFor = &warned
		out.Announce = append(out.Announce, k)
	}
	return out, nil
}
