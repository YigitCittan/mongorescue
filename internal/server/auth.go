package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

// SessionCookieName is the name of the session cookie.
const SessionCookieName = "mr_session"

// CSRFHeader carries the session's CSRF token on unsafe cookie-authenticated requests.
const CSRFHeader = "X-CSRF-Token"

// maxAuthBody bounds the size of JSON request bodies of the auth and management APIs.
const maxAuthBody = 1 << 20

// publicPaths are served without credentials. Everything under /api/ that is not
// listed here, and /metrics unless security.metrics_public is on, requires a session
// or an API key. The single sign-on routes (/auth/oidc/) live outside /api/ and are
// public too.
var publicPaths = map[string]bool{
	"/api/v1/health":       true,
	"/api/v1/setup/status": true,
	"/api/v1/setup":        true,
	"/api/v1/auth/login":   true,
	authMethodsPath:        true,
}

// Sign-in routes, also their audit log actions.
const (
	setupRoute = "POST /api/v1/setup"
	loginRoute = "POST /api/v1/auth/login"
)

// WithAuth sets the authentication service. Without it every protected route answers
// 401: there is no unauthenticated mode.
func WithAuth(svc *auth.Service) Option {
	return func(s *Server) { s.auth = svc }
}

// registerAuthRoutes adds setup, login, session, user and API key endpoints.
func (s *Server) registerAuthRoutes(mux *router) {
	mux.HandleFunc("GET /api/v1/setup/status", s.handleSetupStatus)
	mux.HandleFunc(setupRoute, s.handleSetup)
	mux.HandleFunc(loginRoute, s.handleLogin)
	mux.HandleFunc(logoutRoute, s.handleLogout)
	mux.HandleFunc("GET "+meRoute, s.handleMe)
	mux.HandleFunc("GET /api/v1/auth/sessions", s.handleListSessions)
	mux.HandleFunc(revokeSessionRoute, s.handleRevokeSession)

	mux.HandleFunc("GET /api/v1/users", s.handleListUsers)
	mux.HandleFunc(userNamesRoute, s.handleListUserNames)
	mux.HandleFunc("POST /api/v1/users", s.handleCreateUser)
	mux.HandleFunc("DELETE /api/v1/users/{id}", s.handleDeleteUser)
	mux.HandleFunc(userRoleRoute, s.handleSetUserRole)
	mux.HandleFunc(changePasswordRoute, s.handleChangePassword)

	mux.HandleFunc(listAPIKeysRoute, s.handleListAPIKeys)
	mux.HandleFunc(createAPIKeyRoute, s.handleCreateAPIKey)
	mux.HandleFunc(deleteAPIKeyRoute, s.handleDeleteAPIKey)
}

