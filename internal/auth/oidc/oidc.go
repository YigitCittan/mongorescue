// Package oidc is the OpenID Connect protocol glue of single sign-on: provider
// discovery, the authorization request with PKCE and a nonce, the code exchange, the
// verification of the ID token and the extraction of the claims the sign-in rules
// (auth.Service.LoginOIDC) work with. It has no HTTP handlers and decides nothing
// about users or roles.
//
// The JOSE work (discovery, JWKS caching and rotation, signature, issuer, audience
// and expiry checks) is done by github.com/coreos/go-oidc/v3, the code exchange and
// PKCE by golang.org/x/oauth2: audited libraries rather than hand-written token
// parsing. This package adds the checks they leave to the caller: the nonce, azp,
// iat and nbf, the allowed algorithms (RS256 and ES256 only) and a bounded HTTP
// client whose responses are capped at MaxResponseBytes.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

// Protocol limits.
const (
	// MaxResponseBytes caps every response body read from the provider (discovery,
	// JWKS, token).
	MaxResponseBytes = 1 << 20
	// ExchangeTimeout bounds the code exchange and the verification of its ID
	// token, key fetches included.
	ExchangeTimeout = 10 * time.Second
	// Leeway is the clock skew tolerated on exp, nbf and iat.
	Leeway = 60 * time.Second
	// MaxTokenAge is the oldest iat accepted.
	MaxTokenAge = 10 * time.Minute
	// providerTTL is how long a discovered provider (and its key cache) is reused.
	providerTTL = time.Hour
	// maxGroups bounds the groups read from a token.
	maxGroups = 1000
	// maxUsernameBytes is the longest username (auth.ValidateUsername).
	maxUsernameBytes = 64
)

// FlowSubkeyPurpose derives the key that seals the flow cookie
// (secretbox.DeriveSubkey).
const FlowSubkeyPurpose = "auth/oidc-flow"

// SupportedAlgorithms are the only ID token signature algorithms accepted.
var SupportedAlgorithms = []string{gooidc.RS256, gooidc.ES256}

// Sentinel errors. Their messages never contain tokens, codes or claims.
var (
	// ErrDiscovery is returned when the provider's discovery document or keys
	// cannot be fetched or are unusable.
	ErrDiscovery = errors.New("oidc: provider discovery failed")
	// ErrExchange is returned when the provider refuses or fails the code exchange.
	ErrExchange = errors.New("oidc: code exchange failed")
	// ErrTokenInvalid is returned when the ID token is missing or fails
	// verification.
	ErrTokenInvalid = errors.New("oidc: invalid ID token")
	// ErrResponseTooLarge is returned when a provider response exceeds
	// MaxResponseBytes.
	ErrResponseTooLarge = errors.New("oidc: provider response too large")
)

// Config is the relying party configuration, taken from the oidc settings.
type Config struct {
	// Issuer is the issuer URL; discovery must report exactly this issuer.
	Issuer string
	// ClientID is the registered client, the required audience of ID tokens.
	ClientID string
	// ClientSecret authenticates the client at the token endpoint ("" for a public
	// client).
	ClientSecret string
	// RedirectURL is the registered callback URL.
	RedirectURL string
	// Scopes are requested; openid is always added.
	Scopes []string
	// UsernameClaim names new users; email and then sub are the fallbacks.
	UsernameClaim string
	// GroupsClaim is a claim name or dot path holding a string or a list of
	// strings ("" = no groups).
	GroupsClaim string
}

// Client talks to OpenID Connect providers. It is safe for concurrent use.
type Client struct {
	http *http.Client
	now  func() time.Time

	mu        sync.Mutex
	providers map[string]*cachedProvider
}

// cachedProvider is a discovered provider and when it was fetched.
type cachedProvider struct {
	p         *gooidc.Provider
	meta      providerMetadata
	fetchedAt time.Time
}

// providerMetadata holds the discovery fields go-oidc does not expose.
type providerMetadata struct {
	JWKSURI            string   `json:"jwks_uri"`
	EndSessionEndpoint string   `json:"end_session_endpoint"`
	CodeChallenge      []string `json:"code_challenge_methods_supported"`
	TokenAuthMethods   []string `json:"token_endpoint_auth_methods_supported"`
}

