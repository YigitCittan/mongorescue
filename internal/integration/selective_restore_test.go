//go:build integration

package integration

import (
	"context"
	"slices"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

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
