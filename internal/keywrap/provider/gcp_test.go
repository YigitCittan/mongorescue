package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/oauth2"

	"github.com/yigitcittan/mongorescue/internal/keywrap"
)

const gcpKeyName = "projects/p-1/locations/europe-west1/keyRings/ring/cryptoKeys/mongorescue"

// fakeGCPKMS answers the encrypt and decrypt REST methods of a crypto key, with
// the CRC32C fields of the Cloud KMS contract.
type fakeGCPKMS struct {
	paths   []string
	auth    []string
	bodies  []map[string]any
	status  int
	corrupt bool
}

func (f *fakeGCPKMS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.bodies = append(f.bodies, body)
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"Permission 'cloudkms.cryptoKeyVersions.useToDecrypt' denied","status":"PERMISSION_DENIED"}}`))
		return
	}
	decode := func(field string) []byte {
		var b []byte
		raw, _ := json.Marshal(body[field])
		_ = json.Unmarshal(raw, &b)
		return b
	}
	switch r.URL.Path {
	case "/v1/" + gcpKeyName + ":encrypt":
		pt := decode("plaintext")
		if body["plaintextCrc32c"] != checksum(pt) || body["additionalAuthenticatedDataCrc32c"] != checksum(decode("additionalAuthenticatedData")) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		ct := append([]byte("gcp:"), pt...)
		crc := checksum(ct)
		if f.corrupt {
			crc = "1"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": gcpKeyName + "/cryptoKeyVersions/3", "ciphertext": ct, "ciphertextCrc32c": crc,
			"verifiedPlaintextCrc32c": true, "verifiedAdditionalAuthenticatedDataCrc32c": true})
	case "/v1/" + gcpKeyName + ":decrypt":
		ct := decode("ciphertext")
		if body["ciphertextCrc32c"] != checksum(ct) || string(decode("additionalAuthenticatedData")) != keywrap.Purpose {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		pt := bytes.TrimPrefix(ct, []byte("gcp:"))
		_ = json.NewEncoder(w).Encode(map[string]any{"plaintext": pt, "plaintextCrc32c": checksum(pt)})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestGCPKMSRequests(t *testing.T) {
	fake := &fakeGCPKMS{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	w, err := OpenWith(context.Background(), keywrap.Spec{Provider: keywrap.ProviderGCPKMS, KeyURI: gcpKeyName}, Options{
		HTTPClient: srv.Client(), GCPEndpoint: srv.URL + "/", GCPTokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "ya29.test"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if w.Provider() != keywrap.ProviderGCPKMS || w.KeyID() != gcpKeyName {
		t.Fatalf("Provider/KeyID = %s %s", w.Provider(), w.KeyID())
	}
	key := bytes.Repeat([]byte{9}, 32)
	ct, err := w.Wrap(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.Unwrap(context.Background(), ct)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("Unwrap = %v", err)
	}
	want := []string{"POST /v1/" + gcpKeyName + ":encrypt", "POST /v1/" + gcpKeyName + ":decrypt"}
	for i := range want {
		if fake.paths[i] != want[i] || fake.auth[i] != "Bearer ya29.test" {
			t.Errorf("request %d = %s (%s)", i, fake.paths[i], fake.auth[i])
		}
	}

	fake.corrupt = true
	if _, err = w.Wrap(context.Background(), key); !errors.Is(err, errGCPIntegrity) || keywrap.IsPermanent(err) {
		t.Errorf("corrupted response = %v; want a transient integrity error", err)
	}
	fake.status = http.StatusForbidden
	_, err = w.Unwrap(context.Background(), ct)
	if !keywrap.IsPermanent(err) || !bytes.Contains([]byte(err.Error()), []byte("PERMISSION_DENIED")) {
		t.Errorf("403 = %v; want a permanent PERMISSION_DENIED error", err)
	}
	fake.status = http.StatusServiceUnavailable
	if _, err = w.Unwrap(context.Background(), ct); err == nil || keywrap.IsPermanent(err) {
		t.Errorf("503 = %v; want a transient error", err)
	}
}
