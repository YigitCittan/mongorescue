package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/keywrap"
	"github.com/yigitcittan/mongorescue/internal/vault"
)

// Environment variables of the vault-transit provider (see internal/vault, which
// the credentials read from Vault share). The Vault address comes from the key
// URI; neither VAULT_ADDR nor MONGORESCUE_VAULT_ADDR is read.
const (
	// EnvVaultToken is a Vault token (the simplest authentication).
	EnvVaultToken = vault.EnvToken
	// EnvVaultRoleID and EnvVaultSecretID authenticate with AppRole.
	EnvVaultRoleID   = vault.EnvRoleID
	EnvVaultSecretID = vault.EnvSecretID
	// EnvVaultAppRoleMount is the mount of the AppRole method (default approle).
	EnvVaultAppRoleMount = vault.EnvAppRoleMount
	// EnvVaultK8sRole authenticates with the Kubernetes method as this role, with
	// the pod's service account token.
	EnvVaultK8sRole = vault.EnvK8sRole
	// EnvVaultK8sMount is the mount of the Kubernetes method (default kubernetes).
	EnvVaultK8sMount = vault.EnvK8sMount
	// EnvVaultK8sTokenPath is the service account token file (default
	// /var/run/secrets/kubernetes.io/serviceaccount/token).
	EnvVaultK8sTokenPath = vault.EnvK8sTokenPath
	// EnvVaultNamespace is the Vault Enterprise namespace (X-Vault-Namespace).
	EnvVaultNamespace = vault.EnvNamespace
	// EnvVaultCACert is a PEM file of the CA certificates that verify Vault's TLS
	// certificate (default: the system roots).
	EnvVaultCACert = vault.EnvCACert
)

// vaultTransit wraps with the encrypt and decrypt endpoints of a Transit key.
type vaultTransit struct {
	addr, mount, name string
	client            *vault.Client
}

// newVault returns the Transit wrapper of spec, authenticated from the
// environment: VAULT_TOKEN, else AppRole (VAULT_ROLE_ID and VAULT_SECRET_ID), else
// Kubernetes (VAULT_K8S_ROLE).
func newVault(spec keywrap.Spec, opts Options) (keywrap.Wrapper, error) {
	addr, mount, name, err := keywrap.SplitVaultKeyURI(spec.KeyURI)
	if err != nil {
		return nil, err
	}
	auth, err := vault.AuthFromEnv(opts.Getenv)
	if err != nil {
		return nil, errors.New("vault-transit: " + err.Error())
	}
	cfg := vault.Config{Addr: addr, Namespace: opts.Getenv(EnvVaultNamespace), Auth: auth, Name: "vault-transit",
		// OpenWith refused a plain http:// key URI without the opt-in already.
		AllowInsecure: true}
	if opts.HTTPClient != nil {
		cfg.HTTPClient = opts.HTTPClient
	} else {
		cfg.CACert = strings.TrimSpace(opts.Getenv(EnvVaultCACert))
	}
	client, err := vault.New(cfg)
	if err != nil {
		return nil, err
	}
	return &vaultTransit{addr: addr, mount: mount, name: name, client: client}, nil
}

func (v *vaultTransit) Provider() string { return keywrap.ProviderVaultTransit }
func (v *vaultTransit) KeyID() string    { return v.addr + "/" + v.mount + "/keys/" + v.name }

func (v *vaultTransit) Wrap(ctx context.Context, key []byte) ([]byte, error) {
	var out struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
		} `json:"data"`
	}
	if err := v.call(ctx, "/v1/"+v.mount+"/encrypt/"+v.name, map[string]string{"plaintext": base64.StdEncoding.EncodeToString(key)}, &out); err != nil {
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
	if err := v.call(ctx, "/v1/"+v.mount+"/decrypt/"+v.name, map[string]string{"ciphertext": string(ciphertext)}, &out); err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(out.Data.Plaintext)
	if err != nil {
		return nil, errors.New("vault-transit: decrypt returned a malformed plaintext")
	}
	return key, nil
}

// call POSTs body to path, marking the failures a retry cannot fix permanent.
func (v *vaultTransit) call(ctx context.Context, path string, body, out any) error {
	err := v.client.Do(ctx, http.MethodPost, path, body, out)
	var se *vault.StatusError
	if errors.Is(err, vault.ErrAuthConfig) || (errors.As(err, &se) && se.Permanent()) {
		return keywrap.Permanent(err)
	}
	return err
}
