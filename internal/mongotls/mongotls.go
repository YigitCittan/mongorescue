// Package mongotls turns the TLS material of a connection (models.ConnectionTLS: a
// custom CA, an x509 client certificate and key) into a crypto/tls configuration for
// the MongoDB driver, validates it when a connection is saved, and carries it through
// a context.Context to the code that connects (the driver adapter in mongoconn and
// the tools configuration in mongotools), which only receives the URI.
//
// Errors never quote PEM contents or the key password.
package mongotls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"github.com/youmark/pkcs8"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// Sentinel errors. They name the field at fault and never include its value.
var (
	// ErrInvalidCA is returned when tls_ca_pem holds no PEM certificate.
	ErrInvalidCA = errors.New("mongotls: tls_ca_pem holds no valid PEM certificate")
	// ErrInvalidClientCert is returned when the client certificate or key cannot be
	// parsed, or the key does not belong to the certificate.
	ErrInvalidClientCert = errors.New("mongotls: invalid client certificate or key")
	// ErrIncompleteClientCert is returned when only one of the client certificate and
	// key is set, or a key password is set without a key.
	ErrIncompleteClientCert = errors.New("mongotls: tls_client_cert_pem and tls_client_key_pem must be set together")
	// ErrKeyPassword is returned when an encrypted key has no password or a wrong
	// one, or a password is set for a key that is not encrypted.
	ErrKeyPassword = errors.New("mongotls: the client key password is wrong or missing, or the key is not encrypted")
	// ErrLegacyEncryptedKey is returned for a key encrypted with the legacy PEM
	// scheme (Proc-Type: 4,ENCRYPTED), which is insecure and unsupported.
	ErrLegacyEncryptedKey = errors.New("mongotls: legacy PEM key encryption is not supported; convert the key with 'openssl pkcs8 -topk8 -v2 aes256'")
	// ErrTooLarge is returned when a PEM value exceeds MaxPEMSize.
	ErrTooLarge = errors.New("mongotls: PEM value is too large")
)

// MaxPEMSize bounds each PEM value of a connection.
const MaxPEMSize = 64 << 10

// encryptedPKCS8Type is the PEM block type of an encrypted PKCS#8 private key.
const encryptedPKCS8Type = "ENCRYPTED PRIVATE KEY"

// Validate checks that t can be used: sizes, a parseable CA bundle and a matching,
// decryptable client certificate and key. A zero t is valid.
func Validate(t *models.ConnectionTLS) error {
	if t.IsZero() {
		return nil
	}
	for _, v := range []string{t.CAPEM, t.ClientCertPEM, t.ClientKeyPEM, t.ClientKeyPassword} {
		if len(v) > MaxPEMSize {
			return ErrTooLarge
		}
	}
	_, err := Config(t)
	return err
}

// Config returns the client TLS configuration for t, or nil when t is zero (the URI
// alone then decides whether and how TLS is used). The driver fills in the server
// name per host.
func Config(t *models.ConnectionTLS) (*tls.Config, error) {
	if t.IsZero() {
		return nil, nil //nolint:nilnil // nil means "no TLS material".
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if t.CAPEM != "" {
		pool, err := CertPool(t.CAPEM)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}
	if t.ClientCertPEM != "" || t.ClientKeyPEM != "" || t.ClientKeyPassword != "" {
		cert, err := ClientCertificate(t.ClientCertPEM, t.ClientKeyPEM, t.ClientKeyPassword)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	switch {
	case t.Insecure:
		// Explicitly confirmed by the user when the connection was saved.
		cfg.InsecureSkipVerify = true //nolint:gosec // G402: opt-in tls_insecure.
	case t.AllowInvalidHostnames:
		// Skip the built-in verification, which includes the hostname, and verify
		// the chain alone, like the driver's tlsAllowInvalidHostnames.
		cfg.InsecureSkipVerify = true //nolint:gosec // G402: the chain is verified in VerifyConnection.
		roots := cfg.RootCAs
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			return verifyChain(cs.PeerCertificates, roots)
		}
	}
	return cfg, nil
}

// verifyChain verifies the server chain certs against roots (nil: the system
// roots) without checking the hostname.
func verifyChain(certs []*x509.Certificate, roots *x509.CertPool) error {
	if len(certs) == 0 {
		return errors.New("mongotls: server sent no certificate")
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	if _, err := certs[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter}); err != nil {
		return fmt.Errorf("mongotls: verify server certificate: %w", err)
	}
	return nil
}

// CertPool parses the PEM certificates of caPEM into a pool.
func CertPool(caPEM string) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		return nil, ErrInvalidCA
	}
	return pool, nil
}

