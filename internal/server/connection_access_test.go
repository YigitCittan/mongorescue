package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/integrity"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/pitr/collector"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

// The two sides of the connection access fixture: connection A (testConnID) is
// the limited caller's, connection B another team's. Each has a job, a completed
// backup, a restore and a storage target; B's database is "billing".
const (
	accessConnB = "conn_team_b"
	accessJobA  = "job_team_a"
	accessJobB  = "job_team_b"
	accessBkpA  = "bkp_team_a"
	accessBkpB  = "bkp_team_b"
	accessRstA  = "rst_team_a"
	accessRstB  = "rst_team_b"
	accessTgtA  = "tgt_team_a"
	accessTgtB  = "tgt_team_b"
	accessPstA  = "pst_team_a"
	accessPstB  = "pst_team_b"
	accessDBB   = "billing"
	accessNameB = "Team B server"
)

// accessMarkers are B's identifiers: no answer to a caller limited to A may contain
// one, except an ID the request itself named.
var accessMarkers = []string{accessConnB, accessJobB, accessBkpB, accessRstB, accessTgtB, accessPstB, accessDBB, accessNameB}

// accessKind is the kind of record a route's {id} names.
type accessKind string

const (
	kindConnection accessKind = "connection"
	kindTarget     accessKind = "target"
	kindJob        accessKind = "job"
	kindBackup     accessKind = "backup"
	kindRestore    accessKind = "restore"
	kindStream     accessKind = "pitr stream"
)

// accessID returns A's (side 0) or B's (side 1) record of kind. (A switch, not a
// map: with -race, Go 1.26.0 read the wrong entry of a map of [2]string here.)
func accessID(kind accessKind, side int) string {
	var ids [2]string
	switch kind {
	case kindConnection:
		ids = [2]string{testConnID, accessConnB}
	case kindTarget:
		ids = [2]string{accessTgtA, accessTgtB}
	case kindJob:
		ids = [2]string{accessJobA, accessJobB}
	case kindBackup:
		ids = [2]string{accessBkpA, accessBkpB}
	case kindRestore:
		ids = [2]string{accessRstA, accessRstB}
	case kindStream:
		ids = [2]string{accessPstA, accessPstB}
	}
	return ids[side]
}

// accessPathRoutes are the routes whose path names a connection, or a record of
// one; the matrix calls them with A's record and with B's.
var accessPathRoutes = map[string]accessKind{
	"GET /api/v1/connections/{id}":                            kindConnection,
	"PUT /api/v1/connections/{id}":                            kindConnection,
	"DELETE /api/v1/connections/{id}":                         kindConnection,
	"POST /api/v1/connections/{id}/test":                      kindConnection,
	"GET /api/v1/connections/{id}/databases":                  kindConnection,
	"GET /api/v1/connections/{id}/databases/{db}/collections": kindConnection,

	"GET /api/v1/storage-targets/{id}":                     kindTarget,
	"PUT /api/v1/storage-targets/{id}":                     kindTarget,
	"DELETE /api/v1/storage-targets/{id}":                  kindTarget,
	"POST /api/v1/storage-targets/{id}/test":               kindTarget,
	"POST /api/v1/storage-targets/{id}/default":            kindTarget,
	"POST /api/v1/storage-targets/{id}/rotate-credentials": kindTarget,
	"GET /api/v1/storage-targets/{id}/scan":                kindTarget,
	"POST /api/v1/storage-targets/{id}/scan":               kindTarget,
	"POST /api/v1/storage-targets/{id}/import":             kindTarget,

	"GET /api/v1/jobs/{id}":                   kindJob,
	"PUT /api/v1/jobs/{id}":                   kindJob,
	"DELETE /api/v1/jobs/{id}":                kindJob,
	"POST /api/v1/jobs/{id}/run":              kindJob,
	"GET /api/v1/jobs/{id}/databases/preview": kindJob,
	"GET /api/v1/jobs/{id}/runs":              kindJob,
	"POST /api/v1/jobs/{id}/cancel":           kindJob,
	"GET /api/v1/jobs/{id}/retention/preview": kindJob,
	"GET /api/v1/jobs/{id}/retention/log":     kindJob,
	"POST /api/v1/jobs/{id}/restore-test":     kindJob,
	"GET /api/v1/jobs/{id}/restore-tests":     kindJob,
	"DELETE /api/v1/backups/{id}":             kindBackup,
	"POST /api/v1/backups/{id}/retry":         kindBackup,
	"GET /api/v1/backups/{id}/collections":    kindBackup,
	"POST /api/v1/backups/{id}/cancel":        kindBackup,
	"GET /api/v1/backups/{id}/log":            kindBackup,
	"POST /api/v1/backups/{id}/verify":        kindBackup,
	"POST /api/v1/backups/{id}/pin":           kindBackup,
	"POST /api/v1/backups/{id}/unpin":         kindBackup,
	undeleteRoute:                             kindBackup,
	"POST /api/v1/restores/{id}/cancel":       kindRestore,
	"POST /api/v1/restores/{id}/drop-clones":  kindRestore,
	"GET /api/v1/restores/{id}/log":           kindRestore,

	// A PITR stream belongs to its connection.
	pitrStreamRoute:       kindStream,
	pitrUpdateStreamRoute: kindStream,
	pitrDeleteStreamRoute: kindStream,
	pitrChunksRoute:       kindStream,
	pitrBaseRoute:         kindStream,
	pitrChainTestRoute:    kindStream,
}