// Option customises a Client.
type Option func(*Client)

// WithClock sets the clock tokens are checked against (tests; the auth service's
// clock in production).
func WithClock(now func() time.Time) Option { return func(c *Client) { c.now = now } }

// NewClient returns a Client sending every request through httpClient, which the
// application builds with a timeout, without redirects and refusing link-local and
// metadata addresses (notify.NewHTTPClient). Response bodies are capped at
// MaxResponseBytes on top of it.
func NewClient(httpClient *http.Client, opts ...Option) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: ExchangeTimeout}
	}
	c := &Client{http: limitResponses(httpClient), now: time.Now, providers: map[string]*cachedProvider{}}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// limitResponses returns a copy of hc whose response bodies fail with
// ErrResponseTooLarge beyond MaxResponseBytes.
func limitResponses(hc *http.Client) *http.Client {
	cp := *hc
	base := hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	cp.Transport = limitTransport{base: base}
	return &cp
}

// limitTransport caps response bodies.
type limitTransport struct{ base http.RoundTripper }

// RoundTrip implements http.RoundTripper.
func (t limitTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = &limitedBody{rc: resp.Body, left: MaxResponseBytes}
	return resp, nil
}

// limitedBody reads at most left bytes and then fails.
type limitedBody struct {
	rc   io.ReadCloser
	left int64
}

// Read implements io.Reader.
func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		// One more byte tells a body of exactly the limit from a longer one.
		var one [1]byte
		if n, _ := b.rc.Read(one[:]); n > 0 {
			return 0, ErrResponseTooLarge
		}
		return 0, io.EOF
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	return n, err
}

// Close implements io.Closer.
func (b *limitedBody) Close() error { return b.rc.Close() }

// context returns ctx carrying the HTTP client, for go-oidc (oidc.ClientContext)
// and x/oauth2, which read it under the same key (oauth2.HTTPClient).
func (c *Client) context(ctx context.Context) context.Context {
	return gooidc.ClientContext(ctx, c.http)
}

// loopback reports whether host is localhost or a loopback address.
func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkEndpoint refuses provider endpoints that are not https, except http on a
// loopback host for a loopback issuer (development and tests).
func checkEndpoint(name, raw string, issuerLoopback bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("%w: the %s is not a URL", ErrDiscovery, name)
	}
	if u.Scheme == "https" || (u.Scheme == "http" && issuerLoopback && loopback(u.Hostname())) {
		return nil
	}
	return fmt.Errorf("%w: the %s must use https", ErrDiscovery, name)
}

// discover fetches the discovery document of issuer.
func (c *Client) discover(ctx context.Context, issuer string) (*cachedProvider, error) {
	iu, err := url.Parse(issuer)
	if err != nil || iu.Hostname() == "" {
		return nil, fmt.Errorf("%w: the issuer is not a URL", ErrDiscovery)
	}
	p, err := gooidc.NewProvider(c.context(ctx), issuer)
	if err != nil {
		var mismatch *gooidc.IssuerMismatchError
		if errors.As(err, &mismatch) {
			return nil, fmt.Errorf("%w: the provider reports another issuer; use exactly the issuer of its discovery document", ErrDiscovery)
		}
		return nil, fmt.Errorf("%w: %s", ErrDiscovery, safeError(err, "the discovery document", iu.Host))
	}
	var meta providerMetadata
	if err = p.Claims(&meta); err != nil {
		return nil, fmt.Errorf("%w: unreadable discovery document", ErrDiscovery)
	}
	// Every endpoint, on every (re-)discovery, must use https (http only for a
	// loopback issuer and loopback endpoints).
	lb := iu.Scheme == "http" && loopback(iu.Hostname())
	ep := p.Endpoint()
	for _, e := range []struct{ name, url string }{
		{"authorization endpoint", ep.AuthURL}, {"token endpoint", ep.TokenURL}, {"jwks_uri", meta.JWKSURI},
	} {
		if err = checkEndpoint(e.name, e.url, lb); err != nil {
			return nil, err
		}
	}
	if meta.EndSessionEndpoint != "" {
		if err = checkEndpoint("end session endpoint", meta.EndSessionEndpoint, lb); err != nil {
			return nil, err
		}
	}
	return &cachedProvider{p: p, meta: meta, fetchedAt: c.now()}, nil
}

