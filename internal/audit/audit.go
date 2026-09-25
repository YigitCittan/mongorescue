// Package audit records what automated clients did: every MCP tool call, resource
// read and prompt request, and every REST request authenticated by an API key, is
// stored with the calling API key, the transport, the tool (or route), its arguments
// (secrets redacted), the outcome and the duration. The log is shown in the dashboard and
// served by GET /api/v1/audit.
//
// The package is the domain core; persistence is the Repository port implemented by
// internal/store. Recording never fails the audited operation: write errors are
// logged, not returned.
//
// Refused calls cannot flush the log: repeated denied or rate-limited calls of one
// API key (and entries marked Coalesce) are merged into a single entry per
// CoalesceWindow whose Count says how many calls it stands for.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	// TransportREST is the REST API (/api/v1/...) called with an API key.
	TransportREST = "rest"
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
	// CoalesceWindow is how long identical coalesced entries are merged into one.
	CoalesceWindow = 10 * time.Second
	// maxCoalesceKeys bounds the in-memory index of recently coalesced entries.
	maxCoalesceKeys = 4096
	// MaxDistinctValues is how many distinct values of one argument a coalesced entry
	// keeps (for example the IDs a polling client read).
	MaxDistinctValues = 20
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
	// Tool is the MCP tool name, the MCP method (resources/read, prompts/get) or, for
	// REST requests, the route pattern ("GET /api/v1/backups").
	Tool string `json:"tool"`
	// Arguments are the call arguments as JSON, secrets redacted.
	Arguments json.RawMessage `json:"arguments"`
	// Result is ResultOK, ResultError, ResultDenied or ResultRateLimited.
	Result string `json:"result"`
	// Error is the (redacted) error shown to the client, if any.
	Error string `json:"error,omitempty"`
	// DurationMS is how long the call took in milliseconds.
	DurationMS int64 `json:"duration_ms"`
	// HTTPStatus is the response status of a REST request (0 for MCP calls).
	HTTPStatus int `json:"http_status,omitempty"`
	// Count is the number of calls the entry stands for: 1, or more for an entry
	// that coalesced repeated calls (see CoalesceWindow).
	Count int `json:"count"`
	// Coalesce asks Record to merge the entry into an identical one (same API key,
	// transport, tool and result) recorded less than CoalesceWindow earlier, instead
	// of storing a new one. Denied and rate-limited entries are always coalesced. The
	// merged entry counts the calls and keeps the distinct values of each top-level
	// string argument (up to MaxDistinctValues, as an array once there are several;
	// "_more_values" is true when some were dropped).
	Coalesce bool `json:"-"`
}

// Repository is the persistence port for audit entries.
type Repository interface {
	// AppendAudit stores e, assigns e.ID and prunes all but the newest keep entries.
	AppendAudit(ctx context.Context, e *Entry, keep int) error
	// ListAudit returns up to limit entries, newest first.
	ListAudit(ctx context.Context, limit int) ([]*Entry, error)
	// MergeAudit adds n to the Count of entry id and, when arguments is not nil,
	// replaces its arguments. It returns an error wrapping ErrEntryNotFound when the
	// entry no longer exists (it was pruned).
	MergeAudit(ctx context.Context, id int64, n int, arguments json.RawMessage) error
}

// Errors.
var (
	// ErrNoRepository is returned by List when the service has no repository.
	ErrNoRepository = errors.New("audit: no repository configured")
	// ErrEntryNotFound is returned by Repository.MergeAudit for a missing entry.
	ErrEntryNotFound = errors.New("audit: entry not found")
)

// Service records and lists audit entries. It is safe for concurrent use; a nil
// *Service records nothing.
type Service struct {
	repo   Repository
	logger *slog.Logger
	now    func() time.Time

	// mu guards recent and serialises coalesced writes.
	mu sync.Mutex
	// recent indexes the latest stored entry of every coalescing key.
	recent map[string]*recentEntry
}

// recentEntry is a stored entry later identical entries may be merged into.
type recentEntry struct {
	id    int64
	since time.Time
	// args are the stored entry's decoded arguments (nil when not a JSON object).
	args map[string]any
	// values are the distinct values seen per top-level string argument.
	values map[string][]string
	// more reports that distinct values were dropped (MaxDistinctValues).
	more bool
}

