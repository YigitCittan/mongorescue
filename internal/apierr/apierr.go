// Package apierr defines the stable, machine-readable error codes of the REST API:
// the "code" field of every error response. The codes are a closed set of snake_case
// words that never carry user input or secrets; clients (the apiclient, the CLI, the
// MCP tools) match them instead of the English error text, which may change. It is
// a leaf package shared by the server and the clients and imports nothing.
package apierr

import "net/http"

// Codes of error responses. docs/api.md lists each with its status and meaning.
const (
	// InvalidRequest is a malformed or unacceptable request (400, 405, 415 and other 4xx
	// without a more precise code).
	InvalidRequest = "invalid_request"
	// ValidationFailed is a well-formed request whose content is refused (422).
	ValidationFailed = "validation_failed"
	// Unauthorized is a missing or invalid credential (401).
	Unauthorized = "unauthorized"
	// APIKeyExpired is the 401 of an API key past its expiry.
	APIKeyExpired = "api_key_expired"
	// Forbidden is a refusal of an authenticated caller: scope, role, connection
	// access or CSRF (403).
	Forbidden = "forbidden"
	// ApprovalRefused is a refused approval decision: self-approval, an approver that
	// is not a recent session, an unknown requester (403).
	ApprovalRefused = "approval_refused"
	// ApprovalRequired is the code of the 202 answer of a destructive action that waits
	// for a second administrator (not an error).
	ApprovalRequired = "approval_required"
	// NotFound is a record or route that does not exist (404).
	NotFound = "not_found"
	// Conflict is a request that conflicts with the current state (409).
	Conflict = "conflict"
	// Busy is an action refused because the same one is already running (409).
	Busy = "busy"
	// JobChanged is a bulk or retry action whose job was recreated or changed since the
	// caller looked (409).
	JobChanged = "job_changed"
	// Protected is a record that is pinned or on legal hold and so cannot be changed or
	// deleted.
	Protected = "protected"
	// PreflightFailed is a restore refused by its preflight checks (409, the checks are
	// in data).
	PreflightFailed = "preflight_failed"
	// ShardedUnsupported is a request for a sharded cluster, which is not supported (409).
	ShardedUnsupported = "sharded_unsupported"
	// BodyTooLarge is a request body above the limit (413).
	BodyTooLarge = "body_too_large"
	// RateLimited is a request over its rate limit (429, see Retry-After).
	RateLimited = "rate_limited"
	// InternalError is an unexpected failure of the server (500); details are in the log.
	InternalError = "internal_error"
	// BadGateway is a failure of a system the server called, such as MongoDB or storage (502).
	BadGateway = "bad_gateway"
	// Unavailable is a service that cannot take the request now, such as a standby or
	// shutting-down instance or an unconfigured feature (503); retry later.
	Unavailable = "unavailable"
)

// known is the closed set of codes.
var known = map[string]bool{
	InvalidRequest: true, ValidationFailed: true, Unauthorized: true, APIKeyExpired: true,
	Forbidden: true, ApprovalRefused: true, ApprovalRequired: true, NotFound: true,
	Conflict: true, Busy: true, JobChanged: true, Protected: true, PreflightFailed: true,
	ShardedUnsupported: true, BodyTooLarge: true, RateLimited: true, InternalError: true,
	BadGateway: true, Unavailable: true,
}

// Known reports whether code is one of the codes of this package.
func Known(code string) bool { return known[code] }

// All returns every code, in no particular order.
func All() []string {
	out := make([]string, 0, len(known))
	for c := range known {
		out = append(out, c)
	}
	return out
}

// ForStatus returns the default code of an HTTP status of 400 or more, used when an
// error has no more precise code. It is never empty for such a status.
func ForStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return Unauthorized
	case http.StatusForbidden:
		return Forbidden
	case http.StatusNotFound:
		return NotFound
	case http.StatusConflict:
		return Conflict
	case http.StatusRequestEntityTooLarge:
		return BodyTooLarge
	case http.StatusUnprocessableEntity:
		return ValidationFailed
	case http.StatusTooManyRequests:
		return RateLimited
	case http.StatusBadGateway:
		return BadGateway
	case http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return Unavailable
	}
	if status >= 500 {
		return InternalError
	}
	return InvalidRequest
}
