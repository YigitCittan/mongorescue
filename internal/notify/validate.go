package notify

import (
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
)

// maxNameLength bounds channel and rule display names.
const maxNameLength = 128

var (
	telegramTokenPattern = regexp.MustCompile(`^[0-9]{1,20}:[A-Za-z0-9_-]{20,100}$`)
	telegramChatPattern  = regexp.MustCompile(`^(-?[0-9]{1,20}|@[A-Za-z0-9_]{4,64})$`)
	twilioSIDPattern     = regexp.MustCompile(`^AC[0-9a-fA-F]{32}$`)
	twilioMsgSvcPattern  = regexp.MustCompile(`^MG[0-9a-fA-F]{32}$`)
	e164Pattern          = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
	headerNamePattern    = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]{1,128}$")
)

// invalidf wraps ErrInvalidChannelConfig with a formatted detail.
func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidChannelConfig, fmt.Sprintf(format, args...))
}

// hasCRLF reports whether s contains a carriage return or line feed.
func hasCRLF(s string) bool {
	return strings.ContainsAny(s, "\r\n")
}

// checkHeaderValues rejects CR/LF in any of the named values.
func checkHeaderValues(fields map[string]string) error {
	for name, v := range fields {
		if hasCRLF(v) {
			return fmt.Errorf("%w: %w (%s)", ErrInvalidChannelConfig, ErrHeaderInjection, name)
		}
	}
	return nil
}

// validateName checks a display name.
func validateName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("name is required")
	}
	if utf8.RuneCountInString(name) > maxNameLength {
		return fmt.Errorf("name must be at most %d characters", maxNameLength)
	}
	if strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("name must not contain control characters")
	}
	return nil
}

// Validate checks the channel identity and its type-specific configuration. Every
// failure wraps ErrInvalidChannelConfig (or models.ErrInvalidID for the ID).
func (c *Channel) Validate() error {
	if c == nil {
		return invalidf("channel is required")
	}
	if err := models.ValidateID(c.ID); err != nil {
		return err
	}
	if err := validateName(c.Name); err != nil {
		return invalidf("%v", err)
	}
	if !c.Type.Valid() {
		return invalidf("unsupported type %q", c.Type)
	}

	configured := 0
	for _, set := range []bool{c.Webhook != nil, c.Telegram != nil, c.Email != nil, c.Twilio != nil} {
		if set {
			configured++
		}
	}
	if configured != 1 {
		return invalidf("exactly one configuration block matching type %q is required", c.Type)
	}

	switch c.Type {
	case ChannelWebhook:
		return validateWebhook(c.Webhook)
	case ChannelTelegram:
		return validateTelegram(c.Telegram)
	case ChannelEmail:
		return validateEmail(c.Email)
	case ChannelTwilio:
		return validateTwilio(c.Twilio)
	}
	return nil
}

