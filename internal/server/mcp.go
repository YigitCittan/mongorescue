package server

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/audit"
)

// MCPPath is the Streamable HTTP endpoint of the MCP server.
const MCPPath = "/mcp"

// WithMCPHandler mounts h (the MCP Streamable HTTP handler) at /mcp. The endpoint
// accepts API keys only, never session cookies, and is subject to the
// security.mcp_enabled setting, Origin validation and DNS-rebinding protection.
func WithMCPHandler(h http.Handler) Option {
	return func(s *Server) { s.mcpHandler = h }
}

// WithAudit enables GET /api/v1/audit, the log of API/MCP activity.
func WithAudit(svc *audit.Service) Option {
	return func(s *Server) { s.audit = svc }
}

// registerMCPRoutes adds the MCP endpoint and the audit log.
func (s *Server) registerMCPRoutes(mux *router) {
	mux.HandleFunc("GET /api/v1/audit", s.handleListAudit)
	if s.mcpHandler != nil {
		// Explicit methods: a method-less "/mcp" would conflict with "GET /". The
		// stateless endpoint answers GET and DELETE with 405 itself.
		h := s.mcpGuard(s.mcpHandler)
		for _, m := range mcpMethods {
			mux.Handle(m+" "+MCPPath, h)
		}
	}
}

// mcpMethods are the HTTP methods of the Streamable HTTP transport.
var mcpMethods = []string{http.MethodPost, http.MethodGet, http.MethodDelete}

// mcpGuard applies the HTTP policy of the MCP endpoint before next: the enable
// switch, Origin validation (browsers may not call it cross-origin) and, unless the
// server trusts a reverse proxy, the MCP DNS-rebinding rule (a request that arrived
// on a loopback address must name a loopback host).
func (s *Server) mcpGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sec := s.security()
		if !sec.MCPEnabled {
			writeError(w, http.StatusForbidden, "forbidden: the MCP endpoint is disabled (Settings → Security)")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !s.sameOrAllowedOrigin(origin, r.Host) {
			writeError(w, http.StatusForbidden, "forbidden: cross-origin request")
			return
		}
		if !sec.TrustProxyHeaders && arrivedOnLoopback(r) && !isLoopbackHost(r.Host) {
			writeError(w, http.StatusForbidden, "forbidden: invalid Host header for a local MCP endpoint")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// arrivedOnLoopback reports whether r was accepted on a loopback address.
func arrivedOnLoopback(r *http.Request) bool {
	addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || addr == nil {
		return false
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		host = addr.String()
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isLoopbackHost reports whether a Host header (host[:port]) names localhost or a
// loopback address.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > audit.MaxListLimit {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and "+strconv.Itoa(audit.MaxListLimit))
			return
		}
		limit = n
	}
	entries, err := s.audit.List(r.Context(), limit)
	if errors.Is(err, audit.ErrNoRepository) {
		writeError(w, http.StatusServiceUnavailable, "the audit log is not configured")
		return
	}
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}
