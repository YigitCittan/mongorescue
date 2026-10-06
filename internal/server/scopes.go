package server

import (
	"net/http"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

// routeScopes is the single table of the scope every authenticated route requires,
// keyed by its ServeMux pattern. The scope is the caller's effective scope: an API
// key's scope (capped by its creator's dashboard role) or a signed-in user's role
// (viewer read, operator operator, admin admin). The auth middleware looks the
// matched pattern up for every request; a route missing from the table requires
// admin, and a test fails when a registered route is missing or an entry is stale.
//
// read covers every GET except the audit log and the user list (only the IDs and
// names of users are readable, through /api/v1/users/names), and the
// selfServiceRoutes, whose finer rules the auth service enforces; operator adds
// starting, retrying and cancelling backups,
// running jobs, cancelling restores that are not in place and safe-clone restores into the backup's own connection (in-place and cross-connection
// restores need admin, which the operations service enforces because it depends on the
// request body); everything else is admin.
// Public routes (publicPaths) and the dashboard assets need no credentials at all.
var routeScopes = map[string]auth.Scope{
	logoutRoute:      auth.ScopeRead,
	"GET " + meRoute: auth.ScopeRead,
	// Without ?all=true a key sees only its creator's sessions; all needs admin,
	// which the auth service enforces.
	"GET /api/v1/auth/sessions": auth.ScopeRead,
	revokeSessionRoute:          auth.ScopeRead,

	"GET /api/v1/users":         auth.ScopeAdmin,
	userNamesRoute:              auth.ScopeRead,
	"POST /api/v1/users":        auth.ScopeAdmin,
	"DELETE /api/v1/users/{id}": auth.ScopeAdmin,
	userRoleRoute:               auth.ScopeAdmin,
	userConnectionsRoute:        auth.ScopeAdmin,
	changePasswordRoute:         auth.ScopeRead,
	listAPIKeysRoute:            auth.ScopeRead,
	createAPIKeyRoute:           auth.ScopeRead,
	deleteAPIKeyRoute:           auth.ScopeRead,

	"GET /api/v1/connections":            auth.ScopeRead,
	"POST /api/v1/connections":           auth.ScopeAdmin,
	"POST /api/v1/connections/test":      auth.ScopeAdmin,
	"GET /api/v1/connections/{id}":       auth.ScopeRead,
	"PUT /api/v1/connections/{id}":       auth.ScopeAdmin,
	"DELETE /api/v1/connections/{id}":    auth.ScopeAdmin,
	"POST /api/v1/connections/{id}/test": auth.ScopeAdmin,

	"GET /api/v1/connections/{id}/databases":                  auth.ScopeRead,
	"GET /api/v1/connections/{id}/databases/{db}/collections": auth.ScopeRead,

	"GET /api/v1/settings":                          auth.ScopeRead,
	"PUT /api/v1/settings":                          auth.ScopeAdmin,
	"POST /api/v1/settings/encryption/generate-key": auth.ScopeAdmin,
	"POST /api/v1/settings/warnings/{id}/dismiss":   auth.ScopeAdmin,
	heartbeatTestRoute:                              auth.ScopeAdmin,
	oidcTestRoute:                                   auth.ScopeAdmin,

	"GET /api/v1/storage-targets":                          auth.ScopeRead,
	"POST /api/v1/storage-targets":                         auth.ScopeAdmin,
	"POST /api/v1/storage-targets/test":                    auth.ScopeAdmin,
	"GET /api/v1/storage-targets/{id}":                     auth.ScopeRead,
	"PUT /api/v1/storage-targets/{id}":                     auth.ScopeAdmin,
	"DELETE /api/v1/storage-targets/{id}":                  auth.ScopeAdmin,
	"POST /api/v1/storage-targets/{id}/test":               auth.ScopeAdmin,
	"POST /api/v1/storage-targets/{id}/default":            auth.ScopeAdmin,
	"POST /api/v1/storage-targets/{id}/rotate-credentials": auth.ScopeAdmin,

	"GET /api/v1/stats": auth.ScopeRead,
	// The dashboard overview (SQL aggregates) and the cron builder's preview.
	"GET /api/v1/stats/history":    auth.ScopeRead,
	"GET /api/v1/schedule/preview": auth.ScopeRead,
	// Recovery readiness per database (no credentials, no URIs).
	"GET /api/v1/readiness": auth.ScopeRead,

	"GET /api/v1/jobs":           auth.ScopeRead,
	"POST /api/v1/jobs":          auth.ScopeAdmin,
	"GET /api/v1/jobs/{id}":      auth.ScopeRead,
	"PUT /api/v1/jobs/{id}":      auth.ScopeAdmin,
	"DELETE /api/v1/jobs/{id}":   auth.ScopeAdmin,
	"POST /api/v1/jobs/{id}/run": auth.ScopeOperator,
	// Multi-database jobs: previewing a selection lists the connection's database
	// names (like GET /api/v1/connections/{id}/databases, read); stopping a job's
	// run is cancelling its backups (operator, like POST /api/v1/backups/{id}/cancel).
	"GET /api/v1/jobs/{id}/databases/preview": auth.ScopeRead,
	"GET /api/v1/jobs/databases/preview":      auth.ScopeRead,
	"GET /api/v1/jobs/{id}/runs":              auth.ScopeRead,
	"POST /api/v1/jobs/{id}/cancel":           auth.ScopeOperator,
	"GET /api/v1/backups":                     auth.ScopeRead,
	"GET /api/v1/backups/databases":           auth.ScopeRead,
	"POST /api/v1/backups":                    auth.ScopeOperator,
	"DELETE /api/v1/backups/{id}":             auth.ScopeAdmin,
	"POST /api/v1/backups/{id}/retry":         auth.ScopeOperator,
	"GET /api/v1/backups/{id}/collections":    auth.ScopeRead,
	"GET /api/v1/restores":                    auth.ScopeRead,
	"GET /api/v1/restores/databases":          auth.ScopeRead,
	"POST /api/v1/restore":                    auth.ScopeOperator,
	// A preflight applies the scope rules of the restore it checks (in-place and
	// cross-connection need admin, enforced by the operations service).
	"POST /api/v1/restores/preflight": auth.ScopeOperator,

	// Run control. Cancelling an in-place restore needs admin (checked by the
	// operations service, which knows the restore).
	"POST /api/v1/backups/{id}/cancel":  auth.ScopeOperator,
	"POST /api/v1/restores/{id}/cancel": auth.ScopeOperator,
	"GET /api/v1/backups/{id}/log":      auth.ScopeRead,
	"GET /api/v1/restores/{id}/log":     auth.ScopeRead,
	"GET /api/v1/runs/active":           auth.ScopeRead,

	// Verifying, pinning and restore-testing only read archives, protect a backup or
	// write into a temporary database the test drops again: operator. Unpinning
	// lifts a legal hold and makes the backup deletable: admin. Sweeps, storage scans
	// (which mark records missing) and imports (which create records): admin.
	"POST /api/v1/backups/{id}/verify":         auth.ScopeOperator,
	"POST /api/v1/backups/{id}/pin":            auth.ScopeOperator,
	"POST /api/v1/backups/{id}/unpin":          auth.ScopeAdmin,
	"GET /api/v1/jobs/{id}/retention/preview":  auth.ScopeRead,
	"GET /api/v1/jobs/{id}/retention/log":      auth.ScopeRead,
	"POST /api/v1/jobs/{id}/restore-test":      auth.ScopeOperator,
	"GET /api/v1/jobs/{id}/restore-tests":      auth.ScopeRead,
	"GET /api/v1/integrity":                    auth.ScopeRead,
	"POST /api/v1/integrity/sweep":             auth.ScopeAdmin,
	"GET /api/v1/storage-targets/{id}/scan":    auth.ScopeRead,
	"POST /api/v1/storage-targets/{id}/scan":   auth.ScopeAdmin,
	"POST /api/v1/storage-targets/{id}/import": auth.ScopeAdmin,

	// Metadata backups: the status is read; taking a snapshot is admin. The recovery
	// kit carries every secret: admin, and the handler also requires a signed-in
	// user who confirms their password (API keys are refused by the auth service).
	// PITR streams (experimental): reading them is read, configuring them admin;
	// taking a base backup now is operator, like starting a backup.
	pitrStreamsRoute:      auth.ScopeRead,
	pitrStreamRoute:       auth.ScopeRead,
	pitrChunksRoute:       auth.ScopeRead,
	pitrCreateStreamRoute: auth.ScopeAdmin,
	pitrUpdateStreamRoute: auth.ScopeAdmin,
	pitrDeleteStreamRoute: auth.ScopeAdmin,
	pitrBaseRoute:         auth.ScopeOperator,
	// A chain test restores into temporary clones: admin, like PITR restores.
	pitrChainTestRoute: auth.ScopeAdmin,

	"GET /api/v1/metadata-backup":      auth.ScopeRead,
	"POST /api/v1/metadata-backup/run": auth.ScopeAdmin,
	"GET /api/v1/recovery-kit":         auth.ScopeRead,
	"POST /api/v1/recovery-kit":        auth.ScopeAdmin,

	// Key rotation: secret.key additionally needs a session and the password.
	"GET /api/v1/security/key-rotation":       auth.ScopeRead,
	"POST /api/v1/security/rotate-secret-key": auth.ScopeAdmin,
	"POST /api/v1/encryption/rotate":          auth.ScopeAdmin,
	"GET /api/v1/encryption/reencryption":     auth.ScopeRead,

	// Bulk endpoints need the lowest scope of their actions (operator: running jobs
	// now; verifying, pinning and cancelling plug in here too). The operations service
	// then requires the scope of the requested action, mirroring its single-item
	// route: deleting backups, restore records and jobs, and enabling or disabling
	// jobs, need admin.
	"GET /api/v1/bulk/actions":   auth.ScopeRead,
	"POST /api/v1/backups/bulk":  auth.ScopeOperator,
	"POST /api/v1/restores/bulk": auth.ScopeOperator,
	"POST /api/v1/jobs/bulk":     auth.ScopeOperator,

	// Delete protection: undeleting a backup, deciding approval requests and
	// cancelling a pending change (which keeps a protection) are admin; approving
	// also needs a signed-in administrator other than the requester (enforced by the
	// auth rules the operations service applies), so admin API keys can request and
	// reject but never approve. The pending changes are readable, like the settings.
	undeleteRoute:      auth.ScopeAdmin,
	listApprovalsRoute: auth.ScopeAdmin,
	getApprovalRoute:   auth.ScopeAdmin,
	approveRoute:       auth.ScopeAdmin,
	rejectRoute:        auth.ScopeAdmin,
	listPendingRoute:   auth.ScopeRead,
	cancelPendingRoute: auth.ScopeAdmin,

	"GET /api/v1/notifications/channels":            auth.ScopeRead,
	"POST /api/v1/notifications/channels":           auth.ScopeAdmin,
	"PUT /api/v1/notifications/channels/{id}":       auth.ScopeAdmin,
	"DELETE /api/v1/notifications/channels/{id}":    auth.ScopeAdmin,
	"POST /api/v1/notifications/channels/{id}/test": auth.ScopeAdmin,
	"GET /api/v1/notifications/rules":               auth.ScopeRead,
	"POST /api/v1/notifications/rules":              auth.ScopeAdmin,
	"PUT /api/v1/notifications/rules/{id}":          auth.ScopeAdmin,
	"DELETE /api/v1/notifications/rules/{id}":       auth.ScopeAdmin,

	"GET /metrics":      auth.ScopeRead,
	"GET /api/v1/audit": auth.ScopeAdmin,
	// The audit log of every action, its export and verification: admin.
	auditListRoute:   auth.ScopeAdmin,
	auditExportRoute: auth.ScopeAdmin,
	auditVerifyRoute: auth.ScopeAdmin,
	// MCP needs at least read; each tool then requires its own scope.
	"POST " + MCPPath:   auth.ScopeRead,
	"GET " + MCPPath:    auth.ScopeRead,
	"DELETE " + MCPPath: auth.ScopeRead,
}

// Self-service routes: every signed-in user acts on their own account through them.
//
//nolint:gosec // G101: route patterns, not credentials.
const (
	logoutRoute         = "POST /api/v1/auth/logout"
	revokeSessionRoute  = "DELETE /api/v1/auth/sessions/{id}"
	changePasswordRoute = "PUT /api/v1/users/{id}/password"
	listAPIKeysRoute    = "GET /api/v1/api-keys"
	createAPIKeyRoute   = "POST /api/v1/api-keys"
	deleteAPIKeyRoute   = "DELETE /api/v1/api-keys/{id}"
)

// User routes beyond the self-service ones.
const (
	userNamesRoute = "GET /api/v1/users/names"
	userRoleRoute  = "PUT /api/v1/users/{id}/role"
	// userConnectionsRoute limits a user to some connections (admin).
	userConnectionsRoute = "PUT /api/v1/users/{id}/connections"
)

// selfServiceRoutes need only the read scope in routeScopes, because every role
// may use them on its own account; the auth service then applies the finer rule:
//   - logout ends the request's own session;
//   - changing your own password needs a session and the current password, another
//     user's needs admin;
//   - ending your own session is allowed, another user's needs admin;
//   - non-admins list only the API keys they created;
//   - creating a key needs a session or an admin key, and a scope no higher than
//     the caller's own;
//   - revoking your own key is allowed, another user's needs admin.
var selfServiceRoutes = []string{
	logoutRoute, changePasswordRoute, revokeSessionRoute, listAPIKeysRoute, createAPIKeyRoute, deleteAPIKeyRoute,
}

// passwordChangeRoutes are the only routes a session may use while its user must
// choose a new password (auth.Principal.PasswordChangeRequired): changing it (the
// auth service refuses another user's), signing out and reading the session.
var passwordChangeRoutes = map[string]bool{
	changePasswordRoute: true,
	logoutRoute:         true,
	"GET " + meRoute:    true,
}

// requiredScope returns the scope pattern requires; unknown patterns require admin.
func requiredScope(pattern string) auth.Scope {
	if s, ok := routeScopes[pattern]; ok {
		return s
	}
	return auth.ScopeAdmin
}

// router registers routes on a ServeMux and records their patterns, so tests can
// check that every route has an entry in routeScopes.
type router struct {
	mux      *http.ServeMux
	patterns []string
}

// HandleFunc registers h for pattern.
func (rt *router) HandleFunc(pattern string, h http.HandlerFunc) {
	rt.Handle(pattern, h)
}

// Handle registers h for pattern.
func (rt *router) Handle(pattern string, h http.Handler) {
	rt.mux.Handle(pattern, h)
	rt.patterns = append(rt.patterns, pattern)
}