// authMiddleware authenticates every non-public request by API key (Authorization:
// Bearer or X-API-Key) or session cookie, stores the principal in the request context,
// enforces the CSRF token on cookie-authenticated unsafe requests and the scope the
// matched route requires, records mutating requests (and audited downloads) in the
// audit log (see auditsAction), and audits REST requests made with an API key in the
// activity log (see auditREST). /metrics and /mcp accept API keys only.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		isMetrics := path == "/metrics"
		isMCP := path == MCPPath
		switch {
		case publicPaths[path]:
			if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.allowPublicWriteAudited(w, r) {
				return
			}
			next.ServeHTTP(w, r)
			return
		case isMetrics && s.security().MetricsPublic,
			!isMetrics && !isMCP && !strings.HasPrefix(path, "/api/"):
			next.ServeHTTP(w, r)
			return
		}
		if s.auth == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized: authentication is not configured")
			return
		}

		pattern := s.routePattern(r)
		var (
			principal *auth.Principal
			err       error
		)
		if key := presentedKey(r); key != "" {
			principal, err = s.auth.AuthenticateAPIKey(r.Context(), key)
		} else if isMetrics || isMCP {
			// Prometheus and MCP clients use bearer tokens; sessions are for the
			// dashboard (and never reach /mcp, so it has no CSRF surface).
			err = auth.ErrUnauthenticated
		} else if c, cerr := r.Cookie(SessionCookieName); cerr == nil {
			principal, err = s.auth.AuthenticateSession(r.Context(), c.Value)
			if errors.Is(err, auth.ErrUnauthenticated) {
				s.clearSessionCookie(w, r)
			}
		} else {
			err = auth.ErrUnauthenticated
		}
		if err != nil {
			// Refused mutating requests are recorded, anonymously.
			if s.auditLog != nil && auditsAction(r.Method, path, pattern) {
				rec, done := s.auditedAction(w, r, nil, pattern)
				defer done()
				w = rec
			}
			if !errors.Is(err, auth.ErrUnauthenticated) {
				s.logger.Error("authentication failed", slog.Any("error", err))
				writeError(w, http.StatusInternalServerError, "authentication failed")
				return
			}
			// GET /api/v1/auth/me tells a signed-out visitor so with 200 and an empty
			// session, so the dashboard needs no failing request to find out.
			if path == meRoute && r.Method == http.MethodGet {
				next.ServeHTTP(w, r)
				return
			}
			if isMCP {
				w.Header().Set("WWW-Authenticate", `Bearer realm="mongorescue"`)
				writeError(w, http.StatusUnauthorized, "unauthorized: supply a valid API key (Authorization: Bearer)")
				return
			}
			writeError(w, http.StatusUnauthorized, "unauthorized: log in or supply a valid API key")
			return
		}
		ctx := auth.WithPrincipal(r.Context(), principal)
		ctx = auditlog.WithClient(ctx, auditlog.Client{IP: s.clientIP(r), UserAgent: r.UserAgent()})
		recordsAction := s.auditLog != nil && auditsAction(r.Method, path, pattern)
		if recordsAction {
			ctx = auditlog.WithAnnotations(ctx)
		}
		req := r.WithContext(ctx)
		if recordsAction || auditsREST(principal, path) {
			rec := &statusRecorder{ResponseWriter: w}
			if recordsAction {
				// The activity log below is deferred later, so it runs first.
				wrapped, done := s.auditedAction(w, req, principal, pattern)
				defer done()
				rec = wrapped
			}
			if auditsREST(principal, path) {
				defer s.auditREST(req, principal, pattern, rec, time.Now())
			}
			w = rec
		}
		// Refused CSRF tokens are recorded too: they may be a cross-site attack.
		if err := auth.CheckCSRF(principal, r.Method, r.Header.Get(CSRFHeader)); err != nil {
			writeError(w, http.StatusForbidden, "forbidden: missing or invalid "+CSRFHeader+" header")
			return
		}
		if err := s.checkScope(principal, pattern); err != nil {
			writeError(w, http.StatusForbidden, "forbidden: "+scopeMessage(err))
			return
		}
		next.ServeHTTP(w, req)
	})
}

// routePattern returns the ServeMux pattern r matches, or "" when none does.
func (s *Server) routePattern(r *http.Request) string {
	if s.mux == nil {
		return ""
	}
	_, pattern := s.mux.Handler(r)
	return pattern
}

// checkScope enforces the scope of the matched route pattern (see routeScopes).
// Requests that match no route pass, so that the mux answers them with 404 or 405;
// without a mux everything needs admin.
func (s *Server) checkScope(p *auth.Principal, pattern string) error {
	if s.mux == nil {
		return p.Require(auth.ScopeAdmin)
	}
	if pattern == "" {
		return nil
	}
	return p.Require(requiredScope(pattern))
}

// scopeMessage renders a scope error for API clients: what the request needs and
// what limits the caller (their role, the key's scope, or the creator's role
// capping the key).
func scopeMessage(err error) string {
	var se *auth.ScopeError
	if errors.As(err, &se) {
		return se.Message()
	}
	return "insufficient scope"
}

