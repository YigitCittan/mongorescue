// Package notify delivers backup and restore event notifications to panel-managed
// channels (webhook, Telegram, SMTP e-mail, Twilio SMS) according to simple rule
// workflows: an event matches a Rule by type and job, and every channel referenced
// by the rule receives a rendered Message.
//
// Delivery is asynchronous and isolated from the backup pipeline: the Service
// subscribes to the events Bus, enqueues deliveries into a bounded queue, and a fixed
// worker pool sends them with a per-attempt timeout and exponential-backoff retries.
package notify

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
)

// Sentinel errors for notification configuration and delivery.
var (
	// ErrChannelNotFound is returned when a channel ID does not exist.
	ErrChannelNotFound = errors.New("notify: channel not found")
	// ErrRuleNotFound is returned when a rule ID does not exist.
	ErrRuleNotFound = errors.New("notify: rule not found")
	// ErrAlreadyExists is returned when creating a channel or rule whose ID is taken.
	ErrAlreadyExists = errors.New("notify: id already exists")
	// ErrInvalidChannelConfig is returned when a channel fails validation.
	ErrInvalidChannelConfig = errors.New("notify: invalid channel config")
	// ErrInvalidRule is returned when a rule fails validation.
	ErrInvalidRule = errors.New("notify: invalid rule")
	// ErrHeaderInjection is returned when a value destined for a protocol header
	// (e-mail or HTTP) contains CR or LF characters.
	ErrHeaderInjection = errors.New("notify: header value must not contain CR or LF")
	// ErrMaskedSecret is returned when a request carries the redaction placeholder for a
	// secret that cannot be matched to a stored value of the same channel.
	ErrMaskedSecret = errors.New("notify: secret is masked; supply the real value")
	// ErrPermanent marks a delivery failure that must not be retried (for example an
	// HTTP 4xx response other than 408/429). Test with errors.Is.
	ErrPermanent = errors.New("notify: permanent delivery failure")
)

// ChannelType identifies a notification transport.
type ChannelType string

const (
	// ChannelWebhook posts a signed JSON payload to an HTTP(S) endpoint. Generic SMS
	// gateways and chat tools with inbound webhooks are covered by this type.
	ChannelWebhook ChannelType = "webhook"
	// ChannelTelegram sends a message through the Telegram Bot API.
	ChannelTelegram ChannelType = "telegram"
	// ChannelEmail sends an e-mail through an SMTP relay.
	ChannelEmail ChannelType = "email"
	// ChannelTwilio sends an SMS through the Twilio Messages API.
	ChannelTwilio ChannelType = "twilio"
)

// Valid reports whether t is a supported channel type.
func (t ChannelType) Valid() bool {
	switch t {
	case ChannelWebhook, ChannelTelegram, ChannelEmail, ChannelTwilio:
		return true
	}
	return false
}

// Email security modes.
const (
	// SecurityNone sends mail over plain TCP (only advisable for local relays).
	SecurityNone = "none"
	// SecuritySTARTTLS upgrades a plain connection with STARTTLS (typically port 587).
	SecuritySTARTTLS = "starttls"
	// SecurityTLS uses implicit TLS from the first byte (typically port 465).
	SecurityTLS = "tls"
)

// Telegram parse modes.
const (
	// ParseModePlain sends plain text (the default).
	ParseModePlain = ""
	// ParseModeMarkdownV2 sends MarkdownV2 with all dynamic content escaped.
	ParseModeMarkdownV2 = "MarkdownV2"
)

// WebhookConfig configures a webhook channel.
type WebhookConfig struct {
	// URL is the http:// or https:// endpoint receiving a POST request.
	URL string `json:"url"`
	// Headers are extra request headers. Values of credential-bearing headers
	// (Authorization, X-API-Key, names containing "token" or "secret") are masked in
	// API responses.
	Headers map[string]string `json:"headers,omitempty"`
	// Secret, when set, signs the body with HMAC-SHA256 and sends
	// "X-MongoRescue-Signature: sha256=<hex>".
	Secret string `json:"secret,omitempty"`
}

