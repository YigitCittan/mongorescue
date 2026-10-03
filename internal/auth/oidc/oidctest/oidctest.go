// Package oidctest is a fake OpenID Connect provider for tests: an httptest server
// with generated RSA and ECDSA keys that serves discovery, the JWKS, an
// authorization endpoint that signs the user in at once, and a token endpoint
// enforcing one-time codes, the redirect URI, client authentication and S256 PKCE.
// Hooks change the claims, the signature (algorithm, key ID, alg=none, HS256 with
// the client secret, a mismatched key type), rotate keys and make endpoints fail,
// so tests can attack the relying party. It is test support only.
package oidctest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Signing modes of ID tokens.
const (
	// SignRS256 signs with the current RSA key (the default).
	SignRS256 = "RS256"
	// SignES256 signs with the ECDSA P-256 key.
	SignES256 = "ES256"
	// SignNone produces an unsigned token with alg "none".
	SignNone = "none"
	// SignHS256 signs with HMAC-SHA256 keyed with the client secret.
	SignHS256 = "HS256"
	// SignMismatch claims ES256 with the RSA key's ID but signs with RSA.
	SignMismatch = "mismatch"
	// SignUnknownKey signs with an RSA key the JWKS does not publish.
	SignUnknownKey = "unknown_key"
)

// Client is the client registered at the provider.
type Client struct {
	// ID is the client_id.
	ID string
	// Secret is the client secret ("" for a public client).
	Secret string
	// RedirectURL is the only accepted redirect_uri.
	RedirectURL string
}

// signingKey is an RSA key with its key ID.
type signingKey struct {
	kid string
	key *rsa.PrivateKey
}

// Provider is a running fake provider. Its fields may be changed between
// requests; the methods are safe for concurrent use.
type Provider struct {
	// Server is the underlying test server.
	Server *httptest.Server
	// Issuer is the issuer URL (the server URL).
	Issuer string

	mu       sync.Mutex
	client   Client
	rsaKeys  []signingKey // the last one signs
	ecKey    *ecdsa.PrivateKey
	strayKey *rsa.PrivateKey
	codes    map[string]*grant
	now      func() time.Time

	// subject, claims and hooks
	subject      string
	claims       map[string]any
	claimsHook   func(map[string]any)
	signMode     string
	kidOverride  *string
	authorizeErr string
	tokenErr     string
	omitIDToken  bool
	discoveryIss string

	// counters
	jwksFetches  int
	tokenCalls   int
	lastAuthForm url.Values
}

// grant is an issued authorization code.
type grant struct {
	clientID, redirectURI, challenge, nonce string
	used                                    bool
}

// New starts a provider for client, stopped when the test ends. The signed-in user
// has the subject "user-1" and the claims set with SetClaims.
func New(t testing.TB, client Client) *Provider {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	stray, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{
		client: client, rsaKeys: []signingKey{{kid: "rsa-1", key: rsaKey}}, ecKey: ecKey, strayKey: stray,
		codes: map[string]*grant{}, now: time.Now, subject: "user-1", claims: map[string]any{}, signMode: SignRS256,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.handleDiscovery)
	mux.HandleFunc("GET /jwks", p.handleJWKS)
	mux.HandleFunc("GET /authorize", p.handleAuthorize)
	mux.HandleFunc("POST /token", p.handleToken)
	mux.HandleFunc("GET /logout", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	p.Server = httptest.NewServer(mux)
	p.Issuer = p.Server.URL
	t.Cleanup(p.Server.Close)
	return p
}

// SetClock sets the clock iat, exp and code lifetimes use.
func (p *Provider) SetClock(now func() time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.now = now
}

// SetSubject sets the sub claim of the next tokens.
func (p *Provider) SetSubject(sub string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subject = sub
}

// SetClaims replaces the extra claims (email, groups, ...) of the next tokens.
func (p *Provider) SetClaims(claims map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.claims = claims
}

// SetClaimsHook lets fn change every token's claims last (iss, aud, exp, nonce...).
func (p *Provider) SetClaimsHook(fn func(map[string]any)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.claimsHook = fn
}

// SetSigning selects how the next tokens are signed (Sign* constants).
func (p *Provider) SetSigning(mode string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.signMode = mode
}

// SetKeyID overrides the kid header of the next tokens (nil restores it).
func (p *Provider) SetKeyID(kid *string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.kidOverride = kid
}

// SetAuthorizeError makes the authorization endpoint redirect back with error=code.
func (p *Provider) SetAuthorizeError(code string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.authorizeErr = code
}

// SetTokenError makes the token endpoint answer 400 with error=code.
func (p *Provider) SetTokenError(code string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tokenErr = code
}

// OmitIDToken makes the token endpoint answer without an id_token.
func (p *Provider) OmitIDToken(omit bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.omitIDToken = omit
}

// SetDiscoveryIssuer makes discovery report another issuer ("" restores it).
func (p *Provider) SetDiscoveryIssuer(iss string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.discoveryIss = iss
}

// Rotate adds a new RSA key that signs from now on; with dropOld the JWKS stops
// publishing the previous keys.
func (p *Provider) Rotate(t testing.TB, dropOld bool) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	next := signingKey{kid: "rsa-" + strconv.Itoa(len(p.rsaKeys)+1), key: k}
	if dropOld {
		p.rsaKeys = []signingKey{next}
	} else {
		p.rsaKeys = append(p.rsaKeys, next)
	}
}

