package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

// TestPITRRestoreAPI checks the point-in-time body of POST /api/v1/restore and its
// preflight: operator (read is refused), safe clones only, and unavailable without
// PITR restores; chain tests stay admin.
func TestPITRRestoreAPI(t *testing.T) {
	f := newPITRFixture(t)
	body := `{"pitr":{"stream_id":"str_a","at":"2026-10-05T12:00:00Z"},"databases":["shop"]}`
	for _, path := range []string{"/api/v1/restore", "/api/v1/restores/preflight"} {
		if code, out := f.do(auth.ScopeRead, "POST", path, body); code != http.StatusForbidden {
			t.Fatalf("%s as reader: %d %s", path, code, out)
		}
		// An operator passes the scope checks; this fixture has no PITR restores.
		if code, out := f.do(auth.ScopeOperator, "POST", path, body); code != http.StatusServiceUnavailable {
			t.Fatalf("%s as operator: %d %s", path, code, out)
		}
		if code, out := f.do(auth.ScopeOperator, "POST", path,
			`{"pitr":{"stream_id":"str_a","at":"2026-10-05T12:00:00Z"},"safe_clone":false,"confirm_in_place":true}`); code != http.StatusBadRequest || !strings.Contains(out, "safe clones only") {
			t.Fatalf("%s in place as operator: %d %s", path, code, out)
		}
		if code, out := f.do(auth.ScopeAdmin, "POST", path,
			`{"pitr":{"stream_id":"str_a","at":"2026-10-05T12:00:00Z"},"safe_clone":false,"confirm_in_place":true}`); code != http.StatusBadRequest || !strings.Contains(out, "safe clones only") {
			t.Fatalf("%s in place: %d %s", path, code, out)
		}
		if code, out := f.do(auth.ScopeAdmin, "POST", path, `{"pitr":{"stream_id":"str_a","at":"yesterday"}}`); code != http.StatusBadRequest {
			t.Fatalf("%s bad time: %d %s", path, code, out)
		}
		if code, out := f.do(auth.ScopeAdmin, "POST", path, body); code != http.StatusServiceUnavailable {
			t.Fatalf("%s without PITR restores: %d %s", path, code, out)
		}
	}
	chainTest := "/api/v1/pitr/streams/str_a/chain-test"
	if code, out := f.do(auth.ScopeOperator, "POST", chainTest, ""); code != http.StatusForbidden {
		t.Fatalf("chain test as operator: %d %s", code, out)
	}
	if code, out := f.do(auth.ScopeAdmin, "POST", chainTest, ""); code != http.StatusServiceUnavailable {
		t.Fatalf("chain test without PITR restores: %d %s", code, out)
	}
}