// validateWebhook checks a webhook configuration.
func validateWebhook(w *WebhookConfig) error {
	if w == nil {
		return invalidf("webhook configuration is required")
	}
	if hasCRLF(w.URL) {
		return invalidf("url must not contain CR or LF")
	}
	u, err := url.Parse(w.URL)
	if err != nil {
		return invalidf("url is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return invalidf("url scheme must be http or https")
	}
	if u.Host == "" || u.Hostname() == "" {
		return invalidf("url must include a host")
	}
	if len(w.Headers) > 32 {
		return invalidf("at most 32 custom headers are allowed")
	}
	for name, value := range w.Headers {
		if !headerNamePattern.MatchString(name) {
			return invalidf("invalid header name %q", name)
		}
		if hasCRLF(value) {
			return fmt.Errorf("%w: %w (header %s)", ErrInvalidChannelConfig, ErrHeaderInjection, name)
		}
		switch strings.ToLower(name) {
		case "host", "content-length", "transfer-encoding", "connection", strings.ToLower(signatureHeader):
			return invalidf("header %q is managed by MongoRescue and cannot be overridden", name)
		}
	}
	return nil
}

// validateTelegram checks a Telegram configuration.
func validateTelegram(t *TelegramConfig) error {
	if t == nil {
		return invalidf("telegram configuration is required")
	}
	if !telegramTokenPattern.MatchString(t.BotToken) {
		return invalidf("bot_token must look like <digits>:<token>")
	}
	if !telegramChatPattern.MatchString(t.ChatID) {
		return invalidf("chat_id must be a numeric ID or @channelusername")
	}
	if t.ParseMode != ParseModePlain && t.ParseMode != ParseModeMarkdownV2 {
		return invalidf("parse_mode must be empty or %q", ParseModeMarkdownV2)
	}
	return nil
}

// validateEmail checks an SMTP configuration, rejecting header injection in every
// field that reaches the SMTP dialogue or message headers.
func validateEmail(e *EmailConfig) error {
	if e == nil {
		return invalidf("email configuration is required")
	}
	fields := map[string]string{
		"host": e.Host, "username": e.Username, "password": e.Password, "from": e.From, "security": e.Security,
	}
	for i, to := range e.To {
		fields[fmt.Sprintf("to[%d]", i)] = to
	}
	if err := checkHeaderValues(fields); err != nil {
		return err
	}
	if strings.TrimSpace(e.Host) == "" || strings.ContainsAny(e.Host, " /\\@") {
		return invalidf("host is required and must be a hostname")
	}
	if e.Port < 1 || e.Port > 65535 {
		return invalidf("port must be between 1 and 65535")
	}
	switch e.Security {
	case SecurityNone, SecuritySTARTTLS, SecurityTLS:
	default:
		return invalidf("security must be one of none, starttls, tls")
	}
	if e.Password != "" && e.Username == "" {
		return invalidf("username is required when a password is set")
	}
	if _, err := parseAddress(e.From); err != nil {
		return invalidf("from: %v", err)
	}
	if len(e.To) == 0 || len(e.To) > 50 {
		return invalidf("between 1 and 50 recipients are required")
	}
	for _, to := range e.To {
		if _, err := parseAddress(to); err != nil {
			return invalidf("to %q: %v", to, err)
		}
	}
	return nil
}

// parseAddress parses a single RFC 5322 address and returns the bare addr-spec.
func parseAddress(s string) (string, error) {
	if hasCRLF(s) {
		return "", ErrHeaderInjection
	}
	addr, err := mail.ParseAddress(s)
	if err != nil {
		return "", fmt.Errorf("invalid address")
	}
	return addr.Address, nil
}

// validateTwilio checks a Twilio configuration.
func validateTwilio(t *TwilioConfig) error {
	if t == nil {
		return invalidf("twilio configuration is required")
	}
	if !twilioSIDPattern.MatchString(t.AccountSID) {
		return invalidf("account_sid must be AC followed by 32 hex characters")
	}
	if strings.TrimSpace(t.AuthToken) == "" || hasCRLF(t.AuthToken) {
		return invalidf("auth_token is required")
	}
	if !e164Pattern.MatchString(t.From) && !twilioMsgSvcPattern.MatchString(t.From) {
		return invalidf("from must be an E.164 number (+15551234567) or a Messaging Service SID")
	}
	if len(t.To) == 0 || len(t.To) > 20 {
		return invalidf("between 1 and 20 recipients are required")
	}
	for _, to := range t.To {
		if !e164Pattern.MatchString(to) {
			return invalidf("recipient %q must be an E.164 number", to)
		}
	}
	return nil
}

// Validate checks rule identity and contents. Channel existence is checked by the
// Service. Every failure wraps ErrInvalidRule (or models.ErrInvalidID for the ID).
func (r *Rule) Validate() error {
	if r == nil {
		return fmt.Errorf("%w: rule is required", ErrInvalidRule)
	}
	if err := models.ValidateID(r.ID); err != nil {
		return err
	}
	if err := validateName(r.Name); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRule, err)
	}
	if len(r.Events) == 0 {
		return fmt.Errorf("%w: at least one event is required", ErrInvalidRule)
	}
	for _, et := range r.Events {
		if !et.Subscribable() {
			return fmt.Errorf("%w: unsupported event %q", ErrInvalidRule, et)
		}
	}
	if len(r.ChannelIDs) == 0 {
		return fmt.Errorf("%w: at least one channel is required", ErrInvalidRule)
	}
	for _, id := range r.JobIDs {
		// Legacy job IDs may predate the strict ID pattern; only reject clearly unsafe values.
		if strings.TrimSpace(id) == "" || len(id) > 256 || strings.ContainsFunc(id, func(r rune) bool { return r < 0x20 }) {
			return fmt.Errorf("%w: invalid job id %q", ErrInvalidRule, id)
		}
	}
	return nil
}

