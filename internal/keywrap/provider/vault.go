package provider

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/yigitcittan/mongorescue/internal/keywrap"
)

// Environment variables of the vault-transit provider. The Vault address comes
// from the key URI; VAULT_ADDR is not read.
const (
	// EnvVaultToken is a Vault token (the simplest authentication).
	EnvVaultToken = "VAULT_TOKEN" //nolint:gosec // G101: a variable name, not a credential.
	// EnvVaultRoleID and EnvVaultSecretID authenticate with AppRole.
	EnvVaultRoleID   = "VAULT_ROLE_ID"
	EnvVaultSecretID = "VAULT_SECRET_ID" //nolint:gosec // G101: a variable name, not a credential.
	// EnvVaultAppRoleMount is the mount of the AppRole method (default approle).
	EnvVaultAppRoleMount = "VAULT_APPROLE_MOUNT"
	// EnvVaultK8sRole authenticates with the Kubernetes method as this role, with
	// the pod's service account token.
	EnvVaultK8sRole = "VAULT_K8S_ROLE"
	// EnvVaultK8sMount is the mount of the Kubernetes method (default kubernetes).
	EnvVaultK8sMount = "VAULT_K8S_MOUNT"
	// EnvVaultK8sTokenPath is the service account token file (default
	// /var/run/secrets/kubernetes.io/serviceaccount/token).
	EnvVaultK8sTokenPath = "VAULT_K8S_TOKEN_PATH" //nolint:gosec // G101: a variable name, not a credential.
	// EnvVaultNamespace is the Vault Enterprise namespace (X-Vault-Namespace).
	EnvVaultNamespace = "VAULT_NAMESPACE"
	// EnvVaultCACert is a PEM file of the CA certificates that verify Vault's TLS
	// certificate (default: the system roots).
	EnvVaultCACert = "VAULT_CACERT"
)

// defaultK8sTokenPath is where Kubernetes mounts the service account token.
const defaultK8sTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // G101: a path, not a credential.

// vaultAuth is how the provider obtains its token.
type vaultAuth struct {
	kind  string // token, approle or kubernetes
	mount string
	// role is the AppRole role ID or the Kubernetes role.
	role string
	// secretID is the AppRole secret ID; tokenPath the service account token.
	secretID, tokenPath string
}

// vaultTransit wraps with the encrypt and decrypt endpoints of a Transit key.
type vaultTransit struct {
	addr, mount, name string
	namespace         string
	client            *http.Client
	auth              vaultAuth

	mu    sync.Mutex
	token string
}

// newVault returns the Transit wrapper of spec, authenticated from the
// environment: VAULT_TOKEN, else AppRole (VAULT_ROLE_ID and VAULT_SECRET_ID), else
// Kubernetes (VAULT_K8S_ROLE).
func newVault(spec keywrap.Spec, opts Options) (keywrap.Wrapper, error) {
	addr, mount, name, err := keywrap.SplitVaultKeyURI(spec.KeyURI)
	if err != nil {
		return nil, err
	}
	env := func(k string) string { return strings.TrimSpace(opts.Getenv(k)) }
	v := &vaultTransit{addr: addr, mount: mount, name: name, namespace: env(EnvVaultNamespace)}
	switch {
	case env(EnvVaultToken) != "":
		v.auth = vaultAuth{kind: "token"}
		v.token = env(EnvVaultToken)
	case env(EnvVaultRoleID) != "" && env(EnvVaultSecretID) != "":
		v.auth = vaultAuth{kind: "approle", mount: orDefault(env(EnvVaultAppRoleMount), "approle"), role: env(EnvVaultRoleID), secretID: env(EnvVaultSecretID)}
	case env(EnvVaultK8sRole) != "":
		v.auth = vaultAuth{kind: "kubernetes", mount: orDefault(env(EnvVaultK8sMount), "kubernetes"), role: env(EnvVaultK8sRole),
			tokenPath: orDefault(env(EnvVaultK8sTokenPath), defaultK8sTokenPath)}
	default:
		return nil, errors.New("vault-transit: no Vault authentication: set " + EnvVaultToken + ", " + EnvVaultRoleID + " and " +
			EnvVaultSecretID + " (AppRole), or " + EnvVaultK8sRole + " (Kubernetes)")
	}
	v.client = baseClient(opts)
	if opts.HTTPClient == nil {
		if ca := env(EnvVaultCACert); ca != "" {
			if v.client, err = vaultClientWithCA(ca); err != nil {
				return nil, err
			}
		}
	}
	return v, nil
}

// orDefault returns v, or def when v is empty.
func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// vaultClientWithCA returns an HTTP client trusting the CA certificates of path.
func vaultClientWithCA(path string) (*http.Client, error) {
	pem, err := os.ReadFile(path) //nolint:gosec // G304: an operator-configured CA file.
	if err != nil {
		return nil, fmt.Errorf("vault-transit: read %s: %w", EnvVaultCACert, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("vault-transit: %s holds no PEM certificate", EnvVaultCACert)
	}
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("vault-transit: unexpected default transport")
	}
	tr = tr.Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Timeout: requestTimeout, Transport: tr, CheckRedirect: noRedirect}, nil
}

