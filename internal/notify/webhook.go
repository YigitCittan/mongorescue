package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Webhook protocol constants.
const (
	// WebhookPayloadVersion is the schema version carried in every webhook payload.
	WebhookPayloadVersion = 1
	// signatureHeader carries the hex HMAC-SHA256 of the request body.
	signatureHeader = "X-MongoRescue-Signature"
	// eventHeader carries the event type for cheap routing on the receiver side.
	eventHeader = "X-MongoRescue-Event"
	// userAgent identifies MongoRescue notifier requests.
	userAgent = "MongoRescue-Notifier/1"
)

// WebhookPayload is the stable JSON document POSTed by webhook channels.
//
// Schema (version 1):
//
//	{
//	  "version": 1,
//	  "event": "backup.failed",          // backup.succeeded|backup.failed|restore.succeeded|restore.failed|notification.test
//	  "time": "2026-09-24T03:00:00Z",   // RFC 3339, UTC
//	  "job_id": "nightly-shop",          // omitted for manual runs
//	  "backup_id": "bkp_shop_...",       // omitted when unknown
//	  "restore_id": "rst_shop_...",      // restore events only
//	  "database": "shop",                // backup source or restore target
//	  "status": "failed",
//	  "error": "…",                      // redacted; omitted on success
//	  "duration_seconds": 12.5,
//	  "size_bytes": 1048576,             // backup events only
//	  "subject": "❌ Backup failed: job nightly-shop (db shop)",
//	  "text": "❌ Backup failed: … — 2026-09-24T03:00Z"
//	}
//
// When a secret is configured the request carries
// "X-MongoRescue-Signature: sha256=<hex(HMAC-SHA256(secret, body))>".
type WebhookPayload struct {
	// Version is the payload schema version (always WebhookPayloadVersion).
	Version int `json:"version"`
	// Event is the event type.
	Event string `json:"event"`
	// Time is the event time in UTC.
	Time time.Time `json:"time"`
	// JobID is the scheduled job, if any.
	JobID string `json:"job_id,omitempty"`
	// BackupID is the backup record, if any.
	BackupID string `json:"backup_id,omitempty"`
	// RestoreID is the restore record, if any.
	RestoreID string `json:"restore_id,omitempty"`
	// Database is the backed-up database or restore target.
	Database string `json:"database,omitempty"`
	// Status is the final record status.
	Status string `json:"status,omitempty"`
	// Error is the redacted failure reason.
	Error string `json:"error,omitempty"`
	// DurationSeconds is the operation duration.
	DurationSeconds float64 `json:"duration_seconds"`
	// SizeBytes is the backup archive size.
	SizeBytes int64 `json:"size_bytes,omitempty"`
	// Subject is the rendered one-line summary.
	Subject string `json:"subject"`
	// Text is the rendered human-readable body.
	Text string `json:"text"`
}

// NewWebhookPayload builds the payload for msg.
func NewWebhookPayload(msg Message) WebhookPayload {
	e := msg.Event
	return WebhookPayload{
		Version:         WebhookPayloadVersion,
		Event:           string(e.Type),
		Time:            e.Time.UTC(),
		JobID:           e.JobID,
		BackupID:        e.BackupID,
		RestoreID:       e.RestoreID,
		Database:        e.Database,
		Status:          e.Status,
		Error:           e.Error,
		DurationSeconds: e.Duration.Seconds(),
		SizeBytes:       e.SizeBytes,
		Subject:         msg.Subject,
		Text:            msg.Body,
	}
}

// Sign returns the "sha256=<hex>" signature of body under secret.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// WebhookNotifier POSTs a signed JSON payload to an HTTP(S) endpoint.
type WebhookNotifier struct {
	cfg    WebhookConfig
	client *http.Client
}

// NewWebhookNotifier returns a webhook notifier. A nil client uses NewHTTPClient.
func NewWebhookNotifier(cfg WebhookConfig, client *http.Client) *WebhookNotifier {
	if client == nil {
		client = NewHTTPClient()
	}
	return &WebhookNotifier{cfg: cfg, client: client}
}

// Type returns "webhook".
func (n *WebhookNotifier) Type() string { return string(ChannelWebhook) }

// Send POSTs the JSON payload for msg.
func (n *WebhookNotifier) Send(ctx context.Context, msg Message) error {
	if err := validateWebhook(&n.cfg); err != nil {
		return err
	}
	body, err := json.Marshal(NewWebhookPayload(msg))
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return scrub(sanitizeURLError("build webhook request", err), n.secrets()...)
	}
	for k, v := range n.cfg.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set(eventHeader, string(msg.Event.Type))
	if n.cfg.Secret != "" {
		req.Header.Set(signatureHeader, Sign(n.cfg.Secret, body))
	}

	if _, err := doHTTP(ctx, n.client, req); err != nil {
		return scrub(fmt.Errorf("webhook: %w", err), n.secrets()...)
	}
	return nil
}

// secrets lists values that must never appear in error messages.
func (n *WebhookNotifier) secrets() []string {
	return secretsOf(&Channel{Webhook: &n.cfg})
}
