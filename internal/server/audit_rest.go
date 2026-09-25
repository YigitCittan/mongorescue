package server

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/auth"
)

// maxAuditPathValue bounds a path parameter stored in an audit entry.
const maxAuditPathValue = 256

// wildcardPattern matches the wildcards of a ServeMux pattern ({id}, {path...}).
var wildcardPattern = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)(?:\.\.\.)?\}`)

// statusRecorder remembers the status code a handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

// WriteHeader implements http.ResponseWriter.
func (w *statusRecorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write implements http.ResponseWriter.
func (w *statusRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// auditsREST reports whether a request by p to path is audited as a REST call: API
// key requests to the REST API (MCP audits its own calls; /metrics scrapes are not
// API activity).
func auditsREST(p *auth.Principal, path string) bool {
	return p != nil && p.Method == auth.MethodAPIKey && strings.HasPrefix(path, "/api/")
}

// auditREST records a REST request authenticated by an API key: the route pattern
// (never the raw path), its path parameters, the response status and the principal.
// Reads and failed or refused requests are coalesced (see audit.Entry.Coalesce), so a
// polling or misbehaving key cannot flush the log.
func (s *Server) auditREST(r *http.Request, p *auth.Principal, pattern string, rec *statusRecorder, start time.Time) {
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	result := audit.ResultOK
	switch {
	case status == http.StatusForbidden:
		result = audit.ResultDenied
	case status == http.StatusTooManyRequests:
		result = audit.ResultRateLimited
	case status >= http.StatusBadRequest:
		result = audit.ResultError
	}
	entry := audit.Entry{
		Time: start, APIKeyID: p.APIKeyID, APIKeyName: p.APIKeyName, Transport: audit.TransportREST,
		Tool: routeLabel(r.Method, pattern), Arguments: pathArguments(r, pattern), Result: result,
		HTTPStatus: status, DurationMS: time.Since(start).Milliseconds(),
		Coalesce: r.Method == http.MethodGet || r.Method == http.MethodHead || result != audit.ResultOK,
	}
	if status >= http.StatusBadRequest {
		entry.Error = http.StatusText(status)
	}
	s.audit.Record(r.Context(), entry)
}

// routeLabel names the route of a request for the audit log: its ServeMux pattern,
// or the method and "(no route)" for requests no route matched. Methods outside the
// standard set are labelled OTHER so clients cannot mint labels.
func routeLabel(method, pattern string) string {
	if pattern != "" {
		if !strings.Contains(pattern, " ") {
			return method + " " + pattern
		}
		return pattern
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions:
	default:
		method = "OTHER"
	}
	return method + " (no route)"
}

// pathArguments returns the path parameters of the matched route as a JSON object.
func pathArguments(r *http.Request, pattern string) json.RawMessage {
	args := map[string]string{}
	for _, m := range wildcardPattern.FindAllStringSubmatch(pattern, -1) {
		if v := r.PathValue(m[1]); v != "" {
			args[m[1]] = truncateUTF8(v, maxAuditPathValue)
		}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return json.RawMessage("{}")
	}
	return raw
}

// truncateUTF8 cuts s to at most n bytes on a rune boundary, as valid UTF-8.
func truncateUTF8(s string, n int) string {
	if len(s) > n {
		cut := n
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return strings.ToValidUTF8(s, "?")
}
