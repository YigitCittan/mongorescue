package mongotls_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotls"
	"github.com/yigitcittan/mongorescue/internal/mongotls/mongotlstest"
)

func newCA(t *testing.T) *mongotlstest.CA {
	t.Helper()
	ca, err := mongotlstest.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func newClient(t *testing.T, ca *mongotlstest.CA) *mongotlstest.Pair {
	t.Helper()
	p, err := ca.Client("backup")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfigZero(t *testing.T) {
	for _, in := range []*models.ConnectionTLS{nil, {}} {
		cfg, err := mongotls.Config(in)
		if err != nil || cfg != nil {
			t.Fatalf("Config(%v) = %v, %v; want nil, nil", in, cfg, err)
		}
	}
}

func TestConfigCAAndClientCert(t *testing.T) {
	ca := newCA(t)
	client := newClient(t, ca)
	cfg, err := mongotls.Config(&models.ConnectionTLS{CAPEM: ca.CertPEM, ClientCertPEM: client.CertPEM, ClientKeyPEM: client.KeyPEM})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RootCAs == nil || len(cfg.Certificates) != 1 {
		t.Fatalf("config misses roots or certificate: %+v", cfg)
	}
	if cfg.InsecureSkipVerify || cfg.MinVersion < tls.VersionTLS12 {
		t.Fatalf("insecure defaults: skip=%v min=%x", cfg.InsecureSkipVerify, cfg.MinVersion)
	}
}

func TestConfigEncryptedKey(t *testing.T) {
	ca := newCA(t)
	client := newClient(t, ca)
	enc, err := client.EncryptedKeyPEM("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	in := &models.ConnectionTLS{ClientCertPEM: client.CertPEM, ClientKeyPEM: enc, ClientKeyPassword: "s3cret"}
	if _, err = mongotls.Config(in); err != nil {
		t.Fatalf("encrypted key: %v", err)
	}
	in.ClientKeyPassword = "Zq9-other"
	if _, err = mongotls.Config(in); !errors.Is(err, mongotls.ErrKeyPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	if strings.Contains(err.Error(), "Zq9-other") {
		t.Fatal("error quotes the password")
	}
	in.ClientKeyPassword = ""
	if _, err = mongotls.Config(in); !errors.Is(err, mongotls.ErrKeyPassword) {
		t.Fatalf("missing password: %v", err)
	}
	plain := &models.ConnectionTLS{ClientCertPEM: client.CertPEM, ClientKeyPEM: client.KeyPEM, ClientKeyPassword: "x"}
	if _, err = mongotls.Config(plain); !errors.Is(err, mongotls.ErrKeyPassword) {
		t.Fatalf("password for a plain key: %v", err)
	}
}

func TestValidateErrors(t *testing.T) {
	ca := newCA(t)
	client := newClient(t, ca)
	other := newClient(t, newCA(t))
	legacy := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Headers: map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-256-CBC,00"}, Bytes: []byte{1}}))
	cases := map[string]struct {
		in   models.ConnectionTLS
		want error
	}{
		"bad CA":          {models.ConnectionTLS{CAPEM: "not a pem"}, mongotls.ErrInvalidCA},
		"cert only":       {models.ConnectionTLS{ClientCertPEM: client.CertPEM}, mongotls.ErrIncompleteClientCert},
		"key only":        {models.ConnectionTLS{ClientKeyPEM: client.KeyPEM}, mongotls.ErrIncompleteClientCert},
		"password only":   {models.ConnectionTLS{ClientKeyPassword: "x"}, mongotls.ErrIncompleteClientCert},
		"mismatched key":  {models.ConnectionTLS{ClientCertPEM: client.CertPEM, ClientKeyPEM: other.KeyPEM}, mongotls.ErrInvalidClientCert},
		"garbage key":     {models.ConnectionTLS{ClientCertPEM: client.CertPEM, ClientKeyPEM: "nope"}, mongotls.ErrInvalidClientCert},
		"legacy key":      {models.ConnectionTLS{ClientCertPEM: client.CertPEM, ClientKeyPEM: legacy, ClientKeyPassword: "x"}, mongotls.ErrLegacyEncryptedKey},
		"too large":       {models.ConnectionTLS{CAPEM: strings.Repeat("a", mongotls.MaxPEMSize+1)}, mongotls.ErrTooLarge},
		"flags only (ok)": {models.ConnectionTLS{AllowInvalidHostnames: true}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := mongotls.Validate(&tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Validate = %v, want %v", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), "BEGIN") {
				t.Fatal("error quotes PEM content")
			}
		})
	}
}

// verify checks the certificate of server against cfg as a TLS client connecting to
// host would, without a network: the built-in verification (roots and hostname)
// unless cfg skips it, then cfg.VerifyConnection.
func verify(t *testing.T, server *mongotlstest.Pair, cfg *tls.Config, host string) error {
	t.Helper()
	block, _ := pem.Decode([]byte(server.CertPEM))
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.InsecureSkipVerify {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: cfg.RootCAs, DNSName: host}); err != nil {
			return err
		}
	}
	if cfg.VerifyConnection != nil {
		return cfg.VerifyConnection(tls.ConnectionState{ServerName: host, PeerCertificates: []*x509.Certificate{leaf}})
	}
	return nil
}

func TestConfigVerification(t *testing.T) {
	ca := newCA(t)
	server, err := ca.Server("db.example")
	if err != nil {
		t.Fatal(err)
	}
	strict, err := mongotls.Config(&models.ConnectionTLS{CAPEM: ca.CertPEM})
	if err != nil {
		t.Fatal(err)
	}
	if err = verify(t, server, strict, "db.example"); err != nil {
		t.Fatalf("strict, right host: %v", err)
	}
	if err = verify(t, server, strict, "other.example"); err == nil {
		t.Fatal("strict accepted a wrong hostname")
	}
	loose, err := mongotls.Config(&models.ConnectionTLS{CAPEM: ca.CertPEM, AllowInvalidHostnames: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = verify(t, server, loose, "other.example"); err != nil {
		t.Fatalf("allow invalid hostnames: %v", err)
	}
	foreign, err := mongotls.Config(&models.ConnectionTLS{CAPEM: newCA(t).CertPEM, AllowInvalidHostnames: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = verify(t, server, foreign, "db.example"); err == nil {
		t.Fatal("allow invalid hostnames accepted an untrusted chain")
	}
	insecure, err := mongotls.Config(&models.ConnectionTLS{Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	if !insecure.InsecureSkipVerify || insecure.VerifyConnection != nil {
		t.Fatalf("insecure config verifies: %+v", insecure)
	}
}

func TestDecryptKeyReencodes(t *testing.T) {
	client := newClient(t, newCA(t))
	enc, err := client.EncryptedKeyPEM("pw")
	if err != nil {
		t.Fatal(err)
	}
	out, err := mongotls.DecryptKey(enc, "pw")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(out)
	if block == nil || block.Type != "PRIVATE KEY" {
		t.Fatalf("decrypted key is not PKCS#8 PEM: %v", block)
	}
	if _, err := x509.ParsePKCS8PrivateKey(block.Bytes); err != nil {
		t.Fatal(err)
	}
}

func TestContext(t *testing.T) {
	ctx := context.Background()
	if mongotls.FromContext(ctx) != nil {
		t.Fatal("empty context carries material")
	}
	in := &models.ConnectionTLS{CAPEM: "x"}
	ctx = mongotls.NewContext(ctx, in)
	got := mongotls.FromContext(ctx)
	if got == nil || got.CAPEM != "x" {
		t.Fatalf("FromContext = %v", got)
	}
	in.CAPEM = "changed"
	if mongotls.FromContext(ctx).CAPEM != "x" {
		t.Fatal("context shares the caller's struct")
	}
	if mongotls.FromContext(mongotls.NewContext(ctx, nil)) != nil {
		t.Fatal("zero material does not replace the outer connection's")
	}
}

func TestRedacted(t *testing.T) {
	in := &models.ConnectionTLS{CAPEM: "ca", ClientCertPEM: "cert", ClientKeyPEM: "key", ClientKeyPassword: "pw"}
	out := in.Redacted()
	if out.ClientKeyPEM == "key" || out.ClientKeyPassword == "pw" || out.CAPEM != "ca" || out.ClientCertPEM != "cert" {
		t.Fatalf("Redacted = %+v", out)
	}
	if in.ClientKeyPEM != "key" {
		t.Fatal("Redacted changed the original")
	}
	c := &models.Connection{URI: "mongodb://h", ConnectionTLS: *in}
	if r := c.Redacted(); r.ClientKeyPEM == "key" || r.ClientKeyPassword == "pw" {
		t.Fatalf("Connection.Redacted leaks the key: %+v", r.ConnectionTLS)
	}
}
