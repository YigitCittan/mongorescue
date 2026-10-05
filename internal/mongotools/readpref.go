package mongotools

import (
	"maps"
	"net/url"
	"slices"
	"strings"
)

// WithReadPreference returns uri with its read preference set to mode and tagSets:
// the connection string options readPreference and readPreferenceTags (names match
// case-insensitively, also percent-encoded) are removed and
// "readPreference=<mode>" is appended, followed by one "readPreferenceTags=k:v,..."
// per tag set in order (an empty set is "readPreferenceTags=", which matches any
// member). Tag names and values are query-escaped; callers validate that they hold
// no ',' or ':' (see models.ReadPreference.Validate).
//
// The primary takes no maxStalenessSeconds (the driver and the tools refuse the
// pair), so mode "primary" also removes that option.
//
// An empty mode returns uri unchanged, so the connection string's own read
// preference (the primary by default) applies as before. Both the Database Tools
// and the Go driver read these options from the URI, so mongodump (through
// WriteURIConfig, never --uri) and the driver-based manifest capture select the
// same member. Like WithConnectionDefaults the result carries credentials.
func WithReadPreference(uri, mode string, tagSets []map[string]string) string {
	if mode == "" {
		return uri
	}
	schemeEnd := strings.Index(uri, "://")
	if schemeEnd == -1 {
		return uri
	}
	rest := uri[schemeEnd+3:]
	base, query := uri, ""
	if i := strings.IndexByte(rest, '?'); i != -1 {
		base, query = uri[:schemeEnd+3+i], rest[i+1:]
	} else if !strings.Contains(rest, "/") {
		base = uri + "/"
	}

	var kept []string
	for _, opt := range strings.Split(query, "&") {
		if opt == "" {
			continue
		}
		key, _, _ := strings.Cut(opt, "=")
		if unescaped, err := url.QueryUnescape(key); err == nil {
			key = unescaped
		}
		switch strings.ToLower(key) {
		case "readpreference", "readpreferencetags":
			continue
		case "maxstalenessseconds":
			if mode == "primary" {
				continue
			}
		}
		kept = append(kept, opt)
	}
	kept = append(kept, "readPreference="+url.QueryEscape(mode))
	for _, set := range tagSets {
		kept = append(kept, "readPreferenceTags="+encodeTagSet(set))
	}
	return base + "?" + strings.Join(kept, "&")
}

// HasOption reports whether uri sets the connection string option name (matched
// case-insensitively, also percent-encoded).
func HasOption(uri, name string) bool {
	rest := uri
	if i := strings.Index(uri, "://"); i != -1 {
		rest = uri[i+3:]
	}
	i := strings.IndexByte(rest, '?')
	if i == -1 {
		return false
	}
	for _, opt := range strings.Split(rest[i+1:], "&") {
		key, _, _ := strings.Cut(opt, "=")
		if unescaped, err := url.QueryUnescape(key); err == nil {
			key = unescaped
		}
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

// encodeTagSet renders a tag set as "k1:v1,k2:v2", sorted by name and
// query-escaped.
func encodeTagSet(set map[string]string) string {
	parts := make([]string, 0, len(set))
	for _, k := range slices.Sorted(maps.Keys(set)) {
		parts = append(parts, url.QueryEscape(k)+":"+url.QueryEscape(set[k]))
	}
	return strings.Join(parts, ",")
}