// accessLimitedNotFound are path routes a limited caller is refused for every
// record: a storage scan lists every archive on the target, whichever connection
// it came from.
var accessLimitedNotFound = []string{"GET /api/v1/storage-targets/{id}/scan"}

// accessBodyRoutes are the routes that name a connection, or a record of one, in
// the query or the body: the request for A and for B.
var accessBodyRoutes = map[string][2]accessRequest{
	"POST /api/v1/backups": {
		{body: `{"connection_id":"` + testConnID + `","database":"shop"}`},
		{body: `{"connection_id":"` + accessConnB + `","database":"billing"}`},
	},
	"POST /api/v1/restore": {
		{body: `{"backup_id":"` + accessBkpA + `"}`},
		{body: `{"backup_id":"` + accessBkpB + `"}`},
	},
	"POST /api/v1/restores/preflight": {
		{body: `{"backup_id":"` + accessBkpA + `"}`},
		{body: `{"backup_id":"` + accessBkpB + `"}`},
	},
	"GET /api/v1/jobs/databases/preview": {
		{query: "?connection_id=" + testConnID + "&mode=list&databases=shop"},
		{query: "?connection_id=" + accessConnB + "&mode=list&databases=billing"},
	},
	createAPIKeyRoute: {
		{body: `{"name":"team a ci","scope":"read","connection_ids":["` + testConnID + `"]}`},
		{body: `{"name":"team b ci","scope":"read","connection_ids":["` + accessConnB + `"]}`},
	},
	"POST /api/v1/users": {
		{body: `{"username":"a2","password":"` + testPassword + `","role":"viewer","connection_ids":["` + testConnID + `"]}`},
		{body: `{"username":"b2","password":"` + testPassword + `","role":"viewer","connection_ids":["` + accessConnB + `"]}`},
	},
	userConnectionsRoute: {
		{body: `{"connection_ids":["` + testConnID + `"]}`},
		{body: `{"connection_ids":["` + accessConnB + `"]}`},
	},
	pitrCreateStreamRoute: {
		{body: `{"connection_id":"` + testConnID + `"}`},
		{body: `{"connection_id":"` + accessConnB + `"}`},
	},
	"POST /api/v1/jobs": {
		{body: `{"name":"a2","database":"shop","connection_id":"` + testConnID + `","cron_expression":"@daily"}`},
		{body: `{"name":"b2","database":"billing","connection_id":"` + accessConnB + `","cron_expression":"@daily"}`},
	},
}

