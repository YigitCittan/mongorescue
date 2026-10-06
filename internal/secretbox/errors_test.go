package secretbox

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// TestOpenErrorsCarryNothingFromTheValue checks that the errors of Open, which
// reach logs, hold no part of the value they were given: not the version byte,
// not the payload.
func TestOpenErrorsCarryNothingFromTheValue(t *testing.T) {
	b := newBox(t)
	at := At("connections", "conn_a", "tls_client_key_password")
	sealed, err := b.Seal(at, "pw-secret-7731")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, Prefix))
	if err != nil {
		t.Fatal(err)
	}
	raw[0] = 0xd7 // an unknown format version
	payload := base64.StdEncoding.EncodeToString(raw)
	_, err = b.Open(at, Prefix+payload)
	if !errors.Is(err, ErrUnsupportedVersion) || !errors.Is(err, ErrMalformed) {
		t.Fatalf("unknown version = %v; want ErrUnsupportedVersion", err)
	}
	msg := err.Error()
	if strings.Contains(msg, strconv.Itoa(int(raw[0]))) || strings.Contains(msg, payload[:12]) {
		t.Fatalf("error %q carries data from the sealed value", msg)
	}
}
