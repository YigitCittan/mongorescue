package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPresentedKey checks that the key is read from Authorization: Bearer first and
// from X-API-Key in any spelling, as Header.Get would.
func TestPresentedKey(t *testing.T) {
	if keyHeader != http.CanonicalHeaderKey("X-API-Key") {
		t.Fatalf("keyHeader = %q; want the canonical form", keyHeader)
	}
	for _, tc := range []struct {
		name    string
		headers map[string]string
		raw     map[string][]string
		want    string
	}{
		{"none", nil, nil, ""},
		{"bearer", map[string]string{"Authorization": "Bearer mr_abc"}, nil, "mr_abc"},
		{"bearer any case", map[string]string{"Authorization": "bearer  mr_abc "}, nil, "mr_abc"},
		{"bearer wins", map[string]string{"Authorization": "Bearer one", "X-API-Key": "two"}, nil, "one"},
		{"basic is ignored", map[string]string{"Authorization": "Basic Zm9v", "X-API-Key": "two"}, nil, "two"},
		{"x-api-key", map[string]string{"X-API-Key": " mr_def "}, nil, "mr_def"},
		{"lower case header", map[string]string{"x-api-key": "mr_ghi"}, nil, "mr_ghi"},
		{"first value", nil, map[string][]string{"X-Api-Key": {"first", "second"}}, "first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/v1/jobs", nil)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			for k, v := range tc.raw {
				r.Header[k] = v
			}
			if got := presentedKey(r); got != tc.want {
				t.Fatalf("presentedKey = %q; want %q", got, tc.want)
			}
		})
	}
}
