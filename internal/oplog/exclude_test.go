package oplog

import (
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestFilterExcludeDropsDatabases(t *testing.T) {
	entry := func(i uint32, ns string) bson.Raw {
		return mustMarshal(t, bson.D{
			{Key: "op", Value: "i"}, {Key: "ns", Value: ns}, {Key: "o", Value: bson.D{{Key: "_id", Value: int32(i)}}},
			{Key: "ts", Value: bson.Timestamp{T: 100, I: i}}, {Key: "t", Value: int64(1)},
		})
	}
	f := &Filter{
		Rename:  func(db string) string { return db + "_c" },
		Exclude: func(db string) bool { return strings.Contains(db, "_rescue_") },
	}
	var out []string
	for i, ns := range []string{"shop.orders", "shop_rescue_x.orders", "crm.users"} {
		if _, err := f.Apply(entry(uint32(i+1), ns), func(b []byte) error {
			out = append(out, bson.Raw(b).Lookup("ns").StringValue())
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(out, ",") != "shop_c.orders,crm_c.users" {
		t.Fatalf("emitted %v", out)
	}
}
