package mongotls

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// Kinds of certificate a connection's TLS material holds (see Expiries).
const (
	// KindClient is the x509 client certificate (and its chain).
	KindClient = "client"
	// KindCA is the bundle of certificate authorities that sign the server.
	KindCA = "ca"
)

// Expiry is the earliest expiry among the certificates of one kind.
type Expiry struct {
	// Kind is KindClient or KindCA.
	Kind string
	// NotAfter is the earliest NotAfter of the certificates of that kind.
	NotAfter time.Time
	// Fingerprint is the hex SHA-256 of the certificate that expires first, so a
	// renewed certificate is told apart from the one that was announced.
	Fingerprint string
}

// Expiries returns the earliest expiry of the client certificate chain and of the
// CA bundle of t, in that order, for the kinds that are set and parse. PEM that
// does not parse yields no entry (Validate reports it when saving). It never
// returns or logs PEM.
func Expiries(t *models.ConnectionTLS) []Expiry {
	if t == nil {
		return nil
	}
	var out []Expiry
	if e, ok := earliest(KindClient, t.ClientCertPEM); ok {
		out = append(out, e)
	}
	if e, ok := earliest(KindCA, t.CAPEM); ok {
		out = append(out, e)
	}
	return out
}

// earliest returns the Expiry of the certificate of pemData that expires first.
func earliest(kind, pemData string) (Expiry, bool) {
	var best *x509.Certificate
	rest := []byte(pemData)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		if best == nil || cert.NotAfter.Before(best.NotAfter) {
			best = cert
		}
	}
	if best == nil {
		return Expiry{}, false
	}
	sum := sha256.Sum256(best.Raw)
	return Expiry{Kind: kind, NotAfter: best.NotAfter.UTC(), Fingerprint: hex.EncodeToString(sum[:])}, true
}