// provider returns the discovered provider of issuer, from the cache while it is
// younger than providerTTL.
func (c *Client) provider(ctx context.Context, issuer string) (*cachedProvider, error) {
	c.mu.Lock()
	cp, ok := c.providers[issuer]
	c.mu.Unlock()
	if ok && c.now().Sub(cp.fetchedAt) < providerTTL {
		return cp, nil
	}
	cp, err := c.discover(ctx, issuer)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	// Only the current issuer is kept: a changed issuer drops the old keys.
	c.providers = map[string]*cachedProvider{issuer: cp}
	c.mu.Unlock()
	return cp, nil
}

// oauth2Config returns the x/oauth2 configuration of cfg at p. A confidential
// client authenticates with HTTP Basic unless the provider only offers
// client_secret_post; a public client sends its client_id in the form.
func oauth2Config(cp *cachedProvider, cfg Config) *oauth2.Config {
	ep := cp.p.Endpoint()
	switch {
	case cfg.ClientSecret == "":
		ep.AuthStyle = oauth2.AuthStyleInParams
	case len(cp.meta.TokenAuthMethods) > 0 && !slices.Contains(cp.meta.TokenAuthMethods, "client_secret_basic") &&
		slices.Contains(cp.meta.TokenAuthMethods, "client_secret_post"):
		ep.AuthStyle = oauth2.AuthStyleInParams
	default:
		ep.AuthStyle = oauth2.AuthStyleInHeader
	}
	scopes := []string{gooidc.ScopeOpenID}
	for _, s := range cfg.Scopes {
		if s != "" && !slices.Contains(scopes, s) {
			scopes = append(scopes, s)
		}
	}
	return &oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, Endpoint: ep, RedirectURL: cfg.RedirectURL, Scopes: scopes}
}

// AuthRequest holds the per-sign-in secrets: the state, the nonce and the PKCE
// verifier. They travel only in the sealed flow cookie.
type AuthRequest struct {
	// State binds the callback to this browser.
	State string
	// Nonce binds the ID token to this sign-in.
	Nonce string
	// Verifier is the PKCE code verifier.
	Verifier string
}

// randomValue returns 32 random bytes in URL-safe base64.
func randomValue() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oidc: random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// NewAuthRequest returns fresh random values (32 bytes each) for a sign-in.
func NewAuthRequest() (AuthRequest, error) {
	state, err := randomValue()
	if err != nil {
		return AuthRequest{}, err
	}
	nonce, err := randomValue()
	if err != nil {
		return AuthRequest{}, err
	}
	return AuthRequest{State: state, Nonce: nonce, Verifier: oauth2.GenerateVerifier()}, nil
}

// AuthCodeURL returns the authorization endpoint URL that starts a sign-in: the
// authorization code flow (query response mode) with an S256 PKCE challenge and
// the nonce.
func (c *Client) AuthCodeURL(ctx context.Context, cfg Config, req AuthRequest) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ExchangeTimeout)
	defer cancel()
	cp, err := c.provider(ctx, cfg.Issuer)
	if err != nil {
		return "", err
	}
	return oauth2Config(cp, cfg).AuthCodeURL(req.State, gooidc.Nonce(req.Nonce), oauth2.S256ChallengeOption(req.Verifier)), nil
}

// Exchange redeems code with the PKCE verifier, verifies the ID token and returns
// the identity it carries. Access and refresh tokens are dropped. The checks: the
// signature (RS256 or ES256 with a key of the provider's JWKS, refetched for an
// unknown key ID), iss, aud containing the client, azp equal to the client when aud
// has several values or azp is present, the nonce, exp, nbf and iat (at most
// MaxTokenAge old, not in the future) with Leeway, and a subject.
func (c *Client) Exchange(ctx context.Context, cfg Config, code string, req AuthRequest) (*auth.ExternalIdentity, error) {
	if code == "" || req.Verifier == "" || req.Nonce == "" {
		return nil, fmt.Errorf("%w: missing code, verifier or nonce", ErrExchange)
	}
	ctx, cancel := context.WithTimeout(ctx, ExchangeTimeout)
	defer cancel()
	ctx = c.context(ctx)
	cp, err := c.provider(ctx, cfg.Issuer)
	if err != nil {
		return nil, err
	}
	oc := oauth2Config(cp, cfg)
	tok, err := oc.Exchange(ctx, code, oauth2.VerifierOption(req.Verifier))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrExchange, safeError(err, "the token endpoint", hostOf(oc.Endpoint.TokenURL)))
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return nil, fmt.Errorf("%w: the token response has no id_token", ErrTokenInvalid)
	}
	return c.verify(ctx, cp, cfg, raw, req.Nonce)
}

