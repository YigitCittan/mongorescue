package scheduler

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// filterArgs returns the collection-related mongodump arguments of db's dump.
func (f *multiFixture) filterArgs(db string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, a := range f.args[db] {
		if strings.HasPrefix(a, "--collection=") || strings.HasPrefix(a, "--excludeCollection=") || a == "--dumpDbUsersAndRoles" {
			out = append(out, a)
		}
	}
	return out
}

// runAdHoc prepares, begins and executes an ad-hoc run of req.
func (f *multiFixture) runAdHoc(t *testing.T, req AdHocRun) (*JobRunPlan, *models.JobRun) {
	t.Helper()
	plan, err := f.sched.PrepareAdHocRun(req)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = f.sched.BeginJobRun(ctx, plan); err != nil {
		t.Fatal(err)
	}
	run, _ := f.sched.ExecuteJobRun(ctx, plan)
	return plan, run
}

func TestAdHocRunGivesEveryDatabaseItsOwnCollectionFilter(t *testing.T) {
	f := newMultiFixtureWith(t, map[string][]string{
		"a": {"orders", "logs", "tmp"},
		"b": {"orders", "customers", "events"},
		"c": {"x"},
	})
	opts := adHocOptions()
	// Shared options never leak a filter into the databases of a run.
	opts.Collections, opts.ExcludeCollections = []string{"leak"}, []string{"leak"}
	plan, run := f.runAdHoc(t, AdHocRun{
		Options: opts, Databases: []string{"a", "b", "c"},
		Filters: []models.DatabaseFilter{
			{Name: "a", ExcludeCollections: []string{"logs", "tmp"}},
			{Name: "b", Collections: []string{"orders", "customers"}},
		},
	})
	if run.Status != models.JobRunOK {
		t.Fatalf("run = %+v", run)
	}
	want := map[string][]string{
		"a": {"--excludeCollection=logs", "--excludeCollection=tmp"},
		// Several collections become the exclusion of the others (expandCollections).
		"b": {"--excludeCollection=events"},
		"c": nil,
	}
	for db, w := range want {
		if got := f.filterArgs(db); !slices.Equal(got, w) {
			t.Errorf("args of %s = %q; want %q", db, got, w)
		}
	}
	// The record of an included filter names its collections, as for one database.
	if rec := plan.Records[1]; !slices.Equal(rec.Collections, []string{"orders", "customers"}) {
		t.Errorf("record of b = %+v", rec)
	}
	if rec := plan.Records[0]; len(rec.Collections) != 0 {
		t.Errorf("record of a = %+v; want no included collections", rec)
	}
}

func TestAdHocRunCollectionFilterFollowsTheSingleDatabaseRules(t *testing.T) {
	// Without a collection lister: one collection passes as --collection, several
	// fail the database with ErrCollectionFilter, and users and roles turn even one
	// collection into exclusions, which also needs the lister.
	f := newMultiFixture(t)
	opts := adHocOptions()
	_, run := f.runAdHoc(t, AdHocRun{
		Options: opts, Databases: []string{"one", "two"},
		Filters: []models.DatabaseFilter{
			{Name: "one", Collections: []string{"orders"}},
			{Name: "two", Collections: []string{"orders", "customers"}},
		},
	})
	if got := f.filterArgs("one"); !slices.Equal(got, []string{"--collection=orders"}) {
		t.Errorf("args of one = %q", got)
	}
	byDB := map[string]models.JobRunDatabase{}
	for _, d := range run.Databases {
		byDB[d.Database] = d
	}
	if d := byDB["two"]; d.Status != models.StatusFailed || !strings.Contains(d.Error, "collection filter") {
		t.Errorf("database two = %+v; want failed with the collection filter error", d)
	}

	g := newMultiFixtureWith(t, map[string][]string{"shop": {"orders", "logs"}})
	opts.IncludeUsersAndRoles = true
	g.runAdHoc(t, AdHocRun{
		Options: opts, Databases: []string{"shop", "crm"},
		Filters: []models.DatabaseFilter{{Name: "shop", Collections: []string{"orders"}}},
	})
	if got := g.filterArgs("shop"); !slices.Equal(got, []string{"--dumpDbUsersAndRoles", "--excludeCollection=logs"}) {
		t.Errorf("args of shop with users and roles = %q", got)
	}
	if got := g.filterArgs("crm"); !slices.Equal(got, []string{"--dumpDbUsersAndRoles"}) {
		t.Errorf("args of crm = %q", got)
	}
}

func TestAdHocRunOfOneDatabaseTakesItsEntryFilter(t *testing.T) {
	f := newMultiFixture(t)
	f.runAdHoc(t, AdHocRun{
		Options: adHocOptions(), Databases: []string{"shop"},
		Filters: []models.DatabaseFilter{{Name: "shop", ExcludeCollections: []string{"logs"}}},
	})
	if got := f.filterArgs("shop"); !slices.Equal(got, []string{"--excludeCollection=logs"}) {
		t.Errorf("args = %q", got)
	}
}

func TestJobRunAppliesTheSelectionsCollectionFilters(t *testing.T) {
	f := newMultiFixture(t, "a", "b", "c", "admin")
	f.job(t, "job_list", models.DatabaseSelection{
		Mode: models.SelectionList, Databases: []string{"a", "b"},
		CollectionFilters: []models.DatabaseFilter{{Name: "b", ExcludeCollections: []string{"logs"}}},
	}, nil)
	f.job(t, "job_all", models.DatabaseSelection{
		Mode:              models.SelectionAll,
		CollectionFilters: []models.DatabaseFilter{{Name: "c", Collections: []string{"orders"}}},
	}, nil)
	ctx := context.Background()
	for _, id := range []string{"job_list", "job_all"} {
		if _, err := f.sched.runBackupForJob(ctx, mustJob(t, f.store, id)); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		switch id {
		case "job_list":
			if got := f.filterArgs("a"); len(got) != 0 {
				t.Errorf("job_list a = %q; want the whole database", got)
			}
			if got := f.filterArgs("b"); !slices.Equal(got, []string{"--excludeCollection=logs"}) {
				t.Errorf("job_list b = %q", got)
			}
		case "job_all":
			if got := f.filterArgs("c"); !slices.Equal(got, []string{"--collection=orders"}) {
				t.Errorf("job_all c = %q", got)
			}
			for _, db := range []string{"a", "b"} {
				if got := f.filterArgs(db); len(got) != 0 {
					t.Errorf("job_all %s = %q; want the whole database", db, got)
				}
			}
		}
	}
}
