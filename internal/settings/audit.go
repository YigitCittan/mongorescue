package settings

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Limits of the audit log settings.
const (
	// MinAuditRetentionDays is the shortest audit log retention.
	MinAuditRetentionDays = 30
	// DefaultAuditRetentionDays is the audit log retention of a fresh installation.
	DefaultAuditRetentionDays = 365
	// maxAuditURLLength bounds Audit.WebhookURL.
	maxAuditURLLength = 2048
	// maxAuditSecretLength bounds Audit.WebhookSecret.
	maxAuditSecretLength = 1024
)

// Audit configures the audit log of every action (internal/auditlog): how long its
// entries are kept and the optional webhook every new entry is forwarded to.
type Audit struct {
	// RetentionDays keeps entries this many days (30 to 36500, default 365).
	RetentionDays int `json:"retention_days"`
	// WebhookURL receives every new entry as a JSON POST ("" = off). Secret: it
	// often carries a token, so API responses show only its origin.
	WebhookURL string `json:"webhook_url"`
	// WebhookSecret signs the requests (HMAC-SHA256 in X-MongoRescue-Signature).
	// Secret.
	WebhookSecret string `json:"webhook_secret"`
}

// defaultAudit returns the audit settings of a fresh installation.
func defaultAudit() Audit {
	return Audit{RetentionDays: DefaultAuditRetentionDays}
}

// masked returns a with its secrets masked: the webhook URL reduced to its origin
// when it has more (see maskEndpoint), the signing secret replaced by SecretMask.
func (a Audit) masked() Audit {
	a.WebhookURL = maskEndpoint(a.WebhookURL)
	if a.WebhookSecret != "" {
		a.WebhookSecret = SecretMask
	}
	return a
}

// maskEndpoint masks everything after the origin of an HTTP(S) URL, the rule of
// notify.RedactEndpoint: webhook URLs often carry their credential in the path or
// query. A bare origin is returned unchanged, an unparsable value is SecretMask.
func maskEndpoint(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return SecretMask
	}
	if u.User == nil && (u.Path == "" || u.Path == "/") && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" {
		return raw
	}
	return u.Scheme + "://" + u.Host + "/" + SecretMask
}

// AuditPatch updates Audit. WebhookURL keeps the stored URL when it is sent back
// masked (as GET /api/v1/settings shows it) or as SecretMask, "" turns forwarding
// off and any other value replaces it; WebhookSecret follows the keep-secret rule.
type AuditPatch struct {
	RetentionDays *int    `json:"retention_days,omitempty"`
	WebhookURL    *string `json:"webhook_url,omitempty"`
	WebhookSecret *string `json:"webhook_secret,omitempty"`
}

// apply sets the fields of p that are present on a, resolving masked secrets
// against a. A nil p changes nothing.
func (p *AuditPatch) apply(a *Audit) error {
	if p == nil {
		return nil
	}
	setIf(&a.RetentionDays, p.RetentionDays)
	if p.WebhookURL != nil {
		in := strings.TrimSpace(*p.WebhookURL)
		switch {
		case in == SecretMask && a.WebhookURL == "":
			return fmt.Errorf("audit.webhook_url: %w", ErrMaskedSecret)
		case in == SecretMask, a.WebhookURL != "" && in == maskEndpoint(a.WebhookURL):
			// keep the stored URL
		default:
			// The signing secret is never kept for another host: it must be sent
			// again with the new URL.
			if a.WebhookSecret != "" && in != "" && !sameHost(in, a.WebhookURL) &&
				(p.WebhookSecret == nil || *p.WebhookSecret == SecretMask) {
				return fmt.Errorf("%w: audit.webhook_secret: the webhook host changed; enter the signing secret again", ErrSecretReentry)
			}
			a.WebhookURL = in
		}
	}
	if p.WebhookSecret != nil {
		v, err := keepSecret(*p.WebhookSecret, a.WebhookSecret)
		if err != nil {
			return fmt.Errorf("audit.webhook_secret: %w", err)
		}
		a.WebhookSecret = v
	}
	return nil
}

// sameHost reports whether two URLs name the same host and port, after
// normalisation (see hostKey); unparsable URLs or URLs without a host never match.
func sameHost(a, b string) bool {
	ka, kb := hostKey(a), hostKey(b)
	return ka != "" && ka == kb
}

// hostKey returns "host:port" of raw: the host lower-cased without a trailing
// dot, the port explicit (443 for https, 80 for http when omitted), or "".
func hostKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return net.JoinHostPort(host, port)
}

// validateAudit checks a. The webhook host is checked again on every connection
// (after DNS resolution), where link-local and cloud metadata addresses are refused.
func validateAudit(a *Audit) error {
	switch {
	case a.RetentionDays < MinAuditRetentionDays || a.RetentionDays > maxRetentionDays:
		return fmt.Errorf("%w: audit.retention_days must be between %d and %d", ErrInvalid, MinAuditRetentionDays, maxRetentionDays)
	case len(a.WebhookSecret) > maxAuditSecretLength:
		return fmt.Errorf("%w: audit.webhook_secret is longer than %d bytes", ErrInvalid, maxAuditSecretLength)
	}
	if a.WebhookURL == "" {
		return nil
	}
	if len(a.WebhookURL) > maxAuditURLLength || strings.ContainsFunc(a.WebhookURL, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("%w: audit.webhook_url is not an http or https URL", ErrInvalid)
	}
	u, err := url.Parse(a.WebhookURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("%w: audit.webhook_url is not an http or https URL", ErrInvalid)
	}
	return nil
}
