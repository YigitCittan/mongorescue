//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/kms"

	"github.com/yigitcittan/mongorescue/internal/app"
	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/keywrap"
	"github.com/yigitcittan/mongorescue/internal/keywrap/provider"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

// Key wrapping (internal/keywrap) against real KMS implementations: AWS KMS as
// served by LocalStack and the Transit engine of a Vault dev server, both started
// by scripts/test-integration-docker.sh. Google Cloud KMS and Azure Key Vault have
// no reliable emulator; their request shapes are covered by the httptest contract
// tests of internal/keywrap/provider.

// itRetry keeps a failing KMS test short.
var itRetry = keywrap.Retry{Attempts: 2, Initial: 100 * time.Millisecond, Timeout: 20 * time.Second}

// requireLocalStackKMS returns the LocalStack endpoint or skips.
func requireLocalStackKMS(t *testing.T) string {
	t.Helper()
	ep := os.Getenv("MONGORESCUE_TEST_KMS_LOCALSTACK_ENDPOINT")
	if ep == "" {
		t.Skip("MONGORESCUE_TEST_KMS_LOCALSTACK_ENDPOINT not set (run scripts/test-integration-docker.sh with IT_PROVIDERS containing localstack)")
	}
	return ep
}

// localStackKey creates a symmetric KMS key on LocalStack and returns its ARN.
func localStackKey(t *testing.T, endpoint string) string {
	t.Helper()
	cfg := aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""), BaseEndpoint: aws.String(endpoint)}
	out, err := kms.NewFromConfig(cfg).CreateKey(context.Background(), &kms.CreateKeyInput{Description: aws.String("mongorescue key wrapping test")})
	if err != nil {
		t.Fatalf("create a LocalStack KMS key: %v", err)
	}
	return aws.ToString(out.KeyMetadata.Arn)
}

func loadWrapped(dir string, spec keywrap.Spec, w keywrap.Wrapper) (*keywrap.Loaded, error) {
	return keywrap.Load(context.Background(), keywrap.LoadConfig{DataDir: dir, Spec: spec, Wrapper: w, Retry: itRetry,
		Logger: slog.New(slog.DiscardHandler)})
}

