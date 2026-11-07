package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/apierr"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

// TestEveryRouteErrorCarriesAKnownCode walks every registered route with requests that
// get refused (no credentials, a bad key, a read-only key, an empty or malformed body,
// an unknown id, a wrong method) and checks that every error response of the API is a
// JSON envelope with a non-empty code of the closed set of apierr.
func TestEveryRouteErrorCarriesAKnownCode(t *testing.T) {
	f := newScopeFixture(t)
	if _, err := f.auth.Setup(context.Background(), "192.0.2.1", f.auth.SetupCode(), "admin", testPassword); err != nil {
		t.Fatal(err)
	}
	patterns := f.authenticatedPatterns()
	slices.Sort(patterns)
	type variant struct {
		name, body string
		headers    map[string]string
	}
	bearer := func(scope auth.Scope) map[string]string {
		return map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + f.keys[scope]}
	}
	variants := []variant{
		{"anonymous", `{}`, map[string]string{"Content-Type": "application/json"}},
		{"bad key", `{}`, map[string]string{"Content-Type": "application/json", "Authorization": "Bearer mr_notakey"}},
		{"read key", `{}`, bearer(auth.ScopeRead)},
		{"admin, empty object", `{}`, bearer(auth.ScopeAdmin)},
		{"admin, malformed body", `{"bad`, bearer(auth.ScopeAdmin)},
		{"admin, unknown field", `{"no_such_field_x":1}`, bearer(auth.ScopeAdmin)},
	}
	var errorsSeen int
	check := func(what string, rec *httptest.ResponseRecorder) {
		if rec.Code < 400 {
			return
		}
		errorsSeen++
		var env apiResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Errorf("%s: %d with a body that is not the JSON envelope: %q", what, rec.Code, rec.Body.String())
			return
		}
		if env.Success || env.Error == "" {
			t.Errorf("%s: %d %s: want success=false and a message", what, rec.Code, rec.Body)
		}
		if env.Code == "" || !apierr.Known(env.Code) {
			t.Errorf("%s: %d %s: code %q is empty or not in the closed set", what, rec.Code, rec.Body, env.Code)
		}
	}
	for _, p := range patterns {
		method, path := concrete(p)
		if !strings.HasPrefix(path, "/api/") {
			continue // metrics and MCP are served by their own handlers
		}
		for _, v := range variants {
			check(fmt.Sprintf("%s (%s)", p, v.name), serve(f.h, method, path, []byte(v.body), v.headers))
		}
		// A method the route does not serve.
		other := http.MethodPatch
		check("PATCH "+path, serve(f.h, other, path, []byte(`{}`), bearer(auth.ScopeAdmin)))
	}
	for _, path := range []string{"/api/v1/nope", "/api/v2/jobs", "/api/v1/jobs/x/y/z"} {
		check("GET "+path+" (admin)", serve(f.h, "GET", path, nil, bearer(auth.ScopeAdmin)))
		check("GET "+path, serve(f.h, "GET", path, nil, nil))
	}
	if errorsSeen < 4*len(patterns) {
		t.Fatalf("only %d error responses over %d routes; the walk is not exercising the refusals", errorsSeen, len(patterns))
	}
}

// TestSpecificErrorCodes pins the code of refusals clients are expected to branch on.
func TestSpecificErrorCodes(t *testing.T) {
	f := newScopeFixture(t)
	if _, err := f.auth.Setup(context.Background(), "192.0.2.1", f.auth.SetupCode(), "admin", testPassword); err != nil {
		t.Fatal(err)
	}
	code := func(rec *httptest.ResponseRecorder) string {
		var env apiResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		return env.Code
	}
	admin := map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + f.keys[auth.ScopeAdmin]}
	read := map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + f.keys[auth.ScopeRead]}
	for _, c := range []struct {
		name         string
		rec          *httptest.ResponseRecorder
		status       int
		wantCode     string
		wantNotEmpty bool
	}{
		{"no credentials", serve(f.h, "GET", "/api/v1/jobs", nil, nil), 401, apierr.Unauthorized, true},
		{"scope", serve(f.h, "POST", "/api/v1/jobs", []byte(`{}`), read), 403, apierr.Forbidden, true},
		{"unknown backup", serve(f.h, "GET", "/api/v1/backups/does_not_exist", nil, admin), 404, apierr.NotFound, true},
		{"malformed json", serve(f.h, "POST", "/api/v1/jobs", []byte(`{"bad`), admin), 400, apierr.InvalidRequest, true},
		{"body too large", serve(f.h, "POST", "/api/v1/jobs", []byte(`{"name":"`+strings.Repeat("a", maxAPIBody+10)+`"}`), admin),
			413, apierr.BodyTooLarge, true},
	} {
		if c.rec.Code != c.status || code(c.rec) != c.wantCode {
			t.Errorf("%s: %d code %q (%s); want %d %q", c.name, c.rec.Code, code(c.rec), c.rec.Body, c.status, c.wantCode)
		}
	}
}

