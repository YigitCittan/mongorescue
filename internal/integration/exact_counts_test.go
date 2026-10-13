//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongoconn"
)

// testExactCountsAt counts collections at a past oplog position after they were
// written to (inserts, deletes and a transaction): a snapshot read and the oplog
// derivation both give the counts at that position, and a collection renamed
// since is unverified.
func testExactCountsAt(t *testing.T) {
	env := requireWholeInstance(t)
	requireReplicaSet(t, env)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	prober := mongoconn.New()
	db := env.uniqueDB(t, "exactct")
	env.seed(t, db, "busy", 100)
	env.seed(t, db, "txn", 100)
	busy, txn := env.Client.Database(db).Collection("busy"), env.Client.Database(db).Collection("txn")
	if err := env.Client.Database(db).CreateCollection(ctx, "capped", options.CreateCollection().SetCapped(true).SetSizeInBytes(1<<20)); err != nil {
		t.Fatal(err)
	}
	namespaces := []string{db + ".busy", db + ".txn"}

	// The position: every write so far is majority-committed, 100 documents each.
	at := mustNewest(ctx, t, prober, env.URI)

	// Writes after it: busy ends at 120, txn at 103.
	for i := range 30 {
		if _, err := busy.InsertOne(ctx, bson.D{{Key: "later", Value: i}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := busy.DeleteMany(ctx, bson.D{{Key: "seq", Value: bson.D{{Key: "$lt", Value: 10}}}}); err != nil {
		t.Fatal(err)
	}
	sess, err := env.Client.StartSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.EndSession(ctx)
	if _, err = sess.WithTransaction(ctx, func(ctx context.Context) (any, error) {
		docs := []any{}
		for i := range 5 {
			docs = append(docs, bson.D{{Key: "txn", Value: i}})
		}
		if _, txnErr := txn.InsertMany(ctx, docs); txnErr != nil {
			return nil, txnErr
		}
		return txn.DeleteMany(ctx, bson.D{{Key: "seq", Value: bson.D{{Key: "$lt", Value: 2}}}})
	}); err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{db + ".busy": 100, db + ".txn": 100}

	got, err := prober.CountsAt(ctx, env.URI, namespaces, at)
	if err != nil {
		t.Fatal(err)
	}
	assertExactCounts(t, "snapshot", got, models.CountMethodSnapshot, want)
	derived, err := prober.OplogCountsAt(ctx, env.URI, namespaces, at)
	if err != nil {
		t.Fatal(err)
	}
	assertExactCounts(t, "oplog", derived, models.CountMethodOplog, want)

	// A collection that never existed is absent at the position; a capped one is
	// counted by the snapshot read only (its evictions leave no oplog entry). MongoDB
	// 5.0 refuses snapshot reads of capped collections, so its count falls back to
	// the oplog and is unverified there.
	more := []string{db + ".capped", db + ".never"}
	snap, err := prober.CountsAt(ctx, env.URI, more, at)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Absent[db+".never"] || snap.Counts[db+".never"] != 0 {
		t.Errorf("snapshot of never: %+v", snap)
	}
	if n, ok := snap.Counts[db+".capped"]; (ok && (n != 0 || snap.Absent[db+".capped"])) ||
		(!ok && !strings.Contains(snap.Unverified[db+".capped"], "capped")) {
		t.Errorf("snapshot of capped: %+v", snap)
	}
	derived, err = prober.OplogCountsAt(ctx, env.URI, more, at)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(derived.Unverified[db+".capped"], "capped") || !derived.Absent[db+".never"] {
		t.Errorf("oplog of capped and never: %+v", derived)
	}

	// A collection renamed since the position has no count there.
	if err = env.Client.Database("admin").RunCommand(ctx, bson.D{{Key: "renameCollection", Value: db + ".txn"}, {Key: "to", Value: db + ".moved"}}).Err(); err != nil {
		t.Fatal(err)
	}
	for name, count := range map[string]func() (*models.ExactCounts, error){
		"snapshot": func() (*models.ExactCounts, error) { return prober.CountsAt(ctx, env.URI, namespaces, at) },
		"oplog":    func() (*models.ExactCounts, error) { return prober.OplogCountsAt(ctx, env.URI, namespaces, at) },
	} {
		res, countErr := count()
		if countErr != nil {
			t.Fatal(countErr)
		}
		if res.Counts[db+".busy"] != 100 || !strings.Contains(res.Unverified[db+".txn"], "renamed to") {
			t.Errorf("%s after the rename: %+v", name, res)
		}
		if _, ok := res.Counts[db+".txn"]; ok {
			t.Errorf("%s counted the renamed collection: %+v", name, res)
		}
	}
}

// assertExactCounts checks counts against want, all exact and taken by method.
func assertExactCounts(t *testing.T, what string, got *models.ExactCounts, method models.CountMethod, want map[string]int64) {
	t.Helper()
	if got.Method != method || len(got.Unverified) != 0 || len(got.Counts) != len(want) {
		t.Fatalf("%s: %+v", what, got)
	}
	for ns, n := range want {
		if got.Counts[ns] != n {
			t.Errorf("%s: %s = %d, want %d", what, ns, got.Counts[ns], n)
		}
	}
}
