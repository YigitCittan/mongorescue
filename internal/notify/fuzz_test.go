package notify

import (
	"net/url"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/redact"
)

// fuzzWebhookURLs are endpoints from the table tests plus known-tricky shapes.
var fuzzWebhookURLs = []string{
	slackURL, discordURL, tokenURL,
	"https://user:userPASS@h.example.com", "https://h.example.com/#frag", "https://h.example.com",
	"https://h.example.com/", "http://10.0.0.1:9000", "::not a url", "", "https://h.example.com/?",
	"HTTPS://H.example.com/Secret", "https://h.example.com/%2e%2e/x", "https://[::1]:8080/x",
	"https://h.example.com\\@evil.example/", "https://h.example.com/a\r\nX-Evil: 1", "ftp://h/x",
	"https://:pass@/x", "https://h.example.com:99999/x", "//h.example.com/x", "https://h/\x7f",
}

// FuzzValidateWebhook checks that webhook validation never panics, that an accepted
// configuration has an http(s) URL with a host and no header injection, and that
// RedactEndpoint of an accepted URL keeps nothing but the origin of a URL with a
// path, query, fragment or userinfo.
func FuzzValidateWebhook(f *testing.F) {
	for _, u := range fuzzWebhookURLs {
		f.Add(u, "X-Token", "value")
	}
	f.Add("https://h.example.com/x", "Host", "evil")
	f.Add("https://h.example.com/x", "X-Ok", "a\nb")
	f.Add("https://h.example.com/x", "Bad Name", "v")
	f.Fuzz(func(t *testing.T, rawURL, headerName, headerValue string) {
		w := &WebhookConfig{URL: rawURL, Headers: map[string]string{headerName: headerValue}}
		if validateWebhook(w) != nil {
			return
		}
		u, err := url.Parse(rawURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			t.Fatalf("accepted webhook URL %q: %v", rawURL, err)
		}
		if hasCRLF(rawURL) || hasCRLF(headerValue) || !headerNamePattern.MatchString(headerName) {
			t.Fatalf("accepted header injection: %q %q: %q", rawURL, headerName, headerValue)
		}

		red := RedactEndpoint(rawURL)
		origin := u.Scheme + "://" + u.Host
		if red == rawURL {
			if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
				t.Fatalf("RedactEndpoint(%q) kept a path, query, fragment or userinfo", rawURL)
			}
			return
		}
		if red != origin+"/"+redact.Mask {
			t.Fatalf("RedactEndpoint(%q) = %q; want %q", rawURL, red, origin+"/"+redact.Mask)
		}
		if p, ok := u.User.Password(); ok && p != "" && strings.Contains(red, p) && !strings.Contains(origin+"/"+redact.Mask, p) {
			t.Fatalf("RedactEndpoint(%q) = %q leaks the password", rawURL, red)
		}
	})
}

// FuzzValidateEmail checks that an accepted SMTP configuration carries no CR or LF
// in any field that reaches the SMTP dialogue or message headers.
func FuzzValidateEmail(f *testing.F) {
	f.Add("smtp.example.com", 587, "user", "pass", "a@example.com", "b@example.com")
	f.Add("smtp.example.com", 25, "", "", "Ops <a@example.com>", "\"B\" <b@example.com>")
	f.Add("smtp.example.com\r\nRCPT TO:<x@evil>", 25, "", "", "a@example.com", "b@example.com")
	f.Add("smtp.example.com", 25, "", "", "a@example.com\r\nBcc: x@evil", "b@example.com")
	f.Add("smtp.example.com", 25, "", "p", "a@example.com", "b@example.com")
	f.Add("h/x", 0, "", "", "=?utf-8?q?a=0D=0A?= <a@example.com>", "b@example.com")
	f.Fuzz(func(t *testing.T, host string, port int, username, password, from, to string) {
		for _, security := range []string{SecurityNone, SecuritySTARTTLS, SecurityTLS} {
			e := &EmailConfig{Host: host, Port: port, Username: username, Password: password, From: from, To: []string{to}, Security: security}
			if validateEmail(e) != nil {
				continue
			}
			for _, v := range []string{host, username, password, from, to} {
				if hasCRLF(v) {
					t.Fatalf("accepted CR/LF in %+v", e)
				}
			}
			for _, addr := range []string{from, to} {
				parsed, err := parseAddress(addr)
				if err != nil || hasCRLF(parsed) {
					t.Fatalf("accepted address %q: %q, %v", addr, parsed, err)
				}
			}
			if port < 1 || port > 65535 || strings.ContainsAny(host, " /\\@") || (password != "" && username == "") {
				t.Fatalf("accepted invalid SMTP settings %+v", e)
			}
		}
	})
}
