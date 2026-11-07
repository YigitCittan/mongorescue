package storage

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"

	"github.com/yigitcittan/mongorescue/internal/netguard"
)

// maxS3Redirects bounds the redirects one S3 request follows (the SDK's own limit).
const maxS3Redirects = 10

// MaxCAPEMBytes bounds a custom CA bundle (a handful of certificates).
const MaxCAPEMBytes = 64 << 10

// ErrInvalidCA is returned for a custom CA bundle that holds no valid PEM
// certificate. It wraps ErrInvalidConfig.
var ErrInvalidCA = fmt.Errorf("%w: ca_pem must be one or more PEM encoded CA certificates", ErrInvalidConfig)

// ParseCAPEM parses a PEM bundle of CA certificates into a pool that holds the
// system roots as well, so the endpoint may use either. It fails with ErrInvalidCA
// when the bundle is empty, too large, holds anything but certificates or none that
// parses.
func ParseCAPEM(pem string) (*x509.CertPool, error) {
	pem = strings.TrimSpace(pem)
	if pem == "" || len(pem) > MaxCAPEMBytes {
		return nil, ErrInvalidCA
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	pool = pool.Clone()
	if !pool.AppendCertsFromPEM([]byte(pem)) {
		return nil, ErrInvalidCA
	}
	return pool, nil
}

// ValidateCAPEM checks a custom CA bundle (see ParseCAPEM). The empty bundle is
// valid and means the system roots only.
func ValidateCAPEM(pem string) error {
	if strings.TrimSpace(pem) == "" {
		return nil
	}
	if _, err := ParseCAPEM(pem); err != nil {
		return err
	}
	// Anything that is not a certificate block is a mistake worth reporting, such
	// as a private key pasted next to the certificate.
	if strings.Contains(pem, "PRIVATE KEY") {
		return fmt.Errorf("%w: the bundle must not contain a private key", ErrInvalidCA)
	}
	return nil
}

// policyRetryer wraps the SDK's retryer so that a request the destination policy
// refused is not retried: the answer will not change, and retrying only delays the
// error of a connection test by seconds.
type policyRetryer struct{ aws.Retryer }

// IsErrorRetryable reports false for policy refusals and defers to the wrapped
// retryer otherwise.
func (r policyRetryer) IsErrorRetryable(err error) bool {
	if errors.Is(err, netguard.ErrBlockedDestination) || errors.Is(err, netguard.ErrInsecureEndpoint) {
		return false
	}
	return r.Retryer.IsErrorRetryable(err)
}

// newGuardedS3Client returns the HTTP client of the S3 requests. base is the
// client the AWS configuration resolved (with the bounds of newS3HTTPClient and a
// bundle from AWS_CA_BUNDLE, if any); its transport is kept and extended with
//
//   - the destination guard (netguard): metadata, link-local and NAT64 forms are
//     always refused, private addresses while the policy denies them; the check
//     covers redirect hops and the target of a proxied request;
//   - the https-only guard, read for every request, so turning
//     security.require_https_endpoints on also stops drivers built before;
//   - a custom CA bundle, when caPEM is set, in addition to the system roots.
//
// The credential providers never use it: they keep the unguarded client, since
// instance and container credentials live on link-local addresses.
func newGuardedS3Client(base aws.HTTPClient, caPEM string, policy *netguard.Policy) (*http.Client, error) {
	if policy == nil {
		policy = netguard.Default
	}
	bc, ok := base.(*awshttp.BuildableClient)
	if !ok {
		return nil, errors.New("storage: unexpected s3 http client")
	}
	tr := bc.GetTransport()
	if strings.TrimSpace(caPEM) != "" {
		pool, err := ParseCAPEM(caPEM)
		if err != nil {
			return nil, err
		}
		if tr.TLSClientConfig == nil {
			tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		tr.TLSClientConfig.RootCAs = pool
	}
	netguard.NewDialer(policy, nil, s3DialTimeout, s3KeepAlive).Guard(tr, tr.Proxy)
	return &http.Client{
		Transport: netguard.RequireHTTPSTransport(policy, tr),
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= maxS3Redirects {
				return fmt.Errorf("stopped after %d redirects", maxS3Redirects)
			}
			return nil
		},
	}, nil
}
