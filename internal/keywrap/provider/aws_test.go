package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/yigitcittan/mongorescue/internal/keywrap"
)

// fakeAWSKMS answers Encrypt and Decrypt of the AWS KMS JSON protocol: the
// "ciphertext" is "ct:" + the plaintext, bound to the key ID and context.
type fakeAWSKMS struct {
	mu       sync.Mutex
	targets  []string
	bodies   []map[string]any
	auth     []string
	errorFor string
}

func (f *fakeAWSKMS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	target := r.Header.Get("X-Amz-Target")
	f.mu.Lock()
	f.targets = append(f.targets, target)
	f.bodies = append(f.bodies, body)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	errorFor := f.errorFor
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	if errorFor != "" {
		w.Header().Set("X-Amzn-Errortype", errorFor)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"__type":"`+errorFor+`","message":"refused"}`)
		return
	}
	switch target {
	case "TrentService.Encrypt":
		pt, _ := base64.StdEncoding.DecodeString(body["Plaintext"].(string))
		_ = json.NewEncoder(w).Encode(map[string]any{"CiphertextBlob": append([]byte("ct:"), pt...), "KeyId": "arn:aws:kms:eu-west-1:123456789012:key/k"})
	case "TrentService.Decrypt":
		blob, _ := base64.StdEncoding.DecodeString(body["CiphertextBlob"].(string))
		_ = json.NewEncoder(w).Encode(map[string]any{"Plaintext": bytes.TrimPrefix(blob, []byte("ct:")), "KeyId": "arn:aws:kms:eu-west-1:123456789012:key/k"})
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func newAWSTest(t *testing.T, keyURI string) (*fakeAWSKMS, keywrap.Wrapper) {
	t.Helper()
	fake := &fakeAWSKMS{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	cfg := aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret-key", ""),
		BaseEndpoint: aws.String(srv.URL), RetryMaxAttempts: 1, HTTPClient: srv.Client()}
	w, err := OpenWith(context.Background(), keywrap.Spec{Provider: keywrap.ProviderAWSKMS, KeyURI: keyURI}, Options{AWSConfig: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	return fake, w
}

func TestAWSKMSRequests(t *testing.T) {
	const arn = "arn:aws:kms:eu-west-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab"
	fake, w := newAWSTest(t, arn)
	if w.Provider() != keywrap.ProviderAWSKMS || w.KeyID() != arn {
		t.Fatalf("Provider/KeyID = %s %s", w.Provider(), w.KeyID())
	}
	key := bytes.Repeat([]byte{7}, 32)
	ct, err := w.Wrap(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.Unwrap(context.Background(), ct)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("Unwrap = %v", err)
	}
	if strings.Join(fake.targets, ",") != "TrentService.Encrypt,TrentService.Decrypt" {
		t.Errorf("targets = %v", fake.targets)
	}
	for i, b := range fake.bodies {
		if b["KeyId"] != arn {
			t.Errorf("request %d KeyId = %v", i, b["KeyId"])
		}
		ec, _ := b["EncryptionContext"].(map[string]any)
		if ec["purpose"] != keywrap.Purpose {
			t.Errorf("request %d EncryptionContext = %v", i, b["EncryptionContext"])
		}
		// The region of the ARN signs the request.
		if !strings.Contains(fake.auth[i], "/eu-west-1/kms/aws4_request") {
			t.Errorf("request %d is not signed for eu-west-1 KMS: %q", i, fake.auth[i])
		}
	}

	fake.errorFor = "InvalidCiphertextException"
	if _, err = w.Unwrap(context.Background(), ct); !keywrap.IsPermanent(err) {
		t.Errorf("InvalidCiphertextException = %v; want a permanent error", err)
	}
	fake.errorFor = "KMSInternalException"
	if _, err = w.Unwrap(context.Background(), ct); err == nil || keywrap.IsPermanent(err) {
		t.Errorf("KMSInternalException = %v; want a transient error", err)
	}
}

func TestAWSRegion(t *testing.T) {
	if r := awsRegion("arn:aws:kms:ap-south-1:123456789012:alias/x"); r != "ap-south-1" {
		t.Errorf("awsRegion = %q", r)
	}
	if r := awsRegion("alias/x"); r != "" {
		t.Errorf("awsRegion of an alias = %q", r)
	}
	if _, err := OpenWith(context.Background(), keywrap.Spec{Provider: keywrap.ProviderAWSKMS, KeyURI: "alias/x"},
		Options{AWSConfig: &aws.Config{}}); err == nil {
		t.Error("an alias without a region was accepted")
	}
	if _, err := OpenWith(context.Background(), keywrap.Spec{Provider: "nope", KeyURI: "x"}, Options{}); !errors.Is(err, keywrap.ErrInvalidSpec) {
		t.Errorf("unknown provider = %v", err)
	}
}
