package mongoconn

import (
	"slices"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestMissingActions(t *testing.T) {
	str := func(s string) *string { return &s }
	priv := func(db, coll *string, anyResource bool, actions ...string) privilege {
		var p privilege
		p.Resource.DB, p.Resource.Collection, p.Resource.AnyResource = db, coll, anyResource
		p.Actions = actions
		return p
	}
	all := append(slices.Clone(restoreTestActions), "dropDatabase")
	for _, tc := range []struct {
		name  string
		privs []privilege
		want  []string
	}{
		{"root", []privilege{priv(nil, nil, true, all...)}, nil},
		{"readWriteAnyDatabase", []privilege{priv(str(""), str(""), false, append(slices.Clone(restoreTestActions), "dropCollection")...)}, nil},
		{"read only", []privilege{priv(str(""), str(""), false, "find", "listCollections", "listIndexes")}, []string{"createCollection", "createIndex", "dropDatabase", "insert"}},
		{"readWrite on the source only", []privilege{priv(str("shop"), str(""), false, all...)}, all},
		{"one collection", []privilege{priv(str("tmp"), str("orders"), false, all...)}, all},
		{"none", nil, all},
	} {
		got := missingActions(tc.privs, "tmp")
		want := slices.Clone(tc.want)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s: missing = %v; want %v", tc.name, got, want)
		}
	}
}

func TestCanonicalKeys(t *testing.T) {
	keys := bson.D{{Key: "a", Value: int32(1)}, {Key: "b", Value: float64(-1)}, {Key: "c", Value: int64(1)}, {Key: "loc", Value: "2dsphere"}, {Key: "w", Value: 0.5}}
	if got := canonicalKeys(keys); got != "a:1,b:-1,c:1,loc:2dsphere,w:0.5" {
		t.Fatalf("canonicalKeys = %q", got)
	}
	if v, ok := numberOf(bson.RawValue{}); ok || v != 0 {
		t.Fatal("an absent value is not a number")
	}
}
