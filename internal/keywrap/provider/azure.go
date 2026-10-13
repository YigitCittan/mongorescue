package provider

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"

	"github.com/yigitcittan/mongorescue/internal/keywrap"
)

// azurePrefix starts the ciphertexts of this provider: "azkv1:<key
// version>:<base64url of the wrapped key>". The key version is part of it because
// Key Vault unwraps with the version that wrapped, which a later version of the
// key replaces as the current one.
const azurePrefix = "azkv1:"

// azureAlgorithm is the wrapping algorithm (RSA keys of a vault or a Managed HSM).
var azureAlgorithm = azkeys.EncryptionAlgorithmRSAOAEP256

// azureKV wraps with the wrapKey and unwrapKey operations of an RSA key.
type azureKV struct {
	client        *azkeys.Client
	name, version string
	keyID         string
}

// newAzure returns the Key Vault wrapper of spec, authenticated with
// DefaultAzureCredential (AZURE_* variables, Workload Identity, a managed
// identity, the Azure CLI).
func newAzure(spec keywrap.Spec, opts Options) (keywrap.Wrapper, error) {
	vaultURL, name, version, err := keywrap.SplitAzureKeyURI(spec.KeyURI)
	if err != nil {
		return nil, err
	}
	cred := opts.AzureCredential
	if cred == nil {
		dc, credErr := azidentity.NewDefaultAzureCredential(nil)
		if credErr != nil {
			return nil, fmt.Errorf("azure-keyvault: set up DefaultAzureCredential: %w", credErr)
		}
		cred = dc
	}
	clientOpts := &azkeys.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: 2}}}
	if opts.AzureClientOptions != nil {
		cp := *opts.AzureClientOptions
		clientOpts = &cp
	}
	// The SDK's default client follows redirects with the request body (the key of
	// wrapKey): the vault client follows none.
	if hc, ok := clientOpts.Transport.(*http.Client); ok && hc != nil {
		clientOpts.Transport = withoutRedirects(hc)
	} else if clientOpts.Transport == nil {
		clientOpts.Transport = azureHTTPClient()
	}
	client, err := azkeys.NewClient(vaultURL, cred, clientOpts)
	if err != nil {
		return nil, fmt.Errorf("azure-keyvault: %w", err)
	}
	return &azureKV{client: client, name: name, version: version, keyID: vaultURL + "/keys/" + name}, nil
}

func (a *azureKV) Provider() string { return keywrap.ProviderAzureKeyVault }
func (a *azureKV) KeyID() string    { return a.keyID }

func (a *azureKV) Wrap(ctx context.Context, key []byte) ([]byte, error) {
	resp, err := a.client.WrapKey(ctx, a.name, a.version, azkeys.KeyOperationParameters{Algorithm: &azureAlgorithm, Value: key}, nil)
	if err != nil {
		return nil, azureError(err)
	}
	version := a.version
	if resp.KID != nil && resp.KID.Version() != "" {
		version = resp.KID.Version()
	}
	if version == "" || len(resp.Result) == 0 {
		return nil, errors.New("azure-keyvault: wrapKey returned no key version or result")
	}
	return []byte(azurePrefix + version + ":" + base64.RawURLEncoding.EncodeToString(resp.Result)), nil
}

func (a *azureKV) Unwrap(ctx context.Context, ciphertext []byte) ([]byte, error) {
	version, raw, err := splitAzureCiphertext(ciphertext)
	if err != nil {
		return nil, keywrap.Permanent(err)
	}
	resp, err := a.client.UnwrapKey(ctx, a.name, version, azkeys.KeyOperationParameters{Algorithm: &azureAlgorithm, Value: raw}, nil)
	if err != nil {
		return nil, azureError(err)
	}
	return resp.Result, nil
}

// azureHTTPClient is the HTTP client of the vault client: TLS 1.2 or newer, the
// proxy of the environment, no redirects.
func azureHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // the standard library's transport.
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: tr, Timeout: requestTimeout, CheckRedirect: noRedirect}
}

// splitAzureCiphertext parses a ciphertext of Wrap.
func splitAzureCiphertext(ct []byte) (version string, raw []byte, err error) {
	rest, ok := strings.CutPrefix(string(ct), azurePrefix)
	if !ok {
		return "", nil, errors.New("azure-keyvault: not a ciphertext of this provider")
	}
	version, enc, ok := strings.Cut(rest, ":")
	if !ok || version == "" {
		return "", nil, errors.New("azure-keyvault: the ciphertext has no key version")
	}
	if raw, err = base64.RawURLEncoding.DecodeString(enc); err != nil || len(raw) == 0 {
		return "", nil, errors.New("azure-keyvault: malformed ciphertext")
	}
	return version, raw, nil
}

// azureError marks the permanent errors of Key Vault.
func azureError(err error) error {
	var re *azcore.ResponseError
	if errors.As(err, &re) && isRedirect(re.StatusCode) {
		return keywrap.Permanent(fmt.Errorf("azure-keyvault: HTTP %d: %w", re.StatusCode, errRedirect))
	}
	if errors.As(err, &re) && statusPermanent(re.StatusCode) {
		return keywrap.Permanent(fmt.Errorf("azure-keyvault: %s (HTTP %d)", re.ErrorCode, re.StatusCode))
	}
	return fmt.Errorf("azure-keyvault: %w", err)
}
