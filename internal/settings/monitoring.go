package settings

import (
	"errors"
	"fmt"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// Limits of the monitoring settings.
const (
	// MinHeartbeatInterval is the shortest interval between global heartbeat pings.
	MinHeartbeatInterval = time.Minute
	// MaxHeartbeatInterval is the longest interval between global heartbeat pings.
	MaxHeartbeatInterval = time.Hour
	// DefaultHeartbeatInterval is the heartbeat interval of a fresh installation.
	DefaultHeartbeatInterval = 5 * time.Minute
)

// Monitoring configures the outbound heartbeat (dead-man's switch) of the instance:
// while the scheduler is healthy, HeartbeatURL is pinged every HeartbeatInterval, so
// an external monitor (healthchecks.io or a compatible service) raises an alert when
// the pings stop. Jobs have their own heartbeat URLs (models.Job.HeartbeatURL).
type Monitoring struct {
	// HeartbeatURL is pinged every HeartbeatInterval ("" = off). Secret: the URL is
	// the credential of the check, so API responses show only its origin.
	HeartbeatURL string `json:"heartbeat_url"`
	// HeartbeatInterval is the time between pings (1m to 1h, default 5m).
	HeartbeatInterval Duration `json:"heartbeat_interval"`
}

// defaultMonitoring returns the monitoring settings of a fresh installation: no
// heartbeat, a 5 minute interval.
func defaultMonitoring() Monitoring {
	return Monitoring{HeartbeatInterval: Duration(DefaultHeartbeatInterval)}
}

// masked returns m with the heartbeat URL reduced to its origin.
func (m Monitoring) masked() Monitoring {
	m.HeartbeatURL = maskEndpoint(m.HeartbeatURL)
	return m
}

// MonitoringPatch updates Monitoring. HeartbeatURL keeps the stored URL when it is
// sent back masked (as GET /api/v1/settings shows it) or as SecretMask; a masked
// value for another host is refused (ErrSecretReentry), "" turns the heartbeat off
// and any other value replaces it.
type MonitoringPatch struct {
	HeartbeatURL      *string   `json:"heartbeat_url,omitempty"`
	HeartbeatInterval *Duration `json:"heartbeat_interval,omitempty"`
}

// apply sets the fields of p that are present on m, resolving a masked URL against
// m. A nil p changes nothing.
func (p *MonitoringPatch) apply(m *Monitoring) error {
	if p == nil {
		return nil
	}
	setIf(&m.HeartbeatInterval, p.HeartbeatInterval)
	if p.HeartbeatURL != nil {
		v, err := models.ResolveHeartbeatURL(*p.HeartbeatURL, m.HeartbeatURL)
		switch {
		case errors.Is(err, models.ErrMaskedHeartbeatURL) && m.HeartbeatURL == "":
			return fmt.Errorf("monitoring.heartbeat_url: %w", ErrMaskedSecret)
		case err != nil:
			return fmt.Errorf("%w: monitoring.heartbeat_url: the host changed or the URL is masked; enter the full URL again", ErrSecretReentry)
		}
		m.HeartbeatURL = v
	}
	return nil
}

// validateMonitoring checks m.
func validateMonitoring(m *Monitoring) error {
	if m.HeartbeatInterval.Std() < MinHeartbeatInterval || m.HeartbeatInterval.Std() > MaxHeartbeatInterval {
		return fmt.Errorf("%w: monitoring.heartbeat_interval must be between 1m and 1h", ErrInvalid)
	}
	if err := models.ValidateHeartbeatURL(m.HeartbeatURL); err != nil {
		return fmt.Errorf("%w: monitoring.%w", ErrInvalid, err)
	}
	return nil
}