// JWKSFetches returns how often the JWKS was fetched.
func (p *Provider) JWKSFetches() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.jwksFetches
}

// TokenCalls returns how often the token endpoint was called.
func (p *Provider) TokenCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tokenCalls
}

// LastAuthorizeRequest returns the query of the last authorization request.
func (p *Provider) LastAuthorizeRequest() url.Values {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastAuthForm
}

func (p *Provider) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	iss := p.Issuer
	if p.discoveryIss != "" {
		iss = p.discoveryIss
	}
	p.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                iss,
		"authorization_endpoint":                p.Issuer + "/authorize",
		"token_endpoint":                        p.Issuer + "/token",
		"jwks_uri":                              p.Issuer + "/jwks",
		"end_session_endpoint":                  p.Issuer + "/logout",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256", "ES256"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post", "none"},
	})
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (p *Provider) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	p.jwksFetches++
	keys := make([]map[string]any, 0, len(p.rsaKeys)+1)
	for _, k := range p.rsaKeys {
		keys = append(keys, map[string]any{
			"kty": "RSA", "kid": k.kid, "use": "sig", "alg": "RS256",
			"n": b64(k.key.N.Bytes()), "e": b64(big.NewInt(int64(k.key.E)).Bytes()),
		})
	}
	keys = append(keys, map[string]any{
		"kty": "EC", "kid": "ec-1", "use": "sig", "alg": "ES256", "crv": "P-256",
		"x": b64(pad32(p.ecKey.X.Bytes())), "y": b64(pad32(p.ecKey.Y.Bytes())),
	})
	p.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

// pad32 left-pads b to 32 bytes.
func pad32(b []byte) []byte {
	if len(b) >= 32 {
		return b
	}
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

func (p *Provider) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p.mu.Lock()
	p.lastAuthForm = q
	client, authErr := p.client, p.authorizeErr
	p.mu.Unlock()
	redirect := q.Get("redirect_uri")
	if q.Get("client_id") != client.ID || redirect != client.RedirectURL {
		http.Error(w, "unknown client or redirect_uri", http.StatusBadRequest)
		return
	}
	// Redirect to the registered URL, never to the request's copy of it.
	back, err := url.Parse(client.RedirectURL)
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	out := url.Values{"state": {q.Get("state")}}
	switch {
	case authErr != "":
		out.Set("error", authErr)
		out.Set("error_description", "the user refused <script>")
	case q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "":
		out.Set("error", "invalid_request")
	default:
		code := randomString()
		p.mu.Lock()
		p.codes[code] = &grant{clientID: client.ID, redirectURI: redirect, challenge: q.Get("code_challenge"), nonce: q.Get("nonce")}
		p.mu.Unlock()
		out.Set("code", code)
	}
	back.RawQuery = out.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func randomString() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return b64(b)
}

