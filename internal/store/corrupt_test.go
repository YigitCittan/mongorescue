package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// rowData returns the stored data column of row id in table, read past the store.
func rowData(t *testing.T, s *store.SQLiteStore, table, id string) string {
	t.Helper()
	var data string
	if err := rawDB(t, s.Path()).QueryRow("SELECT data FROM "+table+" WHERE id = ?", id).Scan(&data); err != nil { //nolint:gosec // G202: test constant.
		t.Fatalf("read %s/%s: %v", table, id, err)
	}
	return data
}

func TestListsSkipRowsThatCannotBeDecoded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mongorescue.db")
	box := storetest.NewBox(t)
	s := storetest.OpenWithBox(t, path, box)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, id := range []string{"job_a", "job_bad", "job_c"} {
		if err := s.SaveJob(ctx, &models.Job{ID: id, Name: id, Database: "shop", CronExpression: "@daily", Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"conn_a", "conn_bad"} {
		if err := s.SaveConnection(ctx, &models.Connection{ID: id, Name: id, URI: "mongodb://u:conn-secret@db/", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	// Valid JSON (the tables CHECK json_valid) that does not fit the record types.
	const badJob = `{"id":"job_bad","name":4242,"database":"shop"}`
	const badConn = `{"id":"conn_bad","name":"bad","created_at":"yesterday-4242"}`
	storetest.CorruptRow(t, s, "jobs", "job_bad", badJob)
	storetest.CorruptRow(t, s, "connections", "conn_bad", badConn)

	jobs, err := s.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs failed as a whole: %v", err)
	}
	if got := jobIDs(jobs); !slices.Equal(got, []string{"job_a", "job_c"}) {
		t.Fatalf("ListJobs = %v; want the two readable jobs", got)
	}
	conns, err := s.ListConnections(ctx)
	if err != nil {
		t.Fatalf("ListConnections failed as a whole: %v", err)
	}
	if len(conns) != 1 || conns[0].ID != "conn_a" || conns[0].URI != "mongodb://u:conn-secret@db/" {
		t.Fatalf("ListConnections = %+v; want conn_a with its decrypted URI", conns)
	}
	if _, getErr := s.GetJob(ctx, "job_bad"); !errors.Is(getErr, store.ErrCorruptRecord) {
		t.Fatalf("GetJob(job_bad) = %v; want ErrCorruptRecord", getErr)
	}

	bad, err := s.CorruptRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []store.CorruptRecord{
		{Table: "connections", ID: "conn_bad", Error: "a timestamp field is not a valid time"},
		{Table: "jobs", ID: "job_bad", Error: "field name: a JSON number does not fit type string"},
	}
	if !slices.Equal(bad, want) {
		t.Fatalf("CorruptRecords = %+v; want %+v", bad, want)
	}
	for _, r := range bad {
		if strings.Contains(r.Error, "4242") {
			t.Fatalf("the error summary quotes stored data: %q", r.Error)
		}
	}

	// Nothing was deleted or rewritten.
	if got := rowData(t, s, "jobs", "job_bad"); got != badJob {
		t.Fatalf("job_bad was rewritten: %s", got)
	}
	if got := rowData(t, s, "connections", "conn_bad"); got != badConn {
		t.Fatalf("conn_bad was rewritten: %s", got)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}

	// A reopened store reports the rows from startup on, before any list.
	s2 := storetest.OpenWithBox(t, path, box)
	if bad, err = s2.CorruptRecords(ctx); err != nil || len(bad) != 2 {
		t.Fatalf("after reopen CorruptRecords = %+v, %v; want both rows", bad, err)
	}

	// A repaired row and a deleted row leave the report.
	if err = s2.SaveJob(ctx, &models.Job{ID: "job_bad", Name: "repaired", Database: "shop", CronExpression: "@daily"}); err != nil {
		t.Fatal(err)
	}
	if err = s2.DeleteConnection(ctx, "conn_bad"); err != nil {
		t.Fatal(err)
	}
	if bad, err = s2.CorruptRecords(ctx); err != nil || len(bad) != 0 {
		t.Fatalf("after repair CorruptRecords = %+v, %v; want none", bad, err)
	}
}

func TestBackupPagesSkipRowsThatCannotBeDecoded(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	start := time.Now().UTC().Add(-time.Hour)
	for i, id := range []string{"bkp_1", "bkp_2", "bkp_3"} {
		if err := s.SaveBackupRecord(ctx, &models.BackupRecord{ID: id, Database: "shop", JobID: "job_a",
			Status: models.StatusCompleted, StartedAt: start.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	storetest.CorruptRow(t, s, "backups", "bkp_3", `{"id":"bkp_3","size_bytes":"big"}`)

	page, err := s.QueryBackupRecords(ctx, store.BackupFilter{Limit: 10})
	if err != nil {
		t.Fatalf("QueryBackupRecords failed as a whole: %v", err)
	}
	if len(page.Rows) != 2 {
		t.Fatalf("rows = %d; want the 2 readable backups", len(page.Rows))
	}
	if list, listErr := s.ListBackupRecords(ctx, ""); listErr != nil || len(list) != 2 {
		t.Fatalf("ListBackupRecords = %d, %v", len(list), listErr)
	}
	// The newest backup is the unreadable one: the stats still load, without it.
	st, err := s.BackupStats(ctx, start.Add(-time.Hour))
	if err != nil {
		t.Fatalf("BackupStats: %v", err)
	}
	if st.Total != 3 || st.Last != nil {
		t.Fatalf("stats = total %d, last %+v; want 3 and no last backup", st.Total, st.Last)
	}
	latest, err := s.LatestJobBackups(ctx, "")
	if err != nil || len(latest) != 0 {
		t.Fatalf("LatestJobBackups = %v, %v", latest, err)
	}
	bad, err := s.CorruptRecords(ctx)
	if err != nil || len(bad) != 1 || bad[0].Table != "backups" || bad[0].ID != "bkp_3" {
		t.Fatalf("CorruptRecords = %+v, %v", bad, err)
	}
}

func jobIDs(jobs []*models.Job) []string {
	ids := make([]string, 0, len(jobs))
	for _, j := range jobs {
		ids = append(ids, j.ID)
	}
	return ids
}
