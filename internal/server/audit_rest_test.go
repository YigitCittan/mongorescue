package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/auth"
)

// TestAPIKeyRESTRequestsAreAudited proves REST requests made with an API key are
// audited with their route pattern, path parameters, status and principal; reads and
// refusals are coalesced; MCP and /metrics requests are not audited here.
func TestAPIKeyRESTRequestsAreAudited(t *testing.T) {
	f := newScopeFixture(t)
	hdr := func(scope auth.Scope) map[string]string {
		return map[string]string{"Authorization": "Bearer " + f.keys[scope], "Content-Type": "application/json"}
	}
	for range 3 {
		if rec := serve(f.h, "GET", "/api/v1/jobs", nil, hdr(auth.ScopeRead)); rec.Code != http.StatusOK {
			t.Fatalf("GET jobs = %d", rec.Code)
		}
	}
	for range 4 {
		if rec := serve(f.h, "POST", "/api/v1/backups", []byte(`{}`), hdr(auth.ScopeRead)); rec.Code != http.StatusForbidden {
			t.Fatalf("read key POST backups = %d", rec.Code)
		}
	}
	serve(f.h, "GET", "/api/v1/connections/does_not_exist", nil, hdr(auth.ScopeOperator))
	serve(f.h, "BREW", "/api/v1/jobs", nil, hdr(auth.ScopeOperator))
	serve(f.h, "GET", "/metrics", nil, hdr(auth.ScopeRead))
	serve(f.h, "POST", MCPPath, []byte(`{}`), hdr(auth.ScopeRead))

	entries, err := f.srv.audit.List(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		if e.Transport != audit.TransportREST || e.APIKeyID == "" || e.APIKeyName == "" {
			t.Fatalf("entry lacks transport or principal: %+v", e)
		}
		got = append(got, fmt.Sprintf("%s|%s|%s|%d|%d|%s", e.APIKeyName, e.Tool, e.Arguments, e.HTTPStatus, e.Count, e.Result))
	}
	slices.Sort(got)
	want := []string{
		`operator key|GET /api/v1/connections/{id}|{"id":"does_not_exist"}|404|1|error`,
		`operator key|OTHER (no route)|{}|405|1|error`,
		`read key|GET /api/v1/jobs|{}|200|3|ok`,
		`read key|POST /api/v1/backups|{}|403|4|denied`,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("audit =\n%q\nwant\n%q", got, want)
	}
}

func TestRouteLabel(t *testing.T) {
	for _, tc := range []struct{ method, pattern, want string }{
		{"GET", "GET /api/v1/jobs", "GET /api/v1/jobs"},
		{"GET", "/api/v1/x", "GET /api/v1/x"},
		{"PATCH", "", "PATCH (no route)"},
		{"X-EVIL\n", "", "OTHER (no route)"},
	} {
		if got := routeLabel(tc.method, tc.pattern); got != tc.want {
			t.Errorf("routeLabel(%q, %q) = %q; want %q", tc.method, tc.pattern, got, tc.want)
		}
	}
}
