package server

import (
	"net/http"
	"strconv"
)

// HA roles reported by HAStatus.Role.
const (
	// HARoleActive is the role of the instance holding the HA lease.
	HARoleActive = "active"
	// HARoleStandby is the role of an instance waiting for the lease.
	HARoleStandby = "standby"
)

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
// /api/v1/health reports the role from fn, and while the role is standby every
// request that could change something answers 503 (see standbyGate).
func WithHA(fn func() HAStatus) Option {
	return func(s *Server) { s.haStatus = fn }
}

// standbyWritable lists the routes a standby still serves for unsafe methods:
// signing in and out, so the read-only dashboard can be used.
var standbyWritable = map[string]bool{
	"/api/v1/auth/login":  true,
	"/api/v1/auth/logout": true,
}

// standbyGate refuses, on a standby, every request with an unsafe method (and every
// MCP request) with 503, Retry-After and an error naming the role, except signing
// in and out. Reads pass.
func (s *Server) standbyGate(next http.Handler) http.Handler {
	if s.haStatus == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := s.haStatus()
		if st.Role != HARoleStandby {
			next.ServeHTTP(w, r)
			return
		}
		safe := r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions
		mcp := r.URL.Path == "/mcp" || len(r.URL.Path) > 5 && r.URL.Path[:5] == "/mcp/"
		if (safe && !mcp) || standbyWritable[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		retry := st.RetryAfterSeconds
		if retry <= 0 {
			retry = 10
		}
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		writeErrorData(w, http.StatusServiceUnavailable,
			"this instance is the standby of HA cluster "+st.Cluster+"; it is read-only: send changes to the active instance",
			map[string]any{"role": st.Role, "cluster": st.Cluster})
	})
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
