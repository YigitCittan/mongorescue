package oplog

import (
	"errors"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func remove(ns string, id any) bson.D {
	return bson.D{{Key: "op", Value: "d"}, {Key: "ns", Value: ns}, {Key: "ui", Value: uuid}, {Key: "o", Value: bson.D{{Key: "_id", Value: id}}}}
}

func update(ns string, id any) bson.D {
	return bson.D{{Key: "op", Value: "u"}, {Key: "ns", Value: ns}, {Key: "ui", Value: uuid},
		{Key: "o", Value: bson.D{{Key: "$v", Value: int32(2)}, {Key: "diff", Value: bson.D{}}}}, {Key: "o2", Value: bson.D{{Key: "_id", Value: id}}}}
}

// addAll feeds entries (fields, at T 10, 11, ...) to d.
func addAll(t *testing.T, d *CountDelta, entries ...bson.D) {
	t.Helper()
	for i, e := range entries {
		if err := d.Add(entryAt(t, ts(10+uint32(i), 1), e)); err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
	}
}

// wantDelta checks the delta of each namespace; a negative want of -1000 means unknown.
func wantDelta(t *testing.T, d *CountDelta, want map[string]int64) {
	t.Helper()
	for ns, w := range want {
		n, unknown := d.Delta(ns)
		switch {
		case w == unknownDelta && unknown == "":
			t.Errorf("%s: delta %d, want unknown", ns, n)
		case w != unknownDelta && unknown != "":
			t.Errorf("%s: unknown (%s), want %d", ns, unknown, w)
		case w != unknownDelta && n != w:
			t.Errorf("%s: delta %d, want %d", ns, n, w)
		}
	}
}

const unknownDelta = -1000

func TestCountDeltaCRUD(t *testing.T) {
	var d CountDelta
	addAll(t, &d,
		insert("shop.orders", 1), insert("shop.orders", 2), update("shop.orders", 1), remove("shop.orders", 2),
		insert("shop.users", 1),
		bson.D{{Key: "op", Value: "n"}, {Key: "ns", Value: ""}, {Key: "o", Value: bson.D{{Key: "msg", Value: "periodic noop"}}}},
		command("shop", bson.D{{Key: "createIndexes", Value: "orders"}, {Key: "v", Value: 2}, {Key: "key", Value: bson.D{{Key: "a", Value: 1}}}, {Key: "name", Value: "a_1"}}),
	)
	wantDelta(t, &d, map[string]int64{"shop.orders": 1, "shop.users": 1, "shop.other": 0, "crm.users": 0})
}

func TestCountDeltaApplyOps(t *testing.T) {
	var d CountDelta
	// Batched writes (7.0 and later) and retryable batched inserts (multiOpType 1)
	// are applyOps entries without a transaction.
	batched := command("admin", bson.D{{Key: "applyOps", Value: applyOps(insert("shop.orders", 1), insert("shop.orders", 2), remove("shop.users", 9))}})
	retryable := append(bson.D{{Key: "lsid", Value: lsid}, {Key: "txnNumber", Value: int64(3)}, {Key: "multiOpType", Value: int32(1)}},
		command("admin", bson.D{{Key: "applyOps", Value: applyOps(insert("shop.orders", 3))}})...)
	nested := command("admin", bson.D{{Key: "applyOps", Value: applyOps(
		command("admin", bson.D{{Key: "applyOps", Value: applyOps(insert("shop.orders", 4))}}))}})
	addAll(t, &d, batched, retryable, nested)
	wantDelta(t, &d, map[string]int64{"shop.orders": 4, "shop.users": -1})
}

func TestCountDeltaTransactions(t *testing.T) {
	t.Run("single entry", func(t *testing.T) {
		var d CountDelta
		addAll(t, &d, txnEntry(1, bson.D{{Key: "applyOps", Value: applyOps(insert("shop.orders", 1), insert("shop.orders", 2), remove("shop.users", 1))}}))
		wantDelta(t, &d, map[string]int64{"shop.orders": 2, "shop.users": -1})
	})
	t.Run("several entries count at the commit", func(t *testing.T) {
		var d CountDelta
		partial := txnEntry(1, bson.D{{Key: "applyOps", Value: applyOps(insert("shop.orders", 1))}, {Key: "partialTxn", Value: true}})
		commit := txnEntry(1, bson.D{{Key: "applyOps", Value: applyOps(insert("shop.orders", 2))}, {Key: "count", Value: int64(2)}})
		commit[len(commit)-1].Value = bson.D{{Key: "ts", Value: ts(10, 1)}, {Key: "t", Value: int64(1)}}
		addAll(t, &d, partial)
		wantDelta(t, &d, map[string]int64{"shop.orders": 0})
		if err := d.Add(entryAt(t, ts(11, 1), commit)); err != nil {
			t.Fatal(err)
		}
		wantDelta(t, &d, map[string]int64{"shop.orders": 2})
	})
	t.Run("uncommitted and aborted count nothing", func(t *testing.T) {
		var d CountDelta
		addAll(t, &d,
			txnEntry(1, bson.D{{Key: "applyOps", Value: applyOps(insert("shop.orders", 1))}, {Key: "partialTxn", Value: true}}),
			txnEntry(2, bson.D{{Key: "applyOps", Value: applyOps(insert("shop.orders", 2))}, {Key: "prepare", Value: true}}),
			txnEntry(2, bson.D{{Key: "abortTransaction", Value: 1}}),
		)
		wantDelta(t, &d, map[string]int64{"shop.orders": 0})
	})
	t.Run("prepared commits at commitTransaction", func(t *testing.T) {
		var d CountDelta
		addAll(t, &d,
			txnEntry(2, bson.D{{Key: "applyOps", Value: applyOps(insert("shop.orders", 2), remove("shop.orders", 1), remove("shop.orders", 3))}, {Key: "prepare", Value: true}}),
			txnEntry(2, bson.D{{Key: "commitTransaction", Value: 1}, {Key: "commitTimestamp", Value: ts(10, 1)}}),
		)
		wantDelta(t, &d, map[string]int64{"shop.orders": -1})
	})
	t.Run("begun before the entries read", func(t *testing.T) {
		var d CountDelta
		commit := txnEntry(1, bson.D{{Key: "applyOps", Value: applyOps(insert("shop.orders", 2))}, {Key: "count", Value: int64(2)}})
		commit[len(commit)-1].Value = bson.D{{Key: "ts", Value: ts(9, 1)}, {Key: "t", Value: int64(1)}}
		addAll(t, &d, insert("crm.users", 1), commit)
		wantDelta(t, &d, map[string]int64{"shop.orders": unknownDelta, "crm.users": unknownDelta})
		if why := d.DDL("shop.orders"); why != "" {
			t.Errorf("DDL = %q: a transaction changes no collection's existence", why)
		}
	})
	t.Run("prepared before the entries read", func(t *testing.T) {
		var d CountDelta
		addAll(t, &d, txnEntry(2, bson.D{{Key: "commitTransaction", Value: 1}}))
		wantDelta(t, &d, map[string]int64{"shop.orders": unknownDelta})
	})
}

func TestCountDeltaDDL(t *testing.T) {
	var d CountDelta
	addAll(t, &d,
		insert("shop.orders", 1),
		command("shop", bson.D{{Key: "drop", Value: "orders"}}),
		command("shop", bson.D{{Key: "create", Value: "fresh"}}),
		command("shop", bson.D{{Key: "renameCollection", Value: "shop.a"}, {Key: "to", Value: "shop.b"}, {Key: "dropTarget", Value: false}}),
		command("shop", bson.D{{Key: "emptycapped", Value: "log"}}),
		command("shop", bson.D{{Key: "convertToCapped", Value: "events"}, {Key: "size", Value: int64(1 << 20)}}),
		command("crm", bson.D{{Key: "dropDatabase", Value: 1}}),
		insert("crm.users", 1),
		command("blog", bson.D{{Key: "importCollection", Value: "posts"}}),
		insert("shop.users", 1),
	)
	wantDelta(t, &d, map[string]int64{
		"shop.orders": unknownDelta, "shop.fresh": unknownDelta, "shop.a": unknownDelta, "shop.b": unknownDelta,
		"shop.log": unknownDelta, "shop.events": unknownDelta, "crm.users": unknownDelta, "crm.other": unknownDelta,
		"blog.posts": unknownDelta, "shop.users": 1,
	})
	if _, why := d.Delta("shop.orders"); why != "dropped at Timestamp(11, 1)" {
		t.Errorf("reason = %q", why)
	}
	if _, why := d.Delta("crm.users"); !strings.Contains(why, "database crm: dropped") {
		t.Errorf("reason = %q", why)
	}
	for ns, ddl := range map[string]bool{"shop.orders": true, "shop.b": true, "crm.any": true, "blog.posts": true, "shop.users": false} {
		if got := d.DDL(ns); (got != "") != ddl {
			t.Errorf("DDL(%s) = %q", ns, got)
		}
	}
}

func TestCountDeltaTransactionDrop(t *testing.T) {
	var d CountDelta
	addAll(t, &d, txnEntry(1, bson.D{{Key: "applyOps", Value: applyOps(
		command("shop", bson.D{{Key: "create", Value: "fresh"}}), insert("shop.fresh", 1), insert("shop.orders", 1))}}))
	wantDelta(t, &d, map[string]int64{"shop.fresh": unknownDelta, "shop.orders": 1})
}

func TestCountDeltaTaintAfter(t *testing.T) {
	d := CountDelta{TaintAfter: ts(11, 1)}
	commitLate := txnEntry(1, bson.D{{Key: "applyOps", Value: applyOps(insert("shop.late", 2))}, {Key: "count", Value: int64(2)}})
	commitLate[len(commitLate)-1].Value = bson.D{{Key: "ts", Value: ts(10, 1)}, {Key: "t", Value: int64(1)}}
	addAll(t, &d,
		txnEntry(1, bson.D{{Key: "applyOps", Value: applyOps(insert("shop.late", 1))}, {Key: "partialTxn", Value: true}}), // 10
		insert("shop.orders", 1), // 11: counted
		insert("shop.users", 1),  // 12: after TaintAfter
		commitLate,               // 13: the transaction is visible after TaintAfter
	)
	wantDelta(t, &d, map[string]int64{"shop.orders": 1, "shop.users": unknownDelta, "shop.late": unknownDelta, "shop.other": 0})
}

func TestCountDeltaMalformed(t *testing.T) {
	cases := map[string]bson.Raw{
		"no ts":        mustMarshal(t, insert("shop.orders", 1)),
		"no op":        entryAt(t, ts(1, 1), bson.D{{Key: "ns", Value: "shop.orders"}}),
		"no ns":        entryAt(t, ts(1, 1), bson.D{{Key: "op", Value: "i"}}),
		"unknown op":   entryAt(t, ts(1, 1), bson.D{{Key: "op", Value: "x"}, {Key: "ns", Value: "shop.orders"}}),
		"bad rename":   entryAt(t, ts(1, 1), command("shop", bson.D{{Key: "renameCollection", Value: "shop.a"}})),
		"bad applyOps": entryAt(t, ts(1, 1), command("admin", bson.D{{Key: "applyOps", Value: "x"}})),
	}
	for name, raw := range cases {
		var d CountDelta
		if err := d.Add(raw); !errors.Is(err, ErrMalformed) && !errors.Is(err, ErrUnknownOp) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// TestCountDeltaFixtures reads the oplog of real servers: every entry is understood,
// and the transactions of the txn scenario (testdata/gen) count what they wrote.
func TestCountDeltaFixtures(t *testing.T) {
	for _, version := range fixtureVersions {
		for scenario := range fixtureScenarios {
			var d CountDelta
			for i, e := range splitEntries(t, readFixture(t, version, scenario)) {
				// The collections the scenarios create make their counts unknown; the
				// transaction scenario is counted from after its creations.
				if scenario == "txn" && commandName(e) == "create" {
					continue
				}
				if err := d.Add(e); err != nil {
					t.Fatalf("%s/%s entry %d: %v", version, scenario, i, err)
				}
			}
			if d.all != "" {
				t.Errorf("%s/%s: %s", version, scenario, d.all)
			}
			if scenario == "txn" {
				// a: seed, small, three documents and late1/late2; b: seed, deleted;
				// fx_txn2.c: seed, cross, o1 and o2. The aborted transaction counts nothing.
				wantDelta(t, &d, map[string]int64{"fx_txn.a": 7, "fx_txn.b": 0, "fx_txn2.c": 4})
			}
		}
	}
}