// accessBulkRoutes are the bulk routes: an item of B is skipped as not found.
var accessBulkRoutes = map[string][2]accessRequest{
	"POST /api/v1/backups/bulk":  {{body: `{"action":"pin","ids":["` + accessBkpA + `"]}`}, {body: `{"action":"pin","ids":["` + accessBkpB + `"]}`}},
	"POST /api/v1/restores/bulk": {{body: `{"action":"cancel","ids":["` + accessRstA + `"]}`}, {body: `{"action":"cancel","ids":["` + accessRstB + `"]}`}},
	"POST /api/v1/jobs/bulk":     {{body: `{"action":"run_now","ids":["` + accessJobA + `"]}`}, {body: `{"action":"run_now","ids":["` + accessJobB + `"]}`}},
}

// accessRequest is a query string or a JSON body.
type accessRequest struct {
	query, body string
}

// accessFixture serves the full middleware chain with connections A and B and
// callers limited to A: an operator's session and an operator key an admin created.
type accessFixture struct {
	srv      *Server
	h        http.Handler
	st       *store.SQLiteStore
	auth     *auth.Service
	operator *auth.User
	key      string
}

func newAccessFixture(t *testing.T) *accessFixture {
	t.Helper()
	ctx := context.Background()
	st := storetest.New(t)
	mock := storage.NewMockStorage()
	bEngine := backup.NewEngine(mock, "", backup.WithRunner(func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(bytes.NewReader([]byte("archive"))), strings.NewReader(""), func() error { return nil }, nil
	}))
	rEngine := restore.NewEngine(mock, "", restore.WithRunner(func(_ context.Context, _ string, stdin io.Reader, _ ...string) (io.Reader, func() error, error) {
		_, _ = io.Copy(io.Discard, stdin)
		return strings.NewReader(""), func() error { return nil }, nil
	}))
	seedTestConnection(t, st)
	now := time.Now().UTC().Truncate(time.Second)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.SaveConnection(ctx, &models.Connection{ID: accessConnB, Name: accessNameB, URI: "mongodb://b.internal:27017", CreatedAt: now, UpdatedAt: now}))
	for _, tg := range []string{accessTgtA, accessTgtB} {
		must(st.CreateStorageTarget(ctx, &models.StorageTarget{ID: tg, Name: tg, Type: models.StorageLocal,
			Local: &models.LocalTarget{Path: t.TempDir()}, CreatedAt: now, UpdatedAt: now}))
	}
	must(st.SetDefaultStorageTarget(ctx, accessTgtA))
	sides := []struct{ conn, job, bkp, rst, tgt, db string }{
		{testConnID, accessJobA, accessBkpA, accessRstA, accessTgtA, "shop"},
		{accessConnB, accessJobB, accessBkpB, accessRstB, accessTgtB, accessDBB},
	}
	for _, s := range sides {
		must(st.SaveJob(ctx, &models.Job{ID: s.job, Name: s.job, Database: s.db, ConnectionID: s.conn, StorageTargetID: s.tgt,
			CronExpression: "@daily", Enabled: true, CreatedAt: now}))
		done := now.Add(time.Minute)
		must(st.SaveBackupRecord(ctx, &models.BackupRecord{ID: s.bkp, JobID: s.job, Database: s.db, ConnectionID: s.conn,
			StorageTargetID: s.tgt, StorageType: models.StorageLocal, StorageKey: s.db + "/" + s.bkp + ".archive",
			Status: models.StatusCompleted, StartedAt: now, CompletedAt: &done, SizeBytes: 7}))
		must(st.SaveRestoreRecord(ctx, &models.RestoreRecord{ID: s.rst, BackupID: s.bkp, SourceDatabase: s.db,
			TargetDatabase: s.db + "_rescue_1", SourceConnectionID: s.conn, TargetConnectionID: s.conn,
			Status: models.RestoreStatusCompleted, StartedAt: now}))
	}
	for i, conn := range []string{testConnID, accessConnB} {
		must(st.CreateStream(ctx, &pitr.Stream{ID: accessID(kindStream, i), ConnectionID: conn, ReplicaSet: "rs0",
			TargetID: accessID(kindTarget, i), ChunkSeconds: 60}))
	}

	conns := connections.NewService(st, &fakeProber{dbs: []connections.Database{{Name: "shop"}}}, connections.WithTestTimeout(2*time.Second))
	tg := targets.NewService(st, func(context.Context, *models.StorageTarget, string) (storage.Storage, error) { return mock, nil }, t.TempDir())
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	sched := scheduler.NewScheduler(st, bEngine, mock, nil, scheduler.WithConnectionResolver(conns), scheduler.WithStorageTargets(tg))
	svc := newTestAuth(t, st, "")
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	_, recipient, err := encryption.GenerateX25519()
	must(err)
	enc, err := encryption.NewX25519Encryptor([]string{recipient})
	must(err)
	col := collector.New(collector.Config{
		Repo:      st,
		Open:      func(context.Context, *pitr.Stream) (collector.Session, error) { return nil, io.ErrUnexpectedEOF },
		Storage:   func(context.Context, string) (storage.Storage, error) { return mock, nil },
		Encryptor: func() *encryption.Encryptor { return enc },
		StartBase: func(_ context.Context, id string, _ models.BackupTrigger) (*models.BackupRecord, error) {
			return &models.BackupRecord{ID: "bkp_pitr-base_x", PITRStreamID: id, Scope: models.ScopeInstance}, nil
		},
		Logger: slog.New(slog.DiscardHandler),
	})
	srv := NewServer(bootConfig(), st, bEngine, rEngine, mock, sched, nil, nil, WithPITR(col),
		WithAuth(svc), WithConnections(conns), WithStorageTargets(tg), WithRunManager(manager),
		WithIntegrity(integrity.New(integrity.Config{Store: st, Targets: tg, Runs: manager})),
		WithSettings(newTestSettings(t, st, newTestConfig().Security)),
		WithMetricsHandler(ok), WithMCPHandler(ok), WithAudit(audit.NewService(st, nil)), withTestOIDC(t))
	// Point-in-time restores read the streams like the application wires them.
	opsCfg := srv.operationsConfig()
	opsCfg.PITR, opsCfg.PITRRestore, opsCfg.PITRBases = st, rEngine, st.ListBaseBackups
	srv.ops = operations.New(opsCfg)
	f := &accessFixture{srv: srv, h: srv.Handler(), st: st, auth: svc}

	res, err := svc.Setup(ctx, "192.0.2.1", svc.SetupCode(), "admin", testPassword)
	must(err)
	admin, err := svc.AuthenticateSession(ctx, res.Token)
	must(err)
	f.operator, err = svc.CreateUserWithConnections(ctx, admin, "team-a", testPassword, auth.RoleOperator, auth.ConnectionAccess{ConnectionIDs: []string{testConnID}})
	must(err)
	_, f.key, err = svc.CreateAPIKeyWithConnections(ctx, admin, "team a automation", auth.ScopeOperator, auth.ConnectionAccess{ConnectionIDs: []string{testConnID}})
	must(err)
	return f
}

