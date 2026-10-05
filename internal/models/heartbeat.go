package models

import (
	"errors"
	"net/url"
	"strings"
)

// MaxHeartbeatURLLength bounds a heartbeat URL (global or per job).
const MaxHeartbeatURLLength = 2048

// Heartbeat URL errors.
var (
	// ErrInvalidHeartbeatURL is returned for a heartbeat URL that is not an absolute
	// http or https URL without credentials, or is longer than MaxHeartbeatURLLength.
	ErrInvalidHeartbeatURL = errors.New("heartbeat_url must be an http or https URL without user info, at most 2048 characters")
	// ErrMaskedHeartbeatURL is returned when a masked heartbeat URL is sent back that
	// does not match the stored one (none is stored, or its host differs): the full
	// URL must be entered again.
	ErrMaskedHeartbeatURL = errors.New("heartbeat_url is masked but does not match the stored URL; enter the full URL")
)

// MaskEndpoint masks everything after the origin of an HTTP(S) URL: heartbeat and
// webhook URLs carry their credential in the path or query, so API responses show
// "scheme://host/" + SecretMask. A bare origin is returned unchanged, an unparsable
// value is SecretMask and "" stays "".
func MaskEndpoint(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return SecretMask
	}
	if u.User == nil && (u.Path == "" || u.Path == "/") && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" {
		return raw
	}
	return u.Scheme + "://" + u.Host + "/" + SecretMask
}

// ResolveHeartbeatURL applies the keep-secret rule to an incoming heartbeat URL:
// SecretMask, or the masked form of stored (as API responses show it), keeps stored;
// any other masked value is ErrMaskedHeartbeatURL, so a stored URL is never kept for
// another host; "" removes the URL and anything else replaces it (check it with
// ValidateHeartbeatURL).
func ResolveHeartbeatURL(incoming, stored string) (string, error) {
	in := strings.TrimSpace(incoming)
	if !strings.Contains(in, SecretMask) {
		return in, nil
	}
	if stored != "" && (in == SecretMask || in == MaskEndpoint(stored)) {
		return stored, nil
	}
	return "", ErrMaskedHeartbeatURL
}

// ValidateHeartbeatURL checks a heartbeat URL: "" (no heartbeat) or an absolute http
// or https URL with a host, without user info or control characters. The host is
// checked again on every connection, after DNS resolution (see
// notify.NewHTTPClient).
func ValidateHeartbeatURL(raw string) error {
	if raw == "" {
		return nil
	}
	if len(raw) > MaxHeartbeatURLLength || strings.ContainsFunc(raw, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return ErrInvalidHeartbeatURL
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return ErrInvalidHeartbeatURL
	}
	return nil
}

// Redacted returns a copy of the job that is safe to serialize to API clients: the
// heartbeat URL is reduced to its origin (see MaskEndpoint).
func (j *Job) Redacted() *Job {
	clone := j.Clone()
	if clone != nil {
		clone.HeartbeatURL = MaskEndpoint(clone.HeartbeatURL)
	}
	return clone
}

// RedactJobs returns redacted copies of jobs (see Job.Redacted).
func RedactJobs(jobs []*Job) []*Job {
	out := make([]*Job, len(jobs))
	for i, j := range jobs {
		out[i] = j.Redacted()
	}
	return out
}
