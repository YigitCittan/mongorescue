package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// auditLogFixture is a full server with auth, the API key activity log and the audit
// log of every action.
type auditLogFixture struct {
	*authFixture
	log *auditlog.Service
	srv *Server
}

func newAuditLogFixture(t *testing.T) *auditLogFixture {
	t.Helper()
	srv, _, _ := setupTestServer(t)
	st := storetest.New(t)
	svc := newTestAuth(t, st, "")
	log := auditlog.New(auditlog.Config{Repo: st, Logger: slog.New(slog.DiscardHandler)})
	full := NewServer(bootConfig(), st, srv.backupEngine, srv.restoreEngine, srv.storageDriver, srv.scheduler, nil, nil,
		WithAuth(svc), withTestConnection(t, st, nil), WithSettings(newTestSettings(t, st, newTestConfig().Security)),
		WithAudit(audit.NewService(st, slog.New(slog.DiscardHandler), audit.WithObserver(log.Mirror()))), WithAuditLog(log))
	return &auditLogFixture{authFixture: &authFixture{h: full.Handler(), auth: svc}, log: log, srv: full}
}

// events returns every entry, oldest first.
func (f *auditLogFixture) events(t *testing.T) []*auditlog.Event {
	t.Helper()
	var out []*auditlog.Event
	if err := f.log.Export(context.Background(), auditlog.Filter{}, func(e *auditlog.Event) error {
		out = append(out, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// find returns the entries with action.
func find(events []*auditlog.Event, action string) []*auditlog.Event {
	var out []*auditlog.Event
	for _, e := range events {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// assertNoBodies fails when a request body value appears in the log.
func assertNoBodies(t *testing.T, events []*auditlog.Event, secrets ...string) {
	t.Helper()
	raw, _ := json.Marshal(events)
	for _, s := range secrets {
		if strings.Contains(string(raw), s) {
			t.Fatalf("the audit log holds a request body value %q: %s", s, raw)
		}
	}
}

// TestAuditLogRecordsSessionActions proves dashboard (session) mutations, refused
// CSRF tokens, setup and sign-out are recorded with the user, route pattern,
// targets, status, client and user agent, while reads and bodies are not.
func TestAuditLogRecordsSessionActions(t *testing.T) {
	f := newAuditLogFixture(t)
	b := f.browser(t)
	setupCode := f.auth.SetupCode()
	admin := b.setup(f.authFixture).User
	const bobPassword = "bob's very long password"
	ua := map[string]string{"User-Agent": "audit-test/1.0"}
	rec := b.do("POST", "/api/v1/users", map[string]string{"username": "bob", "password": bobPassword}, ua)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user = %d %s", rec.Code, rec.Body.String())
	}
	var bob auth.User
	decodeData(t, rec, &bob)
	b.do("GET", "/api/v1/users", nil, nil)
	b.do("GET", "/api/v1/jobs", nil, nil)
	if rec = b.do("DELETE", "/api/v1/users/"+bob.ID, nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("delete user = %d", rec.Code)
	}
	csrf := b.csrf
	b.csrf = ""
	if rec = b.do("POST", "/api/v1/backups", map[string]string{}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("POST without CSRF = %d", rec.Code)
	}
	b.csrf = csrf
	if rec = b.do("POST", "/api/v1/auth/logout", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("logout = %d", rec.Code)
	}

	events := f.events(t)
	var actions []string
	for _, e := range events {
		actions = append(actions, e.Action)
	}
	want := []string{setupRoute, "POST /api/v1/users", "DELETE /api/v1/users/{id}", "POST /api/v1/backups", "POST /api/v1/auth/logout"}
	if !slices.Equal(actions, want) {
		t.Fatalf("actions = %q; want %q (reads are not recorded)", actions, want)
	}
	setup, created, deleted, refused := events[0], events[1], events[2], events[3]
	if setup.ActorKind != auditlog.ActorUser || setup.ActorUserID != admin.ID || setup.Status != http.StatusCreated {
		t.Fatalf("setup = %+v", setup)
	}
	if created.ActorKind != auditlog.ActorUser || created.ActorUserID != admin.ID || created.ActorName != "admin" ||
		created.Status != http.StatusCreated || created.Outcome != auditlog.OutcomeOK || created.ClientIP != "192.0.2.1" ||
		created.UserAgent != "audit-test/1.0" || len(created.Targets) != 1 || created.Targets["role"] != "viewer" || created.ActorKeyID != "" {
		t.Fatalf("create user = %+v", created)
	}
	if deleted.Targets["id"] != bob.ID || deleted.Status != http.StatusOK {
		t.Fatalf("delete user = %+v", deleted)
	}
	if refused.Outcome != auditlog.OutcomeDenied || refused.Status != http.StatusForbidden {
		t.Fatalf("refused CSRF = %+v", refused)
	}
	assertNoBodies(t, events, bobPassword, testPassword, setupCode)
	if v, err := f.log.Verify(context.Background()); err != nil || !v.OK || v.Checked != 5 {
		t.Fatalf("Verify = %+v, %v", v, err)
	}
}

// TestAuditLogRecordsSignIns proves successful and failed sign-ins are recorded:
// failures as anonymous with the name that was tried, never the password.
func TestAuditLogRecordsSignIns(t *testing.T) {
	f := newAuditLogFixture(t)
	b := f.browser(t)
	admin := b.setup(f.authFixture).User
	const wrong = "not-the-password-123"
	if rec := b.login("admin", wrong); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d", rec.Code)
	}
	b.ip = "192.0.2.77"
	if rec := b.login("ghost", wrong); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown user = %d", rec.Code)
	}
	if rec := b.login("admin", testPassword); rec.Code != http.StatusOK {
		t.Fatalf("login = %d", rec.Code)
	}
	logins := find(f.events(t), loginRoute)
	if len(logins) != 3 {
		t.Fatalf("logins = %d; want 3", len(logins))
	}
	if e := logins[0]; e.ActorKind != auditlog.ActorAnonymous || e.ActorName != "admin" || e.ActorUserID != "" ||
		e.Status != http.StatusUnauthorized || e.Outcome != auditlog.OutcomeDenied || e.ClientIP != "192.0.2.1" {
		t.Fatalf("failed login = %+v", e)
	}
	if e := logins[1]; e.ActorName != "ghost" || e.ClientIP != "192.0.2.77" {
		t.Fatalf("unknown user login = %+v", e)
	}
	if e := logins[2]; e.ActorKind != auditlog.ActorUser || e.ActorUserID != admin.ID || e.Outcome != auditlog.OutcomeOK {
		t.Fatalf("login = %+v", e)
	}
	assertNoBodies(t, f.events(t), wrong, testPassword)
}

// TestAuditLogRecordsAPIKeyMutations proves API key requests are recorded with the
// key, and reads are not.
func TestAuditLogRecordsAPIKeyMutations(t *testing.T) {
	f := newAuditLogFixture(t)
	b := f.browser(t)
	b.setup(f.authFixture)
	keys := map[auth.Scope]createdAPIKey{}
	for _, scope := range []auth.Scope{auth.ScopeRead, auth.ScopeAdmin} {
		rec := b.do("POST", "/api/v1/api-keys", map[string]string{"name": string(scope) + " key", "scope": string(scope)}, nil)
		var k createdAPIKey
		decodeData(t, rec, &k)
		keys[scope] = k
	}
	bearer := func(s auth.Scope) map[string]string {
		return map[string]string{"Authorization": "Bearer " + keys[s].Key, "Content-Type": "application/json"}
	}
	serve(f.h, "GET", "/api/v1/jobs", nil, bearer(auth.ScopeAdmin))
	serve(f.h, "DELETE", "/api/v1/users/usr_missing", nil, bearer(auth.ScopeAdmin))
	serve(f.h, "POST", "/api/v1/backups", []byte(`{"database":"secret-db-name"}`), bearer(auth.ScopeRead))

	events := f.events(t)
	del := find(events, "DELETE /api/v1/users/{id}")
	if len(del) != 1 || del[0].ActorKind != auditlog.ActorAPIKey || del[0].ActorKeyID != keys[auth.ScopeAdmin].APIKey.ID ||
		del[0].ActorKeyName != "admin key" || del[0].Targets["id"] != "usr_missing" || del[0].Status != http.StatusNotFound ||
		del[0].Outcome != auditlog.OutcomeError {
		t.Fatalf("API key delete = %+v", del)
	}
	denied := find(events, "POST /api/v1/backups")
	if len(denied) != 1 || denied[0].ActorKeyName != "read key" || denied[0].Outcome != auditlog.OutcomeDenied {
		t.Fatalf("read key POST = %+v", denied)
	}
	if len(find(events, "GET /api/v1/jobs")) != 0 {
		t.Fatal("an API key read was recorded")
	}
	assertNoBodies(t, events, "secret-db-name", keys[auth.ScopeAdmin].Key, keys[auth.ScopeRead].Key)
}

// TestAuditLogEndpoints covers listing with filters and pages, the JSON Lines
// export (itself recorded), verification and the admin scope of all three.
func TestAuditLogEndpoints(t *testing.T) {
	f := newAuditLogFixture(t)
	b := f.browser(t)
	b.setup(f.authFixture)
	b.login("admin", "wrong password!!")
	b.session(b.login("admin", testPassword))
	for range 3 {
		b.do("POST", "/api/v1/users", map[string]string{"username": "x", "password": "short"}, nil)
	}

	var page auditListResponse
	decodeData(t, b.do("GET", "/api/v1/audit/events?limit=2", nil, nil), &page)
	if len(page.Events) != 2 || page.NextBeforeID == 0 || page.Events[0].ID <= page.Events[1].ID {
		t.Fatalf("page = %+v", page)
	}
	var next auditListResponse
	decodeData(t, b.do("GET", "/api/v1/audit/events?limit=2&before_id="+itoa(page.NextBeforeID), nil, nil), &next)
	if len(next.Events) != 2 || next.Events[0].ID >= page.Events[1].ID {
		t.Fatalf("next page = %+v", next)
	}
	var denied auditListResponse
	decodeData(t, b.do("GET", "/api/v1/audit/events?result=denied&actor_kind=anonymous&action=login", nil, nil), &denied)
	if len(denied.Events) != 1 || denied.Events[0].ActorName != "admin" {
		t.Fatalf("filtered = %+v", denied)
	}
	for _, q := range []string{"limit=0", "limit=1001", "result=maybe", "actor_kind=robot", "since=yesterday", "before_id=-1",
		"since=2026-01-02T00:00:00Z&until=2026-01-01T00:00:00Z"} {
		if rec := b.do("GET", "/api/v1/audit/events?"+q, nil, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("?%s = %d; want 400", q, rec.Code)
		}
	}

	rec := b.do("GET", "/api/v1/audit/events/export?actor_kind=user", nil, nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/x-ndjson" ||
		!strings.Contains(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("export = %d %v", rec.Code, rec.Header())
	}
	var lines []auditlog.Event
	sc := bufio.NewScanner(bytes.NewReader(rec.Body.Bytes()))
	for sc.Scan() {
		var e auditlog.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("export line %q: %v", sc.Text(), err)
		}
		lines = append(lines, e)
	}
	if len(lines) < 4 || lines[0].ID >= lines[1].ID || lines[0].Hash == "" {
		t.Fatalf("export = %+v", lines)
	}
	for _, e := range lines {
		if e.ActorKind != auditlog.ActorUser {
			t.Fatalf("export ignores the filter: %+v", e)
		}
	}
	// The export is recorded (after it finished).
	if exports := find(f.events(t), auditExportRoute); len(exports) != 1 || exports[0].Status != http.StatusOK {
		t.Fatalf("export entries = %+v", exports)
	}

	var v auditlog.Verification
	decodeData(t, b.do("GET", "/api/v1/audit/events/verify", nil, nil), &v)
	if !v.OK || v.Checked < 7 || v.HeadHash == "" || v.Anchor.LastHash != auditlog.GenesisHash {
		t.Fatalf("verify = %+v", v)
	}

	// Admin only.
	rec = b.do("POST", "/api/v1/api-keys", map[string]string{"name": "ops", "scope": "operator"}, nil)
	var k createdAPIKey
	decodeData(t, rec, &k)
	for _, path := range []string{"/api/v1/audit", "/api/v1/audit/events", "/api/v1/audit/events/export", "/api/v1/audit/events/verify"} {
		if rec := serve(f.h, "GET", path, nil, map[string]string{"X-API-Key": k.Key}); rec.Code != http.StatusForbidden {
			t.Errorf("operator GET %s = %d; want 403", path, rec.Code)
		}
	}
}

// TestAuditLogNotConfigured answers 503 without an audit log.
func TestAuditLogNotConfigured(t *testing.T) {
	f := newAuthFixture(t, nil)
	b := f.browser(t)
	b.setup(f)
	for _, path := range []string{"/api/v1/audit/events", "/api/v1/audit/events/export", "/api/v1/audit/events/verify"} {
		if rec := b.do("GET", path, nil, nil); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s = %d; want 503", path, rec.Code)
		}
	}
}

func TestAuditsAction(t *testing.T) {
	for _, tc := range []struct {
		method, path, pattern string
		want                  bool
	}{
		{"POST", "/api/v1/jobs", "POST /api/v1/jobs", true},
		{"DELETE", "/api/v1/jobs/x", "DELETE /api/v1/jobs/{id}", true},
		{"PATCH", "/api/v1/nope", "", true},
		{"GET", "/api/v1/jobs", "GET /api/v1/jobs", false},
		{"HEAD", "/api/v1/jobs", "GET /api/v1/jobs", false},
		{"OPTIONS", "/api/v1/jobs", "", false},
		{"GET", "/api/v1/audit/events/export", auditExportRoute, true},
		{"GET", "/api/v1/audit", "GET /api/v1/audit", false},
		{"POST", MCPPath, "POST " + MCPPath, false},
		{"GET", "/metrics", "GET /metrics", false},
	} {
		if got := auditsAction(tc.method, tc.path, tc.pattern); got != tc.want {
			t.Errorf("auditsAction(%s %s) = %v; want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// TestAuditLogRecordsUnauthenticatedRefusals proves mutating requests without valid
// credentials and refused public writes (wrong media type, cross-origin) are
// recorded anonymously, and coalesced per address.
func TestAuditLogRecordsUnauthenticatedRefusals(t *testing.T) {
	f := newAuditLogFixture(t)
	b := f.browser(t)
	b.setup(f.authFixture)
	anon := f.browser(t)
	anon.ip = "198.51.100.9"
	if rec := anon.do("POST", "/api/v1/backups", map[string]string{"database": "shop"}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST without credentials = %d", rec.Code)
	}
	if rec := anon.do("DELETE", "/api/v1/jobs/job_1", nil, map[string]string{"Authorization": "Bearer not-a-key"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("DELETE with a bad key = %d", rec.Code)
	}
	anon.do("GET", "/api/v1/jobs", nil, nil) // reads stay unrecorded
	if rec := anon.do("POST", "/api/v1/auth/login", nil, map[string]string{"Content-Type": "text/plain"}); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain login = %d", rec.Code)
	}
	if rec := anon.do("POST", "/api/v1/auth/login", map[string]string{"username": "a", "password": "b"}, map[string]string{"Origin": "https://evil.example"}); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin login = %d", rec.Code)
	}
	events := f.events(t)
	for _, want := range []struct {
		action string
		status int
	}{
		{"POST /api/v1/backups", http.StatusUnauthorized},
		{"DELETE /api/v1/jobs/{id}", http.StatusUnauthorized},
		{loginRoute, http.StatusUnsupportedMediaType},
		{loginRoute, http.StatusForbidden},
	} {
		if !slices.ContainsFunc(events, func(e *auditlog.Event) bool {
			return e.Action == want.action && e.Status == want.status && e.ActorKind == auditlog.ActorAnonymous && e.ClientIP == "198.51.100.9"
		}) {
			t.Errorf("no anonymous %s %d entry in %+v", want.action, want.status, events)
		}
	}
	if len(find(events, "GET /api/v1/jobs")) != 0 {
		t.Error("an unauthenticated read was recorded")
	}
	// A flood from one address is one entry per window plus a summary.
	for range 20 {
		anon.do("POST", "/api/v1/backups", map[string]string{"database": "shop"}, nil)
	}
	f.log.Flush()
	posts := find(f.events(t), "POST /api/v1/backups")
	total := 0
	for _, e := range posts {
		total += e.Count
	}
	if len(posts) != 2 || total != 21 {
		t.Fatalf("POST /api/v1/backups entries = %d standing for %d; want 2 standing for 21", len(posts), total)
	}
}

// TestAuditLogRecordsPanicsAndMissingStatus proves a handler that panics, or writes
// no status, is recorded as an error with status 0, and the panic still reaches
// net/http.
func TestAuditLogRecordsPanicsAndMissingStatus(t *testing.T) {
	f := newAuditLogFixture(t)
	p := &auth.Principal{Method: auth.MethodSession, User: &auth.User{ID: "usr_1", Username: "admin"}}
	run := func(h func(http.ResponseWriter)) (panicked any) {
		defer func() { panicked = recover() }()
		req := httptest.NewRequest("POST", "/api/v1/jobs", nil)
		rec, done := f.srv.auditedAction(httptest.NewRecorder(), req, p, "POST /api/v1/jobs")
		defer done()
		h(rec)
		return nil
	}
	if v := run(func(http.ResponseWriter) { panic("boom") }); v != "boom" {
		t.Fatalf("recovered %v; the panic must be re-raised", v)
	}
	if v := run(func(http.ResponseWriter) {}); v != nil {
		t.Fatalf("recovered %v", v)
	}
	if v := run(func(w http.ResponseWriter) { w.WriteHeader(http.StatusCreated) }); v != nil {
		t.Fatalf("recovered %v", v)
	}
	events := f.events(t)
	if len(events) != 3 {
		t.Fatalf("entries = %+v", events)
	}
	for i, want := range []struct {
		status  int
		outcome string
	}{{0, auditlog.OutcomeError}, {0, auditlog.OutcomeError}, {http.StatusCreated, auditlog.OutcomeOK}} {
		if e := events[i]; e.Status != want.status || e.Outcome != want.outcome || e.ActorUserID != "usr_1" {
			t.Errorf("entry %d = %+v; want %d %s", i, e, want.status, want.outcome)
		}
	}
}

// TestAuditLogNamesChangedSettings proves a settings update records the sections
// and keys it changed, never their values.
func TestAuditLogNamesChangedSettings(t *testing.T) {
	f := newAuditLogFixture(t)
	b := f.browser(t)
	b.setup(f.authFixture)
	const secret = "webhook-signing-s3cret"
	rec := b.do("PUT", "/api/v1/settings", map[string]any{
		"audit":   map[string]any{"retention_days": 90, "webhook_secret": secret},
		"general": map[string]any{"default_gzip": false},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT settings = %d %s", rec.Code, rec.Body.String())
	}
	b.do("PUT", "/api/v1/settings", map[string]any{"audit": map[string]any{"retention_days": 90}}, nil) // no change
	entries := find(f.events(t), "PUT /api/v1/settings")
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	if e := entries[0]; e.Targets["sections"] != "audit,general" || e.Targets["keys"] != "audit.retention_days,audit.webhook_secret,general.default_gzip" {
		t.Fatalf("targets = %v", e.Targets)
	}
	if len(entries[1].Targets) != 0 {
		t.Fatalf("an update that changed nothing names %v", entries[1].Targets)
	}
	assertNoBodies(t, f.events(t), secret)
}

func TestJoinBounded(t *testing.T) {
	for _, tc := range []struct {
		items []string
		n     int
		want  string
	}{
		{[]string{"a", "b"}, 10, "a,b"},
		{[]string{"aaaa", "bbbb", "cccc"}, 14, "aaaa,bbbb,cccc"},
		{[]string{"aaaa", "bbbb", "cccc"}, 13, "aaaa,+2 more"},
		{[]string{"aaaa", "bbbb", "cccc"}, 11, "+3 more"},
		{[]string{"aaaaaaaaaaaa", "b"}, 5, "+2 more"},
		{[]string{"aaaa", "bbbb"}, 9, "aaaa,bbbb"},
	} {
		if got := joinBounded(tc.items, tc.n); got != tc.want || (len(got) > tc.n && !strings.HasPrefix(got, "+")) {
			t.Errorf("joinBounded(%q, %d) = %q; want %q", tc.items, tc.n, got, tc.want)
		}
	}
}
