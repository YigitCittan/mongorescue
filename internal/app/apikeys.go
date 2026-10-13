package app

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/metrics"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

// apiKeyExpiryInterval is how often the API key expiries are checked; a change of
// the expiry settings checks them at once.
const apiKeyExpiryInterval = time.Hour

// apiKeyExpiryMonitor applies the API key expiry policy on the active instance: it
// gives existing keys the deadline of a maximum lifetime, announces keys that come
// within the warning window and keeps the expiry gauges current.
type apiKeyExpiryMonitor struct {
	auth     *auth.Service
	ops      *operations.Service
	metrics  *metrics.Metrics
	logger   *slog.Logger
	interval time.Duration
	kick     chan struct{}
}

// newAPIKeyExpiryMonitor returns a monitor that is kicked whenever an API key
// expiry setting changes.
func newAPIKeyExpiryMonitor(a *auth.Service, ops *operations.Service, m *metrics.Metrics, svc *settings.Service, logger *slog.Logger) *apiKeyExpiryMonitor {
	mon := &apiKeyExpiryMonitor{auth: a, ops: ops, metrics: m, logger: logger, interval: apiKeyExpiryInterval, kick: make(chan struct{}, 1)}
	svc.OnChange(func(keys []string) {
		if slices.Contains(keys, settings.KeyAPIKeyMaxLifetimeDays) || slices.Contains(keys, settings.KeyAPIKeyExpiryWarningDays) {
			mon.Kick()
		}
	})
	return mon
}

// apiKeyPolicy returns the API key expiry policy of the current settings.
func apiKeyPolicy(svc *settings.Service) auth.APIKeyPolicy {
	sec := svc.Current().Security
	return auth.APIKeyPolicy{MaxLifetime: sec.APIKeyMaxLifetime(), WarnBefore: sec.APIKeyExpiryWarning()}
}

// Kick asks for a check now; it never blocks.
func (m *apiKeyExpiryMonitor) Kick() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// Run checks the expiries at once, then every interval and on every Kick, until
// ctx ends.
func (m *apiKeyExpiryMonitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		m.check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-m.kick:
		}
	}
}

// check runs one expiry check and announces its outcome.
func (m *apiKeyExpiryMonitor) check(ctx context.Context) {
	c, err := m.auth.CheckAPIKeyExpiry(ctx)
	if err != nil {
		if ctx.Err() == nil {
			m.logger.Warn("could not check the API key expiries", logsafe.Error(err))
		}
		return
	}
	if m.metrics != nil {
		m.metrics.SetAPIKeyExpiry(c.ExpiringCount, c.ExpiredCount)
	}
	if m.ops != nil {
		m.ops.APIKeyExpiryChecked(ctx, c)
	}
}