// verify checks the raw ID token and extracts the identity.
func (c *Client) verify(ctx context.Context, cp *cachedProvider, cfg Config, raw, nonce string) (*auth.ExternalIdentity, error) {
	now := c.now()
	verifier := cp.p.Verifier(&gooidc.Config{
		ClientID:             cfg.ClientID,
		SupportedSigningAlgs: SupportedAlgorithms,
		// go-oidc refuses exp before Now; shifting Now back gives exp the leeway.
		Now: func() time.Time { return now.Add(-Leeway) },
	})
	idt, err := verifier.Verify(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrTokenInvalid, verifyProblem(err))
	}
	var claims map[string]any
	if err = idt.Claims(&claims); err != nil {
		return nil, fmt.Errorf("%w: unreadable claims", ErrTokenInvalid)
	}
	azp, hasAzp := claims["azp"]
	if len(idt.Audience) > 1 || hasAzp {
		if s, ok := azp.(string); !ok || s != cfg.ClientID {
			return nil, fmt.Errorf("%w: the authorized party (azp) is not this client", ErrTokenInvalid)
		}
	}
	if idt.Nonce == "" || subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(nonce)) != 1 {
		return nil, fmt.Errorf("%w: the nonce does not match", ErrTokenInvalid)
	}
	switch {
	case idt.IssuedAt.IsZero():
		return nil, fmt.Errorf("%w: no iat", ErrTokenInvalid)
	case idt.IssuedAt.After(now.Add(Leeway)):
		return nil, fmt.Errorf("%w: iat is in the future", ErrTokenInvalid)
	case now.Sub(idt.IssuedAt) > MaxTokenAge:
		return nil, fmt.Errorf("%w: iat is older than %s", ErrTokenInvalid, MaxTokenAge)
	}
	if v, ok := claims["nbf"]; ok {
		nbf, isNum := v.(float64)
		if !isNum || time.Unix(int64(nbf), 0).After(now.Add(Leeway)) {
			return nil, fmt.Errorf("%w: the token is not valid yet (nbf)", ErrTokenInvalid)
		}
	}
	if idt.Subject == "" {
		return nil, fmt.Errorf("%w: no subject", ErrTokenInvalid)
	}
	return identityFrom(idt.Issuer, idt.Subject, claims, cfg), nil
}

// verifyProblem describes a go-oidc verification error without token content.
func verifyProblem(err error) string {
	var expired *gooidc.TokenExpiredError
	msg := err.Error()
	switch {
	case errors.As(err, &expired):
		return "the token has expired"
	case strings.Contains(msg, "signing algorithm"), strings.Contains(msg, "unexpected signature algorithm"):
		return "the signature algorithm is not allowed"
	case strings.Contains(msg, "signature"), strings.Contains(msg, "fetching keys"):
		return "the signature does not verify"
	case strings.Contains(msg, "issued by a different provider"):
		return "the issuer does not match"
	case strings.Contains(msg, "audience"):
		return "the audience does not include this client"
	case strings.Contains(msg, "malformed"):
		return "malformed token"
	}
	return "verification failed"
}

// hostOf returns the host (and port) of raw, or "the provider" when it has none.
func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return "the provider"
}

// statusPrefix matches the HTTP status go-oidc puts first in its errors ("404 Not
// Found: <body>").
var statusPrefix = regexp.MustCompile(`^([1-5][0-9]{2}) `)