// allowPublicWrite guards the unauthenticated POST endpoints (setup, login) against
// cross-site requests (login CSRF): the body must be declared as application/json,
// which a cross-site HTML form cannot send without a CORS preflight, and a present
// Origin header must name this server or a configured CORS origin. It writes 415 or
// 403 and returns false when the request is refused.
func (s *Server) allowPublicWrite(w http.ResponseWriter, r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported media type: send Content-Type: application/json")
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" && !s.sameOrAllowedOrigin(origin, r.Host) {
		writeError(w, http.StatusForbidden, "forbidden: cross-origin request")
		return false
	}
	return true
}

// allowPublicWriteAudited is allowPublicWrite recording a refused request in the
// audit log, anonymously.
func (s *Server) allowPublicWriteAudited(w http.ResponseWriter, r *http.Request) bool {
	if s.auditLog == nil {
		return s.allowPublicWrite(w, r)
	}
	rec := &statusRecorder{ResponseWriter: w}
	if s.allowPublicWrite(rec, r) {
		return true
	}
	s.recordAction(r, nil, s.routePattern(r), rec, false)
	return false
}

// sameOrAllowedOrigin reports whether origin (an Origin header value) is the server's
// own host or exactly matches a configured CORS origin.
func (s *Server) sameOrAllowedOrigin(origin, host string) bool {
	for _, allowed := range s.security().CORSOrigins {
		if origin == allowed {
			return true
		}
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false // includes the opaque "null" origin
	}
	return strings.EqualFold(u.Host, host)
}

// keyHeader is the canonical form (http.CanonicalHeaderKey) of the X-API-Key header.
const keyHeader = "X-Api-Key"

// presentedKey extracts the key a client presents, from Authorization: Bearer or
// X-API-Key. It is a generated 160-bit random key (or one imported from the deprecated
// MONGORESCUE_API_KEY), never a password: auth compares it through its SHA-256 digest
// or HMAC, see auth.HashToken. X-API-Key is read from the canonical header map entry,
// which is what Header.Get does.
func presentedKey(r *http.Request) string {
	if h := r.Header.Get("Authorization"); len(h) > len("Bearer ") && strings.EqualFold(h[:len("Bearer ")], "Bearer ") {
		return strings.TrimSpace(h[len("Bearer "):])
	}
	if v := r.Header[keyHeader]; len(v) > 0 {
		return strings.TrimSpace(v[0])
	}
	return ""
}

// clientIP returns the address used for login throttling. Forwarding headers are
// honoured only with the security.trust_proxy_headers setting: the last
// X-Forwarded-For entry (the one appended by the trusted proxy), else X-Real-IP.
func (s *Server) clientIP(r *http.Request) string {
	if s.security().TrustProxyHeaders {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
		if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// secureRequest reports whether cookies must carry the Secure attribute, following
// the security.secure_cookies policy.
func (s *Server) secureRequest(r *http.Request) bool {
	sec := s.security()
	switch sec.SecureCookies {
	case settings.CookiesAlways:
		return true
	case settings.CookiesNever:
		return false
	}
	if r.TLS != nil {
		return true
	}
	return sec.TrustProxyHeaders && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// sessionCookie builds the session cookie (HttpOnly, SameSite=Strict, Path=/). It is
// the only place the cookie is constructed, so issuing and clearing it cannot drift.
//
// Secure follows the security.secure_cookies policy (secureRequest): by default it is
// set whenever the request arrived over TLS, directly or through a trusted proxy
// reporting https. It stays off only for plain-HTTP requests (the desktop app on
// localhost, development setups), where the browser would drop a Secure cookie and
// no login could succeed.
func (s *Server) sessionCookie(r *http.Request, value string, expires time.Time, maxAge int) *http.Cookie {
	c := &http.Cookie{ //nolint:gosec // G124: Secure is set below on TLS requests.
		Name:     SessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}
	if s.secureRequest(r) {
		c.Secure = true
	}
	return c
}

// setSessionCookie issues the session cookie. Max-Age is measured on the auth
// service's clock, the one that set expires, so the two attributes always agree.
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	now := time.Now()
	if s.auth != nil {
		now = s.auth.Now()
	}
	http.SetCookie(w, s.sessionCookie(r, token, expires, int(expires.Sub(now).Seconds())))
}

// clearSessionCookie deletes the session cookie in the browser.
func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, s.sessionCookie(r, "", time.Time{}, -1))
}

