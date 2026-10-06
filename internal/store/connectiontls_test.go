package store_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// Markers inside the TLS secrets of tlsConnection.
const (
	tlsKeyMarker      = "client-key-marker-5e1d"
	tlsPasswordMarker = "key-password-marker-9b2c"
)

func tlsConnection(id string) *models.Connection {
	now := time.Now().UTC()
	return &models.Connection{ID: id, Name: id, URI: "mongodb://h/?authMechanism=MONGODB-X509", CreatedAt: now, UpdatedAt: now,
		ConnectionTLS: models.ConnectionTLS{
			CAPEM:                 "-----BEGIN CERTIFICATE-----\nca\n-----END CERTIFICATE-----\n",
			ClientCertPEM:         "-----BEGIN CERTIFICATE-----\ncert\n-----END CERTIFICATE-----\n",
			ClientKeyPEM:          "-----BEGIN ENCRYPTED PRIVATE KEY-----\n" + tlsKeyMarker + "\n-----END ENCRYPTED PRIVATE KEY-----\n",
			ClientKeyPassword:     tlsPasswordMarker,
			AllowInvalidHostnames: true,
		}}
}

func TestConnectionTLSSecretsAreSealedAtRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.OpenWithBox(t, path, testBox)
	ctx := context.Background()
	want := tlsConnection("conn_a")
	if err := s.SaveConnection(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetConnection(ctx, "conn_a")
	if err != nil || got.ConnectionTLS != want.ConnectionTLS {
		t.Fatalf("read back %+v, %v", got, err)
	}
	list, err := s.ListConnections(ctx)
	if err != nil || len(list) != 1 || list[0].ConnectionTLS != want.ConnectionTLS {
		t.Fatalf("list = %+v, %v", list, err)
	}

	raw := rawData(t, path, "connections")
	if strings.Contains(raw, tlsKeyMarker) || strings.Contains(raw, tlsPasswordMarker) ||
		!strings.Contains(raw, `"tls_client_key_pem":"`+secretbox.Prefix) ||
		!strings.Contains(raw, `"tls_client_key_password":"`+secretbox.Prefix) {
		t.Fatalf("stored connection = %s", raw)
	}
	// The certificates are public and stay readable.
	if !strings.Contains(raw, `"tls_ca_pem":"-----BEGIN CERTIFICATE-----`) {
		t.Fatalf("stored connection lacks the CA = %s", raw)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		b, readErr := os.ReadFile(f)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
		if bytes.Contains(b, []byte(tlsKeyMarker)) || bytes.Contains(b, []byte(tlsPasswordMarker)) {
			t.Fatalf("%s holds a TLS secret in plain form", filepath.Base(f))
		}
	}
}

