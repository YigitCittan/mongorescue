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
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
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
	// AllowInsecure accepts a key URI with plain http:// (keywrap.Spec.Insecure;
	// MONGORESCUE_KEY_WRAP_ALLOW_INSECURE).
	AllowInsecure bool
}

// Open returns the Wrapper of spec with the standard environment;
// MONGORESCUE_KEY_WRAP_ALLOW_INSECURE allows a plain http:// key URI.
func Open(ctx context.Context, spec keywrap.Spec) (keywrap.Wrapper, error) {
	return OpenWith(ctx, spec, Options{AllowInsecure: AllowInsecure(os.Getenv)})
}

// AllowInsecure reports whether MONGORESCUE_KEY_WRAP_ALLOW_INSECURE is true.
func AllowInsecure(getenv func(string) string) bool {
	v, err := strconv.ParseBool(strings.TrimSpace(getenv(keywrap.EnvKeyWrapAllowInsecure)))
	return err == nil && v
}

// OpenWith returns the Wrapper of spec with opts. It does not call the KMS: a
// key or permission problem shows at the first Wrap or Unwrap.
func OpenWith(ctx context.Context, spec keywrap.Spec, opts Options) (keywrap.Wrapper, error) {
	if _, err := keywrap.ParseSpec(spec.Provider + ":" + spec.KeyURI); err != nil {
		return nil, err
	}
	if spec.Insecure() && !opts.AllowInsecure {
		return nil, keywrap.ErrInsecureKeyURI
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

// errRedirect describes a redirect a KMS answered with. Redirects are never
// followed: every request carries credentials (a token, a signed request) and
// often the key itself, which must not reach another host.
var errRedirect = errors.New("the key management service answered with a redirect, which is not followed")

// noRedirect is the CheckRedirect of every KMS client: the 3xx response is
// returned as it is, and treated as an error.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// withoutRedirects returns a copy of c that follows no redirect.
func withoutRedirects(c *http.Client) *http.Client {
	cp := *c
	cp.CheckRedirect = noRedirect
	return &cp
}

// baseClient returns the HTTP client of the REST providers, which follows no
// redirect.
func baseClient(opts Options) *http.Client {
	if opts.HTTPClient != nil {
		return withoutRedirects(opts.HTTPClient)
	}
	return &http.Client{Timeout: requestTimeout, CheckRedirect: noRedirect}
}

// isRedirect reports whether code is a redirect status.
func isRedirect(code int) bool { return code >= 300 && code < 400 }

// statusPermanent reports whether an HTTP status of a KMS means a retry cannot
// help (a refused request, a missing key or permission, a redirect).
func statusPermanent(code int) bool {
	if isRedirect(code) {
		return true
	}
	switch code {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusMethodNotAllowed, http.StatusConflict, http.StatusGone, http.StatusPreconditionFailed,
		http.StatusUnprocessableEntity:
		return true
	}
	return false
}
