package apierr

import (
	"regexp"
	"testing"
)

func TestCodesAreSnakeCase(t *testing.T) {
	re := regexp.MustCompile(`^[a-z]+(_[a-z]+)*$`)
	for _, c := range All() {
		if !re.MatchString(c) {
			t.Errorf("code %q is not snake_case", c)
		}
	}
}

func TestForStatusIsAlwaysKnown(t *testing.T) {
	for status := 400; status < 600; status++ {
		if c := ForStatus(status); c == "" || !Known(c) {
			t.Errorf("ForStatus(%d) = %q", status, c)
		}
	}
	for status, want := range map[int]string{
		400: InvalidRequest, 401: Unauthorized, 403: Forbidden, 404: NotFound, 405: InvalidRequest, 409: Conflict,
		413: BodyTooLarge, 415: InvalidRequest, 422: ValidationFailed, 429: RateLimited, 500: InternalError,
		502: BadGateway, 503: Unavailable,
	} {
		if got := ForStatus(status); got != want {
			t.Errorf("ForStatus(%d) = %q; want %q", status, got, want)
		}
	}
}

func TestKnown(t *testing.T) {
	if Known("") || Known("nope") || !Known(NotFound) {
		t.Error("Known does not match the closed set")
	}
}
