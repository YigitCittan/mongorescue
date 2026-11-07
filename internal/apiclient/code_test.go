package apiclient

import (
	"errors"
	"net/http"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/apierr"
)

func TestAPIErrorMatchesSentinelsByCode(t *testing.T) {
	for _, tc := range []struct {
		status  int
		code    string
		matches []error
		not     []error
	}{
		{409, apierr.JobChanged, []error{ErrJobChanged, ErrConflict}, []error{ErrBusy, ErrNotFound}},
		{409, apierr.Busy, []error{ErrBusy, ErrConflict}, []error{ErrJobChanged}},
		{409, apierr.Protected, []error{ErrProtected, ErrConflict}, nil},
		{409, apierr.ShardedUnsupported, []error{ErrShardedUnsupported, ErrConflict}, nil},
		{503, apierr.Unavailable, []error{ErrUnavailable, ErrServer}, []error{ErrConflict}},
		{401, apierr.APIKeyExpired, []error{ErrAPIKeyExpired, ErrUnauthorized}, nil},
		{401, apierr.Unauthorized, []error{ErrUnauthorized}, []error{ErrAPIKeyExpired}},
		{403, apierr.ApprovalRefused, []error{ErrForbidden}, nil},
		{404, apierr.NotFound, []error{ErrNotFound}, nil},
		{429, apierr.RateLimited, []error{ErrRateLimited}, nil},
		{413, apierr.BodyTooLarge, []error{ErrBadRequest}, nil},
		{422, apierr.ValidationFailed, []error{ErrBadRequest}, nil},
		// The code wins over a status of another class; no or an unknown code falls back.
		{502, apierr.Conflict, []error{ErrConflict}, []error{ErrServer}},
		{404, "", []error{ErrNotFound}, nil},
		{409, "from_the_future", []error{ErrConflict}, []error{ErrBusy}},
		{500, "", []error{ErrServer}, nil},
	} {
		err := errors.Join(&APIError{Method: "GET", Path: "/x", StatusCode: tc.status, Code: tc.code})
		for _, m := range tc.matches {
			if !errors.Is(err, m) {
				t.Errorf("%d %q: does not match %v", tc.status, tc.code, m)
			}
		}
		for _, n := range tc.not {
			if errors.Is(err, n) {
				t.Errorf("%d %q: matches %v", tc.status, tc.code, n)
			}
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Code != tc.code {
			t.Errorf("%d %q: errors.As lost the code", tc.status, tc.code)
		}
	}
	if !errors.Is(&APIError{StatusCode: http.StatusFound}, ErrUnexpectedResponse) {
		t.Error("a redirect no longer matches ErrUnexpectedResponse")
	}
}
