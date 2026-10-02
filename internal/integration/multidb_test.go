//go:build integration

package integration

import (
	"context"
	"slices"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongoconn"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestPatternJobOverRealDatabases runs a pattern job over three real databases,
// creates a fourth one that matches, and runs it again: with auto_include_new the
// second run backs the new database up (and records it as known); without it, the
// run reports it as new and does not back it up.
func TestPatternJobOverRealDatabases(t *testing.T) {
	env := requireMongo(t)
	for _, auto := range []bool{true, false} {
		name := "auto_off"
		if auto {
			name = "auto_on"
		}
		t.Run(name, func(t *testing.T) {
			base := env.uniqueDB(t, "multi")
			for _, suffix := range []string{"_a", "_b", "_c"} {
				env.seed(t, base+suffix, "items", 5)
			}
			ctx := context.Background()
			metaStore := storetest.New(t)
			st, stErr := storage.NewLocalStorage(t.TempDir())
			if stErr != nil {
				t.Fatal(stErr)
			}
			prober := mongoconn.New()
			sched := scheduler.NewScheduler(metaStore, newBackupEngine(env, st), st, discardLogger,
				scheduler.WithDatabaseLister(func(ctx context.Context, _ string) ([]string, error) {
					dbs, err := prober.ListDatabases(ctx, env.URI)
					if err != nil {
						return nil, err
					}
					names := make([]string, len(dbs))
					for i, d := range dbs {
						names[i] = d.Name
					}
					return names, nil
				}))
			job := &models.Job{ID: "job_" + name, Name: name, CronExpression: "@daily", ConnectionID: "conn", Gzip: true,
				DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionPattern, Include: []string{base + "_*"}, AutoIncludeNew: auto}}
			if err := metaStore.SaveJob(ctx, job); err != nil {
				t.Fatal(err)
			}

			run := func() *models.JobRun {
				t.Helper()
				if _, err := sched.TriggerJob(ctx, job.ID); err != nil {
					t.Fatalf("run: %v", err)
				}
				list, err := metaStore.ListJobRuns(ctx, job.ID, 1)
				if err != nil || len(list) != 1 {
					t.Fatalf("runs = %+v, %v", list, err)
				}
				return list[0]
			}
			databasesOf := func(r *models.JobRun) []string {
				var out []string
				for _, d := range r.Databases {
					if d.Status != models.StatusCompleted {
						t.Errorf("database %s of run %s: %s %s", d.Database, r.ID, d.Status, d.Error)
					}
					out = append(out, d.Database)
				}
				slices.Sort(out)
				return out
			}

			first := run()
			want := []string{base + "_a", base + "_b", base + "_c"}
			if got := databasesOf(first); first.Status != models.JobRunOK || !slices.Equal(got, want) {
				t.Fatalf("first run = %s over %v; want ok over %v", first.Status, got, want)
			}
			// Every database has its own completed, verified-on-write record in the run.
			page, err := metaStore.QueryBackupRecords(ctx, store.BackupFilter{RunID: first.ID})
			if err != nil || page.Total != 3 {
				t.Fatalf("backups of the first run = %d, %v", page.Total, err)
			}
			for _, row := range page.Rows {
				if row.Record.SHA256 == "" || row.Record.SizeBytes == 0 || row.Record.Database == "" {
					t.Errorf("backup %+v", row.Record)
				}
			}

			env.seed(t, base+"_d", "items", 3)
			second := run()
			stored, err := metaStore.GetJob(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if auto {
				want = append(want, base+"_d")
				if got := databasesOf(second); !slices.Equal(got, want) || !slices.Equal(second.AddedDatabases, []string{base + "_d"}) {
					t.Fatalf("second run backed up %v (added %v); want %v with the new database", got, second.AddedDatabases, want)
				}
				if !slices.Contains(stored.KnownDatabases, base+"_d") {
					t.Errorf("known databases %v lack the new one", stored.KnownDatabases)
				}
				return
			}
			if got := databasesOf(second); !slices.Equal(got, want) {
				t.Fatalf("second run backed up %v; want only %v", got, want)
			}
			if !slices.Equal(second.NewDatabases, []string{base + "_d"}) {
				t.Errorf("new databases = %v; want the fourth reported", second.NewDatabases)
			}
			if slices.Contains(stored.KnownDatabases, base+"_d") {
				t.Errorf("known databases %v include the new one without auto_include_new", stored.KnownDatabases)
			}
		})
	}
}
