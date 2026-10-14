package server

import (
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

// HA roles reported by HAStatus.Role.
const (
	// HARoleActive is the role of the instance holding the HA lease.
	HARoleActive = "active"
	// HARoleStandby is the role of an instance waiting for the lease.
	HARoleStandby = "standby"
	// HARoleSwitching is the role an instance reports while it changes role: the
	// App of the old role is closed and the App of the new one not yet serving.
	HARoleSwitching = "switching"
)

// StandbyPage is the static file a standby serves instead of the dashboard.
const StandbyPage = "standby.html"

// HAStatus describes the instance's place in an active-passive HA cluster, as
// reported by GET /api/v1/health.
type HAStatus struct {
	// Role is HARoleActive, HARoleStandby or, between the Apps of two roles,
	// HARoleSwitching.
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

// WithFence makes every request that may change something (any method but GET,
// HEAD and OPTIONS: the API, sign-ins, MCP) check fence before it is served: the
// HA fence of the runs, which fails once this instance no longer holds the lease.
// Such a request answers 503 with Retry-After and does nothing. The operations
// service checks the fence again before it commits (operations.Config.Fence), and
// the HA supervisor cancels the requests in progress when the lease is lost.
func WithFence(fence models.Fence) Option {
	return func(s *Server) { s.fence = fence }
}

// fenceGate refuses the requests that may change something once the HA fence
// fails (see WithFence).
func (s *Server) fenceGate(next http.Handler) http.Handler {
	if s.fence == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if s.fence.Check() != nil {
			s.writeFenced(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeFenced answers a request refused because this instance no longer holds the
// HA lease: 503 with Retry-After.
func (s *Server) writeFenced(w http.ResponseWriter) {
	retry := 10
	if s.haStatus != nil {
		if r := s.haStatus().RetryAfterSeconds; r > 0 {
			retry = r
		}
	}
	w.Header().Set("Retry-After", strconv.Itoa(retry))
	writeError(w, http.StatusServiceUnavailable, operations.ErrNotActive.Error())
}

// SwitchingHandler answers the requests an active-passive HA member receives while
// it changes role, when no App serves: GET /api/v1/health reports the status of fn
// with the role HARoleSwitching (200), GET /metrics is served by metrics when not
// nil, and every other request answers 503 with Retry-After: 1, so clients and
// load balancers retry instead of seeing a refused connection.
func SwitchingHandler(fn func() HAStatus, version string, metrics http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		read := r.Method == http.MethodGet || r.Method == http.MethodHead
		st := fn()
		st.Role = HARoleSwitching
		switch {
		case read && r.URL.Path == "/api/v1/health":
			writeJSON(w, http.StatusOK, map[string]any{"status": "healthy", "version": version,
				"time": time.Now().UTC(), "role": st.Role, "ha": st})
			return
		case read && r.URL.Path == "/metrics" && metrics != nil:
			metrics.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Retry-After", "1")
		writeErrorData(w, http.StatusServiceUnavailable,
			"this instance of HA cluster "+st.Cluster+" is changing role; retry in a second",
			map[string]any{"role": st.Role, "cluster": st.Cluster})
	})
}
