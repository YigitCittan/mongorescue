// Package desktop adapts the MongoRescue HTTP handler for the desktop app, which
// serves it in-process to a native webview (Wails) instead of over a TCP listener.
//
// Webviews treat the custom scheme of the embedded page (wails://wails on macOS and
// Linux, http://wails.localhost on Windows) inconsistently: WKWebView does not reliably
// store cookies for custom schemes, and the scheme is not http(s), so the server's
// same-origin check for the unauthenticated POST endpoints rejects it. Handler works
// around both without changing the server. It must only ever wrap a handler that is
// reachable from the app's own webview, never one mounted on a network listener.
package desktop

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/server"
)

// Handler wraps next for the desktop webview: the session cookie is kept in a
// SessionJar for same-origin requests, and an Origin header naming the request's own
// host (whatever its scheme) is removed, since such requests come from the app's own
// page.
func Handler(next http.Handler) http.Handler {
	return NewSessionJar(server.SessionCookieName).Wrap(StripSameOrigin(next))
}

// SameOrigin reports whether r comes from the page of its own host: Sec-Fetch-Site,
// when present, is "same-origin" or "none" (user-initiated), and Origin, when
// present, names the request host (whatever its scheme).
func SameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	origin := r.Header.Get("Origin")
	return origin == "" || originMatchesHost(origin, r.Host)
}

// originMatchesHost reports whether the Origin header value names host.
func originMatchesHost(origin, host string) bool {
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, host)
}

// StripSameOrigin removes the Origin header of requests whose Origin host equals the
// request host, so the webview's custom scheme passes the server's same-origin check.
// Cross-origin requests keep their Origin header and are judged by the server.
func StripSameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && originMatchesHost(origin, r.Host) {
			r = r.Clone(r.Context())
			r.Header.Del("Origin")
		}
		next.ServeHTTP(w, r)
	})
}

// SessionJar stores one cookie on behalf of the webview. For same-origin requests
// (see SameOrigin) it captures the cookie from the Set-Cookie headers of responses
// (clearing it on MaxAge < 0, an empty value or a past expiry) and adds it to requests
// that do not carry it. Cross-site requests are passed through untouched: they never
// receive the cookie and cannot replace it. It is safe for concurrent use.
type SessionJar struct {
	name string
	now  func() time.Time

	mu      sync.Mutex
	value   string
	expires time.Time // zero: no expiry (a session cookie)
}

// NewSessionJar returns an empty jar for the cookie called name.
func NewSessionJar(name string) *SessionJar {
	return &SessionJar{name: name, now: time.Now}
}

// Value returns the stored cookie value, or "" when the jar is empty or the cookie
// has expired.
func (j *SessionJar) Value() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.value != "" && !j.expires.IsZero() && !j.now().Before(j.expires) {
		j.value, j.expires = "", time.Time{}
	}
	return j.value
}

// Wrap returns a handler that injects the stored cookie into same-origin requests
// lacking it and records the cookie set or cleared by next for them.
func (j *SessionJar) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !SameOrigin(r) {
			next.ServeHTTP(w, r)
			return
		}
		if v := j.Value(); v != "" {
			if _, err := r.Cookie(j.name); err != nil {
				r = r.Clone(r.Context())
				r.AddCookie(&http.Cookie{Name: j.name, Value: v}) //nolint:gosec // G124: a request cookie; the attributes apply to Set-Cookie only.
			}
		}
		cw := &captureWriter{ResponseWriter: w, jar: j}
		next.ServeHTTP(cw, r)
		cw.capture() // the handler may not have written anything
	})
}

// record updates the jar from the Set-Cookie header values of a response.
func (j *SessionJar) record(setCookies []string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.now()
	for _, line := range setCookies {
		c, err := http.ParseSetCookie(line)
		if err != nil || c.Name != j.name {
			continue
		}
		switch {
		case c.MaxAge < 0, c.Value == "", !c.Expires.IsZero() && !c.Expires.After(now):
			j.value, j.expires = "", time.Time{}
		case c.MaxAge > 0:
			j.value, j.expires = c.Value, now.Add(time.Duration(c.MaxAge)*time.Second)
		default:
			j.value, j.expires = c.Value, c.Expires
		}
	}
}

// captureWriter records the jar's cookie from the response headers just before they
// are written.
type captureWriter struct {
	http.ResponseWriter
	jar      *SessionJar
	captured bool
}

// capture records the Set-Cookie headers once.
func (w *captureWriter) capture() {
	if w.captured {
		return
	}
	w.captured = true
	w.jar.record(w.Header().Values("Set-Cookie"))
}

// WriteHeader implements http.ResponseWriter.
func (w *captureWriter) WriteHeader(code int) {
	w.capture()
	w.ResponseWriter.WriteHeader(code)
}

// Write implements http.ResponseWriter.
func (w *captureWriter) Write(b []byte) (int, error) {
	w.capture()
	return w.ResponseWriter.Write(b)
}

// Flush implements http.Flusher when the underlying writer does.
func (w *captureWriter) Flush() {
	w.capture()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the underlying writer for http.ResponseController.
func (w *captureWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
