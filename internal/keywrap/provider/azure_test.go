package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"

	"github.com/yigitcittan/mongorescue/internal/keywrap"
)

// fakeCredential hands out a fixed token and records the scopes asked for.
type fakeCredential struct {
	mu     sync.Mutex
	scopes []string
}

func (c *fakeCredential) GetToken(_ context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	c.mu.Lock()
	c.scopes = append(c.scopes, opts.Scopes...)
	c.mu.Unlock()
	return azcore.AccessToken{Token: "aad-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// fakeKeyVault answers wrapkey and unwrapkey of the Key Vault REST API behind
// the challenge-based authentication: an anonymous request gets the 401
// challenge, an authorized one the result ("w:" + the key, by key version).
type fakeKeyVault struct {
	mu       sync.Mutex
	requests []string
	bodies   []map[string]string
	status   int
}

func (f *fakeKeyVault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer aad-token" {
		w.Header().Set("WWW-Authenticate", `Bearer authorization="https://login.microsoftonline.com/00000000-0000-0000-0000-000000000000", resource="https://vault.azure.net"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.requests = append(f.requests, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.bodies = append(f.bodies, body)
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(`{"error":{"code":"Forbidden","message":"Operation unwrapKey is not permitted on this key."}}`))
		return
	}
	value, _ := base64.RawURLEncoding.DecodeString(body["value"])
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // keys/<name>/<version>/<op>
	version := parts[2]
	if version == "" || len(parts) == 3 {
		version = "v2"
	}
	kid := "https://" + r.Host + "/keys/" + parts[1] + "/" + version
	var result []byte
	switch parts[len(parts)-1] {
	case "wrapkey":
		result = append([]byte("w:"+version+":"), value...)
	case "unwrapkey":
		result = bytes.TrimPrefix(value, []byte("w:"+version+":"))
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"kid": kid, "value": base64.RawURLEncoding.EncodeToString(result)})
}

func TestAzureKeyVaultRequests(t *testing.T) {
	fake := &fakeKeyVault{}
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()
	cred := &fakeCredential{}
	keyURI := srv.URL + "/keys/mongorescue"
	w, err := OpenWith(context.Background(), keywrap.Spec{Provider: keywrap.ProviderAzureKeyVault, KeyURI: keyURI}, Options{
		AzureCredential: cred,
		AzureClientOptions: &azkeys.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: srv.Client(), Retry: policy.RetryOptions{MaxRetries: -1}},
			DisableChallengeResourceVerification: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if w.Provider() != keywrap.ProviderAzureKeyVault || w.KeyID() != keyURI {
		t.Fatalf("Provider/KeyID = %s %s", w.Provider(), w.KeyID())
	}
	key := bytes.Repeat([]byte{5}, 32)
	ct, err := w.Wrap(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(ct), "azkv1:v2:") {
		t.Fatalf("ciphertext %q does not record the key version", ct)
	}
	got, err := w.Unwrap(context.Background(), ct)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("Unwrap = %v", err)
	}
	if len(fake.requests) != 2 || !strings.HasPrefix(fake.requests[0], "POST /keys/mongorescue/wrapkey?api-version=") ||
		!strings.HasPrefix(fake.requests[1], "POST /keys/mongorescue/v2/unwrapkey?api-version=") {
		t.Errorf("requests = %v", fake.requests)
	}
	for i, b := range fake.bodies {
		if b["alg"] != "RSA-OAEP-256" {
			t.Errorf("request %d alg = %q", i, b["alg"])
		}
	}
	if len(cred.scopes) == 0 || cred.scopes[0] != "https://vault.azure.net/.default" {
		t.Errorf("token scopes = %v", cred.scopes)
	}

	if _, err = w.Unwrap(context.Background(), []byte("vault:v1:abc")); !keywrap.IsPermanent(err) {
		t.Errorf("foreign ciphertext = %v; want permanent", err)
	}
	fake.status = http.StatusForbidden
	if _, err = w.Unwrap(context.Background(), ct); !keywrap.IsPermanent(err) || !strings.Contains(err.Error(), "403") {
		t.Errorf("403 = %v; want permanent", err)
	}
	fake.status = http.StatusInternalServerError
	if _, err = w.Unwrap(context.Background(), ct); err == nil || keywrap.IsPermanent(err) {
		t.Errorf("500 = %v; want transient", err)
	}
}

func TestSplitAzureCiphertext(t *testing.T) {
	for _, bad := range []string{"", "azkv1:", "azkv1::abc", "azkv1:v1:!!!", "azkv1:v1:", "other"} {
		if _, _, err := splitAzureCiphertext([]byte(bad)); err == nil {
			t.Errorf("splitAzureCiphertext(%q) succeeded", bad)
		}
	}
}
