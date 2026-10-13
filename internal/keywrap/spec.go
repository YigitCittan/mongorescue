package keywrap

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/redact"
)

// EnvKeyWrap is the bootstrap environment variable that turns key wrapping on:
// "<provider>:<key URI>" (see ParseSpec).
const EnvKeyWrap = "MONGORESCUE_KEY_WRAP"

// EnvKeyWrapAllowInsecure ("true") allows a vault-transit key URI with plain
// http://, which sends the Vault token and the key unencrypted. It is meant for a
// development Vault on the same host only; a start that uses it logs a warning.
const EnvKeyWrapAllowInsecure = "MONGORESCUE_KEY_WRAP_ALLOW_INSECURE"

// Providers.
const (
	// ProviderAWSKMS wraps with AWS KMS: the key URI is a key ARN, an alias ARN, a
	// key ID or "alias/<name>".
	ProviderAWSKMS = "aws-kms"
	// ProviderGCPKMS wraps with Google Cloud KMS: the key URI is
	// projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>.
	ProviderGCPKMS = "gcp-kms"
	// ProviderAzureKeyVault wraps with Azure Key Vault (or Managed HSM): the key URI
	// is https://<vault>.vault.azure.net/keys/<name>[/<version>].
	ProviderAzureKeyVault = "azure-keyvault"
	// ProviderVaultTransit wraps with the Transit secrets engine of HashiCorp Vault
	// (or OpenBao): the key URI is https://<vault>:8200/<mount>/keys/<name>.
	ProviderVaultTransit = "vault-transit"
)

// Providers lists the supported providers.
func Providers() []string {
	return []string{ProviderAWSKMS, ProviderGCPKMS, ProviderAzureKeyVault, ProviderVaultTransit}
}

// ErrInsecureKeyURI is returned for a key URI with plain http:// unless
// EnvKeyWrapAllowInsecure opts in.
var ErrInsecureKeyURI = errors.New("keywrap: the key URI uses plain http://, which would send the Vault token and the key unencrypted; " +
	"use https://, or set " + EnvKeyWrapAllowInsecure + "=true for a development Vault only")

// ErrInvalidSpec is returned for a key wrapping configuration that is not
// "<provider>:<key URI>" with a known provider and a well-formed key URI. Its
// message never contains the configured value.
var ErrInvalidSpec = errors.New("keywrap: " + EnvKeyWrap + " must be <provider>:<key URI> with provider " +
	"aws-kms, gcp-kms, azure-keyvault or vault-transit")

// Spec is a parsed key wrapping configuration.
type Spec struct {
	// Provider is one of Providers.
	Provider string
	// KeyURI identifies the KMS key; its form depends on the provider.
	KeyURI string
}

// Enabled reports whether s configures key wrapping.
func (s Spec) Enabled() bool { return s.Provider != "" }

// Insecure reports whether the key URI uses plain http:// (only vault-transit
// accepts it, and only with EnvKeyWrapAllowInsecure).
func (s Spec) Insecure() bool {
	return s.Provider == ProviderVaultTransit && strings.HasPrefix(strings.ToLower(s.KeyURI), "http://")
}

// String returns "<provider>:<key URI>" for logs and errors, scrubbed by
// internal/redact (a valid key URI holds no credential, but a log line never
// depends on that).
func (s Spec) String() string {
	if !s.Enabled() {
		return ""
	}
	return s.Provider + ":" + redact.Text(s.KeyURI)
}

