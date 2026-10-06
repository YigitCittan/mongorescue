package connections

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotls"
	"github.com/yigitcittan/mongorescue/internal/mongotools"
	"github.com/yigitcittan/mongorescue/internal/redact"
)

// ErrMaskedTLSSecret is returned when the TLS client key or its password is the
// redaction placeholder but the connection has no stored value to keep.
var ErrMaskedTLSSecret = errors.New("connections: tls client key or password is masked; supply the value")

// ErrInsecureNotConfirmed is returned when tls_insecure is turned on without
// tls_insecure_confirm.
var ErrInsecureNotConfirmed = errors.New("connections: tls_insecure disables certificate verification; set tls_insecure_confirm to turn it on")

// TLSInput is the client-editable TLS material of a connection. An omitted field
// keeps the stored value (none for a new connection) and an empty string removes
// it. The client key and its password, which API responses mask, also keep the
// stored value when they are exactly redact.Mask.
type TLSInput struct {
	// CAPEM is the PEM bundle of the certificate authorities to trust.
	CAPEM *string `json:"tls_ca_pem,omitempty"`
	// ClientCertPEM and ClientKeyPEM are the x509 client certificate and its key.
	ClientCertPEM *string `json:"tls_client_cert_pem,omitempty"`
	ClientKeyPEM  *string `json:"tls_client_key_pem,omitempty"`
	// ClientKeyPassword decrypts an encrypted PKCS#8 client key.
	ClientKeyPassword *string `json:"tls_client_key_password,omitempty"`
	// AllowInvalidHostnames skips the server hostname check.
	AllowInvalidHostnames *bool `json:"tls_allow_invalid_hostnames,omitempty"`
	// Insecure disables every server certificate check. Turning it on requires
	// ConfirmInsecure.
	Insecure        *bool `json:"tls_insecure,omitempty"`
	ConfirmInsecure bool  `json:"tls_insecure_confirm,omitempty"`
}

// uriTLSOptions are connection string options that configure what the TLS fields
// configure. A connection uses one or the other, so the tools and the driver never
// receive two answers to the same question.
var uriTLSOptions = []string{
	"tlsCAFile", "tlsCertificateKeyFile", "tlsCertificateKeyFilePassword",
	"tlsInsecure", "tlsAllowInvalidCertificates", "tlsAllowInvalidHostnames",
	"sslCAFile", "sslPEMKeyFile", "sslPEMKeyPassword",
	"sslAllowInvalidCertificates", "sslAllowInvalidHostnames",
}

// resolve applies in to stored and returns the validated result for uri.
func (in TLSInput) resolve(stored models.ConnectionTLS, uri string) (models.ConnectionTLS, error) {
	out := stored
	setPEM(&out.CAPEM, in.CAPEM)
	setPEM(&out.ClientCertPEM, in.ClientCertPEM)
	if err := keepMasked(&out.ClientKeyPEM, in.ClientKeyPEM, stored.ClientKeyPEM, true); err != nil {
		return out, err
	}
	if err := keepMasked(&out.ClientKeyPassword, in.ClientKeyPassword, stored.ClientKeyPassword, false); err != nil {
		return out, err
	}
	if in.AllowInvalidHostnames != nil {
		out.AllowInvalidHostnames = *in.AllowInvalidHostnames
	}
	if in.Insecure != nil {
		out.Insecure = *in.Insecure
	}
	if out.Insecure && !stored.Insecure && !in.ConfirmInsecure {
		return out, fmt.Errorf("%w: %w", ErrInvalid, ErrInsecureNotConfirmed)
	}
	if err := mongotls.Validate(&out); err != nil {
		return out, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := checkURITLS(uri, &out); err != nil {
		return out, err
	}
	return out, nil
}

// setPEM replaces *dst with the trimmed v (ending in a newline) when v is set.
func setPEM(dst, v *string) {
	if v == nil {
		return
	}
	*dst = normalizePEM(*v)
}

// normalizePEM trims surrounding whitespace and ends non-empty PEM with a newline.
func normalizePEM(v string) string {
	v = strings.TrimSpace(strings.ReplaceAll(v, "\r\n", "\n"))
	if v == "" {
		return ""
	}
	return v + "\n"
}

// keepMasked sets *dst from v: nil or redact.Mask keeps stored (an error when there
// is none to keep), anything else replaces it. PEM values are normalised.
func keepMasked(dst, v *string, stored string, isPEM bool) error {
	switch {
	case v == nil:
		return nil
	case *v == redact.Mask:
		if stored == "" {
			return fmt.Errorf("%w: %w", ErrInvalid, ErrMaskedTLSSecret)
		}
		*dst = stored
	case isPEM:
		*dst = normalizePEM(*v)
	default:
		*dst = *v
	}
	return nil
}

// checkURITLS rejects TLS material on a URI that turns TLS off or sets the same
// TLS options itself.
func checkURITLS(uri string, t *models.ConnectionTLS) error {
	if t.IsZero() {
		return nil
	}
	for _, name := range []string{"tls", "ssl"} {
		if v, ok := mongotools.OptionValue(uri, name); ok && strings.EqualFold(v, "false") {
			return fmt.Errorf("%w: the uri sets %s=false; remove it to use the TLS settings", ErrInvalid, name)
		}
	}
	for _, name := range uriTLSOptions {
		if mongotools.HasOption(uri, name) {
			return fmt.Errorf("%w: the uri sets %s; configure TLS either in the uri or in the TLS settings, not both", ErrInvalid, name)
		}
	}
	return nil
}

// warnTLS logs the loosened certificate checks of connection id.
func (s *Service) warnTLS(id string, t models.ConnectionTLS) {
	switch {
	case t.Insecure:
		s.logger.Warn("connection does not verify the server certificate (tls_insecure): it is open to interception",
			slog.String("connection_id", id))
	case t.AllowInvalidHostnames:
		s.logger.Warn("connection does not check the server hostname (tls_allow_invalid_hostnames)",
			slog.String("connection_id", id))
	}
}