// session signs the limited operator in afresh and returns the headers of a
// dashboard request.
func (f *accessFixture) session(t *testing.T) map[string]string {
	t.Helper()
	res, err := f.auth.Login(context.Background(), "192.0.2.1", f.operator.Username, testPassword)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{"Cookie": SessionCookieName + "=" + res.Token, CSRFHeader: res.CSRFToken, "Content-Type": "application/json"}
}

// keyHeaders returns the headers of a request with the limited operator key.
func (f *accessFixture) keyHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + f.key, "Content-Type": "application/json"}
}

// hiddenNotFound reports whether an answer is the 404 of a record outside the
// caller's access (or of one that does not exist).
func hiddenNotFound(code int, body string) bool {
	if code != http.StatusNotFound {
		return false
	}
	for _, msg := range []string{"connection not found", "storage target not found", "job not found", "backup not found",
		"restore not found", "source backup not found", "PITR stream not found"} {
		if strings.Contains(body, msg) {
			return true
		}
	}
	return false
}

// leaked returns the first of B's markers in body that the request did not name.
func leaked(body, request string) string {
	for _, m := range accessMarkers {
		if strings.Contains(body, m) && !strings.Contains(request, m) {
			return m
		}
	}
	return ""
}

// accessPath fills the placeholders of a route pattern with side's records (0: A,
// 1: B) for kind.
func accessPath(pattern string, kind accessKind, side int) (method, path string) {
	method, path, _ = strings.Cut(pattern, " ")
	db := "shop"
	if side == 1 {
		db = accessDBB
	}
	return method, strings.NewReplacer("{id}", accessID(kind, side), "{db}", db).Replace(path)
}

