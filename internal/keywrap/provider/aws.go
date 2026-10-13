package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/smithy-go"

	"github.com/yigitcittan/mongorescue/internal/keywrap"
)

// awsContextKey is the key of the AWS KMS encryption context (its value is
// keywrap.Purpose): Decrypt fails for a ciphertext made with another context.
const awsContextKey = "purpose"

// awsKMS wraps with AWS KMS Encrypt and Decrypt.
type awsKMS struct {
	client *kms.Client
	keyID  string
}

// awsRegion returns the region of a key or alias ARN ("" for a key ID or alias
// name, which use the region of the default chain).
func awsRegion(keyURI string) string {
	if !strings.HasPrefix(keyURI, "arn:") {
		return ""
	}
	parts := strings.SplitN(keyURI, ":", 6)
	if len(parts) < 6 {
		return ""
	}
	return parts[3]
}

// newAWS returns the AWS KMS wrapper of spec. Credentials and the endpoint come
// from the default chain (AWS_* variables, shared files, IRSA, ECS and EC2
// roles); AWS_ENDPOINT_URL_KMS points it at another endpoint (LocalStack).
func newAWS(ctx context.Context, spec keywrap.Spec, opts Options) (keywrap.Wrapper, error) {
	var cfg aws.Config
	if opts.AWSConfig != nil {
		cfg = opts.AWSConfig.Copy()
	} else {
		var loadOpts []func(*config.LoadOptions) error
		if r := awsRegion(spec.KeyURI); r != "" {
			loadOpts = append(loadOpts, config.WithRegion(r))
		}
		var err error
		if cfg, err = config.LoadDefaultConfig(ctx, loadOpts...); err != nil {
			return nil, fmt.Errorf("aws-kms: load the AWS configuration: %w", err)
		}
	}
	if r := awsRegion(spec.KeyURI); r != "" {
		cfg.Region = r
	}
	if cfg.Region == "" {
		return nil, errors.New("aws-kms: no region: use a key ARN or set AWS_REGION")
	}
	// The SDK's client follows 307 and 308 redirects to other hosts, with the
	// request body (the plaintext key of Encrypt): KMS calls follow none.
	if hc, ok := cfg.HTTPClient.(*http.Client); ok && hc != nil {
		cfg.HTTPClient = withoutRedirects(hc)
	} else {
		b := awshttp.NewBuildableClient()
		cfg.HTTPClient = &http.Client{Transport: b.GetTransport(), Timeout: b.GetTimeout(), CheckRedirect: noRedirect}
	}
	return &awsKMS{client: kms.NewFromConfig(cfg), keyID: spec.KeyURI}, nil
}

func (a *awsKMS) Provider() string { return keywrap.ProviderAWSKMS }
func (a *awsKMS) KeyID() string    { return a.keyID }

// encryptionContext binds the ciphertext to its purpose.
func encryptionContext() map[string]string { return map[string]string{awsContextKey: keywrap.Purpose} }

func (a *awsKMS) Wrap(ctx context.Context, key []byte) ([]byte, error) {
	out, err := a.client.Encrypt(ctx, &kms.EncryptInput{KeyId: aws.String(a.keyID), Plaintext: key, EncryptionContext: encryptionContext()})
	if err != nil {
		return nil, awsError(err)
	}
	return out.CiphertextBlob, nil
}

func (a *awsKMS) Unwrap(ctx context.Context, ciphertext []byte) ([]byte, error) {
	// KeyId makes KMS refuse a ciphertext of any other key.
	out, err := a.client.Decrypt(ctx, &kms.DecryptInput{CiphertextBlob: ciphertext, KeyId: aws.String(a.keyID), EncryptionContext: encryptionContext()})
	if err != nil {
		return nil, awsError(err)
	}
	return out.Plaintext, nil
}

// awsPermanent lists the KMS error codes a retry does not fix.
var awsPermanent = map[string]bool{
	"InvalidCiphertextException": true, "IncorrectKeyException": true, "AccessDeniedException": true,
	"NotFoundException": true, "DisabledException": true, "KMSInvalidStateException": true,
	"InvalidKeyUsageException": true, "ValidationException": true, "UnrecognizedClientException": true,
	"InvalidSignatureException": true, "IncompleteSignature": true, "InvalidClientTokenId": true,
}

// awsError marks the permanent errors of KMS.
func awsError(err error) error {
	var re *awshttp.ResponseError
	if errors.As(err, &re) && isRedirect(re.HTTPStatusCode()) {
		return keywrap.Permanent(fmt.Errorf("aws-kms: HTTP %d: %w", re.HTTPStatusCode(), errRedirect))
	}
	var ae smithy.APIError
	if errors.As(err, &ae) && awsPermanent[ae.ErrorCode()] {
		return keywrap.Permanent(fmt.Errorf("aws-kms: %w", err))
	}
	return fmt.Errorf("aws-kms: %w", err)
}
