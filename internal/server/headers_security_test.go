package server

import (
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/web"
)

// TestSecurityHeadersOnEveryResponse checks the browser security headers on the
// dashboard, on API successes and errors, on refused requests and on CORS preflights.
func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	h, _ := newChainServer(t, func(c *testConfig) {
		c.APIKey = "headers-test-key"
		c.Security.CORSOrigins = []string{"https://tools.example.com"}
	})
	key := map[string]string{"X-API-Key": "headers-test-key"}
	for _, tc := range []struct {
		name, method, path string
		headers            map[string]string
		api                bool
	}{
		{"dashboard", "GET", "/", nil, false},
		{"dashboard asset", "GET", "/index.html", nil, false},
		{"missing asset", "GET", "/nope.js", nil, false},
		{"public api", "GET", "/api/v1/health", nil, true},
		{"setup status", "GET", "/api/v1/setup/status", nil, true},
		{"authenticated api", "GET", "/api/v1/jobs", key, true},
		{"unauthenticated api", "GET", "/api/v1/jobs", nil, true},
		// Unknown API paths fall through to the dashboard's file server, whose 404
		// drops Cache-Control; its plain-text body carries no data.
		{"unknown api route", "GET", "/api/v1/nope", key, false},
		{"refused public write", "POST", "/api/v1/auth/login", nil, true},
		{"cors preflight", "OPTIONS", "/api/v1/jobs", map[string]string{"Origin": "https://tools.example.com"}, true},
		{"mcp without key", "POST", MCPPath, nil, true},
		{"metrics without key", "GET", "/metrics", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(h, tc.method, tc.path, nil, tc.headers)
			got := rec.Header()
			want := map[string]string{
				"Content-Security-Policy": contentSecurityPolicy,
				"X-Content-Type-Options":  "nosniff",
				"X-Frame-Options":         "DENY",
				"Referrer-Policy":         "no-referrer",
			}
			if tc.api {
				want["Cache-Control"] = "no-store"
			}
			for name, v := range want {
				if got.Get(name) != v {
					t.Errorf("%s %s (%d): %s = %q; want %q", tc.method, tc.path, rec.Code, name, got.Get(name), v)
				}
			}
			if !tc.api && got.Get("Cache-Control") == "no-store" {
				t.Errorf("%s %s: static assets must stay cacheable", tc.method, tc.path)
			}
		})
	}
}

// TestContentSecurityPolicyIsStrict pins the directives that make the policy useful:
// no inline or eval'd script, no plugins, no framing and no foreign origins.
func TestContentSecurityPolicyIsStrict(t *testing.T) {
	directives := map[string]string{}
	for _, d := range strings.Split(contentSecurityPolicy, ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(d), " ")
		directives[name] = value
	}
	for name, want := range map[string]string{
		"default-src":     "'self'",
		"script-src":      "'self'",
		"style-src":       "'self'",
		"object-src":      "'none'",
		"base-uri":        "'none'",
		"frame-ancestors": "'none'",
		"connect-src":     "'self'",
	} {
		if directives[name] != want {
			t.Errorf("CSP %s = %q; want %q", name, directives[name], want)
		}
	}
	for _, banned := range []string{"'unsafe-inline'", "'unsafe-eval'", "*", "http:", "https:"} {
		for name, v := range directives {
			for _, token := range strings.Fields(v) {
				if token == banned {
					t.Errorf("CSP %s allows %s", name, banned)
				}
			}
		}
	}
}

// TestDashboardIsCompatibleWithTheCSP checks the embedded dashboard for markup the
// policy would block (and that would be an XSS sink if it were allowed): inline
// scripts, inline event handlers, style attributes, javascript: URLs and third-party
// resources.
func TestDashboardIsCompatibleWithTheCSP(t *testing.T) {
	sub, err := web.GetSubFS()
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		what string
		re   *regexp.Regexp
	}{
		{"inline script", regexp.MustCompile(`(?i)<script(\s[^>]*)?>\s*[^<\s]`)},
		{"script without a same-origin src", regexp.MustCompile(`(?i)<script[^>]*\ssrc="(https?:)?//`)},
		{"inline event handler", regexp.MustCompile(`(?i)<[^>]+\son[a-z]+\s*=`)},
		{"style attribute", regexp.MustCompile(`(?i)<[^>]+\sstyle\s*=`)},
		{"style element", regexp.MustCompile(`(?i)<style[\s>]`)},
		{"javascript: URL", regexp.MustCompile(`(?i)javascript:`)},
		{"third-party stylesheet", regexp.MustCompile(`(?i)<link[^>]*href="(https?:)?//`)},
		{"eval", regexp.MustCompile(`\beval\(|new Function\(`)},
	}
	found := 0
	err = fs.WalkDir(sub, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || (!strings.HasSuffix(path, ".html") && !strings.HasSuffix(path, ".js")) {
			return err
		}
		found++
		raw, err := fs.ReadFile(sub, path)
		if err != nil {
			return err
		}
		for _, c := range checks {
			if c.what == "eval" && !strings.HasSuffix(path, ".js") {
				continue
			}
			if loc := c.re.FindIndex(raw); loc != nil {
				t.Errorf("%s: %s at byte %d: %q", path, c.what, loc[0], raw[loc[0]:min(loc[1]+40, len(raw))])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found < 2 {
		t.Fatalf("checked %d dashboard files; the embedded assets are missing", found)
	}
}

// TestAPIResponsesAreNotSniffable checks that API responses declare JSON, so that
// nosniff keeps browsers from rendering them as HTML.
func TestAPIResponsesAreNotSniffable(t *testing.T) {
	h, _ := newChainServer(t, nil)
	for _, path := range []string{"/api/v1/health", "/api/v1/jobs", "/api/v1/nope/<script>"} {
		rec := serve(h, http.MethodGet, path, nil, nil)
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") && rec.Code != http.StatusNotFound {
			t.Errorf("GET %s (%d): Content-Type %q; want application/json", path, rec.Code, ct)
		}
	}
}
