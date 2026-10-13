package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/yigitcittan/mongorescue/internal/keywrap"
)

// gcpEndpoint is the Cloud KMS REST endpoint; gcpScope the OAuth scope it needs.
const (
	gcpEndpoint = "https://cloudkms.googleapis.com"
	gcpScope    = "https://www.googleapis.com/auth/cloudkms"
)

// crc32c is the Castagnoli table of the Cloud KMS integrity checks.
var crc32c = crc32.MakeTable(crc32.Castagnoli)

// gcpKMS wraps with the encrypt and decrypt methods of a Cloud KMS crypto key,
// over its REST API (the gRPC client library would add a large dependency tree for
// two calls). Requests carry CRC32C checksums that Cloud KMS verifies, and
// responses are verified in turn.
type gcpKMS struct {
	name     string
	endpoint string
	client   *http.Client
}

// newGCP returns the Cloud KMS wrapper of spec, authenticated with the
// Application Default Credentials (GOOGLE_APPLICATION_CREDENTIALS, Workload
// Identity, the metadata server of GCE, GKE and Cloud Run).
func newGCP(ctx context.Context, spec keywrap.Spec, opts Options) (keywrap.Wrapper, error) {
	base := baseClient(opts)
	// The token source outlives the call: it refreshes tokens on later requests.
	tctx := context.WithValue(context.WithoutCancel(ctx), oauth2.HTTPClient, base)
	ts := opts.GCPTokenSource
	if ts == nil {
		var err error
		if ts, err = google.DefaultTokenSource(tctx, gcpScope); err != nil {
			return nil, fmt.Errorf("gcp-kms: find the Application Default Credentials: %w", err)
		}
	}
	client := oauth2.NewClient(tctx, ts)
	client.Timeout = base.Timeout
	endpoint := strings.TrimRight(opts.GCPEndpoint, "/")
	if endpoint == "" {
		endpoint = gcpEndpoint
	}
	return &gcpKMS{name: spec.KeyURI, endpoint: endpoint, client: client}, nil
}

func (g *gcpKMS) Provider() string { return keywrap.ProviderGCPKMS }
func (g *gcpKMS) KeyID() string    { return g.name }

// checksum returns the CRC32C of b in the JSON form of an int64 (a string).
func checksum(b []byte) string { return strconv.FormatUint(uint64(crc32.Checksum(b, crc32c)), 10) }

// gcpEncryptRequest and the types below are the JSON bodies of the REST methods.
type gcpEncryptRequest struct {
	Plaintext                         []byte `json:"plaintext"`
	PlaintextCrc32c                   string `json:"plaintextCrc32c"`
	AdditionalAuthenticatedData       []byte `json:"additionalAuthenticatedData"`
	AdditionalAuthenticatedDataCrc32c string `json:"additionalAuthenticatedDataCrc32c"`
}

type gcpEncryptResponse struct {
	Name                                      string `json:"name"`
	Ciphertext                                []byte `json:"ciphertext"`
	CiphertextCrc32c                          string `json:"ciphertextCrc32c"`
	VerifiedPlaintextCrc32c                   bool   `json:"verifiedPlaintextCrc32c"`
	VerifiedAdditionalAuthenticatedDataCrc32c bool   `json:"verifiedAdditionalAuthenticatedDataCrc32c"`
}

type gcpDecryptRequest struct {
	Ciphertext                        []byte `json:"ciphertext"`
	CiphertextCrc32c                  string `json:"ciphertextCrc32c"`
	AdditionalAuthenticatedData       []byte `json:"additionalAuthenticatedData"`
	AdditionalAuthenticatedDataCrc32c string `json:"additionalAuthenticatedDataCrc32c"`
}

type gcpDecryptResponse struct {
	Plaintext       []byte `json:"plaintext"`
	PlaintextCrc32c string `json:"plaintextCrc32c"`
}

// errGCPIntegrity is returned when a checksum of Cloud KMS does not match: the
// request or response was corrupted on the way (retried).
var errGCPIntegrity = errors.New("gcp-kms: the response failed its CRC32C integrity check")

func (g *gcpKMS) Wrap(ctx context.Context, key []byte) ([]byte, error) {
	aad := []byte(keywrap.Purpose)
	var resp gcpEncryptResponse
	if err := g.call(ctx, "encrypt", gcpEncryptRequest{Plaintext: key, PlaintextCrc32c: checksum(key),
		AdditionalAuthenticatedData: aad, AdditionalAuthenticatedDataCrc32c: checksum(aad)}, &resp); err != nil {
		return nil, err
	}
	if !resp.VerifiedPlaintextCrc32c || !resp.VerifiedAdditionalAuthenticatedDataCrc32c || resp.CiphertextCrc32c != checksum(resp.Ciphertext) {
		return nil, errGCPIntegrity
	}
	return resp.Ciphertext, nil
}

func (g *gcpKMS) Unwrap(ctx context.Context, ciphertext []byte) ([]byte, error) {
	aad := []byte(keywrap.Purpose)
	var resp gcpDecryptResponse
	if err := g.call(ctx, "decrypt", gcpDecryptRequest{Ciphertext: ciphertext, CiphertextCrc32c: checksum(ciphertext),
		AdditionalAuthenticatedData: aad, AdditionalAuthenticatedDataCrc32c: checksum(aad)}, &resp); err != nil {
		return nil, err
	}
	if resp.PlaintextCrc32c != checksum(resp.Plaintext) {
		return nil, errGCPIntegrity
	}
	return resp.Plaintext, nil
}

// gcpError is the error body of a Google API.
type gcpError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// call POSTs body to <key>:<method> and decodes the response into out.
func (g *gcpKMS) call(ctx context.Context, method string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("gcp-kms: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.endpoint+"/v1/"+g.name+":"+method, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("gcp-kms: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("gcp-kms: %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return fmt.Errorf("gcp-kms: %s: read response: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		var e gcpError
		msg := http.StatusText(resp.StatusCode)
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Status + ": " + e.Error.Message
		}
		err = fmt.Errorf("gcp-kms: %s: HTTP %d: %s", method, resp.StatusCode, msg)
		if statusPermanent(resp.StatusCode) {
			return keywrap.Permanent(err)
		}
		return err
	}
	if err = json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("gcp-kms: %s: decode response: %w", method, err)
	}
	return nil
}