// TestEveryRouteIsClassifiedForConnectionAccess fails when a route is registered
// without saying whether (and how) it names a connection, so the matrix below
// covers every route.
func TestEveryRouteIsClassifiedForConnectionAccess(t *testing.T) {
	f := newAccessFixture(t)
	for _, p := range f.authenticatedPatterns() {
		n := 0
		for _, m := range []bool{accessPathRoutes[p] != "", accessBodyRoutes[p] != [2]accessRequest{}, accessBulkRoutes[p] != [2]accessRequest{}} {
			if m {
				n++
			}
		}
		if n > 1 {
			t.Errorf("%s is classified twice", p)
		}
		if n == 0 && strings.Contains(p, "{id}") && !slices.Contains(accessUnrelatedIDRoutes, p) {
			t.Errorf("%s names an {id}: add it to accessPathRoutes (a connection or a record of one) or accessUnrelatedIDRoutes", p)
		}
	}
}

// accessUnrelatedIDRoutes have an {id} that names no connection and no record of
// one: users, sessions, API keys, approvals, pending changes, warnings and
// notification channels and rules.
var accessUnrelatedIDRoutes = []string{
	"DELETE /api/v1/users/{id}", userRoleRoute, userConnectionsRoute, changePasswordRoute, revokeSessionRoute, deleteAPIKeyRoute,
	getApprovalRoute, approveRoute, rejectRoute, cancelPendingRoute, "POST /api/v1/settings/warnings/{id}/dismiss",
	"PUT /api/v1/notifications/channels/{id}", "DELETE /api/v1/notifications/channels/{id}",
	"POST /api/v1/notifications/channels/{id}/test", "PUT /api/v1/notifications/rules/{id}", "DELETE /api/v1/notifications/rules/{id}",
}

// authenticatedPatterns returns every registered pattern that needs credentials.
func (f *accessFixture) authenticatedPatterns() []string {
	sf := &scopeFixture{srv: f.srv}
	patterns := sf.authenticatedPatterns()
	slices.Sort(patterns)
	return patterns
}

// TestConnectionAccessIsEnforcedForEveryRoute walks every route with an operator
// session and an operator key, both limited to connection A. A route the operator
// scope does not reach is refused for both sides. Otherwise a route naming B (in
// its path, query or body) answers 404 exactly like an unknown record, one naming A
// gets the handler's answer, a bulk item of B is skipped as not found, and no
// answer of any route contains one of B's identifiers.
func TestConnectionAccessIsEnforcedForEveryRoute(t *testing.T) {
	f := newAccessFixture(t)
	callers := []struct {
		name    string
		session bool
		headers func(t *testing.T) map[string]string
	}{
		{"limited session", true, f.session},
		{"limited key", false, func(*testing.T) map[string]string { return f.keyHeaders() }},
	}
	for _, p := range f.authenticatedPatterns() {
		t.Run(p, func(t *testing.T) {
			for _, c := range callers {
				refused := !auth.ScopeOperator.Allows(requiredScope(p))
				if slices.Contains(selfServiceRoutes, p) {
					refused = selfServiceRefused(t, p, c.session, auth.ScopeOperator)
				}
				f.checkRoute(t, p, c.name, c.session, c.headers(t), refused)
			}
		})
	}
}

