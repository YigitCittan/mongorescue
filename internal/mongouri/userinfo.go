package mongouri

import (
	"net/url"
	"strings"
)

// splitAuthority returns the index after the scheme of uri and the end of its
// authority (the first '/' or '?' after it, or the end); ok is false for an
// unsupported scheme.
func splitAuthority(uri string) (start, end int, ok bool) {
	switch {
	case strings.HasPrefix(uri, SchemeStandard):
		start = len(SchemeStandard)
	case strings.HasPrefix(uri, SchemeSRV):
		start = len(SchemeSRV)
	default:
		return 0, 0, false
	}
	end = len(uri)
	if i := strings.IndexAny(uri[start:], "/?"); i != -1 {
		end = start + i
	}
	return start, end, true
}

// HasUserinfo reports whether uri carries user information (a user name, with
// or without a password) in its authority.
func HasUserinfo(uri string) bool {
	start, end, ok := splitAuthority(uri)
	return ok && strings.Contains(uri[start:end], "@")
}

// WithUserinfo returns uri with its user information replaced by username and
// password, percent-encoded as the connection string format requires. A uri
// with an unsupported scheme is returned unchanged. The result holds a
// credential: never log or store it.
func WithUserinfo(uri, username, password string) string {
	start, end, ok := splitAuthority(uri)
	if !ok {
		return uri
	}
	hosts := uri[start:end]
	if at := strings.LastIndexByte(hosts, '@'); at != -1 {
		hosts = hosts[at+1:]
	}
	return uri[:start] + url.UserPassword(username, password).String() + "@" + hosts + uri[end:]
}
