// Package logging builds MongoRescue's slog loggers: the text or JSON output format,
// a defence-in-depth redaction of every string that reaches a log line, and the
// correlation attributes (request_id, run_id, job_id) that a context carries.
//
// Redaction and correlation are properties of the handler, not of the output format,
// so the text and the JSON logger behave the same: a connection string with a password
// never appears in either, and a log call made with a context (InfoContext and its
// siblings) carries the identifiers of the request or run that context belongs to.
package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/redact"
)

// Format is the output format of the logs.
type Format string

// The supported formats.
const (
	// FormatText is slog's key=value text format (the default).
	FormatText Format = "text"
	// FormatJSON is one JSON object per line, for Loki, ELK and CloudWatch.
	FormatJSON Format = "json"
)

// EnvLogFormat is the environment variable that selects the format of the server and
// of the MCP bridge (the -log-format flag takes precedence).
const EnvLogFormat = "MONGORESCUE_LOG_FORMAT"

// ErrInvalidFormat is returned for a log format other than text or json.
var ErrInvalidFormat = errors.New("logging: log format must be text or json")

// ParseFormat parses text or json (case-insensitive); an empty string is text.
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", string(FormatText):
		return FormatText, nil
	case string(FormatJSON):
		return FormatJSON, nil
	}
	return FormatText, fmt.Errorf("%w (got %q)", ErrInvalidFormat, truncate(s))
}

func truncate(s string) string {
	if len(s) > 32 {
		return s[:32] + "..."
	}
	return s
}

// New returns a logger that writes to w in format at level. Every string attribute,
// error and message goes through redact.Text on its way out, and the request and run
// identifiers of the context of a log call are added (see WithRequestID and WithRun).
func New(w io.Writer, format Format, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level, ReplaceAttr: redactAttr}
	var h slog.Handler
	if format == FormatJSON {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(NewContextHandler(h))
}

// redactAttr masks the credentials in string, error and fmt.Stringer attributes and
// in the message. It runs in both formats, after the call sites' own redaction.
func redactAttr(_ []string, a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		if s := a.Value.String(); strings.Contains(s, "://") {
			a.Value = slog.StringValue(redact.Text(s))
		}
	case slog.KindAny:
		switch v := a.Value.Any().(type) {
		case error:
			// Wrapped errors may quote a URI; log the masked text.
			if s := safeError(v); strings.Contains(s, "://") {
				a.Value = slog.StringValue(redact.Text(s))
			}
		case fmt.Stringer:
			if s := safeString(v); strings.Contains(s, "://") {
				a.Value = slog.StringValue(redact.Text(s))
			}
		}
	}
	return a
}

// safeString calls String on v, returning "" if it panics (a nil pointer receiver).
func safeString(v fmt.Stringer) (s string) {
	defer func() {
		if recover() != nil {
			s = ""
		}
	}()
	return v.String()
}

// safeError calls Error on err, returning "" if it panics.
func safeError(err error) (s string) {
	defer func() {
		if recover() != nil {
			s = ""
		}
	}()
	return err.Error()
}

// contextKey is the type of the context keys of this package.
type contextKey int

const (
	requestIDKey contextKey = iota
	runKey
)

// runIDs are the identifiers of the run a context belongs to.
type runIDs struct{ runID, jobID, recordKey, recordID string }

// WithRequestID returns a context that carries the ID of an HTTP request.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the request ID of ctx, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// WithRun returns a context whose log lines carry run_id and job_id (an empty value
// is left out). Components that receive the context inherit them.
func WithRun(ctx context.Context, runID, jobID string) context.Context {
	ids, _ := ctx.Value(runKey).(runIDs)
	ids.runID, ids.jobID = runID, jobID
	return context.WithValue(ctx, runKey, ids)
}

// WithRecord returns a context whose log lines also carry key=id, the record of the
// run (backup_id or restore_id): the ID of its run log file.
func WithRecord(ctx context.Context, key, id string) context.Context {
	ids, _ := ctx.Value(runKey).(runIDs)
	ids.recordKey, ids.recordID = key, id
	return context.WithValue(ctx, runKey, ids)
}

// Record returns the record attribute of ctx (see WithRecord), or "".
func Record(ctx context.Context) (key, id string) {
	ids, _ := ctx.Value(runKey).(runIDs)
	return ids.recordKey, ids.recordID
}

// Run returns the run and job IDs of ctx, or "".
func Run(ctx context.Context) (runID, jobID string) {
	ids, _ := ctx.Value(runKey).(runIDs)
	return ids.runID, ids.jobID
}

// ContextHandler adds the correlation attributes of the context of a log call to its
// record, unless the record already has an attribute of that name.
type ContextHandler struct {
	inner slog.Handler
}

// NewContextHandler wraps inner.
func NewContextHandler(inner slog.Handler) *ContextHandler {
	return &ContextHandler{inner: inner}
}

// Enabled implements slog.Handler.
func (h *ContextHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

// Handle implements slog.Handler.
func (h *ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if ctx != nil {
		add := func(key, v string) {
			if v == "" || hasAttr(r, key) {
				return
			}
			r.AddAttrs(slog.String(key, v))
		}
		add("request_id", RequestID(ctx))
		runID, jobID := Run(ctx)
		add("run_id", runID)
		add("job_id", jobID)
		if key, id := Record(ctx); key != "" {
			add(key, id)
		}
	}
	return h.inner.Handle(ctx, r)
}

func hasAttr(r slog.Record, key string) bool {
	found := false
	r.Attrs(func(a slog.Attr) bool {
		found = a.Key == key
		return !found
	})
	return found
}

// WithAttrs implements slog.Handler.
func (h *ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ContextHandler{inner: h.inner.WithAttrs(attrs)}
}

// WithGroup implements slog.Handler.
func (h *ContextHandler) WithGroup(name string) slog.Handler {
	return &ContextHandler{inner: h.inner.WithGroup(name)}
}