// TestConnectionTLSSecretsSurviveAKeyRotation rotates secret.key and restarts with
// the new key: the client key and its password are re-sealed and open again.
func TestConnectionTLSSecretsSurviveAKeyRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.OpenWithBox(t, path, storetest.NewBox(t))
	ctx := context.Background()
	want := tlsConnection("conn_a")
	if err := s.SaveConnection(ctx, want); err != nil {
		t.Fatal(err)
	}
	var keyBefore, pwBefore string
	if err := rawDB(t, path).QueryRow(`SELECT json_extract(data, '$.tls_client_key_pem'), json_extract(data, '$.tls_client_key_password')
		FROM connections WHERE id = 'conn_a'`).Scan(&keyBefore, &pwBefore); err != nil {
		t.Fatal(err)
	}

	next := storetest.NewBox(t)
	res, err := s.RotateSecretBox(ctx, store.SecretKeyRotation{Next: next})
	if err != nil || len(res.Skipped) != 0 {
		t.Fatalf("rotation = %+v, %v; want nothing skipped", res, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s = storetest.OpenWithBox(t, path, next)
	got, err := s.GetConnection(ctx, "conn_a")
	if err != nil || got.ConnectionTLS != want.ConnectionTLS {
		t.Fatalf("after the rotation = %+v, %v", got, err)
	}
	var keyAfter, pwAfter string
	if err = rawDB(t, path).QueryRow(`SELECT json_extract(data, '$.tls_client_key_pem'), json_extract(data, '$.tls_client_key_password')
		FROM connections WHERE id = 'conn_a'`).Scan(&keyAfter, &pwAfter); err != nil {
		t.Fatal(err)
	}
	if keyAfter == keyBefore || pwAfter == pwBefore {
		t.Fatal("the TLS secrets were not re-sealed")
	}
}

func TestConnectionTLSSecretsAreBoundToTheirConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.OpenWithBox(t, path, testBox)
	ctx := context.Background()
	for _, id := range []string{"conn_a", "conn_b"} {
		if err := s.SaveConnection(ctx, tlsConnection(id)); err != nil {
			t.Fatal(err)
		}
	}
	for _, field := range []string{"tls_client_key_pem", "tls_client_key_password"} {
		if _, err := rawDB(t, path).Exec(`UPDATE connections SET data = json_set(data, '$.` + field + `',
			(SELECT json_extract(data, '$.` + field + `') FROM connections WHERE id = 'conn_a')) WHERE id = 'conn_b'`); err != nil {
			t.Fatal(err)
		}
		if c, err := s.GetConnection(ctx, "conn_b"); err == nil {
			t.Fatalf("a copied sealed %s opened: %+v", field, c.Redacted())
		}
		if _, err := rawDB(t, path).Exec(`UPDATE connections SET data = json_set(data, '$.` + field + `', 'plain') WHERE id = 'conn_b'`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetConnection(ctx, "conn_b"); !errors.Is(err, store.ErrUnsealedSecret) {
			t.Fatalf("planted plain %s = %v; want ErrUnsealedSecret", field, err)
		}
		if err := s.SaveConnection(ctx, tlsConnection("conn_b")); err != nil {
			t.Fatal(err)
		}
	}
}

// TestConnectionTLSForcesTLSOnLoad loads a connection with TLS material whose
// stored URI does not ask for TLS (saved before that was enforced): it comes back
// with tls=true.
func TestConnectionTLSForcesTLSOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.OpenWithBox(t, path, testBox)
	ctx := context.Background()
	c := tlsConnection("conn_a")
	c.URI = "mongodb://h:27017/?authMechanism=MONGODB-X509"
	if err := s.SaveConnection(ctx, c); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetConnection(ctx, "conn_a")
	if err != nil || got.URI != "mongodb://h:27017/?authMechanism=MONGODB-X509&tls=true" {
		t.Fatalf("loaded URI = %q, %v", got.URI, err)
	}
}

// TestUnreadableTLSSecretErrorHoldsNoValue damages the sealed key password of a
// connection (an unknown format version): loading it fails with an error that
// names the location only, never any part of the stored value, since such errors
// are logged by the scheduler and the bulk operations.
func TestUnreadableTLSSecretErrorHoldsNoValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.OpenWithBox(t, path, testBox)
	ctx := context.Background()
	if err := s.SaveConnection(ctx, tlsConnection("conn_a")); err != nil {
		t.Fatal(err)
	}
	var sealed string
	if err := rawDB(t, path).QueryRow(`SELECT json_extract(data, '$.tls_client_key_password') FROM connections WHERE id = 'conn_a'`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, secretbox.Prefix))
	if err != nil {
		t.Fatal(err)
	}
	raw[0] = 0xd7
	damaged := secretbox.Prefix + base64.StdEncoding.EncodeToString(raw)
	if _, err = rawDB(t, path).Exec(`UPDATE connections SET data = json_set(data, '$.tls_client_key_password', ?) WHERE id = 'conn_a'`, damaged); err != nil {
		t.Fatal(err)
	}
	_, err = s.GetConnection(ctx, "conn_a")
	if err == nil {
		t.Fatal("a damaged key password opened")
	}
	msg := err.Error()
	body := damaged[len(secretbox.Prefix):]
	if strings.Contains(msg, "215") || strings.Contains(msg, body[:12]) || strings.Contains(msg, tlsPasswordMarker) {
		t.Fatalf("error %q carries data from the stored value", msg)
	}
}
