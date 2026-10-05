# Design: dashboard roles

Status: implemented in v0.17.0 (#52).

MongoRescue users used to be administrators without exception. Dashboard roles let
an administrator give people less: a **viewer** sees everything and changes nothing,
an **operator** also runs backups and safe-clone restores, an **admin** may do
everything. "Roles" already means MongoDB users and roles inside dumps, so the
concept is called the *dashboard role* in documentation and the field is `role`.

## Roles and scopes

A role maps to one of the three existing API key scopes, so the whole permission
model stays the one routes and MCP tools already declare:

| Role | Scope | May |
| :--- | :--- | :--- |
| `viewer` | `read` | every `GET` of the REST API except the user list and the audit logs; the read-only MCP tools |
| `operator` | `operator` | also start, retry and cancel backups, run and stop jobs, restore into safe clones of the backup's own connection, preflight, verify, pin, restore-test |
| `admin` | `admin` | everything: users, settings, connections, storage targets, notifications, deletions, in-place and cross-connection restores, unpinning, the recovery kit, the audit log |

`auth.Role` is the type; `Role.Scope()` gives the scope, and an unknown or empty role
gives `read` (fail closed). `auth.ParseRole` refuses unknown names with
`auth.ErrInvalidRole`.

## Storage

Migration `0019_user_roles.sql` adds `users.role TEXT NOT NULL DEFAULT 'admin'`
with `CHECK (role IN ('viewer', 'operator', 'admin'))`. Every user stored before the
migration was an administrator and keeps that role. The store always binds the role
of a new user and refuses an empty one (`ErrInvalidRole`), so the column default can
never grant admin to a user created later. The upgrade-compatibility test has a step
for schema 19.

Downgrades: a release before v0.17.0 refuses a database at schema 19
(`store.ErrSchemaTooNew`), like every older binary refuses a newer schema. Restore a
metadata snapshot taken before the upgrade to go back.

## One enforcement point

Authorization stays in `internal/auth`. `Principal.Scope` is the **effective** scope
and is the only thing the rest of the code checks (`Principal.Require`,
`auth.RequireScope`, the route table, MCP tools). `Principal.Role` and
`Principal.KeyScope` say where it came from. `effectiveScope` computes it:

- a session: the scope of the user's role;
- an API key with a creator: the lower of the key's scope and the creator's
  *current* role, recomputed on every request, so demoting a user caps their keys
  at once and promoting them lifts the cap;
- an API key without a creator (imported from `MONGORESCUE_API_KEY`, or created by
  the system): the key's scope;
- the system: admin.

Its return type is a struct (`auth.Access`) so that finer limits can join the scope
without changing callers: per-connection access (below) is the first.

`*auth.ScopeError` carries a `Source` (`role`, `key` or `key_capped_by_role`), the
role and the key's scope, so refusals read correctly: "your role (viewer) has the
"read" scope; this request needs "operator"", or "this API key has the "admin"
scope, capped at "read" by its creator's role (viewer); ...". The operations
service's own checks now say "needs the admin role or an admin API key".

Because the route table, the operations service's body-dependent checks (in-place
and cross-connection restores, cancelling an in-place restore, unpinning, bulk
actions and their `allowed` flags), the recovery kit, `GET /api/v1/auth/sessions?all`,
the `corrupt_records` of `GET /api/v1/stats` and the MCP tool scopes all read
`Principal.Scope`, they follow the role without further changes.

Defence in depth: `Service.CreateUser`, `DeleteUser`, `SetUserRole`, `ListUsers` and
changing another user's password call `actor.Require(ScopeAdmin)` in the service, not
only in the route table.

## Per-connection access