// decodeBody decodes a bounded JSON request body into v.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAuthBody))
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request json")
		return false
	}
	return true
}

// writeAuthError maps auth errors to HTTP responses without leaking internals.
func (s *Server) writeAuthError(w http.ResponseWriter, err error) {
	var throttled *auth.ThrottledError
	switch {
	case errors.As(err, &throttled):
		w.Header().Set("Retry-After", strconv.Itoa(int(throttled.RetryAfter.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many failed attempts; try again later")
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeError(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, auth.ErrInvalidSetupCode), errors.Is(err, auth.ErrCurrentPassword), errors.Is(err, auth.ErrSessionRequired),
		errors.Is(err, auth.ErrScopeExceedsRole), errors.Is(err, auth.ErrLocalLoginDisabled):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, auth.ErrSetupCompleted), errors.Is(err, auth.ErrUserExists), errors.Is(err, auth.ErrLastUser),
		errors.Is(err, auth.ErrLastAdmin), errors.Is(err, auth.ErrLastLocalAdmin), errors.Is(err, auth.ErrRoleManagedByProvider):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, auth.ErrInvalidPassword), errors.Is(err, auth.ErrInvalidUsername),
		errors.Is(err, auth.ErrInvalidName), errors.Is(err, auth.ErrDeleteSelf), errors.Is(err, auth.ErrInvalidScope),
		errors.Is(err, auth.ErrInvalidRole), errors.Is(err, auth.ErrChangeOwnRole), errors.Is(err, auth.ErrNoPassword):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, auth.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden: "+scopeMessage(err))
	case errors.Is(err, auth.ErrUserNotFound):
		writeError(w, http.StatusNotFound, "user not found")
	case errors.Is(err, auth.ErrAPIKeyNotFound):
		writeError(w, http.StatusNotFound, "api key not found")
	case errors.Is(err, auth.ErrSessionNotFound):
		writeError(w, http.StatusNotFound, "session not found")
	case errors.Is(err, auth.ErrUnauthenticated):
		writeError(w, http.StatusUnauthorized, "unauthorized")
	default:
		s.logger.Error("auth request failed", logsafe.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// requireAuth returns the auth service, answering 503 when it is not configured.
func (s *Server) requireAuth(w http.ResponseWriter) (*auth.Service, bool) {
	if s.auth == nil {
		writeError(w, http.StatusServiceUnavailable, "authentication is not configured")
		return nil, false
	}
	return s.auth, true
}

// principal returns the authenticated caller, answering 401 when there is none.
func principal(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
	p := auth.PrincipalFrom(r.Context())
	if p == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return nil, false
	}
	return p, true
}

// sessionResponse is returned by setup, login and me.
type sessionResponse struct {
	User      *auth.User  `json:"user"`
	CSRFToken string      `json:"csrf_token"`
	Auth      auth.Method `json:"auth,omitempty"`
}

func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireAuth(w)
	if !ok {
		return
	}
	required, err := svc.SetupRequired(r.Context())
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"setup_required": required})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireAuth(w)
	if !ok {
		return
	}
	var req struct {
		SetupCode string `json:"setup_code"`
		Username  string `json:"username"`
		Password  string `json:"password"`
	}
	rec := &statusRecorder{ResponseWriter: w}
	w = rec
	var user *auth.User
	defer func() { s.recordSignIn(r, setupRoute, rec, user, req.Username, nil) }()
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := svc.Setup(r.Context(), s.clientIP(r), req.SetupCode, req.Username, req.Password)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	user = res.User
	s.setSessionCookie(w, r, res.Token, res.ExpiresAt)
	writeJSON(w, http.StatusCreated, sessionResponse{User: res.User, CSRFToken: res.CSRFToken, Auth: auth.MethodSession})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireAuth(w)
	if !ok {
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	rec := &statusRecorder{ResponseWriter: w}
	w = rec
	var user *auth.User
	var targets map[string]string
	defer func() { s.recordSignIn(r, loginRoute, rec, user, req.Username, targets) }()
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := svc.Login(r.Context(), s.clientIP(r), req.Username, req.Password)
	if err != nil {
		if errors.Is(err, auth.ErrLocalLoginDisabled) {
			targets = map[string]string{targetReason: reasonLocalLoginDisabled}
		}
		s.writeAuthError(w, err)
		return
	}
	user = res.User
	s.setSessionCookie(w, r, res.Token, res.ExpiresAt)
	writeJSON(w, http.StatusOK, sessionResponse{User: res.User, CSRFToken: res.CSRFToken, Auth: auth.MethodSession})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireAuth(w)
	if !ok {
		return
	}
	if c, err := r.Cookie(SessionCookieName); err == nil {
		if err := svc.Logout(r.Context(), c.Value); err != nil {
			s.writeAuthError(w, err)
			return
		}
	}
	s.clearSessionCookie(w, r)
	out := logoutResponse{LoggedOut: true, EndSessionURL: s.endSessionURL(r.Context(), auth.PrincipalFrom(r.Context()))}
	writeJSON(w, http.StatusOK, out)
}

// logoutResponse is the answer of POST /api/v1/auth/logout.
type logoutResponse struct {
	LoggedOut bool `json:"logged_out"`
	// EndSessionURL, when set, signs a single sign-on user out at the identity
	// provider too (oidc.rp_logout); the dashboard navigates to it.
	EndSessionURL string `json:"end_session_url,omitempty"`
}

// meRoute is the session introspection route; it answers signed-out visitors too.
const meRoute = "/api/v1/auth/me"

// meResponse is returned by GET /api/v1/auth/me. For a signed-out visitor every
// field is empty: {"user": null, "csrf_token": "", "auth": "", "role": "", "scope": ""}.
type meResponse struct {
	User      *auth.User  `json:"user"`
	CSRFToken string      `json:"csrf_token"`
	Auth      auth.Method `json:"auth"`
	// Role is the dashboard role of the user ("" for a key without a user).
	Role auth.Role `json:"role"`
	// Scope is the effective scope: what the caller may do now.
	Scope auth.Scope `json:"scope"`
	// KeyScope is the API key's own scope (API keys only); Scope is lower when the
	// creator's role caps the key.
	KeyScope auth.Scope `json:"key_scope,omitempty"`
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	p := auth.PrincipalFrom(r.Context())
	if p == nil {
		writeJSON(w, http.StatusOK, meResponse{})
		return
	}
	writeJSON(w, http.StatusOK, meResponse{
		User: p.User, CSRFToken: p.CSRFToken, Auth: p.Method, Role: p.Role, Scope: p.Scope, KeyScope: p.KeyScope,
	})
}

// handleListSessions lists the caller's own sessions, or with ?all=true (admin) those
// of every user. Token hashes and CSRF tokens are never part of the answer.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireAuth(w)
	if !ok {
		return
	}
	p, ok := principal(w, r)
	if !ok {
		return
	}
	all := false
	if v := r.URL.Query().Get("all"); v != "" {
		var err error
		if all, err = strconv.ParseBool(v); err != nil {
			writeError(w, http.StatusBadRequest, "invalid all: want true or false")
			return
		}
	}
	sessions, err := svc.ListSessions(r.Context(), p, all)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, sessions)
}

