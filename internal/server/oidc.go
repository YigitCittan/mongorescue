package server

import (
	"container/heap"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/auth/oidc"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

// Single sign-on routes. They live outside /api/: the browser navigates to them
// (the sign-in button is a link, so CSP form-action 'self' needs no exception).
const (
	oidcStartPath     = "/auth/oidc/start"
	oidcCallbackPath  = settings.OIDCCallbackPath
	oidcStartRoute    = "GET " + oidcStartPath
	oidcCallbackRoute = "GET " + oidcCallbackPath
	authMethodsPath   = "/api/v1/auth/methods"
	oidcTestRoute     = "POST /api/v1/settings/oidc/test"
)

// Flow cookie: the state, nonce, PKCE verifier and return path of one sign-in,
// sealed with a subkey of secret.key and bound to its location.
const (
	// OIDCFlowCookieName is the name of the flow cookie.
	OIDCFlowCookieName = "mr_oidc"
	oidcFlowCookiePath = "/auth/oidc/"
	// oidcFlowLifetime bounds a sign-in from start to callback.
	oidcFlowLifetime = 10 * time.Minute
	// maxFlowCookie bounds the sealed cookie value read back.
	maxFlowCookie = 4096
	// maxReturnTo bounds the return path.
	maxReturnTo = 2048
)

// oidcFlowBinding is the secretbox location of the flow cookie.
var oidcFlowBinding = secretbox.At("cookie", OIDCFlowCookieName, "flow")

// WithOIDC enables single sign-on: client talks to the provider and box (secretbox
// with the oidc.FlowSubkeyPurpose subkey) seals the flow cookie. Without it (the
// desktop app) the /auth/oidc/ routes are not registered and /api/v1/auth/methods
// reports single sign-on as unavailable.
func WithOIDC(client *oidc.Client, box *secretbox.Box) Option {
	return func(s *Server) {
		s.oidc, s.oidcBox = client, box
		s.usedStates = newStateSet()
	}
}

// registerOIDCRoutes adds the single sign-on routes. The test endpoint is always
// registered (it answers 404 without WithOIDC); the browser routes only with it.
func (s *Server) registerOIDCRoutes(mux *router) {
	mux.HandleFunc("GET "+authMethodsPath, s.handleAuthMethods)
	mux.HandleFunc(oidcTestRoute, s.handleTestOIDC)
	if s.oidc == nil || s.oidcBox == nil {
		return
	}
	mux.HandleFunc(oidcStartRoute, s.handleOIDCStart)
	mux.HandleFunc(oidcCallbackRoute, s.handleOIDCCallback)
}

// oidcSettings returns the single sign-on settings and whether sign-in through the
// provider is possible now (configured, enabled and not the desktop app).
func (s *Server) oidcSettings() (settings.OIDC, bool) {
	if s.settings == nil || s.auth == nil || s.oidc == nil || s.oidcBox == nil {
		return settings.OIDC{}, false
	}
	o := s.settings.Current().OIDC
	return o, o.Enabled
}

// clientConfig is the relying party configuration of o.
func clientConfig(o settings.OIDC) oidc.Config {
	return oidc.Config{
		Issuer: o.Issuer, ClientID: o.ClientID, ClientSecret: o.ClientSecret, RedirectURL: o.RedirectURL,
		Scopes: o.Scopes, UsernameClaim: o.UsernameClaim, GroupsClaim: o.GroupsClaim,
	}
}

// authMethods is the answer of GET /api/v1/auth/methods.
type authMethods struct {
	// Local is how the password form may be used: settings.OIDCLocalLoginAll or,
	// while single sign-on is on, settings.OIDCLocalLoginAdminsOnly.
	Local string `json:"local"`
	// OIDC describes the single sign-on button.
	OIDC oidcMethod `json:"oidc"`
}

// oidcMethod describes single sign-on for the sign-in page.
type oidcMethod struct {
	// Enabled shows the button.
	Enabled bool `json:"enabled"`
	// DisplayName labels it.
	DisplayName string `json:"display_name"`
	// Available is false in the desktop app, which has no single sign-on.
	Available bool `json:"available"`
}

// handleAuthMethods tells the sign-in page which methods it offers. It is public.
func (s *Server) handleAuthMethods(w http.ResponseWriter, _ *http.Request) {
	out := authMethods{Local: settings.OIDCLocalLoginAll, OIDC: oidcMethod{Available: s.oidc != nil && s.oidcBox != nil}}
	if o, ok := s.oidcSettings(); ok {
		out.OIDC.Enabled, out.OIDC.DisplayName = true, o.DisplayName
		if o.LocalLogin == settings.OIDCLocalLoginAdminsOnly {
			out.Local = settings.OIDCLocalLoginAdminsOnly
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// flowState is the content of the flow cookie.
type flowState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	ReturnTo string `json:"r"`
	// IssuedAt is the Unix time of the start, on the auth service's clock.
	IssuedAt int64 `json:"t"`
	// Issuer is the issuer the sign-in started with; a changed one refuses it.
	Issuer string `json:"i"`
}

// flowCookie builds the flow cookie: HttpOnly, Path=/auth/oidc/, SameSite=Lax (the
// provider's redirect back is a cross-site navigation, which Strict would strip),
// Secure following security.secure_cookies.
func (s *Server) flowCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	c := &http.Cookie{ //nolint:gosec // G124: Secure is set below on TLS requests.
		Name:     OIDCFlowCookieName,
		Value:    value,
		Path:     oidcFlowCookiePath,
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	if s.secureRequest(r) {
		c.Secure = true
	}
	return c
}

// sealFlow seals f for the flow cookie.
func (s *Server) sealFlow(f *flowState) (string, error) {
	raw, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	return s.oidcBox.Seal(oidcFlowBinding, string(raw))
}

// openFlow returns the flow of the request's cookie, or false when the cookie is
// missing, sealed with another key or for another location, malformed, or older
// than oidcFlowLifetime.
func (s *Server) openFlow(r *http.Request) (*flowState, bool) {
	c, err := r.Cookie(OIDCFlowCookieName)
	if err != nil || c.Value == "" || len(c.Value) > maxFlowCookie {
		return nil, false
	}
	plain, err := s.oidcBox.Open(oidcFlowBinding, c.Value)
	if err != nil {
		return nil, false
	}
	var f flowState
	if err = json.Unmarshal([]byte(plain), &f); err != nil || f.State == "" || f.Nonce == "" || f.Verifier == "" {
		return nil, false
	}
	age := s.auth.Now().Sub(time.Unix(f.IssuedAt, 0))
	if age < -oidc.Leeway || age > oidcFlowLifetime {
		return nil, false
	}
	return &f, true
}

// isLocalRedirect reports whether raw is a path on this server that the sign-in may
// return to: it starts with a single "/" (not "//" or "/\"), carries no scheme,
// host or user information once parsed, contains no control characters or
// backslashes (also percent-encoded) and no encoded slashes, and does not point
// back into the sign-in routes.
func isLocalRedirect(raw string) bool {
	if raw == "" || len(raw) > maxReturnTo || !strings.HasPrefix(raw, "/") ||
		strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, "/\\") {
		return false
	}
	unsafeRune := func(r rune) bool { return r < 0x20 || r == 0x7f || r == '\\' }
	if strings.ContainsFunc(raw, unsafeRune) {
		return false
	}
	low := strings.ToLower(raw)
	for _, bad := range []string{"%2f", "%5c", "%00", "%0a", "%0d", "%09"} {
		if strings.Contains(low, bad) {
			return false
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" ||
		!strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") || strings.HasPrefix(u.Path, "/auth/") {
		return false
	}
	return !strings.ContainsFunc(u.Path+u.Fragment, unsafeRune)
}

// safeReturnTo returns raw when it is a local redirect (isLocalRedirect), else "/".
func safeReturnTo(raw string) string {
	if isLocalRedirect(raw) {
		return raw
	}
	return "/"
}

// oidcFailure redirects the browser to the dashboard with the failure code.
func oidcFailure(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/?oidc_error="+url.QueryEscape(code), http.StatusFound)
}

// handleOIDCStart begins a sign-in: fresh state, nonce and PKCE verifier sealed in
// the flow cookie, then a redirect to the provider. 404 while single sign-on is off;
// throttled per client address; not audited.
func (s *Server) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	o, ok := s.oidcSettings()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if err := s.auth.AllowOIDCAttempt("start", s.clientIP(r)); err != nil {
		oidcFailure(w, r, auth.OIDCThrottled)
		return
	}
	req, err := oidc.NewAuthRequest()
	if err != nil {
		s.logger.Error("single sign-on: could not start", logsafe.Error(err))
		oidcFailure(w, r, auth.OIDCIdPError)
		return
	}
	authURL, err := s.oidc.AuthCodeURL(r.Context(), clientConfig(o), req)
	if err != nil {
		s.logger.Warn("single sign-on: the provider is not reachable", logsafe.Error(err))
		oidcFailure(w, r, auth.OIDCIdPError)
		return
	}
	sealed, err := s.sealFlow(&flowState{
		State: req.State, Nonce: req.Nonce, Verifier: req.Verifier, ReturnTo: safeReturnTo(r.URL.Query().Get("return_to")),
		IssuedAt: s.auth.Now().Unix(), Issuer: o.Issuer,
	})
	if err != nil {
		s.logger.Error("single sign-on: could not seal the flow", logsafe.Error(err))
		oidcFailure(w, r, auth.OIDCIdPError)
		return
	}
	http.SetCookie(w, s.flowCookie(r, sealed, int(oidcFlowLifetime.Seconds())))
	http.Redirect(w, r, authURL, http.StatusFound)
}

// oidcAttempt collects what the audit log records of a callback.
type oidcAttempt struct {
	reason string
	name   string // set only once the ID token verified
	login  *auth.OIDCLogin
}

// handleOIDCCallback completes a sign-in: it opens and clears the flow cookie,
// checks the state (constant time, single use), exchanges the code with the PKCE
// verifier, verifies the ID token, applies the sign-in rules (auth.LoginOIDC) and
// starts a session. Failures redirect to /?oidc_error=<code>; every callback is
// audited.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	o, ok := s.oidcSettings()
	if !ok {
		if s.auth == nil {
			http.NotFound(w, r)
			return
		}
		http.SetCookie(w, s.flowCookie(r, "", -1))
		s.recordOIDCCallback(r, &oidcAttempt{reason: auth.OIDCDisabled})
		oidcFailure(w, r, auth.OIDCDisabled)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	a := &oidcAttempt{}
	defer s.recordOIDCCallback(r, a)
	returnTo, err := s.completeOIDC(r, o, a)
	// The flow cookie is single use, whatever happened.
	http.SetCookie(w, s.flowCookie(r, "", -1))
	if err != nil {
		if a.reason == "" {
			a.reason = auth.OIDCIdPError
		}
		oidcFailure(w, r, a.reason)
		return
	}
	s.setSessionCookie(w, r, a.login.Token, a.login.ExpiresAt)
	// The return path was checked at the start; check it again where it is used.
	target := "/"
	if isLocalRedirect(returnTo) {
		target = returnTo
	}
	http.Redirect(w, r, target, http.StatusFound) //nolint:gosec // G710: target is a local path (isLocalRedirect).
}

// errOIDCRefused marks a callback refused with the code in its oidcAttempt.
var errOIDCRefused = errors.New("single sign-on refused")

// completeOIDC runs the checks of a callback, filling a, and returns the return path.
func (s *Server) completeOIDC(r *http.Request, o settings.OIDC, a *oidcAttempt) (string, error) {
	refuse := func(code string) (string, error) {
		a.reason = code
		return "", errOIDCRefused
	}
	if err := s.auth.AllowOIDCAttempt("callback", s.clientIP(r)); err != nil {
		return refuse(auth.OIDCThrottled)
	}
	q := r.URL.Query()
	flow, ok := s.openFlow(r)
	if !ok || flow.Issuer != o.Issuer {
		return refuse(auth.OIDCStateMismatch)
	}
	got := q.Get("state")
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(flow.State)) != 1 || !s.usedStates.consume(flow.State, time.Unix(flow.IssuedAt, 0), s.auth.Now()) {
		return refuse(auth.OIDCStateMismatch)
	}
	if idpErr := q.Get("error"); idpErr != "" {
		s.logger.Warn("single sign-on: the provider reported an error",
			logsafe.Attr("idp_error", truncateUTF8(idpErr, 64)),
			logsafe.Attr("idp_error_description", truncateUTF8(q.Get("error_description"), 200)))
		return refuse(auth.OIDCIdPError)
	}
	code := q.Get("code")
	if code == "" {
		return refuse(auth.OIDCIdPError)
	}
	id, err := s.oidc.Exchange(r.Context(), clientConfig(o), code, oidc.AuthRequest{State: flow.State, Nonce: flow.Nonce, Verifier: flow.Verifier})
	if err != nil {
		s.logger.Warn("single sign-on: sign-in refused", logsafe.Error(err))
		if errors.Is(err, oidc.ErrTokenInvalid) {
			return refuse(auth.OIDCTokenInvalid)
		}
		return refuse(auth.OIDCIdPError)
	}
	// The signature verified: from here on the audit log may name the identity.
	a.name = id.Username
	login, err := s.auth.LoginOIDC(r.Context(), id)
	if err != nil {
		code := auth.OIDCErrorCode(err)
		if code == "" {
			s.logger.Error("single sign-on failed", logsafe.Error(err))
			code = auth.OIDCIdPError
		} else {
			s.logger.Info("single sign-on refused", slog.String("reason", code), logsafe.Attr("username", id.Username))
		}
		return refuse(code)
	}
	a.login = login
	if login.RoleKept && s.settings != nil {
		if err = s.settings.RaiseOIDCRoleKeptWarning(r.Context()); err != nil {
			s.logger.Warn("could not raise the single sign-on warning", logsafe.Error(err))
		}
	}
	return flow.ReturnTo, nil
}

// Audit targets of single sign-on.
const (
	targetProvider = "provider"
	targetReason   = auditlog.TargetReason
	targetCreated  = "created"
)

// recordOIDCCallback records a callback in the audit log: on success the user, with
// created, role_from and role_to; on failure an anonymous actor with the reason,
// named only once the ID token verified. Tokens, codes and claims are never
// recorded.
func (s *Server) recordOIDCCallback(r *http.Request, a *oidcAttempt) {
	if s.auditLog == nil {
		return
	}
	e := auditlog.Event{
		ActorKind: auditlog.ActorAnonymous, ActorName: a.name, Action: oidcCallbackRoute, Status: http.StatusFound,
		Outcome: auditlog.OutcomeDenied, ClientIP: s.clientIP(r), UserAgent: r.UserAgent(),
		Targets: map[string]string{targetProvider: string(auth.ProviderOIDC)},
	}
	switch {
	case a.login != nil:
		e.ActorKind, e.ActorUserID, e.ActorName, e.Outcome = auditlog.ActorUser, a.login.User.ID, a.login.User.Username, auditlog.OutcomeOK
		e.Targets[targetCreated] = boolString(a.login.Created)
		if a.login.RoleFrom != "" {
			e.Targets[targetRoleFrom] = string(a.login.RoleFrom)
		}
		e.Targets[targetRoleTo] = string(a.login.RoleTo)
	case a.reason == auth.OIDCThrottled:
		e.Outcome = auditlog.OutcomeRateLimited
		e.Targets[targetReason] = a.reason
	default:
		e.Targets[targetReason] = a.reason
	}
	s.auditLog.Record(r.Context(), e)
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// endSessionURL returns the provider logout URL for p when rp_logout is on and p is
// a single sign-on user, else "". The post-logout redirect is the origin of the
// configured redirect URL, never one derived from the request.
func (s *Server) endSessionURL(ctx context.Context, p *auth.Principal) string {
	o, ok := s.oidcSettings()
	if !ok || !o.RPLogout || p == nil || p.User == nil || p.User.Local() {
		return ""
	}
	ru, err := url.Parse(o.RedirectURL)
	if err != nil || ru.Host == "" {
		return ""
	}
	u, err := s.oidc.EndSessionURL(ctx, clientConfig(o), ru.Scheme+"://"+ru.Host+"/")
	if err != nil {
		s.logger.Warn("single sign-on: no provider logout URL", logsafe.Error(err))
		return ""
	}
	return u
}

// handleTestOIDC fetches the provider's discovery document and keys and reports the
// endpoints and algorithms (never secrets). The body may name the issuer to test
// ({"issuer": "..."}); without it the stored issuer is tested.
func (s *Server) handleTestOIDC(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		writeError(w, http.StatusNotFound, "single sign-on is not available in the desktop app")
		return
	}
	var req struct {
		Issuer string `json:"issuer"`
	}
	if r.ContentLength != 0 && !decodeBody(w, r, &req) {
		return
	}
	issuer := strings.TrimSpace(req.Issuer)
	if issuer == "" && s.settings != nil {
		issuer = s.settings.Current().OIDC.Issuer
	}
	if err := settings.ValidateOIDCIssuer(issuer); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	d, err := s.oidc.Discover(r.Context(), issuer)
	if err != nil {
		if errors.Is(err, oidc.ErrDiscovery) {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		s.logger.Error("single sign-on test failed", logsafe.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// stateSet remembers consumed states while their flow cookie could still be valid
// (oidcFlowLifetime plus the leeway after the flow's start), so a state is accepted
// once. It is bounded: when full, the state of the oldest flow is forgotten, so an
// attacker filling it can never make a valid fresh state be refused. A forgotten
// state is at worst replayable with its stolen cookie, and its code is single-use
// at the provider anyway.
type stateSet struct {
	mu   sync.Mutex
	max  int
	seen map[string]bool
	// byStart orders the remembered states by the start of their flow.
	byStart stateHeap
}

// maxUsedStates bounds the remembered states.
const maxUsedStates = 100000

func newStateSet() *stateSet { return newStateSetOf(maxUsedStates) }

func newStateSetOf(maxStates int) *stateSet {
	return &stateSet{max: maxStates, seen: map[string]bool{}}
}

// consume records the state of a flow started at issuedAt, at now, and reports
// whether it was not used before. States whose flows expired are forgotten first,
// then the oldest flow's when the set is full.
func (u *stateSet) consume(state string, issuedAt, now time.Time) bool {
	sum := sha256.Sum256([]byte(state))
	k := hex.EncodeToString(sum[:])
	u.mu.Lock()
	defer u.mu.Unlock()
	cutoff := now.Add(-(oidcFlowLifetime + oidc.Leeway))
	for u.byStart.Len() > 0 && u.byStart[0].start.Before(cutoff) {
		delete(u.seen, heap.Pop(&u.byStart).(usedState).key)
	}
	if u.seen[k] {
		return false
	}
	for u.byStart.Len() >= u.max {
		delete(u.seen, heap.Pop(&u.byStart).(usedState).key)
	}
	u.seen[k] = true
	heap.Push(&u.byStart, usedState{key: k, start: issuedAt})
	return true
}

// size returns how many states are remembered.
func (u *stateSet) size() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.seen)
}

// usedState is a remembered state and the start of its flow.
type usedState struct {
	key   string
	start time.Time
}

// stateHeap is a min-heap of used states by flow start (container/heap).
type stateHeap []usedState

func (h stateHeap) Len() int           { return len(h) }
func (h stateHeap) Less(i, j int) bool { return h[i].start.Before(h[j].start) }
func (h stateHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

// Push implements heap.Interface.
func (h *stateHeap) Push(x any) { *h = append(*h, x.(usedState)) }

// Pop implements heap.Interface.
func (h *stateHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}
