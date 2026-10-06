//go:build integration

// Command gencerts writes a throwaway certificate authority and the certificates the
// TLS integration run needs into a directory, using crypto/x509 (see
// internal/mongotls/mongotlstest). scripts/test-integration-docker.sh runs it:
//
//	ca.pem                 the CA certificate
//	server.pem             server certificate (localhost, 127.0.0.1) followed by its key, for mongod
//	client.crt             x509 client certificate
//	client.key             its key, encrypted PKCS#8
//	client.key.password    the key's password
//	client.subject         the client certificate subject (RFC 2253), the $external user name
//
// Usage: go run -tags integration ./internal/integration/gencerts <dir>
package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yigitcittan/mongorescue/internal/mongotls/mongotlstest"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gencerts <dir>")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "gencerts:", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	ca, err := mongotlstest.NewCA()
	if err != nil {
		return err
	}
	server, err := ca.Server("localhost", "127.0.0.1")
	if err != nil {
		return err
	}
	client, err := ca.Client("mongorescue-it")
	if err != nil {
		return err
	}
	b := make([]byte, 12)
	if _, err = rand.Read(b); err != nil {
		return err
	}
	password := "kp" + hex.EncodeToString(b)
	encrypted, err := client.EncryptedKeyPEM(password)
	if err != nil {
		return err
	}
	files := map[string]string{
		"ca.pem":              ca.CertPEM,
		"server.pem":          server.CertPEM + server.KeyPEM,
		"client.crt":          client.CertPEM,
		"client.key":          encrypted,
		"client.key.password": password,
		"client.subject":      client.Subject,
	}
	for name, content := range files {
		// mongod in the container runs as another user and must read the
		// certificates; the directory itself is private to the test run.
		mode := os.FileMode(0o644)
		if name == "client.key" || name == "client.key.password" {
			mode = 0o600
		}
		if err = os.WriteFile(filepath.Join(dir, name), []byte(content), mode); err != nil {
			return err
		}
	}
	return nil
}