var (
	awsARN    = regexp.MustCompile(`^arn:aws[a-z-]*:kms:[a-z0-9-]+:[0-9]{12}:(key/[A-Za-z0-9-]+|alias/[A-Za-z0-9/_-]+)$`)
	awsAlias  = regexp.MustCompile(`^alias/[A-Za-z0-9/_-]+$`)
	awsKeyID  = regexp.MustCompile(`^(mrk-)?[A-Za-z0-9-]{32,40}$`)
	gcpKey    = regexp.MustCompile(`^projects/[A-Za-z0-9._:-]+/locations/[A-Za-z0-9_-]+/keyRings/[A-Za-z0-9_-]+/cryptoKeys/[A-Za-z0-9_-]+$`)
	azureName = regexp.MustCompile(`^[A-Za-z0-9-]{1,127}$`)
	azureVer  = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
	vaultSeg  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// ParseSpec parses "<provider>:<key URI>". An empty value returns the zero Spec
// (key wrapping off). Errors wrap ErrInvalidSpec and only name the provider.
func ParseSpec(v string) (Spec, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return Spec{}, nil
	}
	provider, uri, ok := strings.Cut(v, ":")
	provider = strings.ToLower(strings.TrimSpace(provider))
	uri = strings.TrimSpace(uri)
	if !ok || uri == "" {
		return Spec{}, ErrInvalidSpec
	}
	var err error
	switch provider {
	case ProviderAWSKMS:
		if !awsARN.MatchString(uri) && !awsAlias.MatchString(uri) && !awsKeyID.MatchString(uri) {
			err = errors.New("the key URI must be a KMS key ARN, an alias ARN, a key ID or alias/<name>")
		}
	case ProviderGCPKMS:
		if !gcpKey.MatchString(uri) {
			err = errors.New("the key URI must be projects/<project>/locations/<location>/keyRings/<ring>/cryptoKeys/<key> (no key version)")
		}
	case ProviderAzureKeyVault:
		_, _, _, err = SplitAzureKeyURI(uri)
	case ProviderVaultTransit:
		_, _, _, err = SplitVaultKeyURI(uri)
	default:
		return Spec{}, ErrInvalidSpec
	}
	if err != nil {
		return Spec{}, fmt.Errorf("%w: %s: %w", ErrInvalidSpec, provider, err)
	}
	return Spec{Provider: provider, KeyURI: uri}, nil
}

// parseKeyURL parses an http(s) key URL that carries no credential, query or
// fragment.
func parseKeyURL(raw string, allowHTTP bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return nil, errors.New("the key URI is not a valid URL")
	case u.Scheme != "https" && (u.Scheme != "http" || !allowHTTP):
		if allowHTTP {
			return nil, errors.New("the key URI must be an http or https URL")
		}
		return nil, errors.New("the key URI must be an https URL")
	case u.User != nil:
		return nil, errors.New("the key URI must not contain credentials; use the provider's environment variables")
	case u.RawQuery != "" || u.Fragment != "" || u.Opaque != "":
		return nil, errors.New("the key URI must not have a query or fragment")
	case u.Hostname() == "":
		return nil, errors.New("the key URI has no host")
	}
	return u, nil
}

// SplitAzureKeyURI splits https://<vault>/keys/<name>[/<version>] into the vault
// URL (scheme and host), the key name and the version ("" for the current one).
func SplitAzureKeyURI(raw string) (vaultURL, name, version string, err error) {
	u, err := parseKeyURL(raw, false)
	if err != nil {
		return "", "", "", err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != "keys" || !azureName.MatchString(parts[1]) ||
		(len(parts) == 3 && !azureVer.MatchString(parts[2])) {
		return "", "", "", errors.New("the key URI must be https://<vault>.vault.azure.net/keys/<name>[/<version>]")
	}
	if len(parts) == 3 {
		version = parts[2]
	}
	return u.Scheme + "://" + u.Host, parts[1], version, nil
}

// SplitVaultKeyURI splits http(s)://<vault>[:port]/<mount>/keys/<name> into the
// Vault address, the mount path of the Transit engine (which may have several
// segments) and the key name.
func SplitVaultKeyURI(raw string) (addr, mount, name string, err error) {
	u, err := parseKeyURL(raw, true)
	if err != nil {
		return "", "", "", err
	}
	path := strings.Trim(u.Path, "/")
	i := strings.LastIndex(path, "/keys/")
	if i <= 0 {
		return "", "", "", errors.New("the key URI must be https://<vault>:8200/<mount>/keys/<name>")
	}
	mount, name = path[:i], path[i+len("/keys/"):]
	if strings.HasPrefix(mount, "v1/") || mount == "v1" {
		return "", "", "", errors.New("the key URI names the mount without the /v1 API prefix: https://<vault>:8200/<mount>/keys/<name>")
	}
	for _, seg := range append(strings.Split(mount, "/"), name) {
		if !vaultSeg.MatchString(seg) {
			return "", "", "", errors.New("the key URI must be https://<vault>:8200/<mount>/keys/<name>")
		}
	}
	return u.Scheme + "://" + u.Host, mount, name, nil
}
