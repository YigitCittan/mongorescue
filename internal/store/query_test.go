package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// queryFixture stores a small, varied backup history: ids b01 (oldest) to b08.
func queryFixture(t *testing.T) *store.SQLiteStore {
	t.Helper()
	s := storetest.New(t)
	ctx := context.Background()
	recs := []*models.BackupRecord{
		{ID: "b01", Database: "shop", JobID: "j1", Trigger: models.TriggerScheduled, ConnectionID: "c1", Status: models.StatusCompleted, StartedAt: ts(1)},
		{ID: "b02", Database: "shop", JobID: "j1", Trigger: models.TriggerScheduled, ConnectionID: "c1", Status: models.StatusFailed, StartedAt: ts(2)},
		{ID: "b03", Database: "shop", JobID: "", Trigger: models.TriggerManual, ConnectionID: "c1", Status: models.StatusCompleted, StartedAt: ts(3), RetryOf: "b02"},
		{ID: "b04", Database: "Billing", Trigger: models.TriggerMCP, ConnectionID: "c2", Status: models.StatusFailed, StartedAt: ts(4)},
		{ID: "b05", Database: "billing", Trigger: models.TriggerOnDemand, JobID: "j2", ConnectionID: "c2", Status: models.StatusInProgress, StartedAt: ts(5)},
		// Written before triggers existed: a job record counts as scheduled, others as manual.
		{ID: "b06", Database: "logs", JobID: "j2", ConnectionID: "c2", Status: models.StatusCompleted, StartedAt: ts(6)},
		{ID: "b07", Database: "logs", ConnectionID: "c2", Status: models.StatusPruned, StartedAt: ts(7)},
		{ID: "b08", Database: "shop", Trigger: models.TriggerManual, ConnectionID: "c1", Status: models.StatusCompleted, StartedAt: ts(8), RetryOf: "b02"},
	}
	for _, r := range recs {
		if err := s.SaveBackupRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func queryIDs(t *testing.T, s *store.SQLiteStore, f store.BackupFilter) ([]string, int) {
	t.Helper()
	page, err := s.QueryBackupRecords(context.Background(), f)
	if err != nil {
		t.Fatalf("QueryBackupRecords(%+v): %v", f, err)
	}
	ids := make([]string, 0, len(page.Rows))
	for _, r := range page.Rows {
		ids = append(ids, r.Record.ID)
	}
	return ids, page.Total
}

func TestQueryBackupRecordsFilters(t *testing.T) {
	s := queryFixture(t)
	cases := []struct {
		name string
		f    store.BackupFilter
		want []string
	}{
		{"all, newest first", store.BackupFilter{}, []string{"b08", "b07", "b06", "b05", "b04", "b03", "b02", "b01"}},
		{"status", store.BackupFilter{Status: models.StatusFailed}, []string{"b04", "b02"}},
		{"database is exact and case-sensitive", store.BackupFilter{Database: "billing"}, []string{"b05"}},
		{"connection", store.BackupFilter{ConnectionID: "c2"}, []string{"b07", "b06", "b05", "b04"}},
		{"job", store.BackupFilter{JobID: "j1"}, []string{"b02", "b01"}},
		{"trigger scheduled includes legacy job records", store.BackupFilter{Trigger: models.TriggerScheduled}, []string{"b06", "b02", "b01"}},
		{"trigger manual includes legacy one-off records", store.BackupFilter{Trigger: models.TriggerManual}, []string{"b08", "b07", "b03"}},
		{"trigger mcp", store.BackupFilter{Trigger: models.TriggerMCP}, []string{"b04"}},
		{"retry_of", store.BackupFilter{RetryOf: "b02"}, []string{"b08", "b03"}},
		{"from is inclusive", store.BackupFilter{From: ts(6)}, []string{"b08", "b07", "b06"}},
		{"to is exclusive", store.BackupFilter{To: ts(3)}, []string{"b02", "b01"}},
		{"range", store.BackupFilter{From: ts(2), To: ts(5)}, []string{"b04", "b03", "b02"}},
		{"search id", store.BackupFilter{Search: "B0"}, []string{"b08", "b07", "b06", "b05", "b04", "b03", "b02", "b01"}},
		{"search database ignores case", store.BackupFilter{Search: "BILL"}, []string{"b05", "b04"}},
		{"search has no wildcards", store.BackupFilter{Search: "%"}, nil},
		{"search underscore is literal", store.BackupFilter{Search: "b_1"}, nil},
		{"combination", store.BackupFilter{Database: "shop", Status: models.StatusCompleted, Trigger: models.TriggerManual}, []string{"b08", "b03"}},
		{"combination without match", store.BackupFilter{JobID: "j1", ConnectionID: "c2"}, nil},
		{"oldest first", store.BackupFilter{Database: "shop", Sort: store.SortOldest}, []string{"b01", "b02", "b03", "b08"}},
		{"newest first explicitly", store.BackupFilter{Database: "logs", Sort: store.SortNewest}, []string{"b07", "b06"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, total := queryIDs(t, s, tc.f)
			assertIDs(t, "ids", got, tc.want...)
			if total != len(tc.want) {
				t.Fatalf("total = %d; want %d", total, len(tc.want))
			}
		})
	}
}

func TestQueryBackupRecordsPaging(t *testing.T) {
	s := queryFixture(t)
	cases := []struct {
		limit, offset int
		want          []string
	}{
		{1, 0, []string{"b08"}},
		{3, 0, []string{"b08", "b07", "b06"}},
		{3, 3, []string{"b05", "b04", "b03"}},
		{3, 6, []string{"b02", "b01"}},
		{3, 8, nil},
		{3, 100, nil},
		{store.MaxListLimit, 0, []string{"b08", "b07", "b06", "b05", "b04", "b03", "b02", "b01"}},
		{0, 6, []string{"b02", "b01"}},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("limit %d offset %d", tc.limit, tc.offset), func(t *testing.T) {
			got, total := queryIDs(t, s, store.BackupFilter{Limit: tc.limit, Offset: tc.offset})
			assertIDs(t, "ids", got, tc.want...)
			if total != 8 {
				t.Fatalf("total = %d; want 8", total)
			}
		})
	}

	got, total := queryIDs(t, s, store.BackupFilter{Database: "shop", Sort: store.SortOldest, Limit: 2, Offset: 1})
	assertIDs(t, "filtered page", got, "b02", "b03")
	if total != 4 {
		t.Fatalf("filtered total = %d; want 4", total)
	}
}

func TestQueryBackupRecordsRetriedBy(t *testing.T) {
	s := queryFixture(t)
	page, err := s.QueryBackupRecords(context.Background(), store.BackupFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range page.Rows {
		switch row.Record.ID {
		case "b02":
			// The newest of its two retries.
			if row.RetriedBy == nil || row.RetriedBy.ID != "b08" || !row.RetriedBy.StartedAt.Equal(ts(8)) {
				t.Fatalf("b02 retried_by = %+v; want b08 at %v", row.RetriedBy, ts(8))
			}
		default:
			if row.RetriedBy != nil {
				t.Fatalf("%s retried_by = %+v; want none", row.Record.ID, row.RetriedBy)
			}
		}
	}
}

func TestQueryBackupRecordsInvalidFilter(t *testing.T) {
	s := queryFixture(t)
	for _, f := range []store.BackupFilter{
		{Limit: -1},
		{Limit: store.MaxListLimit + 1},
		{Offset: -1},
		{Sort: "sideways"},
		{From: ts(5), To: ts(4)},
	} {
		if _, err := s.QueryBackupRecords(context.Background(), f); !errors.Is(err, store.ErrInvalidFilter) {
			t.Errorf("QueryBackupRecords(%+v) error = %v; want ErrInvalidFilter", f, err)
		}
	}
}

func TestQueryBackupRecordsInjectionValuesAreLiteral(t *testing.T) {
	s := queryFixture(t)
	hostile := []string{
		"' OR '1'='1",
		"shop' --",
		"shop'; DROP TABLE backups; --",
		`") OR 1=1 --`,
		"%' OR 1=1 OR '%",
	}
	for _, v := range hostile {
		for _, f := range []store.BackupFilter{
			{Database: v}, {Status: models.BackupStatus(v)}, {JobID: v}, {ConnectionID: v},
			{Trigger: models.BackupTrigger(v)}, {RetryOf: v}, {Search: v},
		} {
			got, total := queryIDs(t, s, f)
			if len(got) != 0 || total != 0 {
				t.Fatalf("filter %+v matched %v (total %d); want nothing", f, got, total)
			}
		}
	}
	// A value that is literally stored matches only itself.
	rec := &models.BackupRecord{ID: "evil", Database: "shop' OR '1'='1", Status: models.StatusCompleted, StartedAt: ts(9)}
	if err := s.SaveBackupRecord(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	got, _ := queryIDs(t, s, store.BackupFilter{Database: rec.Database})
	assertIDs(t, "literal database", got, "evil")
	got, _ = queryIDs(t, s, store.BackupFilter{Search: "' or '1'="})
	assertIDs(t, "literal search", got, "evil")
	if all, _ := queryIDs(t, s, store.BackupFilter{}); len(all) != 9 {
		t.Fatalf("backups after hostile queries = %v; want 9", all)
	}
}

func TestQueryRestoreRecords(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	for _, r := range []*models.RestoreRecord{
		{ID: "r1", BackupID: "b01", SourceDatabase: "shop", TargetDatabase: "shop_rescue_1", Status: models.RestoreStatusCompleted, StartedAt: ts(1)},
		{ID: "r2", BackupID: "b01", SourceDatabase: "shop", TargetDatabase: "shop", Status: models.RestoreStatusFailed, StartedAt: ts(2)},
		{ID: "r3", BackupID: "b04", SourceDatabase: "Billing", TargetDatabase: "billing_copy", Status: models.RestoreStatusInProgress, StartedAt: ts(3)},
		{ID: "r4", BackupID: "b05", SourceDatabase: "logs", TargetDatabase: "logs", Status: models.RestoreStatusCompleted, StartedAt: ts(4)},
	} {
		if err := s.SaveRestoreRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name string
		f    store.RestoreFilter
		want []string
	}{
		{"all", store.RestoreFilter{}, []string{"r4", "r3", "r2", "r1"}},
		{"status", store.RestoreFilter{Status: models.RestoreStatusCompleted}, []string{"r4", "r1"}},
		{"backup", store.RestoreFilter{BackupID: "b01"}, []string{"r2", "r1"}},
		{"target database", store.RestoreFilter{TargetDatabase: "shop"}, []string{"r2"}},
		{"range", store.RestoreFilter{From: ts(2), To: ts(4)}, []string{"r3", "r2"}},
		{"search source database", store.RestoreFilter{Search: "billing"}, []string{"r3"}},
		{"search target database", store.RestoreFilter{Search: "RESCUE"}, []string{"r1"}},
		{"search id", store.RestoreFilter{Search: "r4"}, []string{"r4"}},
		{"oldest first", store.RestoreFilter{Sort: store.SortOldest}, []string{"r1", "r2", "r3", "r4"}},
		{"combination", store.RestoreFilter{BackupID: "b01", Status: models.RestoreStatusFailed}, []string{"r2"}},
		{"hostile value", store.RestoreFilter{TargetDatabase: "' OR 1=1 --"}, nil},
		{"hostile search", store.RestoreFilter{Search: "%"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, err := s.QueryRestoreRecords(ctx, tc.f)
			if err != nil {
				t.Fatal(err)
			}
			ids := make([]string, 0, len(page.Records))
			for _, r := range page.Records {
				ids = append(ids, r.ID)
			}
			assertIDs(t, "ids", ids, tc.want...)
			if page.Total != len(tc.want) {
				t.Fatalf("total = %d; want %d", page.Total, len(tc.want))
			}
		})
	}

	page, err := s.QueryRestoreRecords(ctx, store.RestoreFilter{Limit: 2, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 2 || page.Records[0].ID != "r3" || page.Records[1].ID != "r2" || page.Total != 4 {
		t.Fatalf("page = %d records (first %v), total %d; want r3, r2 of 4", len(page.Records), page.Records, page.Total)
	}
	if _, err = s.QueryRestoreRecords(ctx, store.RestoreFilter{Limit: store.MaxListLimit + 1}); !errors.Is(err, store.ErrInvalidFilter) {
		t.Fatalf("limit above max: error = %v; want ErrInvalidFilter", err)
	}

	dbs, err := s.ListRestoreDatabases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, "restore databases", dbs, "billing_copy", "logs", "shop", "shop_rescue_1")
}

func TestListBackupDatabases(t *testing.T) {
	s := queryFixture(t)
	dbs, err := s.ListBackupDatabases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, "databases", dbs, "Billing", "billing", "logs", "shop")

	empty, err := storetest.New(t).ListBackupDatabases(context.Background())
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty store databases = %#v, %v; want []", empty, err)
	}
}

func TestQueryBackupRecordsTimeRangeOutsideNanos(t *testing.T) {
	// The time range bounds compare against the same integer encoding as stored rows,
	// including records far outside the nanosecond range.
	s := storetest.New(t)
	ctx := context.Background()
	old := &models.BackupRecord{ID: "ancient", Database: "x", Status: models.StatusCompleted, StartedAt: time.Date(1200, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := s.SaveBackupRecord(ctx, old); err != nil {
		t.Fatal(err)
	}
	got, _ := queryIDs(t, s, store.BackupFilter{To: ts(0)})
	assertIDs(t, "before", got, "ancient")
	got, _ = queryIDs(t, s, store.BackupFilter{From: ts(0)})
	assertIDs(t, "after", got)
}