// handleRevokeSession ends one session by its public ID. Revoking the session the
// request was made with also clears the cookie.
func (s *Server) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireAuth(w)
	if !ok {
		return
	}
	p, ok := principal(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	current, err := svc.RevokeSession(r.Context(), p, id)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	if current {
		s.clearSessionCookie(w, r)
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked_id": id, "current": current})
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireAuth(w)
	if !ok {
		return
	}
	p, ok := principal(w, r)
	if !ok {
		return
	}
	users, err := svc.ListUsers(r.Context(), p)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, users)
}

// handleListUserNames serves the ID and name of every user (read), so dashboards of
// every role can show who created or pinned something.
func (s *Server) handleListUserNames(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireAuth(w)
	if !ok {
		return
	}
	p, ok := principal(w, r)
	if !ok {
		return
	}
	names, err := svc.ListUserNames(r.Context(), p)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, names)
}

// Audit log targets of user and API key changes.
const (
	targetRole           = "role"
	targetRoleFrom       = "role_from"
	targetRoleTo         = "role_to"
	targetScope          = "scope"
	targetCeilingApplied = "ceiling_applied"
)

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireAuth(w)
	if !ok {
		return
	}
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		// Role is viewer, operator or admin; omitted means viewer.
		Role auth.Role `json:"role"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	user, err := svc.CreateUser(r.Context(), p, req.Username, req.Password, req.Role)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	auditlog.Annotate(r.Context(), targetRole, string(user.Role))
	writeJSON(w, http.StatusCreated, user)
}

// handleSetUserRole changes a user's dashboard role: 400 for an unknown role or the
// caller's own user, 404 for an unknown user, 409 when it would demote the last
// admin. The user's sessions end; their API keys are capped by the new role.
func (s *Server) handleSetUserRole(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireAuth(w)
	if !ok {
		return
	}
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var req struct {
		Role auth.Role `json:"role"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	change, err := svc.SetUserRole(r.Context(), p, r.PathValue("id"), req.Role)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	auditlog.Annotate(r.Context(), targetRoleFrom, string(change.From))
	auditlog.Annotate(r.Context(), targetRoleTo, string(change.User.Role))
	writeJSON(w, http.StatusOK, change.User)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireAuth(w)
	if !ok {
		return
	}
	p, ok := principal(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if err := svc.DeleteUser(r.Context(), p, id); err != nil {
		s.writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted_id": id})
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireAuth(w)
	if !ok {
		return
	}
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	if err := svc.ChangePassword(r.Context(), p, id, req.CurrentPassword, req.NewPassword); err != nil {
		s.writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"updated_id": id})
}

