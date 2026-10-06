// Package mongotlstest generates a throwaway certificate authority and the server
// and client certificates it signs, for tests of TLS and x509 connections. It is
// imported by tests only.
package mongotlstest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"

	"github.com/youmark/pkcs8"
)

// CA is a test certificate authority.
type CA struct {
	// CertPEM is the CA certificate, PEM.
	CertPEM string
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
}

// Pair is a certificate and its unencrypted PKCS#8 private key, PEM.
type Pair struct {
	CertPEM string
	KeyPEM  string
	// Subject is the RFC 2253 subject, as MongoDB names x509 users.
	Subject string
	key     *ecdsa.PrivateKey
}

// NewCA returns a new CA valid for a day.
func NewCA() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "MongoRescue Test CA", Organization: []string{"MongoRescue"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	return &CA{CertPEM: encode("CERTIFICATE", der), cert: cert, key: key}, nil
}

// Server issues a server certificate for hosts (DNS names or IP addresses).
func (ca *CA) Server(hosts ...string) (*Pair, error) {
	tmpl := &x509.Certificate{
		Subject:     pkix.Name{CommonName: "mongodb", Organization: []string{"MongoRescue"}, OrganizationalUnit: []string{"Server"}},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	return ca.issue(tmpl)
}

// Client issues a client certificate with common name cn. Its organization and
// unit differ from the server's, as MongoDB requires of x509 users.
func (ca *CA) Client(cn string) (*Pair, error) {
	return ca.issue(&x509.Certificate{
		Subject:     pkix.Name{CommonName: cn, Organization: []string{"MongoRescue"}, OrganizationalUnit: []string{"Clients"}},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
}

// issue signs tmpl with the CA.
func (ca *CA) issue(tmpl *x509.Certificate) (*Pair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	tmpl.SerialNumber = serial()
	tmpl.NotBefore, tmpl.NotAfter = time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour)
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode key: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	return &Pair{
		CertPEM: encode("CERTIFICATE", der),
		KeyPEM:  encode("PRIVATE KEY", keyDER),
		Subject: cert.Subject.String(),
		key:     key,
	}, nil
}

// EncryptedKeyPEM returns the key of p as an encrypted PKCS#8 PEM block.
func (p *Pair) EncryptedKeyPEM(password string) (string, error) {
	der, err := pkcs8.MarshalPrivateKey(p.key, []byte(password), nil)
	if err != nil {
		return "", fmt.Errorf("encrypt key: %w", err)
	}
	return encode("ENCRYPTED PRIVATE KEY", der), nil
}

// encode returns der as a PEM block of type typ.
func encode(typ string, der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}))
}

// serial returns a random certificate serial number.
func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		panic(err)
	}
	return n
}
