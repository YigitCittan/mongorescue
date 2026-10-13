package mongoconn

import (
	"context"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/oplog"
	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// crudEntry returns an insert ("i") or delete ("d") of ns at (sec, 1).
func crudEntry(t *testing.T, sec uint32, op, ns string) bson.Raw {
	t.Helper()
	raw, err := bson.Marshal(bson.D{
		{Key: "op", Value: op}, {Key: "ns", Value: ns}, {Key: "o", Value: bson.D{{Key: "_id", Value: int64(sec)}}},
		{Key: "ts", Value: bson.Timestamp{T: sec, I: 1}}, {Key: "t", Value: int64(1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestOplogDerivedCounts reads the oplog after the consistent point as ReadOplog
// does and derives the counts there from counts read afterwards.
func TestOplogDerivedCounts(t *testing.T) {
	at := pitr.Timestamp{T: 10, I: 1}
	docs := []bson.Raw{
		entry(t, 10, 1, 1), // the consistent point itself: not after it
		crudEntry(t, 11, "i", "shop.orders"),
		crudEntry(t, 12, "i", "shop.orders"),
		crudEntry(t, 13, "d", "shop.orders"),
		crudEntry(t, 14, "d", "shop.users"),
		crudEntry(t, 16, "i", "shop.busy"), // while the counts were read
	}
	delta := &oplog.CountDelta{TaintAfter: bson.Timestamp{T: 15, I: 1}}
	if _, err := copyOplog(context.Background(), &fakeCursor{docs: docs}, pitr.OplogRange{From: at, To: pitr.Timestamp{T: 16, I: 1}},
		deltaWriter{delta}); err != nil {
		t.Fatal(err)
	}
	namespaces := []string{"shop.orders", "shop.users", "shop.busy", "shop.quiet", "shop.gone"}
	now := map[string]int64{"shop.orders": 101, "shop.users": 5, "shop.busy": 9, "shop.quiet": 3}
	got := deriveCounts(at, namespaces, now, delta)
	want := map[string]int64{"shop.orders": 100, "shop.users": 6, "shop.quiet": 3, "shop.gone": 0}
	if got.At != at || got.Method != models.CountMethodOplog || len(got.Counts) != len(want) {
		t.Fatalf("counts = %+v", got)
	}
	for ns, n := range want {
		if c, ok := got.Counts[ns]; !ok || c != n {
			t.Errorf("%s = %d (%v), want %d", ns, c, ok, n)
		}
	}
	if why := got.Unverified["shop.busy"]; !strings.Contains(why, "while the counts were read") {
		t.Errorf("shop.busy: %q", why)
	}
}

func TestOplogDerivedCountNeverNegative(t *testing.T) {
	delta := &oplog.CountDelta{}
	if err := delta.Add(crudEntry(t, 11, "i", "shop.orders")); err != nil {
		t.Fatal(err)
	}
	got := deriveCounts(pitr.Timestamp{T: 10, I: 1}, []string{"shop.orders"}, map[string]int64{}, delta)
	if _, ok := got.Counts["shop.orders"]; ok || !strings.Contains(got.Unverified["shop.orders"], "negative") {
		t.Fatalf("counts = %+v", got)
	}
}
