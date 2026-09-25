// Package redact provides dependency-free helpers for scrubbing credentials from
// strings before they are logged, persisted in diagnostics, or serialized to API clients.
//
// It deliberately imports nothing from the rest of MongoRescue so that low-level
// packages such as models can use it without creating import cycles.
//
// All helpers err on the side of over-masking: when a string is ambiguous (for example a
// raw, unescaped '@' or '/' inside a password), more of it is masked rather than less.
package redact

import (
	"net/url"
	"regexp"
	"strings"
)

// Mask is the placeholder substituted for redacted secrets.
const Mask = "******"

// sensitiveQueryKeys lists lower-cased query parameter names whose values carry secrets.
// authMechanismProperties is masked in full because it may embed AWS_SESSION_TOKEN.
var sensitiveQueryKeys = map[string]struct{}{
	"password":                      {},
	"authmechanismproperties":       {},
	"tlscertificatekeyfilepassword": {},
	"sslpemkeypassword":             {},
	"secretkey":                     {},
	"secret_key":                    {},
	"aws_session_token":             {},
}

// schemePattern locates the start of every "scheme://" occurrence in free text.
// It has no nested quantifiers and Go's RE2 engine guarantees linear-time matching.
var schemePattern = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://`)

// URI masks secrets in a single connection string to prevent credential leaks in logs,
// HTTP responses, or error messages. The userinfo password is replaced with Mask, as are
// the values of sensitive query parameters (password, authMechanismProperties,
// tlsCertificateKeyFilePassword, sslPEMKeyPassword, secretKey/secret_key, AWS_SESSION_TOKEN),
// matched case-insensitively after percent-decoding the key.
//
// The userinfo separator is the LAST '@' in the string, so passwords containing raw
// (unescaped) '@', '/', '?' or '#' are still masked completely. It supports standard
// (mongodb://), DNS seedlist (mongodb+srv://), and multi-host replica sets.
func URI(rawURI string) string {
	return maskQuerySecrets(maskUserinfo(rawURI))
}

// Text masks credentials in free-form text (for example subprocess stderr or error
// messages) containing any number of connection strings. Each "scheme://" occurrence is
// treated independently: the token running from the scheme to the next whitespace (or the
// next "scheme://") is passed through URI.
//
// Limitation: tokens are whitespace-delimited, so a password containing raw whitespace is
// masked only up to the first space (best effort). Valid connection strings always
// percent-encode such characters.
func Text(s string) string {
	matches := schemePattern.FindAllStringIndex(s, -1)
	if len(matches) == 0 {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	prev := 0
	for i, m := range matches {
		start := m[0]
		if start < prev {
			continue
		}
		end := len(s)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		if ws := strings.IndexFunc(s[m[1]:end], isSpace); ws != -1 {
			end = m[1] + ws
		}

		b.WriteString(s[prev:start])
		b.WriteString(URI(s[start:end]))
		prev = end
	}
	b.WriteString(s[prev:])
	return b.String()
}

// isSpace reports whether r is an ASCII whitespace character.
func isSpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
}

// SplitUserinfo locates the userinfo section of a connection string. It returns the index
// where the authority begins (just after "://") and the index of the userinfo-terminating
// '@', which is the last '@' in the string, or -1 when there is no userinfo. ok is false
// when the string has no "://" separator.
func SplitUserinfo(rawURI string) (authStart, atIdx int, ok bool) {
	schemeIdx := strings.Index(rawURI, "://")
	if schemeIdx == -1 {
		return 0, -1, false
	}
	authStart = schemeIdx + 3
	atIdx = strings.LastIndexByte(rawURI[authStart:], '@')
	if atIdx != -1 {
		atIdx += authStart
	}
	return authStart, atIdx, true
}

// maskUserinfo masks the userinfo password of a single connection string. The username
// (everything before the first ':') is preserved; it may be empty.
func maskUserinfo(rawURI string) string {
	authStart, atIdx, ok := SplitUserinfo(rawURI)
	if !ok || atIdx == -1 {
		// No scheme or no credentials present in URI
		return rawURI
	}

	userInfo := rawURI[authStart:atIdx]
	colonIdx := strings.IndexByte(userInfo, ':')
	if colonIdx == -1 {
		// Username only, no password to mask
		return rawURI
	}

	return rawURI[:authStart] + userInfo[:colonIdx] + ":" + Mask + rawURI[atIdx:]
}

// maskQuerySecrets replaces the values of sensitive query parameters with Mask.
// Every '?'- or '&'-introduced "key=value" pair is examined; values run until the next
// '&' or '#'. Keys are percent-decoded before comparison; undecodable keys are compared raw.
func maskQuerySecrets(s string) string {
	if !strings.ContainsAny(s, "?&") {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		b.WriteByte(c)
		i++
		if c != '?' && c != '&' {
			continue
		}

		// Parse "key=" following the delimiter.
		keyEnd := strings.IndexAny(s[i:], "=&?#")
		if keyEnd == -1 || s[i+keyEnd] != '=' {
			continue
		}
		key := s[i : i+keyEnd]
		if !isSensitiveKey(key) {
			continue
		}

		valStart := i + keyEnd + 1
		valEnd := len(s)
		if j := strings.IndexAny(s[valStart:], "&#"); j != -1 {
			valEnd = valStart + j
		}
		b.WriteString(s[i:valStart])
		b.WriteString(Mask)
		i = valEnd
	}
	return b.String()
}

// isSensitiveKey reports whether a raw query key names a secret-bearing parameter.
func isSensitiveKey(rawKey string) bool {
	key := rawKey
	if decoded, err := url.QueryUnescape(rawKey); err == nil {
		key = decoded
	}
	_, ok := sensitiveQueryKeys[strings.ToLower(key)]
	return ok
}
