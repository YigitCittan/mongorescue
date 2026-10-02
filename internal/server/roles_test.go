package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// roleFixture is the scope fixture with a signed-up admin, a viewer and an operator,
// and API keys of every scope created by a user of every role.
type roleFixture struct {
	*scopeFixture
	users map[auth.Role]*auth.User
	// userKeys holds, per creator role and key scope, a key created while the
	// creator was an admin and capped by the role it has now.
	userKeys map[auth.Role]map[auth.Scope]string
}

func newRoleFixture(t *testing.T) *roleFixture {
	t.Helper()
	f := &roleFixture{scopeFixture: newScopeFixture(t), users: map[auth.Role]*auth.User{}, userKeys: map[auth.Role]map[auth.Scope]string{}}
	ctx := context.Background()
	res, err := f.auth.Setup(ctx, "192.0.2.1", f.auth.SetupCode(), "admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	f.users[auth.RoleAdmin] = res.User
	system := auth.SystemPrincipal()
	for _, role := range []auth.Role{auth.RoleViewer, auth.RoleOperator} {
		u, createErr := f.auth.CreateUser(ctx, system, string(role), testPassword, role)
		if createErr != nil {
			t.Fatal(createErr)
		}
		f.users[role] = u
	}
	for _, role := range auth.Roles() {
		creator, createErr := f.auth.CreateUser(ctx, system, "keys-"+string(role), testPassword, auth.RoleAdmin)
		if createErr != nil {
			t.Fatal(createErr)
		}
		login, loginErr := f.auth.Login(ctx, "192.0.2.1", creator.Username, testPassword)
		if loginErr != nil {
			t.Fatal(loginErr)
		}
		p, authErr := f.auth.AuthenticateSession(ctx, login.Token)
		if authErr != nil {
			t.Fatal(authErr)
		}
		f.userKeys[role] = map[auth.Scope]string{}
		for _, scope := range auth.Scopes() {
			_, plain, keyErr := f.auth.CreateAPIKey(ctx, p, string(scope), scope)
			if keyErr != nil {
				t.Fatal(keyErr)
			}
			f.userKeys[role][scope] = plain
		}
		if _, err = f.auth.SetUserRole(ctx, system, creator.ID, role); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// as signs the user of role in afresh and returns the headers of a dashboard
// request: the session cookie, its CSRF token and a JSON content type. A fresh
// session per request keeps routes that end sessions from affecting the next one.
func (f *roleFixture) as(t *testing.T, role auth.Role) map[string]string {
	t.Helper()
	res, err := f.auth.Login(context.Background(), "192.0.2.1", f.users[role].Username, testPassword)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{"Cookie": SessionCookieName + "=" + res.Token, CSRFHeader: res.CSRFToken, "Content-Type": "application/json"}
}

// selfServiceRefused reports whether the placeholder request of a self-service
// route (see concrete: unknown IDs, an empty body) is refused for a caller with
// the effective scope have, signed in or not.
func selfServiceRefused(t *testing.T, pattern string, session bool, have auth.Scope) bool {
	t.Helper()
	admin := have.Allows(auth.ScopeAdmin)
	switch pattern {
	case logoutRoute, listAPIKeysRoute, deleteAPIKeyRoute:
		// Everyone may sign out and list their own keys; revoking an unknown key is
		// 404 for everyone (who may revoke what is pinned by the auth tests).
		return false
	case revokeSessionRoute, createAPIKeyRoute:
		// Signed-in users of every role may end their own sessions (an unknown one is
		// 404) and create keys up to their role; API keys need the admin scope.
		return !session && !admin
	case changePasswordRoute:
		// The placeholder ID is not the caller's own: another user's needs admin.
		return !admin
	}
	t.Fatalf("%s is in selfServiceRoutes without an expectation here", pattern)
	return false
}

// TestRolesAreEnforcedForEveryRoute walks every registered route with a session of
// every dashboard role (with its CSRF token) and with API keys of every scope
// created by a user of every role. The expected outcome follows from routeScopes
// and the effective scope (the role; for keys, the lower of the key's scope and its
// creator's role), except for selfServiceRoutes, whose finer rules are spelled out
// in selfServiceRefused.
func TestRolesAreEnforcedForEveryRoute(t *testing.T) {
	f := newRoleFixture(t)
	patterns := f.authenticatedPatterns()
	slices.Sort(patterns)
	check := func(t *testing.T, who string, refuse bool, code int, body string) {
		t.Helper()
		switch {
		case refuse && !scopeRefused(code, body):
			t.Errorf("%s: %d %s; want a scope refusal", who, code, body)
		case !refuse && (code == http.StatusUnauthorized || scopeRefused(code, body)):
			t.Errorf("%s: %d %s; want the handler's answer", who, code, body)
		}
	}
	for _, p := range patterns {
		need := requiredScope(p)
		method, path := concrete(p)
		self := slices.Contains(selfServiceRoutes, p)
		t.Run(p, func(t *testing.T) {
			for _, role := range auth.Roles() {
				rec := serve(f.h, method, path, []byte(`{}`), f.as(t, role))
				if path == MCPPath || path == "/metrics" {
					// Bearer tokens only: sessions never reach the machine endpoints.
					if rec.Code != http.StatusUnauthorized {
						t.Errorf("%s session: %d; want 401", role, rec.Code)
					}
					continue
				}
				refuse := !role.Scope().Allows(need)
				if self {
					refuse = selfServiceRefused(t, p, true, role.Scope())
				}
				check(t, string(role)+" session", refuse, rec.Code, rec.Body.String())
			}
			for _, creator := range auth.Roles() {
				for _, scope := range auth.Scopes() {
					effective := scope
					if !creator.Scope().Allows(scope) {
						effective = creator.Scope()
					}
					rec := serve(f.h, method, path, []byte(`{}`), map[string]string{
						"Authorization": "Bearer " + f.userKeys[creator][scope], "Content-Type": "application/json",
					})
					refuse := !effective.Allows(need)
					if self {
						refuse = selfServiceRefused(t, p, false, effective)
					}
					check(t, string(scope)+" key of a "+string(creator), refuse, rec.Code, rec.Body.String())
				}
			}
		})
	}
}

func TestRoleRefusalsNameTheRole(t *testing.T) {
	f := newRoleFixture(t)
	rec := serve(f.h, "POST", "/api/v1/backups", []byte(`{}`), f.as(t, auth.RoleViewer))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `your role (viewer)`) {
		t.Fatalf("viewer starting a backup = %d %s; want 403 naming the role", rec.Code, rec.Body)
	}
	rec = serve(f.h, "POST", "/api/v1/backups", []byte(`{}`), map[string]string{
		"Authorization": "Bearer " + f.userKeys[auth.RoleViewer][auth.ScopeAdmin], "Content-Type": "application/json",
	})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `capped at \"read\" by its creator's role (viewer)`) {
		t.Fatalf("capped key starting a backup = %d %s; want 403 naming the cap", rec.Code, rec.Body)
	}
}

// TestBodyDependentChecksFollowTheRole covers the checks the operations and auth
// services make beyond routeScopes, with real role sessions.
func TestBodyDependentChecksFollowTheRole(t *testing.T) {
	f := newRoleFixture(t)
	ctx := context.Background()
	if err := f.store.SaveBackupRecord(ctx, &models.BackupRecord{ID: "bkp_src", Database: "shop", ConnectionID: testConnID,
		Status: models.StatusCompleted, StorageKey: "shop/src", Pinned: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveRestoreRecord(ctx, &models.RestoreRecord{ID: "rst_inplace", BackupID: "bkp_src", SourceDatabase: "shop",
		TargetDatabase: "shop", Status: models.RestoreStatusInProgress, InPlace: true, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	type want map[auth.Role]func(code int, body string) bool
	refused := func(code int, _ string) bool { return code == http.StatusForbidden }
	refusedWith := func(s string) func(int, string) bool {
		return func(code int, body string) bool { return code == http.StatusForbidden && strings.Contains(body, s) }
	}
	status := func(c int) func(int, string) bool { return func(code int, _ string) bool { return code == c } }
	for _, tc := range []struct {
		name, method, path, body string
		want                     want
	}{
		{"in-place restore", "POST", "/api/v1/restore", `{"backup_id":"bkp_missing","safe_clone":false,"confirm_in_place":true}`,
			want{auth.RoleViewer: refused, auth.RoleOperator: refusedWith("in-place"), auth.RoleAdmin: status(http.StatusNotFound)}},
		{"cross-connection restore", "POST", "/api/v1/restore", `{"backup_id":"bkp_src","target_connection_id":"conn_elsewhere"}`,
			want{auth.RoleViewer: refused, auth.RoleOperator: refusedWith("another connection"), auth.RoleAdmin: status(http.StatusBadRequest)}},
		{"in-place cancel", "POST", "/api/v1/restores/rst_inplace/cancel", `{}`,
			want{auth.RoleViewer: refused, auth.RoleOperator: refusedWith("in-place"), auth.RoleAdmin: func(code int, _ string) bool { return code != http.StatusForbidden }}},
		{"unpin", "POST", "/api/v1/backups/bkp_src/unpin", `{}`,
			want{auth.RoleViewer: refused, auth.RoleOperator: refused, auth.RoleAdmin: status(http.StatusOK)}},
		{"bulk delete", "POST", "/api/v1/backups/bulk", `{"action":"delete","ids":["bkp_none"],"dry_run":true}`,
			want{auth.RoleViewer: refused, auth.RoleOperator: refused, auth.RoleAdmin: status(http.StatusOK)}},
		{"all sessions", "GET", "/api/v1/auth/sessions?all=true", "",
			want{auth.RoleViewer: refused, auth.RoleOperator: refused, auth.RoleAdmin: status(http.StatusOK)}},
		{"own sessions", "GET", "/api/v1/auth/sessions", "",
			want{auth.RoleViewer: status(http.StatusOK), auth.RoleOperator: status(http.StatusOK), auth.RoleAdmin: status(http.StatusOK)}},
		{"user list", "GET", "/api/v1/users", "",
			want{auth.RoleViewer: refused, auth.RoleOperator: refused, auth.RoleAdmin: status(http.StatusOK)}},
		{"user names", "GET", "/api/v1/users/names", "",
			want{auth.RoleViewer: status(http.StatusOK), auth.RoleOperator: status(http.StatusOK), auth.RoleAdmin: status(http.StatusOK)}},
	} {
		for _, role := range auth.Roles() {
			var body []byte
			if tc.body != "" {
				body = []byte(tc.body)
			}
			rec := serve(f.h, tc.method, tc.path, body, f.as(t, role))
			if !tc.want[role](rec.Code, rec.Body.String()) {
				t.Errorf("%s as %s: %d %s", tc.name, role, rec.Code, rec.Body)
			}
		}
	}
	if list, _ := f.store.ListRestoreRecords(ctx); len(list) != 1 {
		t.Fatalf("refused restores must not run: %d records", len(list))
	}

	// The bulk action list marks what the role may do.
	for role, allowed := range map[auth.Role][]string{
		auth.RoleViewer:   {},
		auth.RoleOperator: {"run_now"},
		auth.RoleAdmin:    {"run_now", "delete"},
	} {
		rec := serve(f.h, "GET", "/api/v1/bulk/actions", nil, f.as(t, role))
		var actions []struct {
			Resource string `json:"resource"`
			Name     string `json:"name"`
			Allowed  bool   `json:"allowed"`
		}
		decodeData(t, rec, &actions)
		for _, a := range actions {
			if a.Resource != "jobs" || (a.Name != "run_now" && a.Name != "delete") {
				continue
			}
			if a.Allowed != slices.Contains(allowed, a.Name) {
				t.Errorf("%s: jobs/%s allowed=%v", role, a.Name, a.Allowed)
			}
		}
	}

	// Only administrators see the stored rows that cannot be read.
	if err := f.store.SaveJob(ctx, &models.Job{ID: "job_bad", Name: "bad", Database: "shop", CronExpression: "@daily", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	storetest.CorruptRow(t, f.store, "jobs", "job_bad", `{"id":"job_bad","name":42}`)
	serve(f.h, "GET", "/api/v1/jobs", nil, f.as(t, auth.RoleAdmin))
	for _, role := range auth.Roles() {
		rec := serve(f.h, "GET", "/api/v1/stats", nil, f.as(t, role))
		if got := strings.Contains(rec.Body.String(), "corrupt_records"); got != (role == auth.RoleAdmin) {
			t.Errorf("%s sees corrupt records = %v: %s", role, got, rec.Body)
		}
	}
}

func TestRecoveryKitNeedsTheAdminRole(t *testing.T) {
	f := newKitFixture(t, "")
	admin := f.browser(t)
	admin.setup(f.authFixture)
	for _, role := range []string{"viewer", "operator"} {
		if rec := admin.do("POST", "/api/v1/users", map[string]string{"username": role, "password": testPassword, "role": role}, nil); rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", role, rec.Code, rec.Body)
		}
		b := f.browser(t)
		b.session(b.login(role, testPassword))
		rec := b.do("POST", "/api/v1/recovery-kit", kitBody(testPassword), nil)
		if rec.Code != http.StatusForbidden || rec.Header().Get("Content-Type") == "application/octet-stream" {
			t.Fatalf("%s downloading the recovery kit: %d %s", role, rec.Code, rec.Body)
		}
	}
}

func TestUserRolesOverHTTP(t *testing.T) {
	f := newAuthFixture(t, nil)
	admin := f.browser(t)
	me := admin.setup(f)
	if me.User.Role != auth.RoleAdmin {
		t.Fatalf("setup user role = %q; want admin", me.User.Role)
	}

	// POST /users without a role creates a viewer.
	var vera auth.User
	decodeData(t, admin.do("POST", "/api/v1/users", map[string]string{"username": "vera", "password": testPassword}, nil), &vera)
	if vera.Role != auth.RoleViewer {
		t.Fatalf("user created without a role = %q; want viewer", vera.Role)
	}
	if rec := admin.do("POST", "/api/v1/users", map[string]string{"username": "x", "password": testPassword, "role": "root"}, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown role: %d; want 400", rec.Code)
	}

	// The viewer signs in: /auth/me reports the role and the effective scope.
	b := f.browser(t)
	b.session(b.login("vera", testPassword))
	var mine meResponse
	decodeData(t, b.do("GET", "/api/v1/auth/me", nil, nil), &mine)
	if mine.Role != auth.RoleViewer || mine.Scope != auth.ScopeRead || mine.KeyScope != "" {
		t.Fatalf("viewer me = %+v", mine)
	}
	if rec := b.do("POST", "/api/v1/backups", map[string]string{"connection_id": testConnID, "database": "shop"}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer starting a backup: %d; want 403", rec.Code)
	}
	// Viewers may create read keys only, see only their own, and change their own password.
	if rec := b.do("POST", "/api/v1/api-keys", map[string]string{"name": "too much", "scope": "operator"}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer creating an operator key: %d %s; want 403", rec.Code, rec.Body)
	}
	var created createdAPIKey
	decodeData(t, b.do("POST", "/api/v1/api-keys", map[string]string{"name": "vera's", "scope": "read"}, nil), &created)
	var keyMe meResponse
	decodeData(t, serve(f.h, "GET", "/api/v1/auth/me", nil, map[string]string{"X-API-Key": created.Key}), &keyMe)
	if keyMe.Auth != auth.MethodAPIKey || keyMe.Role != auth.RoleViewer || keyMe.Scope != auth.ScopeRead || keyMe.KeyScope != auth.ScopeRead {
		t.Fatalf("key me = %+v", keyMe)
	}
	if rec := admin.do("POST", "/api/v1/api-keys", map[string]string{"name": "admin's", "scope": "admin"}, nil); rec.Code != http.StatusCreated {
		t.Fatalf("admin creating a key: %d", rec.Code)
	}
	var keys []map[string]any
	decodeData(t, b.do("GET", "/api/v1/api-keys", nil, nil), &keys)
	if len(keys) != 1 || keys[0]["name"] != "vera's" || keys[0]["effective_scope"] != "read" {
		t.Fatalf("viewer key list = %v; want only their own, with effective_scope", keys)
	}

	// PUT /users/{id}/role: 400, 404, 409 and the change itself.
	path := "/api/v1/users/" + vera.ID + "/role"
	if rec := admin.do("PUT", path, map[string]string{"role": "root"}, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown role: %d; want 400", rec.Code)
	}
	if rec := admin.do("PUT", "/api/v1/users/usr_missing/role", map[string]string{"role": "operator"}, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown user: %d; want 404", rec.Code)
	}
	if rec := admin.do("PUT", "/api/v1/users/"+me.User.ID+"/role", map[string]string{"role": "viewer"}, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("own role: %d; want 400", rec.Code)
	}
	if rec := b.do("PUT", "/api/v1/users/"+me.User.ID+"/role", map[string]string{"role": "viewer"}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer changing a role: %d; want 403", rec.Code)
	}
	var promoted auth.User
	decodeData(t, admin.do("PUT", path, map[string]string{"role": "admin"}, nil), &promoted)
	if promoted.Role != auth.RoleAdmin {
		t.Fatalf("promoted = %+v", promoted)
	}
	// The role change ended vera's session.
	if rec := b.do("GET", "/api/v1/jobs", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("vera's session after the role change: %d; want 401", rec.Code)
	}
	var users []auth.User
	decodeData(t, admin.do("GET", "/api/v1/users", nil, nil), &users)
	for _, u := range users {
		if u.Role == "" {
			t.Fatalf("user list without roles: %+v", users)
		}
	}
	// As an admin, vera may demote and then delete the setup admin.
	b.session(b.login("vera", testPassword))
	if rec := b.do("PUT", "/api/v1/users/"+me.User.ID+"/role", map[string]string{"role": "viewer"}, nil); rec.Code != http.StatusOK {
		t.Fatalf("vera demoting the setup admin: %d %s", rec.Code, rec.Body)
	}
	b.session(b.login("vera", testPassword))
	if rec := b.do("DELETE", "/api/v1/users/"+me.User.ID, nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("vera deleting the demoted admin: %d %s", rec.Code, rec.Body)
	}
	var carol auth.User
	decodeData(t, b.do("POST", "/api/v1/users", map[string]string{"username": "carol", "password": testPassword, "role": "operator"}, nil), &carol)
	if rec := b.do("PUT", "/api/v1/users/"+carol.ID+"/role", map[string]string{"role": "viewer"}, nil); rec.Code != http.StatusOK {
		t.Fatalf("demoting carol: %d %s", rec.Code, rec.Body)
	}
	// vera is now the last admin.
	if _, err := f.auth.SetUserRole(context.Background(), auth.SystemPrincipal(), promoted.ID, auth.RoleViewer); !errors.Is(err, auth.ErrLastAdmin) {
		t.Fatalf("demoting the last admin: %v; want ErrLastAdmin", err)
	}
}

func TestLastAdminConflictOverHTTP(t *testing.T) {
	const static = "role-static-api-key-0001"
	f := newAuthFixture(t, func(c *testConfig) { c.APIKey = static })
	me := f.browser(t).setup(f)
	api := map[string]string{"X-API-Key": static, "Content-Type": "application/json"}
	rec := serve(f.h, "POST", "/api/v1/users", []byte(`{"username":"bob","password":"`+testPassword+`"}`), api)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create bob: %d %s", rec.Code, rec.Body)
	}
	rec = serve(f.h, "PUT", "/api/v1/users/"+me.User.ID+"/role", []byte(`{"role":"operator"}`), api)
	if rec.Code != http.StatusConflict {
		t.Fatalf("demoting the last admin: %d %s; want 409", rec.Code, rec.Body)
	}
	if rec = serve(f.h, "DELETE", "/api/v1/users/"+me.User.ID, nil, api); rec.Code != http.StatusConflict {
		t.Fatalf("deleting the last admin: %d %s; want 409", rec.Code, rec.Body)
	}
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if !strings.Contains(body.Error, "last administrator") {
		t.Fatalf("409 body = %s", rec.Body)
	}
}
