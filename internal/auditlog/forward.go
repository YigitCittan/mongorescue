package auditlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/notify"
	"github.com/yigitcittan/mongorescue/internal/redact"
)

// Forwarding limits.
const (
	// DefaultQueueSize is the number of entries waiting to be forwarded; entries
	// recorded while the queue is full are dropped and counted.
	DefaultQueueSize = 1024
	// SignatureHeader carries "sha256=<hex>", the HMAC-SHA256 of the body under the
	// signing secret, when one is set (the same header as notification webhooks).
	SignatureHeader = "X-MongoRescue-Signature"
	// drainTimeout bounds delivering the queued entries at shutdown.
	drainTimeout = 5 * time.Second
	// failureLogInterval spaces the warnings about failing deliveries.
	failureLogInterval = time.Minute
	// maxErrorLength caps the last delivery error kept for the status.
	maxErrorLength = 300
)

// Endpoint is where entries are forwarded: an HTTP(S) URL and an optional HMAC
// signing secret. An empty URL turns forwarding off.
type Endpoint struct {
	URL    string
	Secret string
}

// ForwarderConfig configures a Forwarder.
type ForwarderConfig struct {
	// Endpoint returns the current endpoint (read for every entry, so settings
	// changes apply at once).
	Endpoint func() Endpoint
	// Client sends the requests (notify.NewHTTPClient when nil: a timeout, no
	// redirects, blocked link-local and metadata addresses).
	Client *http.Client
	// QueueSize bounds the queue (DefaultQueueSize when <= 0).
	QueueSize int
	// Observe is told the outcome of every entry: "sent", "failed" or "dropped".
	Observe func(outcome string)
	// Logger reports failing deliveries (slog.Default() when nil).
	Logger *slog.Logger
}

// Forward outcomes passed to ForwarderConfig.Observe.
const (
	// ForwardSent is an entry the endpoint accepted (2xx).
	ForwardSent = "sent"
	// ForwardFailed is an entry the endpoint refused or could not be reached for.
	ForwardFailed = "failed"
	// ForwardDropped is an entry dropped because the queue was full or stopped.
	ForwardDropped = "dropped"
)

// ForwardStatus reports the forwarder's counters since start.
type ForwardStatus struct {
	// Enabled reports whether an endpoint is configured.
	Enabled bool `json:"enabled"`
	// Queued is the number of entries waiting.
	Queued int `json:"queued"`
	// Sent, Failed and Dropped count entries by outcome.
	Sent    uint64 `json:"sent"`
	Failed  uint64 `json:"failed"`
	Dropped uint64 `json:"dropped"`
	// LastError is the last delivery error (credentials and the URL path removed).
	LastError string `json:"last_error,omitempty"`
	// LastErrorAt is when it happened.
	LastErrorAt time.Time `json:"last_error_at,omitzero"`
}

// Forwarder sends audit entries to a webhook asynchronously: Enqueue never blocks,
// a full queue drops the entry and counts it, and a single worker (Run) delivers
// them one by one. Each request is a POST of the entry's JSON, signed when a secret
// is set. Delivery is best effort: failed entries are counted, not retried; the
// chain in the database stays authoritative. A nil *Forwarder forwards nothing.
type Forwarder struct {
	cfg   ForwarderConfig
	queue chan Event

	stopped               atomic.Bool
	sent, failed, dropped atomic.Uint64

	mu          sync.Mutex
	lastError   string
	lastErrorAt time.Time
	lastLogged  time.Time
}

// NewForwarder returns a Forwarder; start its worker with Run.
func NewForwarder(cfg ForwarderConfig) *Forwarder {
	if cfg.Client == nil {
		cfg.Client = notify.NewHTTPClient()
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = DefaultQueueSize
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Endpoint == nil {
		cfg.Endpoint = func() Endpoint { return Endpoint{} }
	}
	return &Forwarder{cfg: cfg, queue: make(chan Event, cfg.QueueSize)}
}

// Enqueue queues e when an endpoint is configured. It never blocks: when the queue
// is full or the worker stopped, e is dropped and counted.
func (f *Forwarder) Enqueue(e Event) {
	if f == nil || f.cfg.Endpoint().URL == "" {
		return
	}
	if f.stopped.Load() {
		f.drop()
		return
	}
	select {
	case f.queue <- e:
	default:
		f.drop()
	}
}

// drop counts one dropped entry.
func (f *Forwarder) drop() {
	f.dropped.Add(1)
	f.observe(ForwardDropped)
}

// observe reports an outcome.
func (f *Forwarder) observe(outcome string) {
	if f.cfg.Observe != nil {
		f.cfg.Observe(outcome)
	}
}

// Run delivers queued entries until ctx ends, then delivers what is still queued
// within a short drain timeout and counts the rest as dropped. A delivery in
// progress when ctx ends is finished (each request is bounded by the client's
// timeout).
func (f *Forwarder) Run(ctx context.Context) {
	if f == nil {
		return
	}
	sendCtx := context.WithoutCancel(ctx)
	for {
		select {
		case e := <-f.queue:
			f.deliver(sendCtx, e)
		case <-ctx.Done():
			f.stopped.Store(true)
			f.drain()
			return
		}
	}
}

// drain delivers the queued entries until the queue is empty or drainTimeout passed.
func (f *Forwarder) drain() {
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	for {
		select {
		case e := <-f.queue:
			if ctx.Err() != nil {
				f.drop()
				continue
			}
			f.deliver(ctx, e)
		default:
			return
		}
	}
}

// deliver sends e to the current endpoint.
func (f *Forwarder) deliver(ctx context.Context, e Event) {
	ep := f.cfg.Endpoint()
	if ep.URL == "" {
		return // forwarding was turned off while e waited
	}
	if err := f.send(ctx, ep, e); err != nil {
		f.failed.Add(1)
		f.observe(ForwardFailed)
		f.noteError(err)
		return
	}
	f.sent.Add(1)
	f.observe(ForwardSent)
}

// send POSTs e to ep.
func (f *Forwarder) send(ctx context.Context, ep Endpoint, e Event) error {
	body, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode entry: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid webhook URL")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "MongoRescue-Audit/1")
	if ep.Secret != "" {
		req.Header.Set(SignatureHeader, notify.Sign(ep.Secret, body))
	}
	resp, err := f.cfg.Client.Do(req)
	if err != nil {
		// url.Error repeats the URL, which may carry a token; keep the cause only.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return fmt.Errorf("request failed: %s", redact.Text(err.Error()))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("the endpoint answered %d", resp.StatusCode)
	}
	return nil
}

// noteError keeps err for the status and logs it at most once per
// failureLogInterval.
func (f *Forwarder) noteError(err error) {
	msg := err.Error()
	if len(msg) > maxErrorLength {
		msg = msg[:maxErrorLength]
	}
	now := time.Now().UTC()
	f.mu.Lock()
	f.lastError, f.lastErrorAt = msg, now
	logIt := now.Sub(f.lastLogged) >= failureLogInterval
	if logIt {
		f.lastLogged = now
	}
	f.mu.Unlock()
	if logIt {
		f.cfg.Logger.Warn("failed to forward audit log entries", slog.String("error", logsafe.String(msg)))
	}
}

// Status returns the forwarder's counters.
func (f *Forwarder) Status() ForwardStatus {
	if f == nil {
		return ForwardStatus{}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return ForwardStatus{
		Enabled: f.cfg.Endpoint().URL != "", Queued: len(f.queue),
		Sent: f.sent.Load(), Failed: f.failed.Load(), Dropped: f.dropped.Load(),
		LastError: f.lastError, LastErrorAt: f.lastErrorAt,
	}
}
