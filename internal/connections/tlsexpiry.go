package connections

import (
	"context"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotls"
)

// withExpiry fills c.TLSCertExpiry from the certificates of c and returns c.
func withExpiry(c *models.Connection) *models.Connection {
	c.TLSCertExpiry = nil
	for _, e := range mongotls.Expiries(&c.ConnectionTLS) {
		c.TLSCertExpiry = append(c.TLSCertExpiry, models.TLSCertExpiry{Kind: e.Kind, NotAfter: e.NotAfter})
	}
	return c
}

// WithOnChange registers fn, called (without arguments, never blocking for long)
// after a connection was created, updated or deleted, so the certificate expiry
// checks can run at once.
func WithOnChange(fn func()) Option { return func(s *Service) { s.onChange = fn } }

// changed calls the OnChange hook.
func (s *Service) changed() {
	if s.onChange != nil {
		s.onChange()
	}
}

// TLSCertStatus is the earliest expiry of one kind of certificate of a connection.
type TLSCertStatus struct {
	// ConnectionID and ConnectionName identify the connection.
	ConnectionID, ConnectionName string
	// Kind is mongotls.KindClient or mongotls.KindCA.
	Kind string
	// NotAfter is when the certificate that expires first expires.
	NotAfter time.Time
	// Fingerprint identifies that certificate (hex SHA-256), so a renewed
	// certificate is announced again.
	Fingerprint string
	// Level is how close NotAfter is at the time of the check.
	Level models.TLSExpiryLevel
}

// TLSCertStatuses returns the certificate expiries of every connection at now. It
// is a system task: it reads the stored connections without a caller check and
// never returns key material.
func (s *Service) TLSCertStatuses(ctx context.Context, now time.Time) ([]TLSCertStatus, error) {
	list, err := s.repo.ListConnections(ctx)
	if err != nil {
		return nil, err
	}
	var out []TLSCertStatus
	for _, c := range list {
		for _, e := range mongotls.Expiries(&c.ConnectionTLS) {
			out = append(out, TLSCertStatus{ConnectionID: c.ID, ConnectionName: c.Name, Kind: e.Kind,
				NotAfter: e.NotAfter, Fingerprint: e.Fingerprint, Level: models.TLSLevel(e.NotAfter, now)})
		}
	}
	return out, nil
}

// TLSExpiryTracker remembers which expiry levels were announced for which
// certificate, so each threshold is announced once per certificate. A renewed
// certificate has another fingerprint and starts over. The state is in memory: a
// restart announces the current level of each expiring certificate once more.
type TLSExpiryTracker struct {
	mu        sync.Mutex
	announced map[tlsCertKey]models.TLSExpiryLevel
}

// tlsCertKey identifies one certificate of one connection.
type tlsCertKey struct{ connection, kind, fingerprint string }

// NewTLSExpiryTracker returns an empty tracker.
func NewTLSExpiryTracker() *TLSExpiryTracker {
	return &TLSExpiryTracker{announced: map[tlsCertKey]models.TLSExpiryLevel{}}
}

// Announce returns the statuses of statuses whose level is higher than the one
// announced for that certificate (an OK level announces nothing) and records them
// as announced. Certificates that are gone are forgotten.
func (t *TLSExpiryTracker) Announce(statuses []TLSCertStatus) []TLSCertStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	seen := make(map[tlsCertKey]models.TLSExpiryLevel, len(statuses))
	var out []TLSCertStatus
	for _, st := range statuses {
		k := tlsCertKey{st.ConnectionID, st.Kind, st.Fingerprint}
		prev := t.announced[k]
		if st.Level > prev {
			out = append(out, st)
			prev = st.Level
		}
		seen[k] = prev
	}
	t.announced = seen
	return out
}