// isSecretHeader reports whether a webhook header carries a credential.
func isSecretHeader(name string) bool {
	n := strings.ToLower(name)
	switch n {
	case "authorization", "proxy-authorization", "x-api-key", "cookie":
		return true
	}
	return strings.Contains(n, "token") || strings.Contains(n, "secret") || strings.Contains(n, "password")
}

// maskValue returns redact.Mask for non-empty secrets and "" otherwise.
func maskValue(v string) string {
	if v == "" {
		return ""
	}
	return redact.Mask
}

// RedactEndpoint masks everything after the origin of an HTTP(S) endpoint. Webhook
// URLs frequently carry their credential in the path or query (Slack
// /services/T/B/<secret>, Discord /api/webhooks/<id>/<token>, ?token=, ?sig=), so any
// URL with userinfo, a non-root path, a query or a fragment is rendered as
// "scheme://host[:port]/******". A bare origin is returned unchanged; an unparsable
// value is replaced entirely by redact.Mask.
func RedactEndpoint(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return redact.Mask
	}
	if u.User == nil && (u.Path == "" || u.Path == "/") && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" {
		return raw
	}
	return u.Scheme + "://" + u.Host + "/" + redact.Mask
}

// Redacted returns a deep copy that is safe to serialize to API clients: bot tokens,
// SMTP passwords, Twilio auth tokens, webhook HMAC secrets, credential header values
// and everything after the origin of webhook URLs are replaced by redact.Mask.
func (c *Channel) Redacted() *Channel {
	out := c.Clone()
	if out == nil {
		return nil
	}
	if w := out.Webhook; w != nil {
		w.URL = RedactEndpoint(w.URL)
		w.Secret = maskValue(w.Secret)
		for k, v := range w.Headers {
			if isSecretHeader(k) {
				w.Headers[k] = maskValue(v)
			}
		}
	}
	if tg := out.Telegram; tg != nil {
		tg.BotToken = maskValue(tg.BotToken)
	}
	if e := out.Email; e != nil {
		e.Password = maskValue(e.Password)
	}
	if tw := out.Twilio; tw != nil {
		tw.AuthToken = maskValue(tw.AuthToken)
	}
	return out
}

// errDestinationChanged is returned when a masked secret would be carried over to a
// different destination, which would let a client exfiltrate stored credentials.
var errDestinationChanged = fmt.Errorf("%w: re-enter secrets when changing the destination", ErrMaskedSecret)

// keepSecret resolves a possibly-masked incoming secret against the stored value.
// A masked value is replaced by the stored secret when one exists; otherwise the
// request is rejected with ErrMaskedSecret.
func keepSecret(field, incoming, stored string) (string, error) {
	if incoming != redact.Mask {
		return incoming, nil
	}
	if stored == "" {
		return "", fmt.Errorf("%w (%s)", ErrMaskedSecret, field)
	}
	return stored, nil
}

