package config

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"unicode"
)

// haTargetSeeds are MONGORESCUE_HA_TARGET values from the table tests plus tricky
// shapes.
var haTargetSeeds = []string{
	"",
	"shared-s3",
	"s3://bucket",
	"s3://bucket/ha/prod",
	"s3://bucket/p?endpoint=https://minio:9000&region=eu-west-1&path_style=true",
	"s3://bucket?endpoint=http://u:p@minio",
	"s3://bucket?endpoint=ftp://minio",
	"s3://bucket?endpoint=https://minio/?x=1#f",
	"s3://bucket?secret_key=abc",
	"s3://AKIA:secret@bucket/p",
	"s3:///p",
	"file:///srv/mongorescue-ha",
	"file://localhost/srv/ha",
	"file://server/share/ha",
	"file:///C:/ha",
	"file://C:/ha",
	"file:relative",
	"file:///",
	"ftp://host/x",
	"s3://b/%zz",
	" s3://bucket/p ",
}

// FuzzParseHATarget checks ParseHATarget on arbitrary values: it never panics, fails
// only with ErrInvalidHA, never accepts credentials or unknown query parameters in a
// location, accepts only http(s) endpoints without credentials, and turns a file
// location into a clean absolute path that FileURL writes back to the same target
// (for paths FileURL does not need to escape).
func FuzzParseHATarget(f *testing.F) {
	for _, s := range haTargetSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		target, err := ParseHATarget(v)
		if err != nil {
			if !errors.Is(err, ErrInvalidHA) {
				t.Fatalf("ParseHATarget(%q) error %v is not ErrInvalidHA", v, err)
			}
			return
		}
		switch target.Scheme {
		case "":
			if target.Ref == "" || target.Ref != strings.TrimSpace(target.Ref) || strings.Contains(target.Ref, "://") {
				t.Fatalf("ParseHATarget(%q) = reference %q", v, target.Ref)
			}
		case "s3":
			u, perr := url.Parse(strings.TrimSpace(v))
			if perr != nil || u.User != nil || target.Bucket == "" || strings.Contains(target.Bucket, ":") {
				t.Fatalf("ParseHATarget(%q) accepted an s3 location without a bucket or with credentials", v)
			}
			for k := range u.Query() {
				if k != "endpoint" && k != "region" && k != "path_style" {
					t.Fatalf("ParseHATarget(%q) accepted the query parameter %q", v, k)
				}
			}
			if target.Endpoint != "" {
				eu, eerr := url.Parse(target.Endpoint)
				if eerr != nil || (eu.Scheme != "http" && eu.Scheme != "https") || eu.Host == "" || eu.User != nil {
					t.Fatalf("ParseHATarget(%q) accepted the endpoint %q", v, target.Endpoint)
				}
			}
		case "file":
			if !fileURLSafe(target.Path) {
				return
			}
			back, berr := ParseHATarget(FileURL(target.Path))
			if berr != nil || back != target {
				t.Fatalf("ParseHATarget(%q) = %+v; FileURL round trip %q gives %+v, %v", v, target, FileURL(target.Path), back, berr)
			}
		default:
			t.Fatalf("ParseHATarget(%q) = scheme %q", v, target.Scheme)
		}
	})
}

// fileURLSafe reports whether FileURL writes p back unchanged: it does not escape, so
// a path with '%', '#', '?', control characters or surrounding spaces does not round
// trip (FileURL labels a target for logs; it is not used to store one).
func fileURLSafe(p string) bool {
	return p == strings.TrimSpace(p) && !strings.ContainsAny(p, "%#?") &&
		!strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f || r > 0x7e })
}

// FuzzFileTargetPath checks the file:// path conversion for both path flavours: it
// never panics and returns only absolute, clean paths (a drive or UNC path on
// Windows, a rooted path elsewhere).
func FuzzFileTargetPath(f *testing.F) {
	f.Add("", "/srv/ha")
	f.Add("localhost", "/srv/ha")
	f.Add("server", "/share/ha")
	f.Add("C:", "/ha")
	f.Add("", "/C:/ha")
	f.Add("", "/C:/../..")
	f.Add("server", "/")
	f.Add("", "relative")
	f.Fuzz(func(t *testing.T, host, p string) {
		if strings.ContainsAny(host, `/\`) {
			return // net/url never puts a separator into a host
		}
		if got, err := fileTargetPath(host, p, "linux"); err == nil {
			if !strings.HasPrefix(got, "/") || strings.Contains(got, "/../") || strings.HasSuffix(got, "/..") {
				t.Fatalf("fileTargetPath(%q, %q, linux) = %q, not a clean absolute path", host, p, got)
			}
		} else if !errors.Is(err, ErrInvalidHA) {
			t.Fatalf("fileTargetPath(%q, %q, linux): %v is not ErrInvalidHA", host, p, err)
		}
		if strings.Contains(p, `\`) {
			// A literal backslash in a file:// URL is a separator on Windows, which
			// path.Clean does not know; the value comes from the operator's own
			// environment, so this is not checked here.
			return
		}
		got, err := fileTargetPath(host, p, "windows")
		if err != nil {
			if !errors.Is(err, ErrInvalidHA) {
				t.Fatalf("fileTargetPath(%q, %q, windows): %v is not ErrInvalidHA", host, p, err)
			}
			return
		}
		unc := strings.HasPrefix(got, `\\`)
		drive := len(got) >= 3 && isDrive(got[:2]) && got[2] == '\\'
		if !unc && !drive {
			t.Fatalf("fileTargetPath(%q, %q, windows) = %q, neither a drive nor a UNC path", host, p, got)
		}
		// The server name of a UNC path is the URL's host, not a path element.
		rest := got
		if unc {
			rest = strings.TrimPrefix(got, `\\`+host)
		}
		if strings.Contains(rest, `\..\`) || strings.HasSuffix(rest, `\..`) || strings.Contains(got, "/") {
			t.Fatalf("fileTargetPath(%q, %q, windows) = %q, not clean", host, p, got)
		}
	})
}

// FuzzHATargetLabel checks that the label of an HA target, which goes into errors and
// logs, never contains the password of "s3://<user>:<password>@<rest>" (refused, but
// still labelled) or the value of a query parameter, whatever the parts contain.
// Passwords with whitespace or '@' are skipped for the reasons FuzzTextPassword in
// internal/redact gives.
func FuzzHATargetLabel(f *testing.F) {
	f.Add("AKIA", "secret-1234", "bucket/p")
	f.Add("u", "pa/ss", "bucket?endpoint=https://m")
	f.Add("", "pa?ss#x", "bucket")
	f.Add("u", "p", "bucket?secret_key=abcdef")
	f.Fuzz(func(t *testing.T, user, password, rest string) {
		if len(password) < 4 || strings.ContainsAny(password, "*@?") || strings.ContainsFunc(password, unicode.IsSpace) ||
			strings.ContainsAny(user, "@/?#") || strings.ContainsFunc(user, unicode.IsSpace) {
			// A '/', '?' or '#' in the user ends the authority: the rest is a path, a
			// query or a fragment, not a password.
			return
		}
		in := "s3://" + user + ":" + password + "@" + rest
		label := HATargetLabel(in)
		shell := "s3://" + user + ":@" + rest
		if strings.Contains(label, password) && !strings.Contains(shell, password) {
			t.Fatalf("HATargetLabel(%q) = %q leaks the password %q", in, label, password)
		}
		// Neither a parsed target's label nor the fallback ever keeps a query.
		if strings.Contains(label, "?") {
			t.Fatalf("HATargetLabel(%q) = %q keeps a query", in, label)
		}
	})
}