// TestWriteOperationErrorCodes maps the sentinels of the operations, auth, settings and
// models packages to their status and code, wrapped as the services wrap them.
func TestWriteOperationErrorCodes(t *testing.T) {
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	wrap := func(err error) error { return fmt.Errorf("context: %w", err) }
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{operations.ErrNotFound, 404, apierr.NotFound},
		{operations.ErrInvalid, 400, apierr.InvalidRequest},
		{operations.ErrConnectionRequired, 400, apierr.InvalidRequest},
		{operations.ErrUnknownConnection, 400, apierr.InvalidRequest},
		{settings.ErrInvalid, 400, apierr.InvalidRequest},
		{operations.ErrBulkTooLarge, 422, apierr.ValidationFailed},
		{operations.ErrKeyRequired, 422, apierr.ValidationFailed},
		{operations.ErrPITRNotRestorable, 422, apierr.ValidationFailed},
		{operations.ErrBusy, 409, apierr.Busy},
		{operations.ErrBackupRunning, 409, apierr.Busy},
		{operations.ErrNotRunning, 409, apierr.Conflict},
		{operations.ErrNotRetryable, 409, apierr.Conflict},
		{operations.ErrAlreadyDeleted, 409, apierr.Conflict},
		{operations.ErrTooFewAdmins, 409, apierr.Conflict},
		{operations.ErrPinned, 409, apierr.Protected},
		{operations.ErrJobChanged, 409, apierr.JobChanged},
		{operations.ErrJobRecreated, 409, apierr.JobChanged},
		{models.ErrShardedUnsupported, 409, apierr.ShardedUnsupported},
		{operations.ErrSelfApproval, 403, apierr.ApprovalRefused},
		{auth.ErrApproverTooRecent, 403, apierr.ApprovalRefused},
		{auth.ErrForbidden, 403, apierr.Forbidden},
		{operations.ErrUnavailable, 503, apierr.Unavailable},
		{operations.ErrShuttingDown, 503, apierr.Unavailable},
		{operations.ErrNotActive, 503, apierr.Unavailable},
		{operations.ErrDatabaseListing, 502, apierr.BadGateway},
		{operations.ErrLegalHold, 502, apierr.BadGateway},
		{targets.ErrNotFound, 404, apierr.NotFound},
		{targets.ErrInUse, 409, apierr.Conflict},
		{errors.New("disk exploded at /secret/path"), 500, apierr.InternalError},
	} {
		rec := httptest.NewRecorder()
		s.writeOperationError(rec, wrap(c.err))
		var env apiResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("%v: %v", c.err, err)
		}
		if rec.Code != c.status || env.Code != c.code || !apierr.Known(env.Code) {
			t.Errorf("%v: %d %q; want %d %q", c.err, rec.Code, env.Code, c.status, c.code)
		}
	}
	// A failing preflight carries its own code and the checks.
	rec := httptest.NewRecorder()
	s.writeOperationError(rec, &operations.PreflightError{})
	var env apiResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if rec.Code != http.StatusConflict || env.Code != apierr.PreflightFailed {
		t.Errorf("preflight: %d %q; want 409 %q", rec.Code, env.Code, apierr.PreflightFailed)
	}
}

// TestWriteErrorDefaultsCodeByStatus checks that an error written without a code gets
// the default of its status.
func TestWriteErrorDefaultsCodeByStatus(t *testing.T) {
	for status := 400; status < 600; status++ {
		rec := httptest.NewRecorder()
		writeError(rec, status, "x")
		var env apiResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		if env.Code == "" || env.Code != apierr.ForStatus(status) || !apierr.Known(env.Code) {
			t.Fatalf("status %d: code %q", status, env.Code)
		}
	}
}
