// Package audit records what automated clients did: every MCP tool call is stored
// with the calling API key, the transport, the tool, its arguments (secrets
// redacted), the outcome and the duration. The log is shown in the dashboard and
// served by GET /api/v1/audit.
//
// The package is the domain core; persistence is the Repository port implemented by
// internal/store. Recording never fails the audited operation: write errors are
// logged, not returned.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yigitcittan/mongorescue/internal/redact"
)

// Results of an audited call.
const (
	// ResultOK is a call that succeeded.
	ResultOK = "ok"
	// ResultError is a call that ran and failed (invalid input, a busy database, ...).
	ResultError = "error"
	// ResultDenied is a call refused because the API key's scope is too small.
	ResultDenied = "denied"
	// ResultRateLimited is a call refused by the per-key rate limit.
	ResultRateLimited = "rate_limited"
)

// Transports.
const (
	// TransportHTTP is the Streamable HTTP endpoint used directly.
	TransportHTTP = "http"
	// TransportStdio is the stdio bridge (mongorescue mcp), which forwards to the
	// HTTP endpoint and identifies itself.
	TransportStdio = "stdio"
)

// Limits.
const (
	// DefaultListLimit is the number of entries List returns by default.
	DefaultListLimit = 200
	// MaxListLimit caps List.
	MaxListLimit = 1000
	// MaxEntries is the number of entries kept; older ones are pruned on insert.
	MaxEntries = 10000
	// maxArgumentsBytes caps the stored arguments document.
	maxArgumentsBytes = 4 << 10
	// maxErrorLength caps the stored error message.
	maxErrorLength = 500
	// writeTimeout bounds one audit write.
	writeTimeout = 5 * time.Second
)

// Entry is one audited call.
type Entry struct {
	// ID orders entries (assigned by the repository).
	ID int64 `json:"id"`
	// Time is when the call started.
	Time time.Time `json:"time"`
	// APIKeyID identifies the calling API key.
	APIKeyID string `json:"api_key_id"`
	// APIKeyName is a snapshot of the key's name.
	APIKeyName string `json:"api_key_name"`
	// Transport is TransportHTTP or TransportStdio.
	Transport string `json:"transport"`
	// Tool is the MCP tool name.
	Tool string `json:"tool"`
	// Arguments are the call arguments as JSON, secrets redacted.
	Arguments json.RawMessage `json:"arguments"`
	// Result is ResultOK, ResultError, ResultDenied or ResultRateLimited.
	Result string `json:"result"`
	// Error is the (redacted) error shown to the client, if any.
	Error string `json:"error,omitempty"`
	// DurationMS is how long the call took in milliseconds.
	DurationMS int64 `json:"duration_ms"`
}

// Repository is the persistence port for audit entries.
type Repository interface {
	// AppendAudit stores e, assigns e.ID and prunes all but the newest keep entries.
	AppendAudit(ctx context.Context, e *Entry, keep int) error
	// ListAudit returns up to limit entries, newest first.
	ListAudit(ctx context.Context, limit int) ([]*Entry, error)
}

// ErrNoRepository is returned by List when the service has no repository.
var ErrNoRepository = errors.New("audit: no repository configured")

// Service records and lists audit entries. It is safe for concurrent use; a nil
// *Service records nothing.
type Service struct {
	repo   Repository
	logger *slog.Logger
	now    func() time.Time
}

// NewService returns a Service backed by repo. A nil logger means slog.Default().
func NewService(repo Repository, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{repo: repo, logger: logger, now: time.Now}
}

// Record stores e after redacting its arguments and error. It never fails the caller:
// the write is detached from ctx's cancellation (so an aborted request is still
// audited), bounded by a timeout, and errors are logged.
func (s *Service) Record(ctx context.Context, e Entry) {
	if s == nil || s.repo == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = s.now()
	}
	e.Time = e.Time.UTC()
	e.Arguments = RedactArguments(e.Arguments)
	e.Error = truncate(redact.Text(e.Error), maxErrorLength)
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	if err := s.repo.AppendAudit(writeCtx, &e, MaxEntries); err != nil {
		s.logger.Error("failed to write audit entry", slog.String("tool", e.Tool), slog.String("api_key_id", e.APIKeyID), slog.Any("error", err))
	}
}

// List returns up to limit entries, newest first (DefaultListLimit for limit <= 0,
// at most MaxListLimit).
func (s *Service) List(ctx context.Context, limit int) ([]*Entry, error) {
	if s == nil || s.repo == nil {
		return nil, ErrNoRepository
	}
	switch {
	case limit <= 0:
		limit = DefaultListLimit
	case limit > MaxListLimit:
		limit = MaxListLimit
	}
	return s.repo.ListAudit(ctx, limit)
}

// sensitiveKeys are argument names whose values are always masked.
var sensitiveKeys = []string{"password", "passwd", "secret", "token", "key", "uri", "url", "identity", "passphrase", "credential", "authorization"}

// isSensitive reports whether an argument name suggests a secret value.
func isSensitive(name string) bool {
	n := strings.ToLower(name)
	for _, k := range sensitiveKeys {
		if strings.Contains(n, k) {
			return true
		}
	}
	return false
}

// RedactArguments returns a JSON document safe to store: values of secret-looking
// keys are replaced by redact.Mask, credentials inside strings are masked, and a
// document larger than 4 KiB is replaced by a truncation marker. Invalid or empty
// input becomes {}.
func RedactArguments(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return json.RawMessage(`{"_invalid":true}`)
	}
	out, err := json.Marshal(redactValue(v))
	if err != nil {
		return json.RawMessage(`{"_invalid":true}`)
	}
	if len(out) > maxArgumentsBytes {
		return json.RawMessage(`{"_truncated":true}`)
	}
	return out
}

// redactValue walks a decoded JSON value, masking secrets.
func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if isSensitive(k) {
				t[k] = redact.Mask
				continue
			}
			t[k] = redactValue(val)
		}
		return t
	case []any:
		for i := range t {
			t[i] = redactValue(t[i])
		}
		return t
	case string:
		return redact.Text(t)
	default:
		return v
	}
}

// truncate shortens s to at most n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
