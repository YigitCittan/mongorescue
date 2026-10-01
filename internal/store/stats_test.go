package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func TestBackupStatsAggregates(t *testing.T) {
	s := queryFixture(t)
	ctx := context.Background()
	// Sizes on completed and on failed records: only completed ones count.
	for id, size := range map[string]int64{"b01": 100, "b03": 20, "b08": 3, "b02": 1000} {
		rec, err := s.GetBackupRecord(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		rec.SizeBytes = size
		if err := s.SaveBackupRecord(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	st, err := s.BackupStats(ctx, ts(3))
	if err != nil {
		t.Fatal(err)
	}
	want := map[models.BackupStatus]int{models.StatusCompleted: 4, models.StatusFailed: 2, models.StatusInProgress: 1, models.StatusPruned: 1}
	if st.Total != 8 || fmt.Sprint(st.ByStatus) != fmt.Sprint(want) {
		t.Fatalf("counts = %d %v; want 8 %v", st.Total, st.ByStatus, want)
	}
	if st.CompletedBytes != 123 {
		t.Fatalf("completed bytes = %d; want 123", st.CompletedBytes)
	}
	// Failures: b02 at ts(2) and b04 at ts(4); since ts(3) only b04.
	if st.FailedSince != 1 || st.Active() != 1 {
		t.Fatalf("failed since = %d, active = %d; want 1, 1", st.FailedSince, st.Active())
	}
	if st.Last == nil || st.Last.ID != "b08" {
		t.Fatalf("last = %+v; want b08", st.Last)
	}

	empty, err := storetest.New(t).BackupStats(ctx, ts(0))
	if err != nil || empty.Total != 0 || empty.Last != nil || empty.CompletedBytes != 0 {
		t.Fatalf("empty store stats = %+v, %v", empty, err)
	}
}

func TestLatestJobBackups(t *testing.T) {
	s := queryFixture(t)
	ctx := context.Background()
	// Two records of j2 share the newest start time: the greater ID wins.
	if err := s.SaveBackupRecord(ctx, &models.BackupRecord{ID: "b06z", Database: "logs", JobID: "j2", Status: models.StatusFailed, StartedAt: ts(6)}); err != nil {
		t.Fatal(err)
	}
	latest, err := s.LatestJobBackups(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 2 || latest["j1"].ID != "b02" || latest["j2"].ID != "b06z" {
		t.Fatalf("latest = %v", ids(latest))
	}
	done, err := s.LatestJobBackups(ctx, models.StatusCompleted)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 2 || done["j1"].ID != "b01" || done["j2"].ID != "b06" {
		t.Fatalf("latest completed = %v", ids(done))
	}
}

func ids(m map[string]*models.BackupRecord) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v.ID
	}
	return out
}

func TestRestoreStats(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	for i, st := range []models.RestoreStatus{models.RestoreStatusCompleted, models.RestoreStatusCompleted, models.RestoreStatusInProgress, models.RestoreStatusPending, models.RestoreStatusFailed} {
		if err := s.SaveRestoreRecord(ctx, &models.RestoreRecord{ID: fmt.Sprintf("r%d", i), Status: st, StartedAt: ts(i)}); err != nil {
			t.Fatal(err)
		}
	}
	rs, err := s.RestoreStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rs.Total != 5 || rs.Active() != 2 || rs.ByStatus[models.RestoreStatusCompleted] != 2 {
		t.Fatalf("restore stats = %+v", rs)
	}
}

func TestQueryBackupRecordsByIDs(t *testing.T) {
	s := queryFixture(t)
	got, total := queryIDs(t, s, store.BackupFilter{IDs: []string{"b03", "b07", "missing", "b0"}})
	assertIDs(t, "ids", got, "b07", "b03")
	if total != 2 {
		t.Fatalf("total = %d; want 2", total)
	}
	got, _ = queryIDs(t, s, store.BackupFilter{IDs: []string{"b01", "b02"}, Status: models.StatusFailed})
	assertIDs(t, "ids with status", got, "b02")
	got, _ = queryIDs(t, s, store.BackupFilter{IDs: []string{"' OR '1'='1", "b01') OR ('1'='1"}})
	assertIDs(t, "hostile ids", got)

	many := make([]string, store.MaxFilterIDs+1)
	for i := range many {
		many[i] = fmt.Sprintf("id%d", i)
	}
	if _, err := s.QueryBackupRecords(context.Background(), store.BackupFilter{IDs: many}); !errors.Is(err, store.ErrInvalidFilter) {
		t.Fatalf("%d ids: error = %v; want ErrInvalidFilter", len(many), err)
	}
	got, _ = queryIDs(t, s, store.BackupFilter{IDs: many[:store.MaxFilterIDs]})
	assertIDs(t, "max ids", got)
}
