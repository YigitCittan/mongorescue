package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// TestScaleQueryPlans checks the query plans of the jobs list and the history at
// 10,000 jobs (issue #156): every page and count of the jobs list is answered from
// a jobs index in page order (no temporary sort, no table scan), and the
// history's per-day and stored-size aggregates and the newest backup per job and
// database read covering indexes only.
func TestScaleQueryPlans(t *testing.T) {
	s, now := historyFixture(t, 20, 20000)
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 10000 {
		if _, err := tx.ExecContext(ctx, upsertJobSQL, fmt.Sprintf("job_x%05d", i), fmt.Sprintf("load-%05d", i), "db", i%2, 0,
			fmt.Sprintf("conn_%03d", i%500), "", `{}`); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "ANALYZE"); err != nil {
		t.Fatal(err)
	}
	on := true
	plan := func(query string, args ...any) string { return strings.Join(queryPlan(t, s.db, query, args...), "\n") }
	for name, q := range map[string]JobQuery{
		"first page":         {Limit: 100},
		"next page":          {Limit: 100, AfterName: "load-05000", AfterID: "job_x05000"},
		"connection":         {Limit: 100, ConnectionID: "conn_007", AfterName: "load-00007", AfterID: "job_x00007"},
		"enabled":            {Limit: 100, Enabled: &on, AfterName: "load-05000", AfterID: "job_x05000"},
		"connection, access": {Limit: 100, ConnectionID: "conn_007", Connections: auth.OnlyConnections("conn_007", "conn_008")},
	} {
		query, args := jobWindowSQL(q)
		p := plan(query, args...)
		if strings.Contains(p, "TEMP B-TREE") || !strings.Contains(p, "USING INDEX jobs_by_") {
			t.Errorf("%s page: plan %q; want a jobs index in page order", name, p)
		}
		count, cargs := jobCountSQL(q)
		if p := plan(count, cargs...); !strings.Contains(p, "COVERING INDEX") {
			t.Errorf("%s count: plan %q; want a covering index", name, p)
		}
	}

	starts := historyDayStarts(now, 30)
	dayArgs := []any{}
	for i := range 30 {
		dayArgs = append(dayArgs, i, timeKey(starts[i]), timeKey(starts[i+1]))
	}
	completed := string(models.StatusCompleted)
	for name, q := range map[string]struct {
		sql  string
		args []any
	}{
		"history days":  {historyDaysSQL(30), dayArgs},
		"bytes before":  {historyBytesBeforeSQL, []any{completed, timeKey(starts[0])}},
		"latest per db": {latestJobsDatabaseBackupsSQL(2), []any{"job_01", "job_02", completed, completed, completed, completed}},
	} {
		if p := plan(q.sql, q.args...); !strings.Contains(p, "COVERING INDEX backups_by_") {
			t.Errorf("%s: plan %q; want a covering backups index", name, p)
		}
	}
}
