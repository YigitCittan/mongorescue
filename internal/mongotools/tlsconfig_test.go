package mongotools_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotools"
)

const (
	testCA   = "-----BEGIN CERTIFICATE-----\nca\n-----END CERTIFICATE-----\n"
	testCert = "-----BEGIN CERTIFICATE-----\ncert\n-----END CERTIFICATE-----\n"
	testKey  = "-----BEGIN ENCRYPTED PRIVATE KEY-----\nkey\n-----END ENCRYPTED PRIVATE KEY-----\n"
)

// argValue returns the value of the "--name=" argument in args.
func argValue(t *testing.T, args []string, name string) string {
	t.Helper()
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
	}
	t.Fatalf("no %s in %v", name, args)
	return ""
}

func checkPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != want {
		t.Fatalf("%s has mode %v, want %v", filepath.Base(path), info.Mode().Perm(), want)
	}
}

func TestWriteConfigWithoutTLSIsURIConfig(t *testing.T) {
	dir := t.TempDir()
	args, cleanup, err := mongotools.WriteConfig(dir, "mongodb://h/", &models.ConnectionTLS{})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(args) != 1 || !strings.HasPrefix(args[0], "--config=") {
		t.Fatalf("args = %v", args)
	}
	cleanup()
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left behind %v", entries)
	}
}

func TestWriteConfigWithTLS(t *testing.T) {
	dir := t.TempDir()
	uri := "mongodb://h/?authMechanism=MONGODB-X509"
	material := &models.ConnectionTLS{CAPEM: testCA, ClientCertPEM: testCert, ClientKeyPEM: testKey,
		ClientKeyPassword: "it's: a #secret", AllowInvalidHostnames: true}
	args, cleanup, err := mongotools.WriteConfig(dir, uri, material)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	configPath := argValue(t, args, "--config")
	caPath := argValue(t, args, "--sslCAFile")
	keyPath := argValue(t, args, "--sslPEMKeyFile")
	if !slices.Contains(args, "--ssl") || !slices.Contains(args, "--sslAllowInvalidHostnames") || slices.Contains(args, "--sslAllowInvalidCertificates") {
		t.Fatalf("args = %v", args)
	}
	// No secret is ever an argument.
	for _, a := range args {
		if strings.Contains(a, "secret") || strings.Contains(a, "BEGIN") || strings.Contains(a, "mongodb://") {
			t.Fatalf("argument %q carries a secret", a)
		}
	}
	tlsDir := filepath.Dir(configPath)
	if filepath.Dir(caPath) != tlsDir || filepath.Dir(keyPath) != tlsDir || filepath.Dir(tlsDir) != dir {
		t.Fatalf("files outside one private directory of %s: %v", dir, args)
	}
	checkPerm(t, tlsDir, 0o700)
	for _, p := range []string{configPath, caPath, keyPath} {
		checkPerm(t, p, 0o600)
	}

	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "uri: '" + uri + "'\nsslPEMKeyPassword: 'it''s: a #secret'\n"
	if string(config) != want {
		t.Fatalf("config = %q, want %q", config, want)
	}
	if b, _ := os.ReadFile(caPath); string(b) != testCA {
		t.Fatalf("ca.pem = %q", b)
	}
	if b, _ := os.ReadFile(keyPath); string(b) != testCert+testKey {
		t.Fatalf("client.pem = %q", b)
	}

	cleanup()
	cleanup() // idempotent
	if _, err := os.Stat(tlsDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("TLS directory survived cleanup: %v", err)
	}
}

func TestWriteConfigInsecureAndCAOnly(t *testing.T) {
	args, cleanup, err := mongotools.WriteConfig(t.TempDir(), "mongodb://h/", &models.ConnectionTLS{Insecure: true, AllowInvalidHostnames: true})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !slices.Contains(args, "--sslAllowInvalidCertificates") || slices.Contains(args, "--sslCAFile") {
		t.Fatalf("args = %v", args)
	}
	if n := len(slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a != "--sslAllowInvalidHostnames" })); n != 1 {
		t.Fatalf("--sslAllowInvalidHostnames given %d times: %v", n, args)
	}
	config, err := os.ReadFile(argValue(t, args, "--config"))
	if err != nil || strings.Contains(string(config), "sslPEMKeyPassword") {
		t.Fatalf("config = %q, %v", config, err)
	}
}

func TestWriteConfigRejectsControlCharacters(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		uri, password string
		want          error
	}{
		{"mongodb://h/\nx", "", mongotools.ErrInvalidURI},
		{"mongodb://h/", "pass\nuri: evil", mongotools.ErrInvalidKeyPassword},
	} {
		material := &models.ConnectionTLS{CAPEM: testCA, ClientKeyPassword: tc.password}
		if _, _, err := mongotools.WriteConfig(dir, tc.uri, material); !errors.Is(err, tc.want) {
			t.Fatalf("WriteConfig = %v, want %v", err, tc.want)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a failed call left %v", entries)
	}
}

func TestWriteConfigMissingDirectory(t *testing.T) {
	_, _, err := mongotools.WriteConfig(filepath.Join(t.TempDir(), "missing"), "mongodb://h/", &models.ConnectionTLS{CAPEM: testCA})
	if err == nil {
		t.Fatal("WriteConfig into a missing directory succeeded")
	}
}

func TestCleanupStaleRemovesTLSDirectories(t *testing.T) {
	dir := t.TempDir()
	args, _, err := mongotools.WriteConfig(dir, "mongodb://h/", &models.ConnectionTLS{CAPEM: testCA})
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Dir(argValue(t, args, "--config"))
	past := time.Now().Add(-time.Hour)
	if err = os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	fresh, freshCleanup, err := mongotools.WriteConfig(dir, "mongodb://h/", &models.ConnectionTLS{CAPEM: testCA})
	if err != nil {
		t.Fatal(err)
	}
	defer freshCleanup()

	removed, err := mongotools.CleanupStale(dir, time.Minute)
	if err != nil || removed != 1 {
		t.Fatalf("CleanupStale = %d, %v", removed, err)
	}
	if _, err = os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale TLS directory kept")
	}
	if _, err = os.Stat(argValue(t, fresh, "--config")); err != nil {
		t.Fatalf("fresh TLS directory removed: %v", err)
	}
}
