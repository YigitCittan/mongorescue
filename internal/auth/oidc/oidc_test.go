package oidc_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/auth/oidc"
	"github.com/yigitcittan/mongorescue/internal/auth/oidc/oidctest"
)

const redirectURL = "https://backup.example.com/auth/oidc/callback"

type rp struct {
	p   *oidctest.Provider
	c   *oidc.Client
	cfg oidc.Config
}

func newRP(t *testing.T, secret string) *rp {
	t.Helper()
	p := oidctest.New(t, oidctest.Client{ID: "mongorescue", Secret: secret, RedirectURL: redirectURL})
	return &rp{p: p, c: oidc.NewClient(&http.Client{Timeout: 5 * time.Second}), cfg: oidc.Config{
		Issuer: p.Issuer, ClientID: "mongorescue", ClientSecret: secret, RedirectURL: redirectURL,
		Scopes: []string{"email"}, UsernameClaim: "preferred_username", GroupsClaim: "groups",
	}}
}

// start begins a sign-in and returns its request and the code the provider issued.
func (r *rp) start(t *testing.T) (oidc.AuthRequest, string) {
	t.Helper()
	req, err := oidc.NewAuthRequest()
	if err != nil {
		t.Fatal(err)
	}
	u, err := r.c.AuthCodeURL(context.Background(), r.cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	cb := r.p.Authorize(t, u)
	if cb.Query().Get("state") != req.State {
		t.Fatalf("state not echoed: %s", cb)
	}
	return req, cb.Query().Get("code")
}

func (r *rp) login(t *testing.T) (*auth.ExternalIdentity, error) {
	t.Helper()
	req, code := r.start(t)
	return r.c.Exchange(context.Background(), r.cfg, code, req)
}

func TestAuthorizationRequestUsesPKCEAndANonce(t *testing.T) {
	r := newRP(t, "s3cret")
	req, _ := r.start(t)
	q := r.p.LastAuthorizeRequest()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("code_challenge") == req.Verifier ||
		q.Get("nonce") != req.Nonce || q.Get("response_type") != "code" || !strings.Contains(q.Get("scope"), "openid") ||
		q.Get("redirect_uri") != redirectURL || q.Get("response_mode") != "" {
		t.Fatalf("authorization request = %v", q)
	}
	if len(req.State) < 43 || len(req.Nonce) < 43 || len(req.Verifier) < 43 || req.State == req.Nonce {
		t.Fatalf("weak request values %+v", req)
	}
}

func TestExchangeExtractsTheIdentity(t *testing.T) {
	for _, secret := range []string{"s3cret", ""} { // confidential and public clients
		r := newRP(t, secret)
		r.p.SetClaims(map[string]any{
			"preferred_username": "Jane Doe", "email": "jane@corp.com", "email_verified": true,
			"groups": []any{"ops", 7, "dba"},
		})
		id, err := r.login(t)
		if err != nil {
			t.Fatalf("secret %q: %v", secret, err)
		}
		if id.Issuer != r.p.Issuer || id.Subject != "user-1" || id.Username != "Jane_Doe" || id.Email != "jane@corp.com" ||
			!id.EmailVerified || !slices.Equal(id.Groups, []string{"ops", "dba"}) {
			t.Fatalf("identity = %+v", id)
		}
	}
}

func TestClaimExtraction(t *testing.T) {
	r := newRP(t, "s3cret")
	r.cfg.GroupsClaim = "realm_access.roles"
	cases := []struct {
		name   string
		claims map[string]any
		check  func(*auth.ExternalIdentity) bool
	}{
		{"dot path groups", map[string]any{"realm_access": map[string]any{"roles": []any{"backup-admins"}}},
			func(id *auth.ExternalIdentity) bool { return slices.Equal(id.Groups, []string{"backup-admins"}) }},
		{"a single group string", map[string]any{"realm_access": map[string]any{"roles": "one"}},
			func(id *auth.ExternalIdentity) bool { return slices.Equal(id.Groups, []string{"one"}) }},
		{"groups of the wrong type", map[string]any{"realm_access": map[string]any{"roles": map[string]any{"x": 1}}},
			func(id *auth.ExternalIdentity) bool { return len(id.Groups) == 0 }},
		{"username falls back to email", map[string]any{"email": "j.doe@corp.com"},
			func(id *auth.ExternalIdentity) bool { return id.Username == "j.doe@corp.com" }},
		{"then to sub", map[string]any{},
			func(id *auth.ExternalIdentity) bool { return id.Username == "user-1" }},
		{"email_verified as a string is not verified", map[string]any{"email": "a@corp.com", "email_verified": "true"},
			func(id *auth.ExternalIdentity) bool { return !id.EmailVerified }},
		{"email_verified missing", map[string]any{"email": "a@corp.com"},
			func(id *auth.ExternalIdentity) bool { return !id.EmailVerified }},
	}
	for _, c := range cases {
		r.p.SetClaims(c.claims)
		id, err := r.login(t)
		if err != nil || !c.check(id) {
			t.Errorf("%s: %+v, %v", c.name, id, err)
		}
	}
	// A subject without any usable character still gets a valid name.
	r.p.SetClaims(map[string]any{})
	r.p.SetSubject("üïö€")
	id, err := r.login(t)
	if err != nil || auth.ValidateUsername(id.Username) != nil || !strings.HasPrefix(id.Username, "user-") {
		t.Errorf("unusable subject: %+v, %v", id, err)
	}
}

// TestTokenAttacks is the verifier part of the security test list: every forged,
// misdirected or stale ID token is refused with ErrTokenInvalid.
func TestTokenAttacks(t *testing.T) {
	r := newRP(t, "s3cret")
	unknown := "kid-nobody-has"
	cases := []struct {
		name  string
		setup func()
	}{
		{"wrong iss", func() { r.p.SetClaimsHook(func(c map[string]any) { c["iss"] = "https://evil.example.com" }) }},
		{"wrong aud", func() { r.p.SetClaimsHook(func(c map[string]any) { c["aud"] = "another-client" }) }},
		{"several aud without azp", func() {
			r.p.SetClaimsHook(func(c map[string]any) { c["aud"] = []string{"mongorescue", "another-client"} })
		}},
		{"several aud with a wrong azp", func() {
			r.p.SetClaimsHook(func(c map[string]any) { c["aud"] = []string{"mongorescue", "x"}; c["azp"] = "x" })
		}},
		{"a wrong azp", func() { r.p.SetClaimsHook(func(c map[string]any) { c["azp"] = "another-client" }) }},
		{"a non-string azp", func() { r.p.SetClaimsHook(func(c map[string]any) { c["azp"] = 1 }) }},
		{"alg none", func() { r.p.SetSigning(oidctest.SignNone) }},
		{"HS256 with the client secret", func() { r.p.SetSigning(oidctest.SignHS256) }},
		{"alg and key type mismatch", func() { r.p.SetSigning(oidctest.SignMismatch) }},
		{"a key the provider does not publish", func() { r.p.SetSigning(oidctest.SignUnknownKey) }},
		{"an unknown kid", func() { r.p.SetKeyID(&unknown) }},
		{"expired", func() {
			r.p.SetClaimsHook(func(c map[string]any) {
				c["exp"] = time.Now().Add(-2 * oidc.Leeway).Unix()
				c["iat"] = time.Now().Add(-5 * time.Minute).Unix()
			})
		}},
		{"iat in the future", func() {
			r.p.SetClaimsHook(func(c map[string]any) { c["iat"] = time.Now().Add(5 * time.Minute).Unix() })
		}},
		{"iat too old", func() {
			r.p.SetClaimsHook(func(c map[string]any) { c["iat"] = time.Now().Add(-oidc.MaxTokenAge - time.Minute).Unix() })
		}},
		{"no iat", func() { r.p.SetClaimsHook(func(c map[string]any) { delete(c, "iat") }) }},
		{"nbf in the future", func() {
			r.p.SetClaimsHook(func(c map[string]any) { c["nbf"] = time.Now().Add(3 * time.Minute).Unix() })
		}},
		{"a wrong nonce", func() { r.p.SetClaimsHook(func(c map[string]any) { c["nonce"] = "attacker-nonce" }) }},
		{"no nonce", func() { r.p.SetClaimsHook(func(c map[string]any) { delete(c, "nonce") }) }},
		{"no subject", func() { r.p.SetClaimsHook(func(c map[string]any) { c["sub"] = "" }) }},
		{"no id_token", func() { r.p.OmitIDToken(true) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r.p.SetClaimsHook(nil)
			r.p.SetSigning(oidctest.SignRS256)
			r.p.SetKeyID(nil)
			r.p.OmitIDToken(false)
			c.setup()
			if id, err := r.login(t); !errors.Is(err, oidc.ErrTokenInvalid) {
				t.Fatalf("login = %+v, %v; want ErrTokenInvalid", id, err)
			}
		})
	}
	// Small skews within the leeway pass.
	r.p.SetClaimsHook(func(c map[string]any) {
		c["iat"] = time.Now().Add(30 * time.Second).Unix()
		c["nbf"] = time.Now().Add(30 * time.Second).Unix()
	})
	r.p.SetSigning(oidctest.SignRS256)
	r.p.SetKeyID(nil)
	r.p.OmitIDToken(false)
	if _, err := r.login(t); err != nil {
		t.Fatalf("skew within the leeway: %v", err)
	}
	// azp equal to the client is fine with several audiences.
	r.p.SetClaimsHook(func(c map[string]any) { c["aud"] = []string{"mongorescue", "x"}; c["azp"] = "mongorescue" })
	if _, err := r.login(t); err != nil {
		t.Fatalf("several aud with azp: %v", err)
	}
}

func TestES256TokensAreAccepted(t *testing.T) {
	r := newRP(t, "s3cret")
	r.p.SetSigning(oidctest.SignES256)
	if _, err := r.login(t); err != nil {
		t.Fatal(err)
	}
}

func TestKeyRotationRefetchesTheKeys(t *testing.T) {
	r := newRP(t, "s3cret")
	if _, err := r.login(t); err != nil {
		t.Fatal(err)
	}
	fetches := r.p.JWKSFetches()
	if _, err := r.login(t); err != nil || r.p.JWKSFetches() != fetches {
		t.Fatalf("cached keys: %v; fetched %d more times", err, r.p.JWKSFetches()-fetches)
	}
	r.p.Rotate(t, true)
	if _, err := r.login(t); err != nil {
		t.Fatalf("after rotation: %v", err)
	}
	if r.p.JWKSFetches() != fetches+1 {
		t.Fatalf("the unknown kid fetched the keys %d times; want once", r.p.JWKSFetches()-fetches)
	}
}

func TestExchangeRefusals(t *testing.T) {
	r := newRP(t, "s3cret")
	ctx := context.Background()

	// A replayed code is refused by the provider.
	req, code := r.start(t)
	if _, err := r.c.Exchange(ctx, r.cfg, code, req); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.Exchange(ctx, r.cfg, code, req); !errors.Is(err, oidc.ErrExchange) {
		t.Errorf("replayed code = %v; want ErrExchange", err)
	}
	// A missing or wrong verifier.
	req, code = r.start(t)
	missing := req
	missing.Verifier = ""
	if _, err := r.c.Exchange(ctx, r.cfg, code, missing); !errors.Is(err, oidc.ErrExchange) {
		t.Errorf("missing verifier = %v; want ErrExchange", err)
	}
	other, _ := oidc.NewAuthRequest()
	wrong := req
	wrong.Verifier = other.Verifier
	if _, err := r.c.Exchange(ctx, r.cfg, code, wrong); !errors.Is(err, oidc.ErrExchange) {
		t.Errorf("wrong verifier = %v; want ErrExchange", err)
	}
	// A wrong client secret.
	req, code = r.start(t)
	bad := r.cfg
	bad.ClientSecret = "wrong"
	if _, err := r.c.Exchange(ctx, bad, code, req); !errors.Is(err, oidc.ErrExchange) {
		t.Errorf("wrong secret = %v; want ErrExchange", err)
	}
	// Provider errors never carry the response body.
	r.p.SetTokenError("invalid_grant")
	req, code = r.start(t)
	_, err := r.c.Exchange(ctx, r.cfg, code, req)
	if !errors.Is(err, oidc.ErrExchange) || strings.Contains(err.Error(), "refused by the fake provider") {
		t.Errorf("token error = %v", err)
	}
}

func TestDiscoveryChecks(t *testing.T) {
	r := newRP(t, "s3cret")
	d, err := r.c.Discover(context.Background(), r.p.Issuer)
	if err != nil {
		t.Fatal(err)
	}
	if d.Issuer != r.p.Issuer || !strings.HasSuffix(d.TokenEndpoint, "/token") || d.Keys != 2 ||
		!slices.Equal(d.UsableAlgorithms, []string{"RS256", "ES256"}) || !slices.Contains(d.KeyTypes, "EC") ||
		!strings.HasSuffix(d.EndSessionEndpoint, "/logout") {
		t.Fatalf("discovery = %+v", d)
	}
	// Discovery must report exactly the configured issuer.
	r.p.SetDiscoveryIssuer("https://other.example.com")
	if _, err = r.c.Discover(context.Background(), r.p.Issuer); !errors.Is(err, oidc.ErrDiscovery) {
		t.Errorf("issuer mismatch = %v; want ErrDiscovery", err)
	}
	// Responses beyond MaxResponseBytes are refused.
	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"issuer":"`+strings.Repeat("a", oidc.MaxResponseBytes)+`"}`)
	}))
	t.Cleanup(huge.Close)
	if _, err = r.c.Discover(context.Background(), huge.URL); !errors.Is(err, oidc.ErrDiscovery) ||
		!strings.Contains(err.Error(), "too large") {
		t.Errorf("huge discovery = %v", err)
	}
}

func TestEndSessionURL(t *testing.T) {
	r := newRP(t, "s3cret")
	u, err := r.c.EndSessionURL(context.Background(), r.cfg, "https://backup.example.com/")
	if err != nil || !strings.HasPrefix(u, r.p.Issuer+"/logout?") || !strings.Contains(u, "client_id=mongorescue") ||
		!strings.Contains(u, "post_logout_redirect_uri=https%3A%2F%2Fbackup.example.com%2F") || strings.Contains(u, "id_token_hint") {
		t.Fatalf("EndSessionURL = %q, %v", u, err)
	}
}
