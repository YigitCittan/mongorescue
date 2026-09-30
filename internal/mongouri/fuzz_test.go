package mongouri

import (
	"net/url"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/redact"
)

// fuzzSeeds are connection strings from the table tests plus known-tricky shapes.
var fuzzSeeds = []string{
	"mongodb://localhost:27017",
	"mongodb://h1:27017,h2:27017/db?replicaSet=rs0",
	"mongodb://u:p@h/db",
	"mongodb://u%40x:p%2F%3F%23%20s@h/db",
	"mongodb://:secret@h/db",
	"mongodb+srv://u:p@cluster0.example.net/db?retryWrites=true",
	"mongodb://u@h",
	"mongodb://h/?appName=me@corp",
	"mongodb://u:p@h/?authMechanismProperties=SERVICE_NAME:mongodb@REALM",
	"mongodb://u:p@[::1]:27017/db",
	"mongodb://[fe80::1%25en0]:27017/db",
	"mongodb://%2Ftmp%2Fmongodb-27017.sock/db",
	"mongodb://h:27017/",
	"mongodb://h/?",
	"mongodb://u:pa/ss@h/db",
	"mongodb://u:pa?ss@h/db",
	"mongodb://u:pa#ss@h/db",
	"mongodb://u:p@ss@h1,h2/db",
	"mongodb://u:pa ss@h/db",
	"mongodb://u\tx:p@h/db",
	"mongodb://",
	"mongodb://u:p@/db",
	"mongodb://h:abc/db",
	"mongodb://h:123456/db",
	"mongodb://h1,,h2/db",
	"mongodb://h/?replicaSet",
	"mongodb://u:p%zz@h/db",
	"mongodb://u:p%4@h/db",
	"mongodb://u:p x@h/db",
	"mongodb://u:p\u0085x@h/db",
	"mongodb://u:p\xffx@h/db",
	"mongodb://h{x}/db",
	"mongodb://u:p@h/db?password=x&authSource=admin",
}

// FuzzValidate checks that Validate never panics and that an accepted URI has
// components net/url can parse and unescape, and a password redact.URI masks.
func FuzzValidate(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, uri string) {
		if Validate(uri) != nil {
			return
		}
		scheme, rest, _ := strings.Cut(uri, "://")
		authority, tail := rest, ""
		if i := strings.IndexAny(rest, "/?"); i != -1 {
			authority, tail = rest[:i], rest[i:]
		}
		userinfo, hosts, hasUser := "", authority, false
		if at := strings.LastIndexByte(authority, '@'); at != -1 {
			userinfo, hosts, hasUser = authority[:at], authority[at+1:], true
		}

		// With the host list replaced by a plain name, net/url parses the URI and its
		// userinfo, path and query unescape (net/url itself rejects multi-host lists).
		probe := scheme + "://" + userinfo
		if hasUser {
			probe += "@"
		}
		probe += "placeholder" + tail
		u, err := url.Parse(probe)
		if err != nil && rfcUserinfo(userinfo) {
			t.Fatalf("Validate accepted %q but net/url rejects %q: %v", uri, probe, err)
		}
		if _, err := url.PathUnescape(userinfo); err != nil {
			t.Fatalf("Validate accepted %q with undecodable userinfo: %v", uri, err)
		}
		if u != nil {
			for _, opt := range strings.Split(u.RawQuery, "&") {
				key, value, _ := strings.Cut(opt, "=")
				if _, err := url.QueryUnescape(key); err != nil {
					t.Fatalf("Validate accepted %q with undecodable option %q: %v", uri, key, err)
				}
				if _, err := url.QueryUnescape(value); err != nil {
					t.Fatalf("Validate accepted %q with undecodable value of %q: %v", uri, key, err)
				}
			}
		}
		for _, h := range strings.Split(hosts, ",") {
			if _, err := url.PathUnescape(h); err != nil {
				t.Fatalf("Validate accepted %q with undecodable host %q: %v", uri, h, err)
			}
			if !strings.ContainsAny(h, "%[") {
				if _, err := url.Parse(scheme + "://" + h); err != nil {
					t.Fatalf("Validate accepted %q but net/url rejects host %q: %v", uri, h, err)
				}
			}
		}

		// The password never survives redaction.
		user, password, ok := strings.Cut(userinfo, ":")
		if !ok || password == "" {
			return
		}
		out := redact.URI(uri)
		shell := scheme + "://" + user + ":" + redact.Mask + "@" + hosts + tail
		if strings.Contains(out, password) && !strings.Contains(shell, password) && !strings.Contains(password, "*") {
			t.Fatalf("redact.URI(%q) = %q leaks the password", uri, out)
		}
	})
}

// rfcUserinfo reports whether s holds only characters RFC 3986 allows in userinfo
// (net/url rejects others, though MongoDB drivers accept them).
func rfcUserinfo(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9',
			strings.IndexByte("-._~!$&'()*+,;=:%", c) != -1:
		default:
			return false
		}
	}
	return true
}
