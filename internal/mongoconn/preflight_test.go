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

func TestPrivilegeReport(t *testing.T) {
	str := func(s string) *string { return &s }
	priv := func(db, coll *string, actions ...string) privilege {
		var p privilege
		p.Resource.DB, p.Resource.Collection = db, coll
		p.Actions = actions
		return p
	}
	write := []string{"createCollection", "createIndex", "insert"}
	builtin := []roleRef{{Role: "readWrite", DB: "shop"}}
	for _, tc := range []struct {
		name        string
		privs       []privilege
		roles       []roleRef
		collections []string
		missing     []string
		certain     bool
	}{
		{"database-wide grant", []privilege{priv(str("shop"), str(""), write...)}, builtin, nil, nil, true},
		{"collection grants cover the selection", []privilege{priv(str("shop"), str("orders"), write...), priv(str("shop"), str("users"), write...)},
			builtin, []string{"orders", "users"}, nil, true},
		{"collection grants miss a collection", []privilege{priv(str("shop"), str("orders"), write...)},
			builtin, []string{"orders", "users"}, write, false},
		{"collection grants without a known collection list", []privilege{priv(str("shop"), str("orders"), write...)},
			builtin, nil, write, false},
		{"read only with built-in roles", []privilege{priv(str("shop"), str(""), "find")}, []roleRef{{Role: "read", DB: "shop"}}, nil, write, true},
		{"a custom role", []privilege{priv(str("shop"), str(""), "find")}, []roleRef{{Role: "appRole", DB: "admin"}}, nil, write, false},
		{"an admin-only role name in another database", []privilege{priv(str("shop"), str(""), "find")}, []roleRef{{Role: "restore", DB: "shop"}}, nil, write, false},
		{"an unmodelled resource", []privilege{priv(nil, nil, "insert")}, builtin, nil, write, false},
		{"the restore role", []privilege{priv(str(""), str(""), write...)}, []roleRef{{Role: "restore", DB: "admin"}}, nil, nil, true},
	} {
		got := privilegeReport(tc.privs, tc.roles, "shop", write, tc.collections)
		if !slices.Equal(got.Missing, tc.missing) || got.Certain != tc.certain {
			t.Errorf("%s: report = %+v; want missing %v, certain %v", tc.name, got, tc.missing, tc.certain)
		}
	}
}
