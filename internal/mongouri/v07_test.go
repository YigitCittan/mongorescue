package mongouri

import "strings"

// This file holds the connection string validation of v0.7.0 verbatim (renamed), so
// tests can check that ValidateStored keeps accepting everything it accepted.

// v07Validate is Validate as released in v0.7.0.
func v07Validate(uri string) error {
	var rest string
	switch {
	case strings.HasPrefix(uri, SchemeStandard):
		rest = uri[len(SchemeStandard):]
	case strings.HasPrefix(uri, SchemeSRV):
		rest = uri[len(SchemeSRV):]
	default:
		return ErrInvalidMongoURI
	}

	if strings.ContainsFunc(rest, v07IsSpace) || strings.ContainsRune(rest, '#') {
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
	if !v07ValidHostList(hosts) {
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

// v07ValidHostList reports whether hosts is a non-empty, comma-separated list of
// host[:port] or [ipv6][:port] entries with numeric ports.
func v07ValidHostList(hosts string) bool {
	if hosts == "" {
		return false
	}
	for _, h := range strings.Split(hosts, ",") {
		if !v07ValidHost(h) {
			return false
		}
	}
	return true
}

// v07ValidHost reports whether h is a single host[:port] or [ipv6][:port] entry.
func v07ValidHost(h string) bool {
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

	if name == "" || strings.ContainsAny(name, "@[]") {
		return false
	}
	return port == "" || v07IsPort(port)
}

// v07IsPort reports whether p is a 1-5 digit decimal port number.
func v07IsPort(p string) bool {
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

// v07IsSpace reports whether r is an ASCII whitespace or control character.
func v07IsSpace(r rune) bool {
	return r <= ' ' || r == 0x7f
}
