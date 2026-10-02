package mongoconn

import (
	"slices"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestMissingFrom(t *testing.T) {
	str := func(s string) *string { return &s }
	priv := func(db, coll *string, actions ...string) privilege {
		var p privilege
		p.Resource.DB, p.Resource.Collection = db, coll
		p.Actions = actions
		return p
	}
	actions := []string{"insert", "createCollection", "createIndex", "insert"}
	for _, tc := range []struct {
		name  string
		privs []privilege
		want  []string
	}{
		{"readWrite on the target", []privilege{priv(str("shop_rescue"), str(""), "insert", "createCollection", "createIndex")}, nil},
		{"any database", []privilege{priv(str(""), str(""), "insert", "createCollection", "createIndex")}, nil},
		{"another database", []privilege{priv(str("shop"), str(""), "insert", "createCollection", "createIndex")}, []string{"createCollection", "createIndex", "insert"}},
		{"read only", []privilege{priv(str(""), str(""), "find")}, []string{"createCollection", "createIndex", "insert"}},
	} {
		if got := missingFrom(tc.privs, "shop_rescue", actions); !slices.Equal(got, tc.want) {
			t.Errorf("%s: missing = %v; want %v", tc.name, got, tc.want)
		}
	}
}

func TestFsFree(t *testing.T) {
	num := func(v any) bson.RawValue {
		typ, data, err := bson.MarshalValue(v)
		if err != nil {
			t.Fatal(err)
		}
		return bson.RawValue{Type: typ, Value: data}
	}
	if free, ok := fsFree(num(float64(300)), num(int64(1000))); !ok || free != 700 {
		t.Fatalf("free = %d, %v; want 700", free, ok)
	}
	if _, ok := fsFree(num(int32(0)), num(int32(0))); ok {
		t.Fatal("a zero total (a database without files) must be unknown")
	}
	if _, ok := fsFree(bson.RawValue{}, num(int64(10))); ok {
		t.Fatal("a missing field must be unknown")
	}
}

func TestLoopbackURI(t *testing.T) {
	for uri, want := range map[string]bool{
		"mongodb://localhost:27017":                         true,
		"mongodb://u:p@127.0.0.1:27017,localhost:27018/db":  true,
		"mongodb://[::1]:27017":                             true,
		"mongodb://%2Ftmp%2Fmongodb-27017.sock":             true,
		"mongodb://db.internal:27017":                       false,
		"mongodb://localhost:27017,db.internal:27017":       false,
		"mongodb+srv://cluster0.example.mongodb.net/?w=maj": false,
		"not a uri": false,
	} {
		if got := loopbackURI(uri); got != want {
			t.Errorf("loopbackURI(%q) = %v; want %v", uri, got, want)
		}
	}
}
