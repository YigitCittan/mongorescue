package server

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestSessionCookieHelperSecurePolicy checks the single cookie builder directly: the
// issuing and the clearing cookie carry the same attributes, and Secure is set exactly
// when the request is TLS (directly or via a trusted https proxy) or the policy forces
// it.
func TestSessionCookieHelperSecurePolicy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		policy  settings.CookiePolicy
		trust   bool
		tls     bool
		headers map[string]string
		secure  bool
	}{
		{"plain http", settings.CookiesAuto, false, false, nil, false},
		{"direct tls", settings.CookiesAuto, false, true, nil, true},
		{"trusted https proxy", settings.CookiesAuto, true, false, map[string]string{"X-Forwarded-Proto": "https"}, true},
		{"trusted http proxy", settings.CookiesAuto, true, false, map[string]string{"X-Forwarded-Proto": "http"}, false},
		{"untrusted https header", settings.CookiesAuto, false, false, map[string]string{"X-Forwarded-Proto": "https"}, false},
		{"always over http", settings.CookiesAlways, false, false, nil, true},
		{"never over tls", settings.CookiesNever, false, true, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := storetest.New(t)
			cfg := newTestConfig()
			cfg.Security.SecureCookies = tc.policy
			cfg.Security.TrustProxyHeaders = tc.trust
			s := NewServer(bootConfig(), st, nil, nil, nil, nil, nil, nil, WithSettings(newTestSettings(t, st, cfg.Security)))
			req := httptest.NewRequest("POST", "/api/v1/auth/login", nil)
			if tc.tls {
				req.TLS = &tls.ConnectionState{}
			}
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			expires := time.Now().Add(time.Hour)
			issued := s.sessionCookie(req, "token", expires, 3600)
			cleared := s.sessionCookie(req, "", time.Time{}, -1)
			for what, c := range map[string]*http.Cookie{"issued": issued, "cleared": cleared} {
				if c.Name != SessionCookieName || c.Path != "/" || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Domain != "" {
					t.Errorf("%s cookie = %+v; want %s, Path=/, HttpOnly, SameSite=Strict, host-only", what, c, SessionCookieName)
				}
				if c.Secure != tc.secure {
					t.Errorf("%s cookie Secure = %v; want %v", what, c.Secure, tc.secure)
				}
			}
			if issued.Value != "token" || issued.MaxAge != 3600 || !issued.Expires.Equal(expires) {
				t.Errorf("issued cookie = %+v", issued)
			}
			if cleared.Value != "" || cleared.MaxAge != -1 || !cleared.Expires.IsZero() {
				t.Errorf("cleared cookie = %+v", cleared)
			}
		})
	}
}