func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireAuth(w)
	if !ok {
		return
	}
	p, ok := principal(w, r)
	if !ok {
		return
	}
	keys, err := svc.ListAPIKeys(r.Context(), p)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, keys)
}

// createdAPIKey is the one response that ever carries a key's plaintext.
type createdAPIKey struct {
	APIKey *auth.APIKey `json:"api_key"`
	Key    string       `json:"key"`
}

func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireAuth(w)
	if !ok {
		return
	}
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
		// Scope is read, operator or admin; omitted means read.
		Scope auth.Scope `json:"scope"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	k, plain, err := svc.CreateAPIKey(r.Context(), p, req.Name, req.Scope)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	// ceiling_applied: the key has a creator, so that user's role caps it from now on.
	auditlog.Annotate(r.Context(), targetScope, string(k.Scope))
	auditlog.Annotate(r.Context(), targetCeilingApplied, strconv.FormatBool(k.CreatedBy != ""))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, createdAPIKey{APIKey: k, Key: plain})
}

func (s *Server) handleDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireAuth(w)
	if !ok {
		return
	}
	p, ok := principal(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if err := svc.DeleteAPIKey(r.Context(), p, id); err != nil {
		s.writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted_id": id})
}

// setupURL renders the dashboard address shown in the setup log line.
func setupURL(host string, port int) string {
	h := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if h == "" || h == "0.0.0.0" || h == "::" {
		h = "localhost"
	}
	return fmt.Sprintf("http://%s/", net.JoinHostPort(h, strconv.Itoa(port)))
}

// SetupURL returns the dashboard URL for the configured listen address, for the
// setup-mode log line.
func (s *Server) SetupURL() string {
	return setupURL(s.cfg.Host, s.cfg.Port)
}
