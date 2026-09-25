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

	"github.com/yigitcittan/mongorescue/internal/auth"
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
// or an API key.
var publicPaths = map[string]bool{
	"/api/v1/health":       true,
	"/api/v1/setup/status": true,
	"/api/v1/setup":        true,
	"/api/v1/auth/login":   true,
}

// WithAuth sets the authentication service. Without it every protected route answers
// 401: there is no unauthenticated mode.
func WithAuth(svc *auth.Service) Option {
	return func(s *Server) { s.auth = svc }
}

// registerAuthRoutes adds setup, login, session, user and API key endpoints.
func (s *Server) registerAuthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/setup/status", s.handleSetupStatus)
	mux.HandleFunc("POST /api/v1/setup", s.handleSetup)
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)
	mux.HandleFunc("GET "+meRoute, s.handleMe)

	mux.HandleFunc("GET /api/v1/users", s.handleListUsers)
	mux.HandleFunc("POST /api/v1/users", s.handleCreateUser)
	mux.HandleFunc("DELETE /api/v1/users/{id}", s.handleDeleteUser)
	mux.HandleFunc("PUT /api/v1/users/{id}/password", s.handleChangePassword)

	mux.HandleFunc("GET /api/v1/api-keys", s.handleListAPIKeys)
	mux.HandleFunc("POST /api/v1/api-keys", s.handleCreateAPIKey)
	mux.HandleFunc("DELETE /api/v1/api-keys/{id}", s.handleDeleteAPIKey)
}

// authMiddleware authenticates every non-public request by API key (Authorization:
// Bearer or X-API-Key) or session cookie, stores the principal in the request context
// and enforces the CSRF token on cookie-authenticated unsafe requests.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		isMetrics := path == "/metrics"
		switch {
		case publicPaths[path]:
			if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.allowPublicWrite(w, r) {
				return
			}
			next.ServeHTTP(w, r)
			return
		case isMetrics && s.security().MetricsPublic,
			!isMetrics && !strings.HasPrefix(path, "/api/"):
			next.ServeHTTP(w, r)
			return
		}
		if s.auth == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized: authentication is not configured")
			return
		}

		var (
			principal *auth.Principal
			err       error
		)
		if key := apiKeyFromRequest(r); key != "" {
			principal, err = s.auth.AuthenticateAPIKey(r.Context(), key)
		} else if isMetrics {
			// Prometheus scrapes with bearer tokens; sessions are for the dashboard.
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
			writeError(w, http.StatusUnauthorized, "unauthorized: log in or supply a valid API key")
			return
		}
		if err := auth.CheckCSRF(principal, r.Method, r.Header.Get(CSRFHeader)); err != nil {
			writeError(w, http.StatusForbidden, "forbidden: missing or invalid "+CSRFHeader+" header")
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
	})
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

// apiKeyFromRequest extracts an API key from Authorization: Bearer or X-API-Key.
func apiKeyFromRequest(r *http.Request) string {
	if h := r.Header.Get("Authorization"); len(h) > len("Bearer ") && strings.EqualFold(h[:len("Bearer ")], "Bearer ") {
		return strings.TrimSpace(h[len("Bearer "):])
	}
	return strings.TrimSpace(r.Header.Get("X-API-Key"))
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

// setSessionCookie issues the session cookie (HttpOnly, SameSite=Strict, Path=/).
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	// Secure follows the security.secure_cookies policy: by default whenever the request
	// arrived over TLS (directly or via a trusted proxy); plain-HTTP localhost setups
	// must still work.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is decided per request.
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		Secure:   s.secureRequest(r),
		SameSite: http.SameSiteStrictMode,
	})
}

// clearSessionCookie deletes the session cookie in the browser.
func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is decided per request.
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secureRequest(r),
		SameSite: http.SameSiteStrictMode,
	})
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
	case errors.Is(err, auth.ErrInvalidSetupCode), errors.Is(err, auth.ErrCurrentPassword):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, auth.ErrSetupCompleted), errors.Is(err, auth.ErrUserExists), errors.Is(err, auth.ErrLastUser):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, auth.ErrInvalidPassword), errors.Is(err, auth.ErrInvalidUsername),
		errors.Is(err, auth.ErrInvalidName), errors.Is(err, auth.ErrDeleteSelf):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, auth.ErrUserNotFound):
		writeError(w, http.StatusNotFound, "user not found")
	case errors.Is(err, auth.ErrAPIKeyNotFound):
		writeError(w, http.StatusNotFound, "api key not found")
	case errors.Is(err, auth.ErrUnauthenticated):
		writeError(w, http.StatusUnauthorized, "unauthorized")
	default:
		s.logger.Error("auth request failed", slog.Any("error", err))
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
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := svc.Setup(r.Context(), s.clientIP(r), req.SetupCode, req.Username, req.Password)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
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
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := svc.Login(r.Context(), s.clientIP(r), req.Username, req.Password)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
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
	writeJSON(w, http.StatusOK, map[string]bool{"logged_out": true})
}

// meRoute is the session introspection route; it answers signed-out visitors too.
const meRoute = "/api/v1/auth/me"

// meResponse is returned by GET /api/v1/auth/me. For a signed-out visitor every
// field is empty: {"user": null, "csrf_token": "", "auth": ""}.
type meResponse struct {
	User      *auth.User  `json:"user"`
	CSRFToken string      `json:"csrf_token"`
	Auth      auth.Method `json:"auth"`
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	p := auth.PrincipalFrom(r.Context())
	if p == nil {
		writeJSON(w, http.StatusOK, meResponse{})
		return
	}
	writeJSON(w, http.StatusOK, meResponse{User: p.User, CSRFToken: p.CSRFToken, Auth: p.Method})
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireAuth(w)
	if !ok {
		return
	}
	users, err := svc.ListUsers(r.Context())
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, users)
}

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
	}
	if !decodeBody(w, r, &req) {
		return
	}
	user, err := svc.CreateUser(r.Context(), p, req.Username, req.Password)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, user)
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
	keys, err := svc.ListAPIKeys(r.Context())
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
	}
	if !decodeBody(w, r, &req) {
		return
	}
	k, plain, err := svc.CreateAPIKey(r.Context(), p, req.Name)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
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