// ClientCertificate parses a client certificate chain and its private key, which
// may be an encrypted PKCS#8 key opened with password. Parsed certificates are
// cached in memory (see certCache), so an encrypted key is derived once, not
// for every client; failures are not cached.
func ClientCertificate(certPEM, keyPEM, password string) (tls.Certificate, error) {
	if certPEM == "" || keyPEM == "" {
		return tls.Certificate{}, ErrIncompleteClientCert
	}
	id := cache.id(certPEM, keyPEM, password)
	if cert, ok := cache.get(id); ok {
		return cert, nil
	}
	key, err := DecryptKey(keyPEM, password)
	if err != nil {
		return tls.Certificate{}, err
	}
	cert, err := tls.X509KeyPair([]byte(certPEM), key)
	if err != nil {
		// The tls error does not quote key material, but keep it out anyway.
		return tls.Certificate{}, ErrInvalidClientCert
	}
	cache.put(id, cert)
	return cert, nil
}

// DecryptKey returns the first private key of keyPEM as an unencrypted PEM block: an
// encrypted PKCS#8 key is opened with password and re-encoded as PKCS#8; any other
// key is returned as it is, and must not come with a password. An encrypted key
// must use PBES2 with PBKDF2 or scrypt within the limits of checkKDF, checked
// before anything is derived (ErrKDFTooExpensive, ErrUnsupportedKeyEncryption).
func DecryptKey(keyPEM, password string) ([]byte, error) {
	rest := []byte(keyPEM)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, ErrInvalidClientCert
		}
		if !strings.HasSuffix(block.Type, "PRIVATE KEY") {
			continue
		}
		if _, legacy := block.Headers["DEK-Info"]; legacy {
			return nil, ErrLegacyEncryptedKey
		}
		if block.Type != encryptedPKCS8Type {
			if password != "" {
				return nil, ErrKeyPassword
			}
			return pem.EncodeToMemory(block), nil
		}
		if password == "" {
			return nil, ErrKeyPassword
		}
		if err := checkKDF(block.Bytes); err != nil {
			return nil, err
		}
		kdfRuns.Add(1)
		key, err := pkcs8.ParsePKCS8PrivateKey(block.Bytes, []byte(password))
		if err != nil {
			return nil, ErrKeyPassword
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, ErrInvalidClientCert
		}
		return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
	}
}

// ctxKey is the context key of the TLS material.
type ctxKey struct{}

// NewContext returns a copy of ctx carrying t, for the code that connects with a
// connection's URI. A zero t returns ctx without TLS material, even if ctx carried
// some: the material always belongs to the connection being used.
func NewContext(ctx context.Context, t *models.ConnectionTLS) context.Context {
	if t.IsZero() {
		if FromContext(ctx) == nil {
			return ctx
		}
		return context.WithValue(ctx, ctxKey{}, (*models.ConnectionTLS)(nil))
	}
	c := *t
	return context.WithValue(ctx, ctxKey{}, &c)
}

// FromContext returns the TLS material carried by ctx, or nil.
func FromContext(ctx context.Context) *models.ConnectionTLS {
	t, _ := ctx.Value(ctxKey{}).(*models.ConnectionTLS)
	return t
}
