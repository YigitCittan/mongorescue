// Package provider implements keywrap.Wrapper for each key management service:
// AWS KMS (the AWS SDK, credentials from the default chain), Google Cloud KMS (its
// REST API with Application Default Credentials), Azure Key Vault (the Azure SDK
// with DefaultAzureCredential) and the Transit engine of HashiCorp Vault (a minimal
// HTTP client with token, AppRole or Kubernetes authentication).
//
// Credentials always come from the provider's standard environment (an instance
// role, workload identity, the provider's environment variables); none is ever
// part of the key URI, and none is logged.
package provider

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"
	"github.com/aws/aws-sdk-go-v2/aws"
	"golang.org/x/oauth2"

	"github.com/yigitcittan/mongorescue/internal/keywrap"
)

// requestTimeout bounds one HTTP request of the REST providers (GCP, Vault); the
// caller's context usually bounds it more tightly.
const requestTimeout = 30 * time.Second

// maxResponseBody bounds a KMS response body.
const maxResponseBody = 1 << 20

// Options override how providers reach their service. The zero value uses the
// standard endpoints and credentials; tests point them at fakes.
type Options struct {
	// Getenv reads the environment (default os.Getenv); Vault reads its
	// authentication settings with it.
	Getenv func(string) string
	// HTTPClient is the base client of the REST providers (GCP, Vault).
	HTTPClient *http.Client
	// AWSConfig replaces the configuration from the default chain.
	AWSConfig *aws.Config
	// GCPEndpoint replaces https://cloudkms.googleapis.com; GCPTokenSource the
	// Application Default Credentials.
	GCPEndpoint    string
	GCPTokenSource oauth2.TokenSource
	// AzureCredential replaces DefaultAzureCredential; AzureClientOptions the
	// client options of the key vault client.
	AzureCredential    azcore.TokenCredential
	AzureClientOptions *azkeys.ClientOptions
}

// Open returns the Wrapper of spec with the standard environment.
func Open(ctx context.Context, spec keywrap.Spec) (keywrap.Wrapper, error) {
	return OpenWith(ctx, spec, Options{})
}

// OpenWith returns the Wrapper of spec with opts. It does not call the KMS: a
// key or permission problem shows at the first Wrap or Unwrap.
func OpenWith(ctx context.Context, spec keywrap.Spec, opts Options) (keywrap.Wrapper, error) {
	if _, err := keywrap.ParseSpec(spec.Provider + ":" + spec.KeyURI); err != nil {
		return nil, err
	}
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	switch spec.Provider {
	case keywrap.ProviderAWSKMS:
		return newAWS(ctx, spec, opts)
	case keywrap.ProviderGCPKMS:
		return newGCP(ctx, spec, opts)
	case keywrap.ProviderAzureKeyVault:
		return newAzure(spec, opts)
	case keywrap.ProviderVaultTransit:
		return newVault(spec, opts)
	}
	return nil, fmt.Errorf("%w: unknown provider", keywrap.ErrInvalidSpec)
}

// baseClient returns the HTTP client of the REST providers.
func baseClient(opts Options) *http.Client {
	if opts.HTTPClient != nil {
		return opts.HTTPClient
	}
	return &http.Client{Timeout: requestTimeout}
}

// statusPermanent reports whether an HTTP status of a KMS means a retry cannot
// help (a refused request, a missing key or permission).
func statusPermanent(code int) bool {
	switch code {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusMethodNotAllowed, http.StatusConflict, http.StatusGone, http.StatusPreconditionFailed,
		http.StatusUnprocessableEntity:
		return true
	}
	return false
}
