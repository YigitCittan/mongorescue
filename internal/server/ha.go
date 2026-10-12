package server

import (
	"io/fs"
	"net/http"
	"strconv"
	"strings"
)

// HA roles reported by HAStatus.Role.
const (
	// HARoleActive is the role of the instance holding the HA lease.
	HARoleActive = "active"
	// HARoleStandby is the role of an instance waiting for the lease.
	HARoleStandby = "standby"
)

// StandbyPage is the static file a standby serves instead of the dashboard.
const StandbyPage = "standby.html"

// HAStatus describes the instance's place in an active-passive HA cluster, as
// reported by GET /api/v1/health.
type HAStatus struct {
	// Role is HARoleActive or HARoleStandby.
	Role string `json:"role"`
	// Cluster is the cluster name.
	Cluster string `json:"cluster"`
	// Epoch is the lease epoch held (active) or last seen (standby).
	Epoch uint64 `json:"epoch"`
	// Holder labels the instance holding the lease as last seen ("" when unknown
	// or released).
	Holder string `json:"holder,omitempty"`
	// LeaseHealthy reports that the last read or write of the lease succeeded.
	LeaseHealthy bool `json:"lease_healthy"`
	// RetryAfterSeconds is how long a client refused by a standby should wait.
	RetryAfterSeconds int `json:"-"`
}

// WithHA makes the server part of an active-passive HA cluster: GET
// /api/v1/health reports the role from fn, and while the role is standby the server
// serves no data and no sign-in (see standbyGate). standbyMetrics, when not nil,
// answers GET /metrics on a standby, without credentials.
func WithHA(fn func() HAStatus, standbyMetrics http.Handler) Option {
	return func(s *Server) {
		s.haStatus, s.standbyMetrics = fn, standbyMetrics
	}
}

// standbyGate makes a standby serve only what needs no credentials and shows no
// data: the health check, the standby metrics (process, build and HA series), the
// static standby page instead of the dashboard, and the dashboard's static files.
// Every other request (the API, MCP, signing in, single sign-on) answers 503 with
// Retry-After. A standby's database is only as current as its last takeover, so
// neither its data nor its users, sessions and API keys are trusted.
func (s *Server) standbyGate(next http.Handler) http.Handler {
	if s.haStatus == nil {
		return next
	}
	var static http.Handler
	if s.staticFS != nil {
		static = http.FileServer(http.FS(s.staticFS))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := s.haStatus()
		if st.Role != HARoleStandby {
			next.ServeHTTP(w, r)
			return
		}
		read := r.Method == http.MethodGet || r.Method == http.MethodHead
		path := r.URL.Path
		switch {
		case read && path == "/api/v1/health":
			next.ServeHTTP(w, r)
			return
		case read && path == "/metrics" && s.standbyMetrics != nil:
			s.standbyMetrics.ServeHTTP(w, r)
			return
		case read && static != nil && (path == "/" || path == "/index.html"):
			s.serveStandbyPage(w, r)
			return
		case read && static != nil && !standbyClosedPath(path):
			static.ServeHTTP(w, r)
			return
		}
		retry := st.RetryAfterSeconds
		if retry <= 0 {
			retry = 10
		}
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		writeErrorData(w, http.StatusServiceUnavailable,
			"this instance is the standby of HA cluster "+st.Cluster+"; it serves no data and accepts no sign-ins: use the active instance",
			map[string]any{"role": st.Role, "cluster": st.Cluster})
	})
}

// standbyClosedPath reports the paths a standby never serves as static files: the
// API, MCP, the metrics and single sign-on.
func standbyClosedPath(path string) bool {
	for _, p := range []string{"/api/", "/mcp", "/metrics", "/auth/"} {
		if path == strings.TrimSuffix(p, "/") || strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// serveStandbyPage serves the static standby page, never cached.
func (s *Server) serveStandbyPage(w http.ResponseWriter, r *http.Request) {
	page, err := fs.ReadFile(s.staticFS, StandbyPage)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "this instance is an HA standby")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(page)
	}
}

// healthHA adds the HA role to a health response body.
func (s *Server) healthHA(body map[string]any) {
	if s.haStatus == nil {
		return
	}
	st := s.haStatus()
	body["role"] = st.Role
	body["ha"] = st
}