// checkRoute checks pattern for one caller.
func (f *accessFixture) checkRoute(t *testing.T, pattern, who string, session bool, headers map[string]string, refused bool) {
	t.Helper()
	method, path := concrete(pattern)
	if path == MCPPath || path == "/metrics" {
		rec := serve(f.h, method, path, []byte(`{}`), headers)
		switch {
		case session && rec.Code != http.StatusUnauthorized:
			t.Errorf("%s: %d; want 401 (bearer tokens only)", who, rec.Code)
		case !session && path == "/metrics" && rec.Code != http.StatusForbidden:
			t.Errorf("%s: GET /metrics %d; want 403 (the metrics cover every connection)", who, rec.Code)
		}
		return
	}
	type variant struct {
		side          int
		path, request string
		body          []byte
	}
	var variants []variant
	kind, onPath := accessPathRoutes[pattern]
	body, onBody := accessBodyRoutes[pattern]
	bulk, onBulk := accessBulkRoutes[pattern]
	switch {
	case onPath:
		for side := range 2 {
			_, p := accessPath(pattern, kind, side)
			variants = append(variants, variant{side, p, p, []byte(`{}`)})
		}
	case onBody, onBulk:
		reqs := body
		if onBulk {
			reqs = bulk
		}
		for side, r := range reqs {
			b := r.body
			if b == "" {
				b = `{}`
			}
			variants = append(variants, variant{side, path + r.query, path + r.query + r.body, []byte(b)})
		}
	default:
		variants = append(variants, variant{-1, path, path, []byte(`{}`)})
	}
	for _, v := range variants {
		h := headers
		if session && v.side >= 0 {
			// A fresh session per request: some routes end sessions.
			h = f.session(t)
		}
		rec := serve(f.h, method, v.path, v.body, h)
		code, out := rec.Code, rec.Body.String()
		label := who + " " + method + " " + v.path
		if m := leaked(out, v.request); m != "" {
			t.Errorf("%s: the answer names %q of connection B: %d %s", label, m, code, out)
		}
		switch {
		case refused:
			if code != http.StatusForbidden {
				t.Errorf("%s: %d %s; want a scope refusal", label, code, out)
			}
		case code == http.StatusUnauthorized || scopeRefused(code, out):
			t.Errorf("%s: %d %s; want the handler's answer", label, code, out)
		case slices.Contains(accessLimitedNotFound, pattern):
			if !hiddenNotFound(code, out) {
				t.Errorf("%s: %d %s; want 404 for every record", label, code, out)
			}
		case onBulk && v.side == 1:
			if !strings.Contains(out, operationsSkipNotFound) {
				t.Errorf("%s: %d %s; want B's item skipped as not found", label, code, out)
			}
		case onBulk:
			if strings.Contains(out, operationsSkipNotFound) {
				t.Errorf("%s: %d %s; want A's item processed", label, code, out)
			}
		case v.side == 1:
			if !hiddenNotFound(code, out) {
				t.Errorf("%s: %d %s; want 404 like an unknown record", label, code, out)
			}
		case v.side == 0:
			if hiddenNotFound(code, out) {
				t.Errorf("%s: %d %s; want the handler's answer for A", label, code, out)
			}
		}
	}
}

// operationsSkipNotFound is the skip reason of a bulk item that names no record.
const operationsSkipNotFound = `"not_found"`
