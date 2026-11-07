package storage

import (
	"context"
	"encoding/pem"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/netguard"
)

// policyStore builds an S3 driver for endpoint with the given policy and CA.
func policyStore(t *testing.T, endpoint, caPEM string, policy *netguard.Policy) *S3Storage {
	t.Helper()
	isolateAWSEnv(t)
	store, err := NewS3Storage(context.Background(), S3Config{
		Endpoint: endpoint, Bucket: fakeBucket, Region: "us-east-1",
		AccessKey: "test-access-key", SecretKey: "test-secret-key", UsePathStyle: true,
		CAPEM: caPEM, Policy: policy,
	})
	if err != nil {
		t.Fatalf("NewS3Storage: %v", err)
	}
	return store
}

// certPEM returns the PEM of the certificate srv presents (a self-signed CA, as a
// private CA of an on-premises MinIO would be for its own server).
func certPEM(srv *httptest.Server) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
}

func TestS3RefusesMetadataAndLinkLocalEndpoints(t *testing.T) {
	for _, endpoint := range []string{
		"http://169.254.169.254",
		"http://[fd00:ec2::254]",
		"http://[64:ff9b::a9fe:a9fe]",
		"http://169.254.170.2",
	} {
		store := policyStore(t, endpoint, "", &netguard.Policy{})
		_, err := store.Stat(context.Background(), "key")
		if !errors.Is(err, netguard.ErrBlockedDestination) {
			t.Errorf("%s: %v; want netguard.ErrBlockedDestination", endpoint, err)
		}
	}
}

func TestS3RefusesPrivateEndpointsWhenDenied(t *testing.T) {
	f := &fakeS3{objects: map[string][]byte{"key": []byte("v")}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	policy := &netguard.Policy{}
	store := policyStore(t, srv.URL, "", policy)
	if _, err := store.Stat(context.Background(), "key"); err != nil {
		t.Fatalf("loopback endpoint while private addresses are allowed: %v", err)
	}
	// The policy is read for every new connection (a pooled keep-alive connection
	// of the first driver lives on for up to s3IdleConnTimeout).
	policy.SetDenyPrivate(true)
	_, err := policyStore(t, srv.URL, "", policy).Stat(context.Background(), "key")
	if !errors.Is(err, netguard.ErrBlockedDestination) || !strings.Contains(err.Error(), "security.outbound_deny_private") {
		t.Fatalf("loopback endpoint with deny-private: %v", err)
	}
}

func TestS3RefusesHTTPWhenHTTPSRequired(t *testing.T) {
	f := &fakeS3{objects: map[string][]byte{"key": []byte("v")}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	policy := &netguard.Policy{}
	store := policyStore(t, srv.URL, "", policy)
	policy.SetRequireHTTPS(true)
	before := len(f.log())
	_, err := store.Stat(context.Background(), "key")
	if !errors.Is(err, netguard.ErrInsecureEndpoint) {
		t.Fatalf("http endpoint with https required: %v; want ErrInsecureEndpoint", err)
	}
	if _, err = store.Retrieve(context.Background(), "key"); !errors.Is(err, netguard.ErrInsecureEndpoint) {
		t.Fatalf("download over http with https required: %v", err)
	}
	if len(f.log()) != before {
		t.Fatalf("a refused request reached the endpoint: %v", f.log())
	}
}

func TestS3CustomCA(t *testing.T) {
	f := &fakeS3{objects: map[string][]byte{"key": []byte("archive")}}
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	ctx := context.Background()

	if _, err := policyStore(t, srv.URL, "", &netguard.Policy{}).Stat(ctx, "key"); err == nil ||
		!strings.Contains(err.Error(), "certificate") {
		t.Fatalf("a private CA without ca_pem: %v; want a certificate error", err)
	}

	store := policyStore(t, srv.URL, certPEM(srv), &netguard.Policy{})
	if _, err := store.Stat(ctx, "key"); err != nil {
		t.Fatalf("Stat with ca_pem: %v", err)
	}
	rc, err := store.Retrieve(ctx, "key")
	if err != nil {
		t.Fatalf("Retrieve with ca_pem: %v", err)
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(data) != "archive" {
		t.Fatalf("download with ca_pem = %q, %v", data, err)
	}
	if _, err = store.Save(ctx, "new", strings.NewReader("bytes")); err != nil {
		t.Fatalf("Save with ca_pem: %v", err)
	}
}

func TestNewS3StorageRejectsABadCA(t *testing.T) {
	isolateAWSEnv(t)
	_, err := NewS3Storage(context.Background(), S3Config{Bucket: fakeBucket, CAPEM: "not a certificate"})
	if !errors.Is(err, ErrInvalidCA) || !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err = %v; want ErrInvalidCA", err)
	}
}

func TestValidateCAPEM(t *testing.T) {
	srv := httptest.NewTLSServer(nil)
	t.Cleanup(srv.Close)
	good := certPEM(srv)
	key := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("x")}))

	for name, tt := range map[string]struct {
		in string
		ok bool
	}{
		"empty":            {"", true},
		"whitespace":       {" \n ", true},
		"certificate":      {good, true},
		"two certificates": {good + good, true},
		"garbage":          {"hello", false},
		"key only":         {key, false},
		"cert and key":     {good + key, false},
		"too large":        {good + strings.Repeat("#", MaxCAPEMBytes), false},
	} {
		err := ValidateCAPEM(tt.in)
		if (err == nil) != tt.ok || (err != nil && !errors.Is(err, ErrInvalidCA)) {
			t.Errorf("%s: err = %v; want ok = %v", name, err, tt.ok)
		}
	}
}
