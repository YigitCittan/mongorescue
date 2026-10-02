package backup

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

func TestBuildDumpArgsUsersAndRoles(t *testing.T) {
	e := NewEngine(storage.NewMockStorage(), "")
	for _, tc := range []struct {
		name    string
		opts    models.BackupOptions
		want    []string
		applies bool
	}{
		{"off", models.BackupOptions{Database: "shop"}, []string{"--config=x", "--archive", "--db=shop"}, false},
		{"on", models.BackupOptions{Database: "shop", IncludeUsersAndRoles: true, Gzip: true},
			[]string{"--config=x", "--archive", "--gzip", "--db=shop", "--dumpDbUsersAndRoles"}, true},
		{"with exclusions", models.BackupOptions{Database: "shop", IncludeUsersAndRoles: true, ExcludeCollections: []string{"logs"}},
			[]string{"--config=x", "--archive", "--db=shop", "--dumpDbUsersAndRoles", "--excludeCollection=logs"}, true},
		// The admin database holds every user and role as regular data.
		{"admin database", models.BackupOptions{Database: "admin", IncludeUsersAndRoles: true}, []string{"--config=x", "--archive", "--db=admin"}, false},
		// mongodump needs --db for --dumpDbUsersAndRoles.
		{"no database", models.BackupOptions{IncludeUsersAndRoles: true}, []string{"--config=x", "--archive"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.buildDumpArgs("--config=x", tc.opts); !slices.Equal(got, tc.want) {
				t.Fatalf("args = %q\nwant  %q", got, tc.want)
			}
			if got := tc.opts.UsersAndRolesApply(); got != tc.applies {
				t.Fatalf("UsersAndRolesApply = %v; want %v", got, tc.applies)
			}
		})
	}
}

// TestUsersAndRolesDumpRecordsAndExcludes checks that a dump with users and roles is
// recorded as such and that a single-collection filter becomes exclusions, since
// mongodump refuses --collection together with --dumpDbUsersAndRoles.
func TestUsersAndRolesDumpRecordsAndExcludes(t *testing.T) {
	runner := &captureRunner{}
	lister := func(context.Context, string, string) ([]string, error) {
		return []string{"orders", "logs", "system.views"}, nil
	}
	e := NewEngine(storage.NewMockStorage(), "", WithRunner(runner.run), WithCollectionLister(lister))
	rec, err := e.Run(context.Background(), models.BackupOptions{
		Database: "shop", Collections: []string{"orders"}, IncludeUsersAndRoles: true, MongoURI: "mongodb://db.internal:27017/",
	})
	if err != nil || rec.Status != models.StatusCompleted {
		t.Fatalf("backup = %+v, %v", rec, err)
	}
	if !rec.UsersAndRoles {
		t.Fatal("record does not say the backup contains users and roles")
	}
	want := []string{"--archive", "--db=shop", "--dumpDbUsersAndRoles", "--excludeCollection=logs"}
	if got := runner.args[1:]; !slices.Equal(got, want) {
		t.Fatalf("args = %q\nwant  %q", got, want)
	}

	plain, err := e.Run(context.Background(), models.BackupOptions{Database: "shop", MongoURI: "mongodb://db.internal:27017/"})
	if err != nil || plain.UsersAndRoles {
		t.Fatalf("plain backup = %+v, %v; want no users and roles", plain, err)
	}
}

func TestUsersAndRolesSingleCollectionNeedsLister(t *testing.T) {
	runner := &captureRunner{}
	e := NewEngine(storage.NewMockStorage(), "", WithRunner(runner.run))
	_, err := e.Run(context.Background(), models.BackupOptions{
		Database: "shop", Collections: []string{"orders"}, IncludeUsersAndRoles: true, MongoURI: "mongodb://db.internal:27017/",
	})
	if !errors.Is(err, ErrCollectionFilter) {
		t.Fatalf("Run = %v; want ErrCollectionFilter", err)
	}
	if runner.runs != 0 {
		t.Fatal("mongodump ran")
	}
}
