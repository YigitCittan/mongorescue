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
			fmt.Sprintf("conn_%03d", i%500), "", fmt.Sprintf(`{"next_run":"2026-10-%02dT%02d:00:00Z"}`, 1+i%28, i%24)); err != nil {
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
		"all":                {Limit: 100},
		"connection":         {Limit: 100, ConnectionID: "conn_007"},
		"enabled":            {Limit: 100, Enabled: &on},
		"connection, access": {Limit: 100, ConnectionID: "conn_007", Connections: auth.OnlyConnections("conn_007", "conn_008")},
	} {
		count, cargs := jobCountSQL(q)
		if p := plan(count, cargs...); !strings.Contains(p, "COVERING INDEX") {
			t.Errorf("%s count: plan %q; want a covering index", name, p)
		}
		for _, sort := range []JobSort{JobSortName, JobSortCreatedAt, JobSortNextRun} {
			for _, desc := range []bool{false, true} {
				for _, after := range []*JobKey{nil, {Str: "load-05000", Int: 5, Real: 2461320.5, ID: "job_x05000"}, {Null: true, ID: "job_x05000"}} {
					q := q
					q.Sort, q.Desc, q.After = sort, desc, after
					if after != nil && after.Null && sort != JobSortNextRun {
						continue
					}
					for _, seg := range jobWindowSQL(q) {
						p := plan(seg.query+" LIMIT ?", append(seg.args, 101)...)
						// Without filters, the rows without a next run are read in ID
						// order; filtered, the few of the filter are sorted.
						if seg.null {
							if q.ConnectionID == "" && q.Enabled == nil && strings.Contains(p, "TEMP B-TREE") {
								t.Errorf("%s, %s desc=%v, without a value: plan %q; want ID order", name, sort, desc, p)
							}
							continue
						}
						if strings.Contains(p, "TEMP B-TREE") || !strings.Contains(p, "INDEX jobs_by_") {
							t.Errorf("%s, %s desc=%v after=%v: plan %q; want a jobs index in page order", name, sort, desc, after != nil, p)
						}
					}
				}
			}
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
