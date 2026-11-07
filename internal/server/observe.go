package server

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logging"
)

// RequestIDHeader carries the ID of a request: the one a trusted proxy sent or the
// one the server generated. The response always echoes it.
const RequestIDHeader = "X-Request-Id"

// Bounds of an accepted incoming request ID.
const (
	minRequestIDLen = 8
	maxRequestIDLen = 128
)

// metricsPath is the Prometheus endpoint; its scrapes are neither counted nor logged,
// for they would be most of the traffic of a quiet instance.
const metricsPath = "/metrics"

// WithAccessLog logs one line per HTTP request (method, route pattern, status,
// duration, bytes, client address, request ID), never the query string or a body.
func WithAccessLog(enabled bool) Option {
	return func(s *Server) { s.accessLog = enabled }
}

// WithRequestObserver calls fn for every finished HTTP request except the scrapes of
// /metrics, with the route pattern without its method ("" when no route matches),
// the method, the status code and the duration, for mongorescue_http_requests_total
// and mongorescue_http_request_duration_seconds.
func WithRequestObserver(fn func(route, method string, code int, d time.Duration)) Option {
	return func(s *Server) { s.requestObserver = fn }
}

// responseRequestID returns the request ID the middleware set on the response.
func responseRequestID(w http.ResponseWriter) string {
	return w.Header().Get(RequestIDHeader)
}

// requestIDAttr is the request_id attribute for log lines of helpers that have the
// response writer but not the request.
func requestIDAttr(w http.ResponseWriter) slog.Attr {
	return slog.String("request_id", responseRequestID(w))
}

// validRequestID reports whether id is a well-formed request ID: 8 to 128 characters
// of letters, digits and ._:~- (UUIDs, ULIDs, trace IDs), so it is safe in a log line
// and a header.
func validRequestID(id string) bool {
	if len(id) < minRequestIDLen || len(id) > maxRequestIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == ':', c == '~', c == '-':
		default:
			return false
		}
	}
	return true
}

// newRequestID returns a random request ID (128 bits, hex).
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand does not fail on supported platforms
	return hex.EncodeToString(b[:])
}

// requestID returns the ID of the request: a well-formed X-Request-Id of a trusted
// proxy, else a new one. A client that is not a trusted proxy cannot choose it.
func (s *Server) requestID(r *http.Request) string {
	if id := r.Header.Get(RequestIDHeader); id != "" && validRequestID(id) && s.trust().PeerTrusted(r) {
		return id
	}
	return newRequestID()
}

// routeLabel returns the registered ServeMux pattern of r without its method
// ("/api/v1/jobs/{id}"), or "" when no route matches (the API catch-all included),
// so the label has as many values as there are routes.
func (s *Server) routeLabel(r *http.Request) string {
	pattern := s.routePattern(r)
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		pattern = pattern[i+1:]
	}
	return pattern
}

// metricsRecorder remembers the status and the size of a response.
type metricsRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

// WriteHeader implements http.ResponseWriter.
func (w *metricsRecorder) WriteHeader(code int) {
	if w.status == 0 && code >= 200 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write implements http.ResponseWriter.
func (w *metricsRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Flush implements http.Flusher when the underlying writer does.
func (w *metricsRecorder) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *metricsRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// observeMiddleware is the outermost middleware: it assigns the request ID (echoed
// in X-Request-Id and carried by the request context, so every log line of the
// request has it), then counts the request and, when enabled, logs it.
func (s *Server) observeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := s.requestID(r)
		w.Header().Set(RequestIDHeader, id)
		r = r.WithContext(logging.WithRequestID(r.Context(), id))
		rec := &metricsRecorder{ResponseWriter: w}

		finished := false
		defer func() {
			if finished && rec.status != 0 {
				s.observeRequest(r, rec, start)
			} else if !finished {
				// The handler panicked (the server answers or drops the connection).
				rec.status = http.StatusInternalServerError
				s.observeRequest(r, rec, start)
			}
		}()
		next.ServeHTTP(rec, r)
		finished = true
		if rec.status == 0 {
			rec.status = http.StatusOK // a handler that wrote nothing answers 200
		}
	})
}

// observeRequest feeds the request metrics and the access log.
func (s *Server) observeRequest(r *http.Request, rec *metricsRecorder, start time.Time) {
	if r.Method == http.MethodGet && r.URL.Path == metricsPath {
		return
	}
	d := time.Since(start)
	route := s.routeLabel(r)
	if s.requestObserver != nil {
		s.requestObserver(route, r.Method, rec.status, d)
	}
	if s.accessLog {
		if route == "" {
			route = "unmatched"
		}
		s.logger.LogAttrs(r.Context(), slog.LevelInfo, "http request",
			slog.String("method", boundedMethod(r.Method)),
			slog.String("route", route),
			slog.Int("status", rec.status),
			slog.Float64("duration_ms", float64(d.Microseconds())/1000),
			slog.Int64("bytes", rec.bytes),
			slog.String("client_ip", s.clientIP(r)),
		)
	}
}

// boundedMethod returns method if it is a standard HTTP method, else "OTHER", so a
// client cannot put arbitrary text in the access log.
func boundedMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return method
	}
	return "OTHER"
}