Status: implemented (#98).

A user or an API key reaches every connection (the default, and the only access
before) or only some. Roles stay global; the connection limit narrows what the role
may do to a set of connections.

### Model

- `users.connection_ids` and `api_keys.connection_ids` (migration
  `0024_connection_access.sql`, JSON arrays, `[]` for every connection) hold the
  limits. Every user and key stored before the migration comes out unlimited.
- Administrators are always unlimited: a limit on an admin user, an admin-scope key
  or an admin group mapping is refused (`auth.ErrAdminConnections`), and promoting a
  user to admin clears the limit in the same transaction.
- A key is capped by its creator like its scope: `effectiveConnections` intersects
  the key's own list with the creator's *current* connections on every request, so
  limiting a user limits their keys at once, and a key created without a list by a
  limited user reaches the user's connections. A list naming a connection outside
  the creator's is refused (`auth.ErrConnectionsExceedAccess`). Keys without a
  creator (imported) and the system are unlimited.
- `effectiveAccess` returns the scope and the connections (`auth.ConnectionSet`,
  nil for every connection) in `auth.Access`, and caps a limited caller at
  `operator`: users, settings, connections, storage targets and the audit logs are
  global, so a limited caller is never an admin.
- Deleting a connection does not shorten the lists, so a limit never widens to every
  connection by itself.

### One enforcement point

`Principal.Connections` sits next to `Principal.Scope`; `Principal.AllowsConnection`
and `auth.ConnectionFilter(ctx)` (every connection for a context without a
principal: the application itself) are the only rule. It is applied where the data
is read, so every adapter inherits it:

- the connections service filters its repository (`accessRepo`): lists, details,
  tests, database and collection listings, and every `Resolve` the operations
  service does for a backup, a restore or a preview;
- the operations service reads jobs, backups and restores through a view of the
  caller's connections (`accessStore`): details, lists (with SQL filters, so paging
  and totals stay exact), statistics, the history, logs, retries, pins, verification,
  restores and preflights, job runs and previews, bulk actions, active runs and
  pending changes. A record outside the caller's connections reads as
  `store.ErrNotFound`. A restore is visible when both its source and its target
  connection are;
- readiness and restore tests check the job's connection (and the restore test
  server's), the storage targets the connections their jobs and backups use plus the
  default target.

A connection outside the caller's access answers `404`, worded like an unknown ID,
so its existence is not leaked; for a limited caller an unknown connection named in
a body answers `404` too, where an unlimited caller gets the usual `400`. Bulk items
are skipped as `not_found`. Storage scans and the integrity sweep cover every
connection's archives and are not shown to a limited caller.

**Metrics** are refused (`403`, `auth.ErrConnectionsLimited`) rather than filtered:
the Prometheus registry is global and its series carry job and database labels, and
a filtering gatherer would have to know which label of which family names which
connection. A limited team scrapes through an unlimited read key or not at all.

**The audit view** stays admin-only: limited callers are never admins, so they see
no audit rows at all. A per-connection audit view for operators would need audit
records keyed by connection, which they are not; it is out of scope.

### OIDC

A group mapping may carry `connection_ids`. A user's connections are the union over
every matching mapping, or every connection when one matching mapping has none, when
only the default role applies, or when the role is admin; they are recomputed with
the role at every sign-in and cannot be changed by hand while mappings exist.
Changing connections is never an admin grant, so the two-person rule does not hold
it, not even when it widens someone's access to every connection.

### API and dashboard

`PUT /api/v1/users/{id}/connections` (admin), `connection_ids` in `POST
/api/v1/users` and `POST /api/v1/api-keys`, `connection_ids` in `GET
/api/v1/auth/me` for a limited caller and `effective_connection_ids` in the key list.
The dashboard (`access.js`) adds a Connections column with an editor to the users
table, checklists to the Add user and Create API key dialogs (admins only), a chip
for limited keys, a connection select per group mapping and the caller's
connections in the user menu; everything else follows from the filtered API.

### Tests

`TestConnectionAccessIsEnforcedForEveryRoute` walks every registered route with an
operator session and an operator key limited to connection A: a route naming B in
its path, query or body answers `404` like an unknown record, one naming A gets the
handler's answer, a bulk item of B is skipped, admin routes are refused by scope,
and no answer contains an identifier of B. `TestEveryRouteIsClassifiedForConnectionAccess`
fails for a new route with an `{id}` that is not classified. Further tests cover
lists and filters, restores into B, user and key limits over HTTP, key capping,
the admin refusal, OIDC mappings, MCP tools, the CLI, the migration and a browser
spec.

## API keys

