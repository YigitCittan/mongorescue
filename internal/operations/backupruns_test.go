package operations_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/runs"
)

// awaitBackup waits until backup id is no longer in progress.
func (e *multiEnv) awaitBackup(t *testing.T, id string) *models.BackupRecord {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		rec, err := e.st.GetBackupRecord(context.Background(), id)
		if err == nil && rec.Status != models.StatusInProgress && rec.Status != models.StatusPending {
			return rec
		}
		if time.Now().After(deadline) {
			t.Fatalf("backup %s did not finish: %+v, %v", id, rec, err)
		}
	}
}

func TestStartBackupsGroupsTheDatabasesUnderOneRun(t *testing.T) {
	e := newMultiEnv(t)
	ctx := context.Background()
	two := 2
	run, err := e.svc.StartBackups(ctx, operations.BackupRequest{
		BackupOptions: models.BackupOptions{ConnectionID: "conn_a", IncludeUsersAndRoles: true},
		Databases:     []string{" a ", "b", "admin"}, Parallelism: &two,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.RunID == "" || len(run.Backups) != 3 || run.Busy == nil || len(run.Busy) != 0 {
		t.Fatalf("run = %+v", run)
	}
	for i, want := range []string{"a", "b", "admin"} {
		b := run.Backups[i]
		if b.Database != want || b.RunID != run.RunID || b.Status != models.StatusInProgress || b.Trigger != models.TriggerManual {
			t.Errorf("backup %d = %+v; want %s in progress in run %s", i, b, want, run.RunID)
		}
		done := e.awaitBackup(t, b.ID)
		// Users and roles apply per database, as for jobs: never to admin.
		if done.Status != models.StatusCompleted || done.UsersAndRoles != (want != models.AdminDatabase) {
			t.Errorf("backup of %s = status %s, users and roles %v", want, done.Status, done.UsersAndRoles)
		}
	}
	page, err := e.svc.QueryBackups(ctx, operations.BackupFilter{RunID: run.RunID})
	if err != nil || page.Total != 3 {
		t.Errorf("backups of the run = %+v, %v", page, err)
	}
	// Every lock was released when its database ended.
	for _, db := range []string{"a", "b", "admin"} {
		release, lockErr := e.runs.Acquire(runs.BackupKey("conn_a", db))
		if lockErr != nil {
			t.Errorf("lock of %s after the run: %v", db, lockErr)
			continue
		}
		release()
	}
}

func TestStartBackupsSkipsABusyDatabase(t *testing.T) {
	e := newMultiEnv(t)
	ctx := context.Background()
	held, err := e.runs.Acquire(runs.BackupKey("conn_a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := e.svc.StartBackups(ctx, operations.BackupRequest{
		BackupOptions: models.BackupOptions{ConnectionID: "conn_a"}, Databases: []string{"a", "b", "c"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Backups) != 2 || run.Backups[0].Database != "a" || run.Backups[1].Database != "c" {
		t.Fatalf("backups = %+v; want a and c", run.Backups)
	}
	if len(run.Busy) != 1 || run.Busy[0].Database != "b" || !run.Busy[0].Busy || !strings.Contains(run.Busy[0].Error, "already running") {
		t.Fatalf("busy = %+v; want b", run.Busy)
	}
	for _, b := range run.Backups {
		if done := e.awaitBackup(t, b.ID); done.Status != models.StatusCompleted {
			t.Errorf("backup of %s = %s", done.Database, done.Status)
		}
	}
	// The run never touched the lock it did not take.
	if _, err = e.runs.Acquire(runs.BackupKey("conn_a", "b")); !errors.Is(err, runs.ErrBusy) {
		t.Errorf("lock of b while held elsewhere: %v; want ErrBusy", err)
	}
	// Every database busy: nothing starts.
	if _, err = e.svc.StartBackups(ctx, operations.BackupRequest{
		BackupOptions: models.BackupOptions{ConnectionID: "conn_a"}, Databases: []string{"b"},
	}); !errors.Is(err, operations.ErrBusy) {
		t.Errorf("only busy databases: %v; want ErrBusy", err)
	}
	held()
}

func TestStartBackupsValidation(t *testing.T) {
	e := newMultiEnv(t)
	ctx := context.Background()
	five, minus := 5, -1
	many := make([]string, operations.MaxBackupDatabases+1)
	for i := range many {
		many[i] = "db" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	conn := models.BackupOptions{ConnectionID: "conn_a"}
	for name, tc := range map[string]struct {
		req  operations.BackupRequest
		want error
	}{
		"database and databases": {operations.BackupRequest{BackupOptions: models.BackupOptions{ConnectionID: "conn_a", Database: "a"}, Databases: []string{"b"}}, operations.ErrDatabasesConflict},
		"empty":                  {operations.BackupRequest{BackupOptions: conn, Databases: []string{}}, operations.ErrDatabasesCount},
		"too many":               {operations.BackupRequest{BackupOptions: conn, Databases: many}, operations.ErrDatabasesCount},
		"duplicate":              {operations.BackupRequest{BackupOptions: conn, Databases: []string{"a", " a"}}, operations.ErrDuplicateDatabase},
		"invalid name":           {operations.BackupRequest{BackupOptions: conn, Databases: []string{"a", "--drop"}}, models.ErrInvalidNamespace},
		"blank name":             {operations.BackupRequest{BackupOptions: conn, Databases: []string{"a", " "}}, models.ErrInvalidNamespace},
		"collections":            {operations.BackupRequest{BackupOptions: models.BackupOptions{ConnectionID: "conn_a", Collections: []string{"x"}}, Databases: []string{"a", "b"}}, operations.ErrCollectionsNeedOneDatabase},
		"exclusions":             {operations.BackupRequest{BackupOptions: models.BackupOptions{ConnectionID: "conn_a", ExcludeCollections: []string{"x"}}, Databases: []string{"a", "b"}}, operations.ErrCollectionsNeedOneDatabase},
		"parallelism too high":   {operations.BackupRequest{BackupOptions: conn, Databases: []string{"a", "b"}, Parallelism: &five}, operations.ErrInvalidParallelism},
		"parallelism negative":   {operations.BackupRequest{BackupOptions: conn, Databases: []string{"a", "b"}, Parallelism: &minus}, operations.ErrInvalidParallelism},
		"users and roles admin":  {operations.BackupRequest{BackupOptions: models.BackupOptions{ConnectionID: "conn_a", IncludeUsersAndRoles: true}, Databases: []string{"admin"}}, operations.ErrUsersAndRolesAdmin},
		"no connection":          {operations.BackupRequest{Databases: []string{"a", "b"}}, operations.ErrConnectionRequired},
		"unknown connection":     {operations.BackupRequest{BackupOptions: models.BackupOptions{ConnectionID: "conn_x"}, Databases: []string{"a", "b"}}, operations.ErrUnknownConnection},
		"databases on single":    {operations.BackupRequest{BackupOptions: conn, Databases: []string{"a"}}, nil},
	} {
		var err error
		if name == "databases on single" {
			_, err = e.svc.StartBackup(ctx, tc.req)
			if !errors.Is(err, operations.ErrInvalid) {
				t.Errorf("%s: %v; want ErrInvalid", name, err)
			}
			continue
		}
		_, err = e.svc.StartBackups(ctx, tc.req)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: %v; want %v", name, err, tc.want)
		}
		if tc.want != operations.ErrConnectionRequired && tc.want != operations.ErrUnknownConnection && !errors.Is(err, operations.ErrInvalid) {
			t.Errorf("%s: %v; want ErrInvalid", name, err)
		}
	}
	// Collections apply to a run of one database.
	run, err := e.svc.StartBackups(ctx, operations.BackupRequest{
		BackupOptions: models.BackupOptions{ConnectionID: "conn_a", Collections: []string{"orders"}}, Databases: []string{"shop"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if b := run.Backups[0]; b.Database != "shop" || len(b.Collections) != 1 || b.Collections[0] != "orders" {
		t.Errorf("backup = %+v; want shop.orders", b)
	}
	e.awaitBackup(t, run.Backups[0].ID)
	// Nothing was left locked by the refused requests.
	for _, db := range []string{"a", "b"} {
		release, lockErr := e.runs.Acquire(runs.BackupKey("conn_a", db))
		if lockErr != nil {
			t.Errorf("lock of %s: %v", db, lockErr)
			continue
		}
		release()
	}
}

func TestStartBackupOfOneDatabaseIsUnchanged(t *testing.T) {
	e := newMultiEnv(t)
	rec, err := e.svc.StartBackup(context.Background(), operations.BackupRequest{
		BackupOptions: models.BackupOptions{ConnectionID: "conn_a", Database: "shop"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.RunID != "" || rec.Database != "shop" || rec.Status != models.StatusInProgress {
		t.Errorf("single backup = %+v; want no run", rec)
	}
	if done := e.awaitBackup(t, rec.ID); done.Status != models.StatusCompleted || done.RunID != "" {
		t.Errorf("single backup = %+v", done)
	}
}