// TestKeyWrapAWSKMS wraps secret.key with a LocalStack KMS key through the AWS
// default credential chain and endpoint variables, migrates a plaintext key, runs
// a MongoRescue App on it, and checks that another KMS key cannot unwrap it.
func TestKeyWrapAWSKMS(t *testing.T) {
	endpoint := requireLocalStackKMS(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ENDPOINT_URL_KMS", endpoint)
	ctx := context.Background()
	arn := localStackKey(t, endpoint)
	spec, err := keywrap.ParseSpec("aws-kms:" + arn)
	if err != nil {
		t.Fatal(err)
	}
	w, err := provider.Open(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	plain := filepath.Join(dir, secretbox.KeyFileName)
	key, _ := secretbox.GenerateKey()
	if err = secretbox.WriteKeyFile(plain, key); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadWrapped(dir, spec, w)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded.Key, key) || len(loaded.Migrated) != 1 {
		t.Fatalf("migration = %+v", loaded)
	}
	if _, err = os.Stat(plain); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the plaintext secret.key was kept")
	}
	again, err := loadWrapped(dir, spec, w)
	if err != nil || !bytes.Equal(again.Key, key) {
		t.Fatalf("unwrap = %v", err)
	}

	// Another KMS key refuses the ciphertext (Decrypt names the expected key).
	other, err := provider.Open(ctx, keywrap.Spec{Provider: keywrap.ProviderAWSKMS, KeyURI: localStackKey(t, endpoint)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = loaded.File.Open(ctx, other); err == nil {
		t.Fatal("another KMS key unwrapped secret.key")
	}

	// A MongoRescue start on a fresh data directory stores only the wrapped key,
	// and restarts from it.
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.KeyWrap = spec
	for range 2 {
		a, newErr := app.New(cfg, slog.New(slog.DiscardHandler), app.WithGetenv(func(string) string { return "" }))
		if newErr != nil {
			t.Fatalf("app with AWS KMS key wrapping: %v", newErr)
		}
		_ = a.Close()
	}
	if _, err = os.Stat(filepath.Join(cfg.DataDir, secretbox.KeyFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the App wrote a plaintext secret.key")
	}
}

// vaultEnv is the Vault dev server of the test run.
type vaultEnv struct {
	addr, token string
}

// requireVault returns the Vault dev server or skips.
func requireVault(t *testing.T) vaultEnv {
	t.Helper()
	v := vaultEnv{addr: strings.TrimRight(os.Getenv("MONGORESCUE_TEST_VAULT_ADDR"), "/"), token: os.Getenv("MONGORESCUE_TEST_VAULT_TOKEN")}
	if v.addr == "" || v.token == "" {
		t.Skip("MONGORESCUE_TEST_VAULT_ADDR not set (run scripts/test-integration-docker.sh with IT_PROVIDERS containing vault)")
	}
	return v
}

// api calls the Vault API as root and decodes the answer into out (may be nil).
func (v vaultEnv) api(t *testing.T, method, path string, body any, out any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, v.addr+"/v1/"+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Vault-Token", v.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("vault %s %s: %d %s", method, path, resp.StatusCode, raw)
	}
	if out != nil {
		if err = json.Unmarshal(raw, out); err != nil {
			t.Fatal(err)
		}
	}
}

func (v vaultEnv) wrapper(t *testing.T, key string, env map[string]string) (keywrap.Spec, keywrap.Wrapper) {
	t.Helper()
	spec, err := keywrap.ParseSpec("vault-transit:" + v.addr + "/transit/keys/" + key)
	if err != nil {
		t.Fatal(err)
	}
	w, err := provider.OpenWith(context.Background(), spec, provider.Options{Getenv: func(k string) string { return env[k] }, AllowInsecure: true})
	if err != nil {
		t.Fatal(err)
	}
	return spec, w
}

// TestKeyWrapVaultTransit wraps secret.key with Vault Transit (token and AppRole
// authentication), checks that a rotation of the Transit key is transparent and
// that rewrap moves the key to another Transit key.
func TestKeyWrapVaultTransit(t *testing.T) {
	v := requireVault(t)
	ctx := context.Background()
	spec, w := v.wrapper(t, "mongorescue", map[string]string{provider.EnvVaultToken: v.token})

	dir := t.TempDir()
	first, err := loadWrapped(dir, spec, w)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || !strings.HasPrefix(string(first.File.Ciphertext), "vault:v") {
		t.Fatalf("first start = %+v", first)
	}

	// A new version of the Transit key: the stored ciphertext still unwraps.
	v.api(t, http.MethodPost, "transit/keys/mongorescue/rotate", nil, nil)
	if again, loadErr := loadWrapped(dir, spec, w); loadErr != nil || !bytes.Equal(again.Key, first.Key) {
		t.Fatalf("unwrap after the Transit key rotated = %v", loadErr)
	}

	// AppRole: a role whose policy only allows this key.
	policy := `path "transit/encrypt/mongorescue" { capabilities = ["update"] }
path "transit/decrypt/mongorescue" { capabilities = ["update"] }`
	v.api(t, http.MethodPut, "sys/policies/acl/mongorescue-it", map[string]string{"policy": policy}, nil)
	var mounts map[string]any
	v.api(t, http.MethodGet, "sys/auth", nil, &mounts)
	if _, ok := mounts["approle/"]; !ok {
		v.api(t, http.MethodPost, "sys/auth/approle", map[string]string{"type": "approle"}, nil)
	}
	v.api(t, http.MethodPost, "auth/approle/role/mongorescue-it", map[string]any{"token_policies": []string{"mongorescue-it"}, "token_ttl": "5m"}, nil)
	var roleID, secretID struct {
		Data map[string]any `json:"data"`
	}
	v.api(t, http.MethodGet, "auth/approle/role/mongorescue-it/role-id", nil, &roleID)
	v.api(t, http.MethodPost, "auth/approle/role/mongorescue-it/secret-id", nil, &secretID)
	_, approle := v.wrapper(t, "mongorescue", map[string]string{provider.EnvVaultRoleID: roleID.Data["role_id"].(string), provider.EnvVaultSecretID: secretID.Data["secret_id"].(string)})
	if got, openErr := first.File.Open(ctx, approle); openErr != nil || !bytes.Equal(got, first.Key) {
		t.Fatalf("unwrap with AppRole = %v", openErr)
	}

	// Rewrap to another Transit key; the old file spec no longer loads.
	v.api(t, http.MethodPost, "transit/keys/mongorescue-next", map[string]string{"type": "aes256-gcm96"}, nil)
	nextSpec, _ := v.wrapper(t, "mongorescue-next", map[string]string{provider.EnvVaultToken: v.token})
	open := func(ctx context.Context, s keywrap.Spec) (keywrap.Wrapper, error) {
		return provider.OpenWith(ctx, s, provider.Options{AllowInsecure: true, Getenv: func(k string) string {
			if k == provider.EnvVaultToken {
				return v.token
			}
			return ""
		}})
	}
	change, err := keywrap.Rewrap(ctx, keywrap.OfflineConfig{DataDir: dir, Open: open, Actor: "it", Retry: itRetry}, nextSpec)
	if err != nil || len(change.Files) != 1 {
		t.Fatalf("rewrap = %+v, %v", change, err)
	}
	if _, err = loadWrapped(dir, spec, w); !errors.Is(err, keywrap.ErrKeyMismatch) {
		t.Fatalf("Load with the old key URI = %v; want ErrKeyMismatch", err)
	}
	nextW, err := open(ctx, nextSpec)
	if err != nil {
		t.Fatal(err)
	}
	if moved, loadErr := loadWrapped(dir, nextSpec, nextW); loadErr != nil || !bytes.Equal(moved.Key, first.Key) {
		t.Fatalf("Load after the rewrap = %v", loadErr)
	}

	// Leaving the KMS writes the plaintext key back.
	if _, err = keywrap.UnwrapToFiles(ctx, keywrap.OfflineConfig{DataDir: dir, Open: open, Actor: "it", Retry: itRetry}); err != nil {
		t.Fatal(err)
	}
	if got, readErr := secretbox.ReadKeyFile(filepath.Join(dir, secretbox.KeyFileName)); readErr != nil || !bytes.Equal(got, first.Key) {
		t.Fatalf("secret.key after unwrap = %v", readErr)
	}
}