`CreateAPIKey` refuses a scope above the creator's own effective scope with
`auth.ErrScopeExceedsRole` (403). Keys are created by a signed-in user of any role, or
by an API key with the admin scope. `GET /api/v1/api-keys` adds `effective_scope`
(the key's scope capped by its creator's role); non-admins see only the keys they
created.

## Self-service routes

Six routes need only `read` in the route table because every role uses them on its
own account; the auth service applies the finer rule (`selfServiceRoutes` in
`internal/server/scopes.go`, which the route tests know about):

| Route | Rule |
| :--- | :--- |
| `POST /api/v1/auth/logout` | ends the request's own session |
| `PUT /api/v1/users/{id}/password` | your own: a session and the current password; another user's: admin |
| `DELETE /api/v1/auth/sessions/{id}` | from a session: your own user's sessions; another user's: admin; an API key below admin: none, not even its creator's |
| `GET /api/v1/api-keys` | non-admins see only the keys they created |
| `POST /api/v1/api-keys` | a session, or an admin-scope key; the scope may not exceed the caller's |
| `DELETE /api/v1/api-keys/{id}` | from a session: the keys your user created; an API key below admin: only itself, never its creator's other keys; admin: any; an unknown ID is 404 for everyone |

Every other route keeps its scope, including the settings `GET` (read, secrets
masked), live database and collection listings, target scans and run logs (read),
preflight and restore tests (operator), pin (operator) and unpin (admin), the
metadata backup run and integrity sweep (admin), and dismissing warnings (admin).

## User administration

- `POST /api/v1/users` takes `{username, password, role}`. An omitted role creates a
  **viewer** (it used to create an administrator: a breaking change).
- Setup always creates an admin.
- `PUT /api/v1/users/{id}/role` (admin) takes `{role}`: `400` for an unknown role or
  your own user (`ErrChangeOwnRole`), `404` for an unknown user, `409` when it would
  demote the last admin (`ErrLastAdmin`).
- `DELETE /api/v1/users/{id}` of the last admin answers `409` (`ErrLastAdmin`).
- `GET /api/v1/users` includes `role`; `GET /api/v1/users/names` (read) returns only
  `[{id, username}]`, so dashboards of every role can show who created or pinned
  something.
- `GET /api/v1/auth/me` adds `role`, `scope` (effective) and, for API keys,
  `key_scope`.

`UpdateUserRole` runs in one transaction: it checks that the acting user is still an
admin (a stale principal of a demoted admin is refused), refuses to demote the last
admin, updates the role and deletes the user's sessions. `DeleteUser` checks in its transaction that the acting user is still an admin too. SQLite write transactions
are serialized (`_txlock=immediate`, one connection), so when two admins demote each
other at the same moment exactly one succeeds; a test runs that race.

## Audit

The audit log annotates a role change with `role_from` and `role_to`, a user creation
with `role`, and a key creation with `scope` and `ceiling_applied` (`true` when the
key has a creator, so that user's role caps it from then on).

## Dashboard

`setAuth` keeps the role and the effective scope from `/auth/me` and offers `can(scope)`.
`roles.js` maps every `data-action` to the scope of the route it calls, disables what
the role may not do (tooltip "Your role (Viewer) can only view"), hides the users
and audit log sections and the MCP activity from non-admins, makes settings forms
read-only below admin, offers only key scopes up to the user's own, and shows the
effective scope of capped keys. The users table has a role column with a select for
administrators, disabled for yourself and the last admin, which asks before a
demotion. The server stays authoritative: the UI only reflects what it enforces.

## Tests

- `TestRolesAreEnforcedForEveryRoute` walks every route with a session of every role
  (with its CSRF token) and API keys of every scope created by users of every role,
  expecting what `requiredScope` and the effective scope say, with explicit
  expectations for `selfServiceRoutes`; `TestEveryRouteHasAScope` knows them too.
- Body-dependent checks per role: in-place and cross-connection restores, cancelling
  an in-place restore, unpinning, bulk actions and `allowed`, the recovery kit,
  `sessions?all` and `corrupt_records`.
- MCP: a key whose creator was demoted to viewer lists only read tools.
- Browser: a viewer sees no usable destructive control, and a forged POST gets 403.