// TelegramConfig configures a Telegram Bot API channel.
type TelegramConfig struct {
	// BotToken is the "<id>:<secret>" token issued by @BotFather.
	BotToken string `json:"bot_token"`
	// ChatID is a numeric chat ID or an "@channelusername".
	ChatID string `json:"chat_id"`
	// ParseMode is "" (plain text) or "MarkdownV2".
	ParseMode string `json:"parse_mode,omitempty"`
}

// EmailConfig configures an SMTP e-mail channel.
type EmailConfig struct {
	// Host is the SMTP server hostname.
	Host string `json:"host"`
	// Port is the SMTP server port.
	Port int `json:"port"`
	// Username enables SMTP AUTH PLAIN when non-empty.
	Username string `json:"username,omitempty"`
	// Password is the SMTP AUTH password.
	Password string `json:"password,omitempty"`
	// From is the envelope and header sender address.
	From string `json:"from"`
	// To lists recipient addresses.
	To []string `json:"to"`
	// Security is "none", "starttls" or "tls".
	Security string `json:"security"`
}

// TwilioConfig configures a Twilio SMS channel.
type TwilioConfig struct {
	// AccountSID is the Twilio account identifier ("AC" + 32 hex characters).
	AccountSID string `json:"account_sid"`
	// AuthToken is the Twilio auth token.
	AuthToken string `json:"auth_token"`
	// From is an E.164 sender number or a Messaging Service SID ("MG...").
	From string `json:"from"`
	// To lists E.164 recipient numbers.
	To []string `json:"to"`
}

// DeliveryStatus records the outcome of the most recent delivery to a channel.
type DeliveryStatus struct {
	// Time is when the delivery finished.
	Time time.Time `json:"time"`
	// Success reports whether the message was accepted by the provider.
	Success bool `json:"success"`
	// Attempts is the number of send attempts made.
	Attempts int `json:"attempts"`
	// Event is the event type that was delivered.
	Event events.EventType `json:"event"`
	// Error is the redacted failure reason, empty on success.
	Error string `json:"error,omitempty"`
}

// Channel is a configured notification destination. Exactly one of the type-specific
// configuration blocks matching Type must be set.
type Channel struct {
	// ID is the unique identifier (^[a-zA-Z0-9_-]{1,64}$).
	ID string `json:"id"`
	// Name is a human-readable label.
	Name string `json:"name"`
	// Type selects the transport.
	Type ChannelType `json:"type"`
	// Enabled controls whether rule-driven deliveries are sent.
	Enabled bool `json:"enabled"`
	// Webhook holds webhook settings when Type is "webhook".
	Webhook *WebhookConfig `json:"webhook,omitempty"`
	// Telegram holds Telegram settings when Type is "telegram".
	Telegram *TelegramConfig `json:"telegram,omitempty"`
	// Email holds SMTP settings when Type is "email".
	Email *EmailConfig `json:"email,omitempty"`
	// Twilio holds Twilio settings when Type is "twilio".
	Twilio *TwilioConfig `json:"twilio,omitempty"`
	// LastDelivery is the outcome of the most recent delivery attempt (server-managed).
	LastDelivery *DeliveryStatus `json:"last_delivery,omitempty"`
	// CreatedAt is when the channel was created (server-managed).
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is when the channel was last modified (server-managed).
	UpdatedAt time.Time `json:"updated_at"`
}

// Clone returns a deep copy of the channel.
func (c *Channel) Clone() *Channel {
	if c == nil {
		return nil
	}
	out := *c
	if c.Webhook != nil {
		w := *c.Webhook
		if c.Webhook.Headers != nil {
			w.Headers = make(map[string]string, len(c.Webhook.Headers))
			for k, v := range c.Webhook.Headers {
				w.Headers[k] = v
			}
		}
		out.Webhook = &w
	}
	if c.Telegram != nil {
		tg := *c.Telegram
		out.Telegram = &tg
	}
	if c.Email != nil {
		e := *c.Email
		e.To = slices.Clone(c.Email.To)
		out.Email = &e
	}
	if c.Twilio != nil {
		tw := *c.Twilio
		tw.To = slices.Clone(c.Twilio.To)
		out.Twilio = &tw
	}
	if c.LastDelivery != nil {
		ld := *c.LastDelivery
		out.LastDelivery = &ld
	}
	return &out
}