// restoreSecrets replaces masked placeholders in in with the secrets of existing,
// which must be the stored channel with the same ID (nil for new channels).
//
// A masked secret is only carried over when the channel type and the destination the
// secret is sent to are unchanged: the exact stored webhook URL (multi-tenant gateways
// may route /tenantA and /tenantB on one host); SMTP host, port, username and security
// mode; Twilio account SID. The Telegram bot token is always sent to the
// fixed Bot API host and may be kept. Any other masked value yields ErrMaskedSecret.
func restoreSecrets(in, existing *Channel) error {
	if existing != nil && existing.Type != in.Type {
		existing = nil
	}
	switch in.Type {
	case ChannelWebhook:
		return restoreWebhookSecrets(in.Webhook, existing)
	case ChannelTelegram:
		if in.Telegram == nil {
			return nil
		}
		stored := ""
		if existing != nil && existing.Telegram != nil {
			stored = existing.Telegram.BotToken
		}
		var err error
		in.Telegram.BotToken, err = keepSecret("bot_token", in.Telegram.BotToken, stored)
		return err
	case ChannelEmail:
		if in.Email == nil || in.Email.Password != redact.Mask {
			return nil
		}
		if existing == nil || existing.Email == nil {
			return fmt.Errorf("%w (password)", ErrMaskedSecret)
		}
		old := existing.Email
		if !strings.EqualFold(old.Host, in.Email.Host) || old.Port != in.Email.Port ||
			old.Username != in.Email.Username || old.Security != in.Email.Security {
			return errDestinationChanged
		}
		var err error
		in.Email.Password, err = keepSecret("password", in.Email.Password, old.Password)
		return err
	case ChannelTwilio:
		if in.Twilio == nil || in.Twilio.AuthToken != redact.Mask {
			return nil
		}
		if existing == nil || existing.Twilio == nil {
			return fmt.Errorf("%w (auth_token)", ErrMaskedSecret)
		}
		if existing.Twilio.AccountSID != in.Twilio.AccountSID {
			return errDestinationChanged
		}
		var err error
		in.Twilio.AuthToken, err = keepSecret("auth_token", in.Twilio.AuthToken, existing.Twilio.AuthToken)
		return err
	}
	return nil
}

// restoreWebhookSecrets applies the restoreSecrets rules to a webhook configuration.
func restoreWebhookSecrets(in *WebhookConfig, existing *Channel) error {
	if in == nil {
		return nil
	}
	var old *WebhookConfig
	if existing != nil {
		old = existing.Webhook
	}

	if strings.Contains(in.URL, redact.Mask) {
		// Only the exact redacted form of the stored URL may stand in for it.
		if old == nil || old.URL == "" || RedactEndpoint(old.URL) != in.URL {
			return fmt.Errorf("%w (url): supply the full URL", ErrMaskedSecret)
		}
		in.URL = old.URL
	}

	masked := in.Secret == redact.Mask
	for _, v := range in.Headers {
		if v == redact.Mask {
			masked = true
		}
	}
	if !masked {
		return nil
	}
	if old == nil {
		return fmt.Errorf("%w (webhook)", ErrMaskedSecret)
	}
	// Retained header values and the HMAC secret are bound to the exact endpoint: the
	// URL must be the stored one (typically round-tripped in its masked form).
	if old.URL == "" || in.URL != old.URL {
		return errDestinationChanged
	}

	var err error
	if in.Secret, err = keepSecret("secret", in.Secret, old.Secret); err != nil {
		return err
	}
	for k, v := range in.Headers {
		if v != redact.Mask {
			continue
		}
		stored := ""
		for ok, ov := range old.Headers {
			if strings.EqualFold(ok, k) {
				stored = ov
			}
		}
		if in.Headers[k], err = keepSecret("header "+k, v, stored); err != nil {
			return err
		}
	}
	return nil
}

// secretsOf returns every non-empty secret of the channel, used to scrub error text.
// Webhook URLs, their paths and query values are included as defense in depth.
func secretsOf(c *Channel) []string {
	var out []string
	add := func(s string) {
		if len(s) >= 4 {
			out = append(out, s)
		}
	}
	if w := c.Webhook; w != nil {
		add(w.Secret)
		for k, v := range w.Headers {
			if isSecretHeader(k) {
				add(v)
			}
		}
		add(w.URL)
		if u, err := url.Parse(w.URL); err == nil {
			if u.Path != "/" {
				add(u.Path)
				add(u.EscapedPath())
			}
			add(u.RawQuery)
			for _, vals := range u.Query() {
				for _, v := range vals {
					add(v)
				}
			}
			if p, ok := u.User.Password(); ok {
				add(p)
			}
		}
	}
	if tg := c.Telegram; tg != nil {
		add(tg.BotToken)
	}
	if e := c.Email; e != nil {
		add(e.Password)
	}
	if tw := c.Twilio; tw != nil {
		add(tw.AuthToken)
	}
	// Longest first so a full URL is replaced before its fragments.
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}
