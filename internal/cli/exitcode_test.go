package cli

import (
	"net/http"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/apiclient"
	"github.com/yigitcittan/mongorescue/internal/apierr"
)

// TestExitCodeFollowsTheErrorCode checks that the exit code comes from the stable error
// code of the response, and that a response without a known code falls back to status.
func TestExitCodeFollowsTheErrorCode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
		want   int
	}{
		{"not_found", http.StatusNotFound, apierr.NotFound, ExitNotFound},
		{"conflict", http.StatusConflict, apierr.Conflict, ExitConflict},
		{"busy", http.StatusConflict, apierr.Busy, ExitConflict},
		{"job_changed", http.StatusConflict, apierr.JobChanged, ExitConflict},
		{"preflight_failed", http.StatusConflict, apierr.PreflightFailed, ExitConflict},
		{"sharded_unsupported", http.StatusConflict, apierr.ShardedUnsupported, ExitConflict},
		{"unauthorized", http.StatusUnauthorized, apierr.Unauthorized, ExitAuth},
		{"api_key_expired", http.StatusUnauthorized, apierr.APIKeyExpired, ExitAuth},
		{"forbidden", http.StatusForbidden, apierr.Forbidden, ExitAuth},
		{"approval_refused", http.StatusForbidden, apierr.ApprovalRefused, ExitAuth},
		{"unavailable", http.StatusServiceUnavailable, apierr.Unavailable, ExitUnavailable},
		{"internal_error", http.StatusInternalServerError, apierr.InternalError, ExitUnavailable},
		{"rate_limited", http.StatusTooManyRequests, apierr.RateLimited, ExitFailed},
		{"validation_failed", http.StatusUnprocessableEntity, apierr.ValidationFailed, ExitFailed},
		{"invalid_request", http.StatusBadRequest, apierr.InvalidRequest, ExitFailed},
		{"body_too_large", http.StatusRequestEntityTooLarge, apierr.BodyTooLarge, ExitFailed},
		// An unknown or missing code (an older server) falls back to the status.
		{"no code 404", http.StatusNotFound, "", ExitNotFound},
		{"unknown code 409", http.StatusConflict, "from_the_future", ExitConflict},
	} {
		err := &apiclient.APIError{Method: "GET", Path: "/x", StatusCode: tc.status, Code: tc.code}
		if got := ExitCode(err); got != tc.want {
			t.Errorf("%s: exit %d; want %d", tc.name, got, tc.want)
		}
	}
}
