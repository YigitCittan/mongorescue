package models

import "time"

// Windows before a TLS certificate's expiry (see TLSLevel).
const (
	// TLSExpiryWarnWindow is how long before its expiry a certificate counts as
	// expiring (connection.tls_cert_expiring, first threshold).
	TLSExpiryWarnWindow = 30 * 24 * time.Hour
	// TLSExpiryCriticalWindow is the second, closer threshold.
	TLSExpiryCriticalWindow = 7 * 24 * time.Hour
)

// TLSExpiryLevel is how close a certificate is to its expiry.
type TLSExpiryLevel int

// Levels of TLSExpiryLevel, from fine to expired; a higher level is more urgent.
const (
	// TLSExpiryOK means the certificate is valid for more than TLSExpiryWarnWindow.
	TLSExpiryOK TLSExpiryLevel = iota
	// TLSExpiryWarn means it expires within TLSExpiryWarnWindow.
	TLSExpiryWarn
	// TLSExpiryCritical means it expires within TLSExpiryCriticalWindow.
	TLSExpiryCritical
	// TLSExpiryExpired means it has expired.
	TLSExpiryExpired
)

// TLSCertExpiry is the earliest expiry among the certificates of one kind of a
// connection's TLS material (the x509 client certificate chain, or the CA
// bundle). It is computed from the stored certificates and never stored.
type TLSCertExpiry struct {
	// Kind is "client" or "ca".
	Kind string `json:"kind"`
	// NotAfter is when the earliest certificate of that kind expires.
	NotAfter time.Time `json:"not_after"`
}

// Level returns how close the certificate is to its expiry at now.
func (e TLSCertExpiry) Level(now time.Time) TLSExpiryLevel {
	return TLSLevel(e.NotAfter, now)
}

// TLSLevel returns how close a certificate that expires at notAfter is to its
// expiry at now.
func TLSLevel(notAfter, now time.Time) TLSExpiryLevel {
	left := notAfter.Sub(now)
	switch {
	case left <= 0:
		return TLSExpiryExpired
	case left <= TLSExpiryCriticalWindow:
		return TLSExpiryCritical
	case left <= TLSExpiryWarnWindow:
		return TLSExpiryWarn
	}
	return TLSExpiryOK
}