func (v *vaultTransit) Provider() string { return keywrap.ProviderVaultTransit }
func (v *vaultTransit) KeyID() string    { return v.addr + "/" + v.mount + "/keys/" + v.name }

func (v *vaultTransit) Wrap(ctx context.Context, key []byte) ([]byte, error) {
	var out struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
		} `json:"data"`
	}
	if err := v.authed(ctx, "/v1/"+v.mount+"/encrypt/"+v.name, map[string]string{"plaintext": base64.StdEncoding.EncodeToString(key)}, &out); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(out.Data.Ciphertext, "vault:") {
		return nil, errors.New("vault-transit: encrypt returned no ciphertext")
	}
	return []byte(out.Data.Ciphertext), nil
}

func (v *vaultTransit) Unwrap(ctx context.Context, ciphertext []byte) ([]byte, error) {
	if !bytes.HasPrefix(ciphertext, []byte("vault:")) {
		return nil, keywrap.Permanent(errors.New("vault-transit: not a Transit ciphertext"))
	}
	var out struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	if err := v.authed(ctx, "/v1/"+v.mount+"/decrypt/"+v.name, map[string]string{"ciphertext": string(ciphertext)}, &out); err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(out.Data.Plaintext)
	if err != nil {
		return nil, errors.New("vault-transit: decrypt returned a malformed plaintext")
	}
	return key, nil
}

// vaultStatusError is a non-2xx answer of Vault.
type vaultStatusError struct {
	path   string
	status int
	msgs   []string
}

func (e *vaultStatusError) Error() string {
	msg := http.StatusText(e.status)
	switch {
	case isRedirect(e.status):
		msg = errRedirect.Error()
	case len(e.msgs) > 0:
		msg = strings.Join(e.msgs, "; ")
	}
	return fmt.Sprintf("vault-transit: %s: HTTP %d: %s", e.path, e.status, msg)
}

// authed calls path with the current token, logging in first (AppRole,
// Kubernetes) and once more when the token was refused (it expired).
func (v *vaultTransit) authed(ctx context.Context, path string, body, out any) error {
	v.mu.Lock()
	token := v.token
	v.mu.Unlock()
	if token == "" {
		var err error
		if token, err = v.login(ctx); err != nil {
			return err
		}
	}
	err := v.do(ctx, path, token, body, out)
	var se *vaultStatusError
	if v.auth.kind != "token" && errors.As(err, &se) && se.status == http.StatusForbidden {
		if token, err = v.login(ctx); err != nil {
			return err
		}
		err = v.do(ctx, path, token, body, out)
	}
	if errors.As(err, &se) && statusPermanent(se.status) {
		return keywrap.Permanent(err)
	}
	return err
}

// login obtains a token with AppRole or Kubernetes authentication.
func (v *vaultTransit) login(ctx context.Context) (string, error) {
	var body map[string]string
	switch v.auth.kind {
	case "approle":
		body = map[string]string{"role_id": v.auth.role, "secret_id": v.auth.secretID}
	case "kubernetes":
		jwt, err := os.ReadFile(v.auth.tokenPath)
		if err != nil {
			return "", keywrap.Permanent(fmt.Errorf("vault-transit: read the service account token: %w", err))
		}
		body = map[string]string{"role": v.auth.role, "jwt": strings.TrimSpace(string(jwt))}
	default:
		return "", keywrap.Permanent(errors.New("vault-transit: the token was refused"))
	}
	var out struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err := v.do(ctx, "/v1/auth/"+v.auth.mount+"/login", "", body, &out); err != nil {
		var se *vaultStatusError
		if errors.As(err, &se) && statusPermanent(se.status) {
			return "", keywrap.Permanent(err)
		}
		return "", err
	}
	if out.Auth.ClientToken == "" {
		return "", errors.New("vault-transit: the login returned no token")
	}
	v.mu.Lock()
	v.token = out.Auth.ClientToken
	v.mu.Unlock()
	return out.Auth.ClientToken, nil
}

// do POSTs body to path and decodes the response into out.
func (v *vaultTransit) do(ctx context.Context, path, token string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("vault-transit: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.addr+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("vault-transit: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Vault-Request", "true")
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	if v.namespace != "" {
		req.Header.Set("X-Vault-Namespace", v.namespace)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("vault-transit: %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return fmt.Errorf("vault-transit: %s: read response: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Errors []string `json:"errors"`
		}
		_ = json.Unmarshal(raw, &e)
		return &vaultStatusError{path: path, status: resp.StatusCode, msgs: e.Errors}
	}
	if err = json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("vault-transit: %s: decode response: %w", path, err)
	}
	return nil
}
