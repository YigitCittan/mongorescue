//go:build integration

package integration

import (
	"context"
	"slices"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// collectionNames returns the sorted collection names of db.
func (m *mongoEnv) collectionNames(t *testing.T, db string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	names, err := m.Client.Database(db).ListCollectionNames(ctx, bson.D{})
	if err != nil {
		t.Fatalf("list collections of %s: %v", db, err)
	}
	slices.Sort(names)
	return names
}

// TestSelectiveRestoreTreatsWildcardsLiterally restores a collection named "a*" next
// to "ab", "abc" and `a\b`: mongorestore reads '*' as a wildcard and '\' as its escape
// character, so an unescaped selection would select, and with --drop drop, every
// collection starting with "a". It also checks a database whose name has a '*', and
// that mongodump's --excludeCollection=a* skips "a*" only.
func TestSelectiveRestoreTreatsWildcardsLiterally(t *testing.T) {
	env := requireMongo(t)
	st, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	db := env.uniqueDB(t, "wild")
	env.seed(t, db, "a*", 5)
	env.seed(t, db, "ab", 3)
	env.seed(t, db, "abc", 2)
	env.seed(t, db, `a\b`, 4)
	bkp := runBackup(t, env, st, db, true)

	// In place with --drop: only "a*" is dropped and restored.
	target := db + "_t"
	env.seed(t, target, "a*", 1)
	env.seed(t, target, "ab", 7)
	env.seed(t, target, "abc", 6)
	no := false
	runRestore(t, env, st, models.RestoreRequest{
		BackupID: bkp.ID, TargetDatabase: target, SafeClone: &no, ConfirmInPlace: true,
		DropTarget: true, SelectedCollections: []string{"a*"},
	}, bkp)
	for coll, want := range map[string]int64{"a*": 5, "ab": 7, "abc": 6} {
		if got := env.count(t, target, coll); got != want {
			t.Fatalf("%s.%s = %d documents after restoring a* with --drop; want %d", target, coll, got, want)
		}
	}
	if got := env.collectionNames(t, target); !slices.Equal(got, []string{"a*", "ab", "abc"}) {
		t.Fatalf("target holds %q", got)
	}

	// Safe clone: "a*" and `a\b` select exactly themselves.
	rst := runRestore(t, env, st, models.RestoreRequest{BackupID: bkp.ID, SelectedCollections: []string{"a*", `a\b`}}, bkp)
	if got := env.collectionNames(t, rst.TargetDatabase); !slices.Equal(got, []string{"a*", `a\b`}) {
		t.Fatalf("clone %s holds %q; want a* and a\\b only", rst.TargetDatabase, got)
	}

	// A database named with a '*' restores into its own clone, selection included.
	starDB := db + "*"
	env.seed(t, starDB, "a*", 2)
	env.seed(t, starDB, "ab", 2)
	starBkp := runBackup(t, env, st, starDB, false)
	rst = runRestore(t, env, st, models.RestoreRequest{BackupID: starBkp.ID, SelectedCollections: []string{"a*"}}, starBkp)
	if got := env.collectionNames(t, rst.TargetDatabase); !slices.Equal(got, []string{"a*"}) {
		t.Fatalf("clone %s of %s holds %q; want a* only", rst.TargetDatabase, starDB, got)
	}
	if got := env.count(t, rst.TargetDatabase, "a*"); got != 2 {
		t.Fatalf("restored %s.a* = %d; want 2", rst.TargetDatabase, got)
	}

	// mongodump matches --excludeCollection literally.
	logger, _ := captureLogger()
	excl, err := backup.NewEngine(st, env.URI, backup.WithLogger(logger)).Run(ctx, models.BackupOptions{Database: db, ExcludeCollections: []string{"a*"}, MongoURI: env.URI})
	if err != nil || excl.Status != models.StatusCompleted {
		t.Fatalf("backup excluding a*: %+v, %v", excl, err)
	}
	t.Cleanup(func() { _ = st.Delete(context.Background(), excl.StorageKey) })
	list, err := restore.NewEngine(st, env.URI).ArchiveCollections(ctx, excl)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range list {
		names = append(names, c.Name)
	}
	if !slices.Equal(names, []string{`a\b`, "ab", "abc"}) {
		t.Fatalf("backup excluding a* holds %q; want every other collection", names)
	}
}

// TestSelectiveRestore backs up three collections and a view with the real
// mongodump, lists them from the archive prelude, restores one collection into a safe
// clone and checks that only that one exists there. A second, in-place restore of one
// collection with --drop must drop only that collection in the target.
func TestSelectiveRestore(t *testing.T) {
	env := requireMongo(t)
	st, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db := env.uniqueDB(t, "sel")
	env.seed(t, db, "orders", 30)
	env.seed(t, db, "customers", 20)
	env.seed(t, db, "events", 10)
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if err := env.Client.Database(db).CreateView(ctx, "big_orders", "orders", bson.A{bson.D{{Key: "$match", Value: bson.D{{Key: "seq", Value: bson.D{{Key: "$gt", Value: 10}}}}}}}); err != nil {
		t.Fatalf("create view: %v", err)
	}

	for _, gzip := range []bool{true, false} {
		bkp := runBackup(t, env, st, db, gzip)

		// The archive prelude lists every collection and the view, without reading data.
		list, err := restore.NewEngine(st, env.URI).ArchiveCollections(ctx, bkp)
		if err != nil {
			t.Fatalf("gzip %v: ArchiveCollections: %v", gzip, err)
		}
		types := map[string]string{}
		for _, c := range list {
			types[c.Name] = c.Type
		}
		if len(list) != 4 || types["orders"] != models.CollectionTypeCollection || types["customers"] != models.CollectionTypeCollection ||
			types["events"] != models.CollectionTypeCollection || types["big_orders"] != models.CollectionTypeView {
			t.Fatalf("gzip %v: archive collections = %+v", gzip, list)
		}

		rst := runRestore(t, env, st, models.RestoreRequest{BackupID: bkp.ID, SelectedCollections: []string{"customers"}}, bkp)
		if !slices.Equal(rst.SelectedCollections, []string{"customers"}) {
			t.Fatalf("restore record selection = %q", rst.SelectedCollections)
		}
		if got := env.collectionNames(t, rst.TargetDatabase); !slices.Equal(got, []string{"customers"}) {
			t.Fatalf("gzip %v: clone %s holds %q; want only customers", gzip, rst.TargetDatabase, got)
		}
		if got := env.count(t, rst.TargetDatabase, "customers"); got != 20 {
			t.Fatalf("restored customers = %d; want 20", got)
		}
		if err := env.Client.Database(rst.TargetDatabase).Drop(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// In place with --drop: only the selected collection is dropped and restored.
	bkp := runBackup(t, env, st, db, true)
	target := db + "_drop"
	env.seed(t, target, "orders", 5)
	env.seed(t, target, "keep", 3)
	no := false
	runRestore(t, env, st, models.RestoreRequest{
		BackupID: bkp.ID, TargetDatabase: target, SafeClone: &no, ConfirmInPlace: true,
		DropTarget: true, SelectedCollections: []string{"orders"},
	}, bkp)
	if got := env.count(t, target, "orders"); got != 30 {
		t.Fatalf("orders after the drop and restore = %d; want 30 (the 5 old documents dropped)", got)
	}
	if got := env.count(t, target, "keep"); got != 3 {
		t.Fatalf("unselected collection keep = %d documents; want 3 (not dropped)", got)
	}
	if got := env.collectionNames(t, target); !slices.Equal(got, []string{"keep", "orders"}) {
		t.Fatalf("target holds %q; want keep and orders only", got)
	}
}
