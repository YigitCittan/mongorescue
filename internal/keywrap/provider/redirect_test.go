package provider

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"golang.org/x/oauth2"

	"github.com/yigitcittan/mongorescue/internal/keywrap"
)

// elsewhere is another host a KMS redirects to; it counts the requests (and the
// credentials) that reach it.
type elsewhere struct {
	hits, withCredentials atomic.Int64
	srv                   *httptest.Server
}

func newElsewhere(t *testing.T, tls bool) *elsewhere {
	t.Helper()
	e := &elsewhere{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hits.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Vault-Token") != "" || r.Header.Get("X-Amz-Security-Token") != "" {
			e.withCredentials.Add(1)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	if tls {
		e.srv = httptest.NewTLSServer(h)
	} else {
		e.srv = httptest.NewServer(h)
	}
	t.Cleanup(e.srv.Close)
	return e
}

// redirector answers every request with a 307 to target (after the Key Vault
// challenge, when challenge is set).
func redirector(target string, challenge bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if challenge && r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Bearer authorization="https://login.microsoftonline.com/00000000-0000-0000-0000-000000000000", resource="https://vault.azure.net"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, target+"/redirected", http.StatusTemporaryRedirect)
	})
}

// checkRedirectRefused asserts that a call failed permanently on the redirect and
// that nothing reached the other host.
func checkRedirectRefused(t *testing.T, e *elsewhere, err error) {
	t.Helper()
	if err == nil || !keywrap.IsPermanent(err) || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("call answered with a redirect = %v; want a permanent redirect error", err)
	}
	if e.hits.Load() != 0 {
		t.Fatalf("%d request(s) followed the redirect to another host (%d with credentials)", e.hits.Load(), e.withCredentials.Load())
	}
}

func TestProvidersFollowNoRedirect(t *testing.T) {
	ctx := context.Background()
	key := bytes.Repeat([]byte{1}, 32)

	t.Run("vault-transit", func(t *testing.T) {
		e := newElsewhere(t, false)
		srv := httptest.NewServer(redirector(e.srv.URL, false))
		defer srv.Close()
		// The test server's own client follows redirects: the provider must not.
		w, err := OpenWith(ctx, keywrap.Spec{Provider: keywrap.ProviderVaultTransit, KeyURI: srv.URL + "/transit/keys/k"},
			Options{HTTPClient: srv.Client(), Getenv: func(k string) string { return map[string]string{EnvVaultToken: "s.tok"}[k] }, AllowInsecure: true})
		if err != nil {
			t.Fatal(err)
		}
		_, err = w.Wrap(ctx, key)
		checkRedirectRefused(t, e, err)
	})
	t.Run("vault-transit login", func(t *testing.T) {
		e := newElsewhere(t, false)
		srv := httptest.NewServer(redirector(e.srv.URL, false))
		defer srv.Close()
		env := map[string]string{EnvVaultRoleID: "r", EnvVaultSecretID: "s"}
		w, err := OpenWith(ctx, keywrap.Spec{Provider: keywrap.ProviderVaultTransit, KeyURI: srv.URL + "/transit/keys/k"},
			Options{Getenv: func(k string) string { return env[k] }, AllowInsecure: true})
		if err != nil {
			t.Fatal(err)
		}
		_, err = w.Unwrap(ctx, []byte("vault:v1:abc"))
		checkRedirectRefused(t, e, err)
	})
	t.Run("gcp-kms", func(t *testing.T) {
		e := newElsewhere(t, false)
		srv := httptest.NewServer(redirector(e.srv.URL, false))
		defer srv.Close()
		w, err := OpenWith(ctx, keywrap.Spec{Provider: keywrap.ProviderGCPKMS, KeyURI: gcpKeyName}, Options{HTTPClient: srv.Client(),
			GCPEndpoint: srv.URL, GCPTokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "ya29.secret"})})
		if err != nil {
			t.Fatal(err)
		}
		_, err = w.Wrap(ctx, key)
		checkRedirectRefused(t, e, err)
	})
	t.Run("aws-kms", func(t *testing.T) {
		e := newElsewhere(t, false)
		srv := httptest.NewServer(redirector(e.srv.URL, false))
		defer srv.Close()
		cfg := aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("AKID", "secret", "session-token"),
			BaseEndpoint: aws.String(srv.URL), RetryMaxAttempts: 1, HTTPClient: srv.Client()}
		w, err := OpenWith(ctx, keywrap.Spec{Provider: keywrap.ProviderAWSKMS, KeyURI: "alias/k"}, Options{AWSConfig: &cfg})
		if err != nil {
			t.Fatal(err)
		}
		_, err = w.Wrap(ctx, key)
		checkRedirectRefused(t, e, err)
	})
	t.Run("azure-keyvault", func(t *testing.T) {
		e := newElsewhere(t, true)
		srv := httptest.NewTLSServer(redirector(e.srv.URL, true))
		defer srv.Close()
		// A transport that would follow redirects (and trusts both test servers).
		follow := &http.Client{Transport: srv.Client().Transport}
		follow.Transport.(*http.Transport).TLSClientConfig.RootCAs.AddCert(e.srv.Certificate())
		w, err := OpenWith(ctx, keywrap.Spec{Provider: keywrap.ProviderAzureKeyVault, KeyURI: srv.URL + "/keys/k"}, Options{
			AzureCredential: &fakeCredential{},
			AzureClientOptions: &azkeys.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: follow, Retry: policy.RetryOptions{MaxRetries: -1}},
				DisableChallengeResourceVerification: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = w.Wrap(ctx, key)
		checkRedirectRefused(t, e, err)
	})
}

func TestPlainHTTPNeedsTheOptIn(t *testing.T) {
	spec := keywrap.Spec{Provider: keywrap.ProviderVaultTransit, KeyURI: "http://127.0.0.1:8200/transit/keys/k"}
	if !spec.Insecure() {
		t.Fatal("Insecure = false for http://")
	}
	env := func(k string) string { return map[string]string{EnvVaultToken: "t"}[k] }
	if _, err := OpenWith(context.Background(), spec, Options{Getenv: env}); !errors.Is(err, keywrap.ErrInsecureKeyURI) {
		t.Fatalf("http:// without the opt-in = %v; want ErrInsecureKeyURI", err)
	}
	if _, err := OpenWith(context.Background(), spec, Options{Getenv: env, AllowInsecure: true}); err != nil {
		t.Fatalf("http:// with the opt-in = %v", err)
	}
	t.Setenv(keywrap.EnvKeyWrapAllowInsecure, "")
	t.Setenv(EnvVaultToken, "t")
	if _, err := Open(context.Background(), spec); !errors.Is(err, keywrap.ErrInsecureKeyURI) {
		t.Fatalf("Open of http:// without the variable = %v", err)
	}
	t.Setenv(keywrap.EnvKeyWrapAllowInsecure, "true")
	if _, err := Open(context.Background(), spec); err != nil {
		t.Fatalf("Open of http:// with %s=true = %v", keywrap.EnvKeyWrapAllowInsecure, err)
	}
	if (keywrap.Spec{Provider: keywrap.ProviderVaultTransit, KeyURI: "https://v:8200/transit/keys/k"}).Insecure() {
		t.Fatal("Insecure = true for https://")
	}
}
