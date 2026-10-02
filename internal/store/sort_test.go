package store_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// sortFixture stores four backups and three restores whose sizes, durations,
// databases and statuses order differently from their start times.
func sortFixture(t *testing.T) *store.SQLiteStore {
	t.Helper()
	s := storetest.New(t)
	ctx := context.Background()
	for _, r := range []*models.BackupRecord{
		{ID: "s1", Database: "shop", Status: models.StatusCompleted, SizeBytes: 300, DurationSeconds: 5, StartedAt: ts(1)},
		{ID: "s2", Database: "billing", Status: models.StatusFailed, SizeBytes: 0, StartedAt: ts(2)},
		{ID: "s3", Database: "crm", Status: models.StatusCompleted, SizeBytes: 900, DurationSeconds: 1.5, StartedAt: ts(3)},
		{ID: "s4", Database: "crm", Status: models.StatusInProgress, SizeBytes: 100, DurationSeconds: 60, StartedAt: ts(4)},
	} {
		if err := s.SaveBackupRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []*models.RestoreRecord{
		{ID: "r1", BackupID: "s1", TargetDatabase: "zeta", Status: models.RestoreStatusFailed, DurationSeconds: 9, StartedAt: ts(1)},
		{ID: "r2", BackupID: "s1", TargetDatabase: "alpha", Status: models.RestoreStatusCompleted, DurationSeconds: 2, StartedAt: ts(2)},
		{ID: "r3", BackupID: "s3", TargetDatabase: "mid", Status: models.RestoreStatusCompleted, StartedAt: ts(3)},
	} {
		if err := s.SaveRestoreRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestQueryBackupRecordsSortColumns(t *testing.T) {
	s := sortFixture(t)
	cases := []struct {
		by    store.SortKey
		order store.SortOrder
		want  string
	}{
		{"", "", "s4,s3,s2,s1"},
		{store.SortStartedAt, store.SortOldest, "s1,s2,s3,s4"},
		{store.SortSize, store.SortNewest, "s3,s1,s4,s2"},
		{store.SortSize, store.SortOldest, "s2,s4,s1,s3"},
		{store.SortDuration, store.SortNewest, "s4,s1,s3,s2"},
		{store.SortDuration, store.SortOldest, "s2,s3,s1,s4"},
		// Ties (two crm backups, two completed ones) keep the newest first.
		{store.SortDatabase, store.SortOldest, "s2,s4,s3,s1"},
		{store.SortDatabase, store.SortNewest, "s1,s4,s3,s2"},
		{store.SortStatus, store.SortOldest, "s3,s1,s2,s4"},
	}
	for _, tc := range cases {
		ids, _ := queryIDs(t, s, store.BackupFilter{SortBy: tc.by, Sort: tc.order})
		if got := strings.Join(ids, ","); got != tc.want {
			t.Errorf("sort %q %q = %s; want %s", tc.by, tc.order, got, tc.want)
		}
	}
	// Paging follows the chosen order.
	ids, total := queryIDs(t, s, store.BackupFilter{SortBy: store.SortSize, Sort: store.SortNewest, Limit: 2, Offset: 1})
	if strings.Join(ids, ",") != "s1,s4" || total != 4 {
		t.Errorf("paged size sort = %v of %d; want s1,s4 of 4", ids, total)
	}
}

func TestQueryRestoreRecordsSortColumns(t *testing.T) {
	s := sortFixture(t)
	ids := func(f store.RestoreFilter) string {
		t.Helper()
		page, err := s.QueryRestoreRecords(context.Background(), f)
		if err != nil {
			t.Fatalf("QueryRestoreRecords(%+v): %v", f, err)
		}
		out := make([]string, 0, len(page.Records))
		for _, r := range page.Records {
			out = append(out, r.ID)
		}
		return strings.Join(out, ",")
	}
	if got := ids(store.RestoreFilter{SortBy: store.SortDatabase, Sort: store.SortOldest}); got != "r2,r3,r1" {
		t.Errorf("database asc = %s; want r2,r3,r1 (by target database)", got)
	}
	if got := ids(store.RestoreFilter{SortBy: store.SortDuration, Sort: store.SortNewest}); got != "r1,r2,r3" {
		t.Errorf("duration desc = %s; want r1,r2,r3", got)
	}
	if got := ids(store.RestoreFilter{SortBy: store.SortStatus, Sort: store.SortOldest}); got != "r3,r2,r1" {
		t.Errorf("status asc = %s; want r3,r2,r1", got)
	}
}

func TestSortColumnsAreAWhitelist(t *testing.T) {
	s := sortFixture(t)
	ctx := context.Background()
	for _, key := range []store.SortKey{"id", "data", "b.id; DROP TABLE backups", "started_at DESC", "SIZE"} {
		if _, err := s.QueryBackupRecords(ctx, store.BackupFilter{SortBy: key}); !errors.Is(err, store.ErrInvalidFilter) {
			t.Errorf("backups sorted by %q: err = %v; want ErrInvalidFilter", key, err)
		}
	}
	// Restores have no size.
	if _, err := s.QueryRestoreRecords(ctx, store.RestoreFilter{SortBy: store.SortSize}); !errors.Is(err, store.ErrInvalidFilter) {
		t.Errorf("restores sorted by size: err = %v; want ErrInvalidFilter", err)
	}
	if slices.Contains(store.RestoreSortKeys, store.SortSize) || !slices.Contains(store.BackupSortKeys, store.SortSize) {
		t.Errorf("sort keys: backups %v, restores %v", store.BackupSortKeys, store.RestoreSortKeys)
	}
	// The table survived.
	if ids, total := queryIDs(t, s, store.BackupFilter{}); total != 4 || len(ids) != 4 {
		t.Fatalf("after hostile sort keys: %v of %d", ids, total)
	}
}
