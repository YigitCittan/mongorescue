// Package mongouri performs lightweight, dependency-free structural validation of
// MongoDB connection strings before they are persisted or handed to the database tools.
//
// It intentionally does not replicate the full MongoDB URI specification (that is the
// job of mongodump/mongorestore); it rejects the malformed shapes that would otherwise
// be silently mis-parsed, most importantly credentials with unescaped reserved characters.
package mongouri

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrInvalidMongoURI is returned when a connection string is structurally invalid.
var ErrInvalidMongoURI = errors.New(
	"invalid mongo_uri: must start with mongodb:// or mongodb+srv:// and credentials must be " +
		"percent-encoded (escape '@', '/', '?', '#' and whitespace in the username and password)",
)

// Supported connection string schemes.
const (
	SchemeStandard = "mongodb://"
	SchemeSRV      = "mongodb+srv://"
)

// maxPortDigits bounds the length of a numeric port (65535).
const maxPortDigits = 5

// Validate performs structural validation of a MongoDB connection string and returns
// ErrInvalidMongoURI on failure. The error never echoes the URI, so it is safe to return
// to API clients or log.
//
// The authority is the text between "://" and the first '/' or '?'; its last '@' (if
// any) separates the userinfo from the host list, so '@' characters in the database path
// or query options do not affect credential parsing. The following are rejected:
//   - a scheme other than mongodb:// or mongodb+srv://;
//   - invalid UTF-8, whitespace, control characters, or a raw '#' anywhere;
//   - a '%' that does not start a two-digit hexadecimal escape, which the driver
//     cannot unescape;
//   - a raw '@' inside the userinfo;
//   - a host character outside the RFC 3986 host syntax (letters, digits,
//     percent escapes, "-._~!$&'()*+;=" and ':' inside IPv6 brackets);
//   - an empty host list, an empty host, or a non-numeric port. This is what catches
//     passwords with a raw '/' or '?': "u:pa/ss@h" cuts the authority to "u:pa", which is
//     not a valid host:port;
//   - a database path containing '/' or '@' (for example the "/ss@h/db" remainder above);
//   - a query option that is not of the form key=value.
func Validate(uri string) error {
	var rest string
	switch {
	case strings.HasPrefix(uri, SchemeStandard):
		rest = uri[len(SchemeStandard):]
	case strings.HasPrefix(uri, SchemeSRV):
		rest = uri[len(SchemeSRV):]
	default:
		return ErrInvalidMongoURI
	}

	if !utf8.ValidString(rest) || strings.ContainsFunc(rest, isSpace) || strings.ContainsRune(rest, '#') ||
		!validEscapes(rest) {
		return ErrInvalidMongoURI
	}

	// Split authority from the optional "/database" path and "?options" query.
	authority, tail := rest, ""
	if i := strings.IndexAny(rest, "/?"); i != -1 {
		authority, tail = rest[:i], rest[i:]
	}

	hosts := authority
	if at := strings.LastIndexByte(authority, '@'); at != -1 {
		if strings.ContainsRune(authority[:at], '@') {
			return ErrInvalidMongoURI
		}
		hosts = authority[at+1:]
	}
	if !validHostList(hosts) {
		return ErrInvalidMongoURI
	}

	path, query := tail, ""
	if i := strings.IndexByte(tail, '?'); i != -1 {
		path, query = tail[:i], tail[i+1:]
	}
	if path != "" && strings.ContainsAny(path[1:], "/@") {
		return ErrInvalidMongoURI
	}
	for _, opt := range strings.Split(query, "&") {
		if opt != "" && !strings.Contains(opt, "=") {
			return ErrInvalidMongoURI
		}
	}

	return nil
}

// validHostList reports whether hosts is a non-empty, comma-separated list of
// host[:port] or [ipv6][:port] entries with numeric ports.
func validHostList(hosts string) bool {
	if hosts == "" {
		return false
	}
	for _, h := range strings.Split(hosts, ",") {
		if !validHost(h) {
			return false
		}
	}
	return true
}

// validHost reports whether h is a single host[:port] or [ipv6][:port] entry.
func validHost(h string) bool {
	name, port := h, ""
	if strings.HasPrefix(h, "[") {
		end := strings.IndexByte(h, ']')
		if end < 2 {
			return false
		}
		name, port = h[1:end], h[end+1:]
		if port != "" {
			if port[0] != ':' {
				return false
			}
			port = port[1:]
			if port == "" {
				return false
			}
		}
	} else if i := strings.IndexByte(h, ':'); i != -1 {
		name, port = h[:i], h[i+1:]
		if port == "" {
			return false
		}
	}

	if name == "" || !validHostName(name, strings.HasPrefix(h, "[")) {
		return false
	}
	return port == "" || isPort(port)
}

// validHostName reports whether every ASCII character of name belongs to the RFC 3986
// host syntax: unreserved characters, percent escapes and sub-delimiters other than
// ',' (the host separator), plus ':' inside IPv6 brackets. Non-ASCII characters
// (internationalized names) are left to the resolver.
func validHostName(name string, bracketed bool) bool {
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= utf8.RuneSelf,
			'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9',
			strings.IndexByte("-._~%!$&'()*+;=", c) != -1,
			bracketed && c == ':':
		default:
			return false
		}
	}
	return true
}

// validEscapes reports whether every '%' in s starts a "%XX" hexadecimal escape.
func validEscapes(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			continue
		}
		if i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
			return false
		}
		i += 2
	}
	return true
}

// isHex reports whether c is a hexadecimal digit.
func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// isPort reports whether p is a 1-5 digit decimal port number.
func isPort(p string) bool {
	if len(p) > maxPortDigits {
		return false
	}
	for _, r := range p {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isSpace reports whether r is a whitespace or control character (ASCII or Unicode,
// such as U+0085 or U+2028).
func isSpace(r rune) bool {
	return r <= ' ' || r == 0x7f || unicode.IsSpace(r) || unicode.IsControl(r)
}
