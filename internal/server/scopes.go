package server

import (
	"net/http"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

// routeScopes is the single table of the API key scope every authenticated route
// requires, keyed by its ServeMux pattern. The auth middleware looks the matched
// pattern up for every request; a route missing from the table requires admin, and a
// test fails when a registered route is missing or an entry is stale.
//
// read covers every GET except the audit log and the user list (read keys must not
// enumerate usernames); operator adds starting backups, running jobs and safe-clone
// restores into the backup's own connection (in-place and cross-connection restores
// need admin, which the operations service enforces because it depends on the request
// body); everything else is admin.
// Public routes (publicPaths) and the dashboard assets need no credentials at all.
var routeScopes = map[string]auth.Scope{
	"POST /api/v1/auth/logout": auth.ScopeAdmin,
	"GET " + meRoute:           auth.ScopeRead,

	"GET /api/v1/users":                  auth.ScopeAdmin,
	"POST /api/v1/users":                 auth.ScopeAdmin,
	"DELETE /api/v1/users/{id}":          auth.ScopeAdmin,
	"PUT /api/v1/users/{id}/password":    auth.ScopeAdmin,
	"GET /api/v1/api-keys":               auth.ScopeRead,
	"POST /api/v1/api-keys":              auth.ScopeAdmin,
	"DELETE /api/v1/api-keys/{id}":       auth.ScopeAdmin,
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

	"GET /api/v1/storage-targets":               auth.ScopeRead,
	"POST /api/v1/storage-targets":              auth.ScopeAdmin,
	"POST /api/v1/storage-targets/test":         auth.ScopeAdmin,
	"GET /api/v1/storage-targets/{id}":          auth.ScopeRead,
	"PUT /api/v1/storage-targets/{id}":          auth.ScopeAdmin,
	"DELETE /api/v1/storage-targets/{id}":       auth.ScopeAdmin,
	"POST /api/v1/storage-targets/{id}/test":    auth.ScopeAdmin,
	"POST /api/v1/storage-targets/{id}/default": auth.ScopeAdmin,

	"GET /api/v1/stats": auth.ScopeRead,

	"GET /api/v1/jobs":            auth.ScopeRead,
	"POST /api/v1/jobs":           auth.ScopeAdmin,
	"DELETE /api/v1/jobs/{id}":    auth.ScopeAdmin,
	"POST /api/v1/jobs/{id}/run":  auth.ScopeOperator,
	"GET /api/v1/backups":         auth.ScopeRead,
	"POST /api/v1/backups":        auth.ScopeOperator,
	"DELETE /api/v1/backups/{id}": auth.ScopeAdmin,
	"GET /api/v1/restores":        auth.ScopeRead,
	"POST /api/v1/restore":        auth.ScopeOperator,

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
	// MCP needs at least read; each tool then requires its own scope.
	"POST " + MCPPath:   auth.ScopeRead,
	"GET " + MCPPath:    auth.ScopeRead,
	"DELETE " + MCPPath: auth.ScopeRead,
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