// newRecentEntry indexes the stored entry e.
func newRecentEntry(e *Entry) *recentEntry {
	r := &recentEntry{id: e.ID, since: e.Time, values: map[string][]string{}}
	if json.Unmarshal(e.Arguments, &r.args) != nil {
		r.args = nil
	}
	for k, v := range r.args {
		if str, ok := v.(string); ok {
			r.values[k] = []string{str}
		}
	}
	return r
}

// merge adds the top-level string arguments of raw to the distinct values and
// returns the new arguments document, or nil when nothing changed.
func (r *recentEntry) merge(raw json.RawMessage) json.RawMessage {
	var args map[string]any
	if r.args == nil || json.Unmarshal(raw, &args) != nil {
		return nil
	}
	changed := false
	for k, v := range args {
		str, ok := v.(string)
		seen, tracked := r.values[k]
		if !ok || !tracked || slices.Contains(seen, str) {
			continue
		}
		if len(seen) >= MaxDistinctValues {
			changed = changed || !r.more
			r.more = true
			continue
		}
		r.values[k] = append(seen, str)
		changed = true
	}
	if !changed {
		return nil
	}
	out := make(map[string]any, len(r.args)+1)
	for k, v := range r.args {
		out[k] = v
		if vals := r.values[k]; len(vals) > 1 {
			out[k] = vals
		}
	}
	if r.more {
		out["_more_values"] = true
	}
	doc, err := json.Marshal(out)
	if err != nil || len(doc) > maxArgumentsBytes {
		return nil
	}
	return doc
}

// NewService returns a Service backed by repo. A nil logger means slog.Default().
func NewService(repo Repository, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{repo: repo, logger: logger, now: time.Now, recent: map[string]*recentEntry{}}
}

// Record stores e after redacting its arguments and error. Denied, rate-limited and
// Coalesce entries are merged into an identical entry stored less than CoalesceWindow
// before (by e.Time), incrementing its Count, so that refused calls cannot flush the
// log. Record never fails the caller: the write is detached from ctx's cancellation
// (so an aborted request is still audited), bounded by a timeout, and errors are
// logged.
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
	if e.Count < 1 {
		e.Count = 1
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	if e.Coalesce || e.Result == ResultDenied || e.Result == ResultRateLimited {
		s.recordCoalesced(writeCtx, &e)
		return
	}
	s.append(writeCtx, &e)
}

// append stores e, logging failures. It reports whether e was stored.
func (s *Service) append(ctx context.Context, e *Entry) bool {
	if err := s.repo.AppendAudit(ctx, e, MaxEntries); err != nil {
		// The entry's own fields stay out of the log: it is the audit log's job.
		s.logger.Error("failed to write audit entry", slog.Any("error", err))
		return false
	}
	return true
}

// recordCoalesced merges e into the entry recently stored under the same key, or
// stores it and remembers it for the next CoalesceWindow.
func (s *Service) recordCoalesced(ctx context.Context, e *Entry) {
	key := coalesceKey(e)
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.recent[key]; ok && !e.Time.Before(r.since) && e.Time.Sub(r.since) < CoalesceWindow {
		err := s.repo.MergeAudit(ctx, r.id, e.Count, r.merge(e.Arguments))
		if err == nil {
			return
		}
		if !errors.Is(err, ErrEntryNotFound) {
			s.logger.Error("failed to update audit entry", slog.Int64("entry_id", r.id), slog.Any("error", err))
			return
		}
	}
	if !s.append(ctx, e) {
		return
	}
	if len(s.recent) >= maxCoalesceKeys {
		for k, r := range s.recent {
			if e.Time.Sub(r.since) >= CoalesceWindow {
				delete(s.recent, k)
			}
		}
		if len(s.recent) >= maxCoalesceKeys {
			clear(s.recent)
		}
	}
	s.recent[key] = newRecentEntry(e)
}

// coalesceKey identifies the entries e may be merged with (for REST requests also
// by response status). Rate limits apply per API
// key, so rate-limited calls are merged whatever the tool; the tool (a client-chosen
// name for unknown tools) then cannot multiply the entries.
func coalesceKey(e *Entry) string {
	tool := e.Tool
	if e.Result == ResultRateLimited {
		tool = ""
	}
	return strings.Join([]string{e.APIKeyID, e.Transport, tool, e.Result, strconv.Itoa(e.HTTPStatus)}, "\x00")
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