// safeError describes a provider or transport error of what (such as "the token
// endpoint") at host from a fixed set of messages, with at most the HTTP status
// code: never response bodies, provider error codes or descriptions, or URLs.
func safeError(err error, what, host string) string {
	where := what + " at " + host
	var re *oauth2.RetrieveError
	switch {
	// go-oidc wraps read errors with %v, so the cap is also recognised by its text.
	case errors.Is(err, ErrResponseTooLarge), strings.Contains(err.Error(), ErrResponseTooLarge.Error()):
		return where + " answered more than 1 MiB"
	case errors.As(err, &re) && re.Response != nil:
		return where + " answered HTTP " + strconv.Itoa(re.Response.StatusCode)
	case errors.As(err, &re):
		return where + " refused the request"
	case errors.Is(err, context.DeadlineExceeded):
		return where + " did not answer in time"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return "could not connect to " + where
	}
	if m := statusPrefix.FindStringSubmatch(err.Error()); m != nil {
		return where + " answered HTTP " + m[1]
	}
	return where + " answered with an unusable response"
}

// truncate shortens s to n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// identityFrom extracts the identity the sign-in rules need from verified claims.
func identityFrom(issuer, subject string, claims map[string]any, cfg Config) *auth.ExternalIdentity {
	id := &auth.ExternalIdentity{Issuer: issuer, Subject: subject}
	id.Email, _ = claims["email"].(string)
	// Only the JSON boolean true counts; the string "true" does not.
	id.EmailVerified, _ = claims["email_verified"].(bool)
	id.Groups = groupsAt(claims, cfg.GroupsClaim)
	claim := cfg.UsernameClaim
	if claim == "" {
		claim = "preferred_username"
	}
	preferred, _ := claims[claim].(string)
	for _, candidate := range []string{preferred, id.Email, subject} {
		if name := cleanUsername(candidate); name != "" {
			id.Username = name
			break
		}
	}
	if id.Username == "" {
		sum := sha256.Sum256([]byte(auth.ExternalSubject(issuer, subject)))
		id.Username = "user-" + hex.EncodeToString(sum[:6])
	}
	return id
}

// groupsAt reads the groups of claims at name: a top-level claim of exactly that
// name first (namespaced claims such as Auth0's "https://app.example.com/groups"
// contain dots), else the dot path ("realm_access.roles"). The value is a string or
// a list of strings (other values are ignored), at most maxGroups.
func groupsAt(claims map[string]any, name string) []string {
	if name == "" {
		return nil
	}
	cur, ok := claims[name]
	if !ok {
		cur = claims
		for _, seg := range strings.Split(name, ".") {
			m, isMap := cur.(map[string]any)
			if !isMap {
				return nil
			}
			if cur, ok = m[seg]; !ok {
				return nil
			}
		}
	}
	switch v := cur.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	case []any:
		out := make([]string, 0, min(len(v), maxGroups))
		for _, g := range v {
			if s, ok := g.(string); ok && s != "" {
				out = append(out, s)
				if len(out) == maxGroups {
					break
				}
			}
		}
		return out
	}
	return nil
}

// cleanUsername maps s to the username charset (letters, digits and . _ @ -):
// other characters become "_", leading and trailing separators are dropped and
// the result is at most 64 bytes.
func cleanUsername(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '@' || r == '-'
		if !ok {
			r = '_'
		}
		b.WriteRune(r)
	}
	out := b.String()
	if len(out) > maxUsernameBytes {
		out = out[:maxUsernameBytes]
	}
	out = strings.Trim(out, "._-@")
	if strings.Trim(out, "_") == "" {
		return ""
	}
	return out
}

// Discovery is what the settings test reports about a provider: its endpoints,
// algorithms and keys, never secrets.
type Discovery struct {
	// Issuer is the issuer the provider reports (equal to the configured one).
	Issuer string `json:"issuer"`
	// AuthorizationEndpoint, TokenEndpoint and JWKSURI are the protocol endpoints.
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	// EndSessionEndpoint is the RP-initiated logout endpoint ("" when absent).
	EndSessionEndpoint string `json:"end_session_endpoint"`
	// SigningAlgorithms are the ID token algorithms the provider advertises.
	SigningAlgorithms []string `json:"signing_algorithms"`
	// UsableAlgorithms are those MongoRescue accepts (RS256, ES256).
	UsableAlgorithms []string `json:"usable_algorithms"`
	// PKCEMethods are the code challenge methods the provider advertises.
	PKCEMethods []string `json:"pkce_methods"`
	// Keys is the number of keys in the JWKS and KeyTypes their kty values.
	Keys     int      `json:"keys"`
	KeyTypes []string `json:"key_types"`
}