// Rule is a notification workflow: when an event whose type is listed in Events is
// emitted for one of JobIDs (or any job when JobIDs is empty), a message is sent to
// every channel in ChannelIDs.
type Rule struct {
	// ID is the unique identifier (^[a-zA-Z0-9_-]{1,64}$).
	ID string `json:"id"`
	// Name is a human-readable label.
	Name string `json:"name"`
	// Enabled controls whether the rule is evaluated.
	Enabled bool `json:"enabled"`
	// Events lists the event types that trigger the rule.
	Events []events.EventType `json:"events"`
	// JobIDs restricts the rule to specific scheduled jobs; empty matches every event,
	// including manual (job-less) backups and restores.
	JobIDs []string `json:"job_ids,omitempty"`
	// ChannelIDs lists the destination channels.
	ChannelIDs []string `json:"channel_ids"`
	// CreatedAt is when the rule was created (server-managed).
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is when the rule was last modified (server-managed).
	UpdatedAt time.Time `json:"updated_at"`
}

// Clone returns a deep copy of the rule.
func (r *Rule) Clone() *Rule {
	if r == nil {
		return nil
	}
	out := *r
	out.Events = slices.Clone(r.Events)
	out.JobIDs = slices.Clone(r.JobIDs)
	out.ChannelIDs = slices.Clone(r.ChannelIDs)
	return &out
}

// Matches reports whether the rule is enabled and selects e.
func (r *Rule) Matches(e events.Event) bool {
	if r == nil || !r.Enabled || !slices.Contains(r.Events, e.Type) {
		return false
	}
	return len(r.JobIDs) == 0 || slices.Contains(r.JobIDs, e.JobID)
}

// Message is a rendered, transport-agnostic notification.
type Message struct {
	// Subject is a single-line summary (CR/LF free).
	Subject string
	// Body is the multi-line human-readable text.
	Body string
	// Event is the source event, used for structured payloads.
	Event events.Event
}

// Notifier sends a Message over one transport.
type Notifier interface {
	// Type returns the channel type implemented by the notifier.
	Type() string
	// Send delivers msg, honouring ctx cancellation and deadlines.
	Send(ctx context.Context, msg Message) error
}

// Repository is the persistence port for channels and rules. Implementations must be
// safe for concurrent use and return copies that callers may mutate freely.
type Repository interface {
	// ListChannels returns all channels sorted by name.
	ListChannels(ctx context.Context) ([]*Channel, error)
	// GetChannel returns a channel or ErrChannelNotFound.
	GetChannel(ctx context.Context, id string) (*Channel, error)
	// SaveChannel creates or replaces a channel.
	SaveChannel(ctx context.Context, ch *Channel) error
	// DeleteChannel removes a channel and, atomically, its ID from every rule that
	// references it. It returns ErrChannelNotFound when the channel does not exist.
	DeleteChannel(ctx context.Context, id string) error
	// SaveDeliveryStatus updates the LastDelivery of an existing channel; it returns
	// ErrChannelNotFound when the channel was deleted meanwhile.
	SaveDeliveryStatus(ctx context.Context, id string, status DeliveryStatus) error

	// ListRules returns all rules sorted by name.
	ListRules(ctx context.Context) ([]*Rule, error)
	// GetRule returns a rule or ErrRuleNotFound.
	GetRule(ctx context.Context, id string) (*Rule, error)
	// SaveRule creates or replaces a rule.
	SaveRule(ctx context.Context, rule *Rule) error
	// DeleteRule removes a rule or returns ErrRuleNotFound.
	DeleteRule(ctx context.Context, id string) error
}
