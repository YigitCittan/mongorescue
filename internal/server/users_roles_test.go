package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func TestJobUsersAndRolesRoundTrip(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	h := srv.buildRoutes()

	created := createTestJob(t, h, models.Job{
		Name: "shop", Database: "shop", CronExpression: "@daily", Enabled: true, ConnectionID: testConnID,
		IncludeUsersAndRoles: true,
	})
	if !created.IncludeUsersAndRoles {
		t.Fatalf("created job = %+v; want include_users_and_roles", created)
	}
	get := func() models.Job {
		t.Helper()
		rec := serve(h, "GET", "/api/v1/jobs/"+created.ID, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET = %d %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"include_users_and_roles"`) {
			t.Fatalf("GET body lacks include_users_and_roles: %s", rec.Body.String())
		}
		var res struct {
			Data models.Job `json:"data"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
			t.Fatal(err)
		}
		return res.Data
	}
	if !get().IncludeUsersAndRoles {
		t.Fatal("GET lost include_users_and_roles")
	}

	put := func(extra string) {
		t.Helper()
		body := `{"name":"shop","cron_expression":"@daily","database":"shop","connection_id":"` + testConnID + `"` + extra + `}`
		if rec := serve(h, "PUT", "/api/v1/jobs/"+created.ID, []byte(body), nil); rec.Code != http.StatusOK {
			t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
		}
	}
	put("") // omitted: kept
	if !get().IncludeUsersAndRoles {
		t.Fatal("an update without include_users_and_roles cleared it")
	}
	put(`,"include_users_and_roles":false`)
	if get().IncludeUsersAndRoles {
		t.Fatal("include_users_and_roles=false was not applied")
	}

	body, _ := json.Marshal(models.Job{Name: "admin", Database: "admin", ConnectionID: testConnID, IncludeUsersAndRoles: true})
	if rec := serve(h, "POST", "/api/v1/jobs", body, nil); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "admin database") {
		t.Fatalf("admin job with users and roles = %d %s; want 400", rec.Code, rec.Body.String())
	}
}

// usersRolesFixture serves the API with mongodump and mongorestore stand-ins that
// record their arguments.
type usersRolesFixture struct {
	mux         http.Handler
	mu          sync.Mutex
	dumpArgs    []string
	restoreArgs []string
}

func newUsersRolesFixture(t *testing.T) *usersRolesFixture {
	t.Helper()
	f := &usersRolesFixture{}
	metaStore := storetest.New(t)
	mockStorage := storage.NewMockStorage()
	bRunner := func(_ context.Context, _ string, args ...string) (io.ReadCloser, io.Reader, func() error, error) {
		f.mu.Lock()
		f.dumpArgs = slices.Clone(args)
		f.mu.Unlock()
		return io.NopCloser(strings.NewReader("backup-data")), strings.NewReader(""), func() error { return nil }, nil
	}
	rRunner := func(_ context.Context, _ string, stdin io.Reader, args ...string) (io.Reader, func() error, error) {
		f.mu.Lock()
		f.restoreArgs = slices.Clone(args)
		f.mu.Unlock()
		_, _ = io.Copy(io.Discard, stdin)
		return strings.NewReader(""), func() error { return nil }, nil
	}
	bEngine := backup.NewEngine(mockStorage, "mongodb://localhost:27017", backup.WithRunner(bRunner))
	rEngine := restore.NewEngine(mockStorage, "mongodb://localhost:27017", restore.WithRunner(rRunner))
	sched := scheduler.NewScheduler(metaStore, bEngine, mockStorage, nil)
	f.mux = asPrincipal(NewServer(bootConfig(), metaStore, bEngine, rEngine, mockStorage, sched, nil, nil,
		withTestConnection(t, metaStore, nil)).buildRoutes(), auth.SystemPrincipal())
	return f
}

// args returns the last mongodump and mongorestore arguments.
func (f *usersRolesFixture) args() (dump, restore []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.dumpArgs), slices.Clone(f.restoreArgs)
}

func (f *usersRolesFixture) backup(t *testing.T, extra string) models.BackupRecord {
	t.Helper()
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/backups", strings.NewReader(`{"connection_id":"conn_test","database":"shop"`+extra+`}`)))
	id := acceptedID(t, rec)
	raw, _ := json.Marshal(awaitRecord(t, f.mux, "/api/v1/backups", id))
	var done models.BackupRecord
	if err := json.Unmarshal(raw, &done); err != nil {
		t.Fatal(err)
	}
	return done
}

func (f *usersRolesFixture) restore(t *testing.T, backupID, extra string) (int, string, models.RestoreRecord) {
	t.Helper()
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/restore", strings.NewReader(`{"backup_id":"`+backupID+`"`+extra+`}`)))
	var res struct {
		Data  models.RestoreRecord `json:"data"`
		Error string               `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != http.StatusAccepted {
		return rec.Code, res.Error, res.Data
	}
	raw, _ := json.Marshal(awaitRecord(t, f.mux, "/api/v1/restores", res.Data.ID))
	var done models.RestoreRecord
	_ = json.Unmarshal(raw, &done)
	return rec.Code, done.ErrorMessage, done
}

func TestRestoreUsersAndRolesMatrix(t *testing.T) {
	f := newUsersRolesFixture(t)
	plain := f.backup(t, "")
	if dump, _ := f.args(); plain.UsersAndRoles || slices.Contains(dump, "--dumpDbUsersAndRoles") {
		t.Fatalf("plain backup = %+v, args %q", plain, dump)
	}
	withUsers := f.backup(t, `,"include_users_and_roles":true`)
	if dump, _ := f.args(); !withUsers.UsersAndRoles || !slices.Contains(dump, "--dumpDbUsersAndRoles") {
		t.Fatalf("backup with users = %+v, args %q", withUsers, dump)
	}

	const inPlace = `,"safe_clone":false,"confirm_in_place":true`
	for _, tc := range []struct {
		name     string
		backupID string
		extra    string
		wantMsg  string
	}{
		{"safe clone by default", withUsers.ID, `,"restore_users_and_roles":true`, "in-place restore"},
		{"explicit safe clone", withUsers.ID, `,"safe_clone":true,"restore_users_and_roles":true`, "in-place restore"},
		{"renamed target", withUsers.ID, inPlace + `,"target_database":"shop_copy","restore_users_and_roles":true`, "target_database"},
		{"backup without users", plain.ID, inPlace + `,"restore_users_and_roles":true`, "does not contain users and roles"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, msg, _ := f.restore(t, tc.backupID, tc.extra)
			if code != http.StatusBadRequest || !strings.Contains(msg, tc.wantMsg) {
				t.Fatalf("restore = %d %q; want 400 mentioning %q", code, msg, tc.wantMsg)
			}
		})
	}

	code, msg, rst := f.restore(t, withUsers.ID, inPlace+`,"restore_users_and_roles":true`)
	if code != http.StatusAccepted || rst.Status != models.RestoreStatusCompleted || !rst.UsersAndRoles {
		t.Fatalf("in-place restore with users = %d %q %+v", code, msg, rst)
	}
	if _, args := f.args(); !slices.Contains(args, "--restoreDbUsersAndRoles") || !slices.Contains(args, "--db=shop") {
		t.Fatalf("mongorestore args %q lack the users and roles flags", args)
	}

	code, msg, rst = f.restore(t, withUsers.ID, inPlace)
	if _, args := f.args(); code != http.StatusAccepted || rst.UsersAndRoles || slices.Contains(args, "--restoreDbUsersAndRoles") {
		t.Fatalf("in-place restore without the option = %d %q %+v, args %q", code, msg, rst, args)
	}
}