// Discover fetches the discovery document and the JWKS of issuer afresh (replacing
// the cached provider) and reports them. It fails when no accepted algorithm is
// advertised, S256 PKCE is not offered, or the JWKS has no key.
func (c *Client) Discover(ctx context.Context, issuer string) (*Discovery, error) {
	ctx, cancel := context.WithTimeout(ctx, ExchangeTimeout)
	defer cancel()
	cp, err := c.discover(ctx, issuer)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Issuer  string   `json:"issuer"`
		JWKSURI string   `json:"jwks_uri"`
		Algs    []string `json:"id_token_signing_alg_values_supported"`
	}
	if err = cp.p.Claims(&doc); err != nil {
		return nil, fmt.Errorf("%w: unreadable discovery document", ErrDiscovery)
	}
	ep := cp.p.Endpoint()
	d := &Discovery{
		Issuer: doc.Issuer, AuthorizationEndpoint: ep.AuthURL, TokenEndpoint: ep.TokenURL, JWKSURI: doc.JWKSURI,
		EndSessionEndpoint: cp.meta.EndSessionEndpoint, SigningAlgorithms: nonNil(doc.Algs), PKCEMethods: nonNil(cp.meta.CodeChallenge),
		UsableAlgorithms: []string{}, KeyTypes: []string{},
	}
	for _, a := range doc.Algs {
		if slices.Contains(SupportedAlgorithms, a) {
			d.UsableAlgorithms = append(d.UsableAlgorithms, a)
		}
	}
	if len(d.UsableAlgorithms) == 0 {
		return nil, fmt.Errorf("%w: the provider signs ID tokens with none of %s", ErrDiscovery, strings.Join(SupportedAlgorithms, ", "))
	}
	if len(d.PKCEMethods) > 0 && !slices.Contains(d.PKCEMethods, "S256") {
		return nil, fmt.Errorf("%w: the provider does not offer S256 PKCE", ErrDiscovery)
	}
	// discover checked that the jwks_uri uses https.
	if d.Keys, d.KeyTypes, err = c.fetchKeys(ctx, doc.JWKSURI); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.providers = map[string]*cachedProvider{issuer: cp}
	c.mu.Unlock()
	return d, nil
}

// fetchKeys reads the JWKS at jwksURL and returns the number of keys and their
// types.
func (c *Client) fetchKeys(ctx context.Context, jwksURL string) (int, []string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURL, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: the jwks_uri is not a URL", ErrDiscovery)
	}
	host := hostOf(jwksURL)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %s", ErrDiscovery, safeError(err, "the jwks_uri", host))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, nil, fmt.Errorf("%w: the jwks_uri at %s answered HTTP %d", ErrDiscovery, host, resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
		} `json:"keys"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return 0, nil, fmt.Errorf("%w: %s", ErrDiscovery, safeError(err, "the jwks_uri", host))
	}
	if len(set.Keys) == 0 {
		return 0, nil, fmt.Errorf("%w: the provider publishes no keys", ErrDiscovery)
	}
	types := []string{}
	for _, k := range set.Keys {
		if k.Kty != "" && !slices.Contains(types, k.Kty) {
			types = append(types, truncate(k.Kty, 16))
		}
	}
	return len(set.Keys), types, nil
}

// nonNil returns s, or an empty slice for nil.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// EndSessionURL returns the provider's RP-initiated logout URL with the client ID
// and postLogout as post_logout_redirect_uri (no id_token_hint: ID tokens are not
// kept), or "" when the provider has no end_session_endpoint.
func (c *Client) EndSessionURL(ctx context.Context, cfg Config, postLogout string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ExchangeTimeout)
	defer cancel()
	cp, err := c.provider(ctx, cfg.Issuer)
	if err != nil {
		return "", err
	}
	if cp.meta.EndSessionEndpoint == "" {
		return "", nil
	}
	u, err := url.Parse(cp.meta.EndSessionEndpoint)
	if err != nil {
		return "", nil
	}
	q := u.Query()
	q.Set("client_id", cfg.ClientID)
	if postLogout != "" {
		q.Set("post_logout_redirect_uri", postLogout)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