func tokenError(w http.ResponseWriter, code string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": code, "error_description": "refused by the fake provider"})
}

func (p *Provider) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		tokenError(w, "invalid_request")
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tokenCalls++
	if p.tokenErr != "" {
		tokenError(w, p.tokenErr)
		return
	}
	// Client authentication: HTTP Basic or the form.
	id, secret, basic := r.BasicAuth()
	if basic {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != p.client.ID || subtle.ConstantTimeCompare([]byte(secret), []byte(p.client.Secret)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	g, ok := p.codes[r.PostForm.Get("code")]
	switch {
	case r.PostForm.Get("grant_type") != "authorization_code", !ok, g.used, g.clientID != id,
		g.redirectURI != r.PostForm.Get("redirect_uri"):
		tokenError(w, "invalid_grant")
		return
	}
	g.used = true
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if r.PostForm.Get("code_verifier") == "" || b64(sum[:]) != g.challenge {
		tokenError(w, "invalid_grant")
		return
	}
	resp := map[string]any{
		"access_token": "fake-access-token-" + randomString(), "token_type": "Bearer", "expires_in": 300,
		"refresh_token": "fake-refresh-token-" + randomString(),
	}
	if !p.omitIDToken {
		resp["id_token"] = p.signLocked(p.claimsLocked(g.nonce))
	}
	writeJSON(w, http.StatusOK, resp)
}

// claimsLocked builds the claims of a token for nonce. Caller holds p.mu.
func (p *Provider) claimsLocked(nonce string) map[string]any {
	now := p.now()
	c := map[string]any{
		"iss": p.Issuer, "sub": p.subject, "aud": p.client.ID, "nonce": nonce,
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
	}
	for k, v := range p.claims {
		c[k] = v
	}
	if p.claimsHook != nil {
		p.claimsHook(c)
	}
	return c
}

// IDToken returns a token for nonce signed the current way, for direct tests of
// the verifier.
func (p *Provider) IDToken(nonce string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.signLocked(p.claimsLocked(nonce))
}

// signLocked serialises claims as a JWT in the current signing mode. Caller holds
// p.mu.
func (p *Provider) signLocked(claims map[string]any) string {
	cur := p.rsaKeys[len(p.rsaKeys)-1]
	header := map[string]any{"typ": "JWT"}
	alg, kid := "RS256", cur.kid
	switch p.signMode {
	case SignES256:
		alg, kid = "ES256", "ec-1"
	case SignNone:
		alg = "none"
	case SignHS256:
		alg = "HS256"
	case SignMismatch:
		alg = "ES256"
	case SignUnknownKey:
		kid = "stray"
	}
	if p.kidOverride != nil {
		kid = *p.kidOverride
	}
	header["alg"] = alg
	if kid != "" {
		header["kid"] = kid
	}
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	input := b64(h) + "." + b64(c)
	digest := sha256.Sum256([]byte(input))
	var sig []byte
	switch p.signMode {
	case SignES256:
		r, s, err := ecdsa.Sign(rand.Reader, p.ecKey, digest[:])
		if err != nil {
			panic(err)
		}
		sig = append(pad32(r.Bytes()), pad32(s.Bytes())...)
	case SignNone:
		sig = nil
	case SignHS256:
		m := hmac.New(sha256.New, []byte(p.client.Secret))
		m.Write([]byte(input))
		sig = m.Sum(nil)
	case SignUnknownKey:
		sig = mustRSA(p.strayKey, digest[:])
	default:
		sig = mustRSA(cur.key, digest[:])
	}
	return input + "." + b64(sig)
}

func mustRSA(k *rsa.PrivateKey, digest []byte) []byte {
	sig, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, digest)
	if err != nil {
		panic(fmt.Sprintf("oidctest: sign: %v", err))
	}
	return sig
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Authorize performs the browser's visit to authURL (the provider's authorization
// endpoint with its query) and returns the callback URL the provider redirects to.
func (p *Provider) Authorize(t testing.TB, authURL string) *url.URL {
	t.Helper()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(authURL) //nolint:noctx // test helper against a local server
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: %d; want 302", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return loc
}
