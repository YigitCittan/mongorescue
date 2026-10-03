package oplog

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// rescue renames db to db_rescue.
func rescue(db string) string { return db + "_rescue" }

// shopFilter keeps database shop and renames it to shop_rescue.
func shopFilter() *Filter {
	return &Filter{Select: map[string]bool{"shop": true}, Rename: rescue}
}

// uuid is a collection UUID value.
var uuid = bson.Binary{Subtype: bson.TypeBinaryUUID, Data: bytes.Repeat([]byte{7}, 16)}

// lsid is a logical session ID.
var lsid = bson.D{{Key: "id", Value: bson.Binary{Subtype: bson.TypeBinaryUUID, Data: bytes.Repeat([]byte{1}, 16)}}}

func insert(ns string, id any) bson.D {
	return bson.D{{Key: "op", Value: "i"}, {Key: "ns", Value: ns}, {Key: "ui", Value: uuid}, {Key: "o", Value: bson.D{{Key: "_id", Value: id}}}}
}

func command(db string, o bson.D) bson.D {
	return bson.D{{Key: "op", Value: "c"}, {Key: "ns", Value: db + ".$cmd"}, {Key: "ui", Value: uuid}, {Key: "o", Value: o}}
}

func applyOps(ops ...bson.D) bson.A {
	a := bson.A{}
	for _, op := range ops {
		a = append(a, op)
	}
	return a
}

// txnEntry builds a transaction entry on admin.$cmd with txnNumber n.
func txnEntry(n int64, o bson.D) bson.D {
	return bson.D{
		{Key: "lsid", Value: lsid}, {Key: "txnNumber", Value: n},
		{Key: "op", Value: "c"}, {Key: "ns", Value: "admin.$cmd"}, {Key: "o", Value: o},
		{Key: "prevOpTime", Value: bson.D{{Key: "ts", Value: ts(0, 0)}, {Key: "t", Value: int64(-1)}}},
	}
}

func keys(t *testing.T, doc bson.Raw) []string {
	t.Helper()
	elems, err := doc.Elements()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range elems {
		out = append(out, e.Key())
	}
	return out
}

func TestFilterCRUD(t *testing.T) {
	in := entryAt(t, ts(10, 1), append(bson.D{
		{Key: "lsid", Value: lsid}, {Key: "txnNumber", Value: int64(4)},
	}, append(insert("shop.orders", 1),
		bson.E{Key: "o2", Value: bson.D{{Key: "_id", Value: 1}}},
		bson.E{Key: "stmtId", Value: int32(0)},
		bson.E{Key: "prevOpTime", Value: bson.D{{Key: "ts", Value: ts(0, 0)}}},
		bson.E{Key: "needsRetryImage", Value: "preImage"},
	)...))
	f := shopFilter()
	out := mustRun(t, f, in)
	if len(out) != 1 {
		t.Fatalf("%d entries, want 1", len(out))
	}
	if got := str(out[0], "ns"); got != "shop_rescue.orders" {
		t.Errorf("ns = %q", got)
	}
	if got, want := strings.Join(keys(t, out[0]), ","), "op,ns,o,o2,ts,t,v,wall"; got != want {
		t.Errorf("fields = %s, want %s", got, want)
	}
	if !bytes.Equal(out[0].Lookup("o").Value, in.Lookup("o").Value) || !bytes.Equal(out[0].Lookup("wall").Value, in.Lookup("wall").Value) {
		t.Error("o or wall changed")
	}
	if f.Ops() != 1 || f.Entries() != 1 {
		t.Errorf("ops %d entries %d, want 1 1", f.Ops(), f.Entries())
	}
}

func TestFilterDrops(t *testing.T) {
	cases := map[string]bson.D{
		"other database":     insert("crm.users", 1),
		"admin":              insert("admin.system.users", 1),
		"config index build": insert("config.system.indexBuilds", 1),
		"local":              insert("local.startup_log", 1),
		"no-op":              {{Key: "op", Value: "n"}, {Key: "ns", Value: ""}, {Key: "o", Value: bson.D{{Key: "msg", Value: "periodic noop"}}}},
		"pre-image no-op":    {{Key: "op", Value: "n"}, {Key: "ns", Value: "shop.orders"}, {Key: "o", Value: bson.D{{Key: "_id", Value: 1}}}},
		"record id":          {{Key: "op", Value: "km"}, {Key: "ns", Value: "shop.orders"}},
		"fromMigrate":        append(insert("shop.orders", 1), bson.E{Key: "fromMigrate", Value: true}),
		"fromMigrate drop":   append(command("shop", bson.D{{Key: "drop", Value: "orders"}}), bson.E{Key: "fromMigrate", Value: true}),
		"dbCheck":            command("shop", bson.D{{Key: "dbCheck", Value: "orders"}}),
		"startIndexBuild":    command("shop", bson.D{{Key: "startIndexBuild", Value: "orders"}}),
		"abortIndexBuild":    command("shop", bson.D{{Key: "abortIndexBuild", Value: "orders"}}),
		"admin command":      command("admin", bson.D{{Key: "create", Value: "x"}}),
		"unselected drop":    command("crm", bson.D{{Key: "dropDatabase", Value: 1}}),
		"unselected commit":  command("crm", bson.D{{Key: "commitIndexBuild", Value: "u"}, {Key: "indexes", Value: bson.A{}}}),
		"empty applyOps":     command("admin", bson.D{{Key: "applyOps", Value: applyOps(insert("crm.users", 1))}}),
		"commit without txn": txnEntry(9, bson.D{{Key: "commitTransaction", Value: 1}}),
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			f := shopFilter()
			if out := mustRun(t, f, entryAt(t, ts(1, 1), d)); len(out) != 0 {
				t.Fatalf("written: %v", out)
			}
			if f.Ops() != 0 {
				t.Errorf("ops = %d", f.Ops())
			}
		})
	}
}

func TestFilterKeepsRenameCopy(t *testing.T) {
	// MongoDB 5.0 copies a collection renamed across databases with fromMigrate
	// inserts into a temporary collection of the target.
	in := append(insert("shop.tmpAb3Xy.renameCollection", 1), bson.E{Key: "fromMigrate", Value: true})
	out := mustRun(t, shopFilter(), entryAt(t, ts(1, 1), in))
	if len(out) != 1 || str(out[0], "ns") != "shop_rescue.tmpAb3Xy.renameCollection" {
		t.Fatalf("out = %v", out)
	}
	if _, err := out[0].LookupErr("fromMigrate"); err == nil {
		t.Error("fromMigrate kept")
	}
}

func TestFilterAllDatabases(t *testing.T) {
	f := &Filter{}
	out := mustRun(t, f,
		entryAt(t, ts(1, 1), insert("shop.orders", 1)),
		entryAt(t, ts(1, 2), insert("crm.users", 1)),
		entryAt(t, ts(1, 3), insert("admin.system.users", 1)),
		entryAt(t, ts(1, 4), insert("config.system.sessions", 1)),
	)
	if len(out) != 2 || str(out[0], "ns") != "shop.orders" || str(out[1], "ns") != "crm.users" {
		t.Fatalf("out = %v", out)
	}
}

func TestFilterDDL(t *testing.T) {
	cases := []struct {
		name  string
		o     bson.D
		o2    bson.D
		check func(t *testing.T, e bson.Raw)
	}{
		{name: "create", o: bson.D{{Key: "create", Value: "orders"}, {Key: "idIndex", Value: bson.D{{Key: "v", Value: 2}, {Key: "key", Value: bson.D{{Key: "_id", Value: 1}}}, {Key: "name", Value: "_id_"}, {Key: "ns", Value: "shop.orders"}}}},
			check: func(t *testing.T, e bson.Raw) {
				if got := str(e, "o", "idIndex", "ns"); got != "shop_rescue.orders" {
					t.Errorf("idIndex.ns = %q", got)
				}
			}},
		{name: "createIndexes", o: bson.D{{Key: "createIndexes", Value: "orders"}, {Key: "v", Value: 2}, {Key: "key", Value: bson.D{{Key: "x", Value: 1}}}, {Key: "name", Value: "x_1"}, {Key: "ns", Value: "shop.orders"}},
			check: func(t *testing.T, e bson.Raw) {
				if got := str(e, "o", "ns"); got != "shop_rescue.orders" {
					t.Errorf("o.ns = %q", got)
				}
			}},
		{name: "dropIndexes", o: bson.D{{Key: "dropIndexes", Value: "orders"}, {Key: "index", Value: "x_1"}}, o2: bson.D{{Key: "v", Value: 2}, {Key: "key", Value: bson.D{{Key: "x", Value: 1}}}, {Key: "name", Value: "x_1"}, {Key: "ns", Value: "shop.orders"}},
			check: func(t *testing.T, e bson.Raw) {
				if got := str(e, "o2", "ns"); got != "shop_rescue.orders" {
					t.Errorf("o2.ns = %q", got)
				}
			}},
		{name: "collMod", o: bson.D{{Key: "collMod", Value: "orders"}, {Key: "index", Value: bson.D{{Key: "name", Value: "x_1"}, {Key: "hidden", Value: true}}}}},
		{name: "drop", o: bson.D{{Key: "drop", Value: "orders"}}},
		{name: "dropDatabase", o: bson.D{{Key: "dropDatabase", Value: 1}}},
		{name: "convertToCapped", o: bson.D{{Key: "convertToCapped", Value: "orders"}, {Key: "size", Value: 4096}}},
		{name: "emptycapped", o: bson.D{{Key: "emptycapped", Value: "orders"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := command("shop", c.o)
			if c.o2 != nil {
				d = append(d, bson.E{Key: "o2", Value: c.o2})
			}
			in := entryAt(t, ts(1, 1), d)
			out := mustRun(t, shopFilter(), in)
			if len(out) != 1 {
				t.Fatalf("%d entries", len(out))
			}
			if got := str(out[0], "ns"); got != "shop_rescue.$cmd" {
				t.Errorf("ns = %q", got)
			}
			if _, err := out[0].LookupErr("ui"); err == nil {
				t.Error("ui kept")
			}
			if first, _ := out[0].Lookup("o").Document().IndexErr(0); first.Key() != c.o[0].Key || !first.Value().Equal(in.Lookup("o").Document().Index(0).Value()) {
				t.Errorf("command changed: %v", first)
			}
			if c.check != nil {
				c.check(t, out[0])
			}
		})
	}
}

func TestFilterViews(t *testing.T) {
	pipeline := bson.A{bson.D{{Key: "$match", Value: bson.D{{Key: "paid", Value: true}}}}}
	viewWrite := func(op string, o bson.D) bson.Raw {
		return entryAt(t, ts(1, 1), bson.D{{Key: "op", Value: op}, {Key: "ns", Value: "shop.system.views"}, {Key: "ui", Value: uuid}, {Key: "o", Value: o}, {Key: "o2", Value: bson.D{{Key: "_id", Value: "shop.v"}}}})
	}
	def := bson.D{{Key: "_id", Value: "shop.v"}, {Key: "viewOn", Value: "orders"}, {Key: "pipeline", Value: pipeline}}
	cases := []struct {
		name string
		in   bson.Raw
		want bson.D
	}{
		{"insert", viewWrite("i", append(def, bson.E{Key: "collation", Value: bson.D{{Key: "locale", Value: "fr"}}})),
			bson.D{{Key: "create", Value: "v"}, {Key: "viewOn", Value: "orders"}, {Key: "pipeline", Value: pipeline}, {Key: "collation", Value: bson.D{{Key: "locale", Value: "fr"}}}}},
		{"update", viewWrite("u", def), bson.D{{Key: "collMod", Value: "v"}, {Key: "viewOn", Value: "orders"}, {Key: "pipeline", Value: pipeline}}},
		{"delete", viewWrite("d", bson.D{{Key: "_id", Value: "shop.v"}}), bson.D{{Key: "drop", Value: "v"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := mustRun(t, shopFilter(), c.in)
			if len(out) != 1 {
				t.Fatalf("%d entries", len(out))
			}
			if got := strings.Join(keys(t, out[0]), ","); got != "op,ns,o,ts,t,v,wall" {
				t.Errorf("fields = %s", got)
			}
			if str(out[0], "op") != "c" || str(out[0], "ns") != "shop_rescue.$cmd" {
				t.Errorf("entry = %s", out[0])
			}
			if got, want := bson.Raw(out[0].Lookup("o").Value), mustMarshal(t, c.want); !bytes.Equal(got, want) {
				t.Errorf("o = %s, want %s", got, want)
			}
		})
	}

	dropped := map[string]bson.Raw{
		"create system.views": entryAt(t, ts(1, 1), command("shop", bson.D{{Key: "create", Value: "system.views"}})),
		"time-series view": viewWrite("i", bson.D{{Key: "_id", Value: "shop.w"}, {Key: "viewOn", Value: "system.buckets.w"}, {Key: "pipeline", Value: bson.A{
			bson.D{{Key: "$_internalUnpackBucket", Value: bson.D{{Key: "timeField", Value: "at"}}}},
		}}}),
		"unselected view": entryAt(t, ts(1, 1), bson.D{{Key: "op", Value: "i"}, {Key: "ns", Value: "crm.system.views"}, {Key: "o", Value: def}}),
	}
	for name, in := range dropped {
		if out := mustRun(t, shopFilter(), in); len(out) != 0 {
			t.Errorf("%s written: %v", name, out)
		}
	}

	refused := map[string]struct {
		in   bson.Raw
		want error
	}{
		"view of another database": {viewWrite("i", bson.D{{Key: "_id", Value: "crm.v"}, {Key: "viewOn", Value: "x"}, {Key: "pipeline", Value: bson.A{}}}), ErrMalformed},
		"no _id":                   {entryAt(t, ts(1, 1), bson.D{{Key: "op", Value: "d"}, {Key: "ns", Value: "shop.system.views"}, {Key: "o", Value: bson.D{}}}), ErrMalformed},
		"no o":                     {entryAt(t, ts(1, 1), bson.D{{Key: "op", Value: "d"}, {Key: "ns", Value: "shop.system.views"}}), ErrMalformed},
		"diff update":              {viewWrite("u", bson.D{{Key: "$v", Value: 2}, {Key: "diff", Value: bson.D{}}}), ErrUnknownCommand},
	}
	for name, c := range refused {
		if _, err := run(t, shopFilter(), c.in); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
}

func TestFilterTimeseries(t *testing.T) {
	create := func(tsOpts bson.D) bson.Raw {
		return entryAt(t, ts(1, 1), command("shop", bson.D{
			{Key: "create", Value: "system.buckets.w"},
			{Key: "validator", Value: bson.D{{Key: "$jsonSchema", Value: bson.D{}}}},
			{Key: "clusteredIndex", Value: true},
			{Key: "timeseries", Value: tsOpts},
			{Key: "expireAfterSeconds", Value: int64(3600)},
		}))
	}
	cases := []struct {
		name string
		in   bson.Raw
		want bson.D
	}{
		{"create with granularity", create(bson.D{{Key: "timeField", Value: "at"}, {Key: "metaField", Value: "m"}, {Key: "granularity", Value: "hours"}, {Key: "bucketMaxSpanSeconds", Value: 2592000}}),
			bson.D{{Key: "create", Value: "w"}, {Key: "timeseries", Value: bson.D{{Key: "timeField", Value: "at"}, {Key: "metaField", Value: "m"}, {Key: "granularity", Value: "hours"}}}, {Key: "expireAfterSeconds", Value: int64(3600)}}},
		{"create with a custom span", create(bson.D{{Key: "timeField", Value: "at"}, {Key: "bucketMaxSpanSeconds", Value: 600}, {Key: "bucketRoundingSeconds", Value: 600}}),
			bson.D{{Key: "create", Value: "w"}, {Key: "timeseries", Value: bson.D{{Key: "timeField", Value: "at"}, {Key: "bucketMaxSpanSeconds", Value: 600}, {Key: "bucketRoundingSeconds", Value: 600}}}, {Key: "expireAfterSeconds", Value: int64(3600)}}},
		{"collMod of buckets", entryAt(t, ts(1, 1), command("shop", bson.D{{Key: "collMod", Value: "system.buckets.w"}, {Key: "expireAfterSeconds", Value: int64(60)}})),
			bson.D{{Key: "collMod", Value: "system.buckets.w"}, {Key: "expireAfterSeconds", Value: int64(60)}}},
		{"plain create", entryAt(t, ts(1, 1), command("shop", bson.D{{Key: "create", Value: "system.buckets.x"}})),
			bson.D{{Key: "create", Value: "system.buckets.x"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := mustRun(t, shopFilter(), c.in)
			if len(out) != 1 || str(out[0], "ns") != "shop_rescue.$cmd" {
				t.Fatalf("out = %v", out)
			}
			if got, want := bson.Raw(out[0].Lookup("o").Value), mustMarshal(t, c.want); !bytes.Equal(got, want) {
				t.Errorf("o = %s, want %s", got, want)
			}
		})
	}
	// Writes to the buckets collection are replayed as they are.
	out := mustRun(t, shopFilter(), entryAt(t, ts(1, 1), insert("shop.system.buckets.w", 1)))
	if len(out) != 1 || str(out[0], "ns") != "shop_rescue.system.buckets.w" {
		t.Fatalf("bucket insert: %v", out)
	}
}

func TestFilterRenameCollection(t *testing.T) {
	rename := func(from, to string) bson.Raw {
		db, _, _ := strings.Cut(from, ".")
		return entryAt(t, ts(1, 1), command(db, bson.D{{Key: "renameCollection", Value: from}, {Key: "to", Value: to}, {Key: "stayTemp", Value: false}}))
	}
	f := &Filter{Select: map[string]bool{"shop": true, "shop2": true}, Rename: rescue}
	out := mustRun(t, f, rename("shop.a", "shop2.b"))
	if len(out) != 1 || str(out[0], "o", "renameCollection") != "shop_rescue.a" || str(out[0], "o", "to") != "shop2_rescue.b" || str(out[0], "ns") != "shop_rescue.$cmd" {
		t.Fatalf("out = %v", out)
	}
	if out := mustRun(t, shopFilter(), rename("crm.a", "crm.b")); len(out) != 0 {
		t.Fatalf("unselected rename written: %v", out)
	}
	for _, c := range [][2]string{{"shop.a", "crm.b"}, {"crm.a", "shop.b"}, {"admin.a", "shop.b"}, {"shop.a", "local.b"}} {
		if _, err := run(t, shopFilter(), rename(c[0], c[1])); !errors.Is(err, ErrCrossSelectionRename) {
			t.Errorf("%s to %s: err = %v, want ErrCrossSelectionRename", c[0], c[1], err)
		}
	}
	if _, err := run(t, shopFilter(), entryAt(t, ts(1, 1), command("shop", bson.D{{Key: "renameCollection", Value: "shop.a"}}))); !errors.Is(err, ErrMalformed) {
		t.Errorf("rename without to: err = %v", err)
	}
}

func TestFilterRefuses(t *testing.T) {
	cases := map[string]struct {
		d    bson.D
		want error
	}{
		"unknown command":              {command("shop", bson.D{{Key: "importCollection", Value: "orders"}}), ErrUnknownCommand},
		"unknown command, unselected":  {command("crm", bson.D{{Key: "futureCommand", Value: 1}}), ErrUnknownCommand},
		"unknown command in applyOps":  {command("admin", bson.D{{Key: "applyOps", Value: applyOps(command("shop", bson.D{{Key: "mystery", Value: 1}}))}}), ErrUnknownCommand},
		"unknown op":                   {bson.D{{Key: "op", Value: "xi"}, {Key: "ns", Value: "shop.orders"}}, ErrUnknownOp},
		"no op":                        {bson.D{{Key: "ns", Value: "shop.orders"}}, ErrMalformed},
		"no ns":                        {bson.D{{Key: "op", Value: "i"}}, ErrMalformed},
		"ns without database":          {bson.D{{Key: "op", Value: "i"}, {Key: "ns", Value: "orders"}}, ErrMalformed},
		"command without o":            {bson.D{{Key: "op", Value: "c"}, {Key: "ns", Value: "shop.$cmd"}}, ErrMalformed},
		"empty command":                {command("shop", bson.D{}), ErrMalformed},
		"applyOps not an array":        {command("admin", bson.D{{Key: "applyOps", Value: 1}}), ErrMalformed},
		"applyOps element not a doc":   {command("admin", bson.D{{Key: "applyOps", Value: bson.A{1}}}), ErrMalformed},
		"commitIndexBuild no indexes":  {command("shop", bson.D{{Key: "commitIndexBuild", Value: "orders"}}), ErrMalformed},
		"commitIndexBuild bad spec":    {command("shop", bson.D{{Key: "commitIndexBuild", Value: "orders"}, {Key: "indexes", Value: bson.A{"x"}}}), ErrMalformed},
		"commitIndexBuild no coll":     {command("shop", bson.D{{Key: "commitIndexBuild", Value: 1}, {Key: "indexes", Value: bson.A{}}}), ErrMalformed},
		"transaction not an array":     {txnEntry(1, bson.D{{Key: "applyOps", Value: "x"}}), ErrMalformed},
		"transaction op without ns":    {txnEntry(1, bson.D{{Key: "applyOps", Value: bson.A{bson.D{{Key: "op", Value: "i"}}}}}), ErrMalformed},
		"transaction with unknown cmd": {txnEntry(1, bson.D{{Key: "applyOps", Value: applyOps(command("shop", bson.D{{Key: "mystery", Value: 1}}))}}), ErrUnknownCommand},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := run(t, shopFilter(), entryAt(t, ts(1, 1), c.d)); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
	t.Run("no ts", func(t *testing.T) {
		if _, err := run(t, shopFilter(), mustMarshal(t, insert("shop.orders", 1))); !errors.Is(err, ErrMalformed) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("invalid BSON", func(t *testing.T) {
		e := entryAt(t, ts(1, 1), insert("shop.orders", 1))
		e[4] = 0x7e
		if _, err := run(t, shopFilter(), e); !errors.Is(err, ErrMalformed) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("nested too deep", func(t *testing.T) {
		d := insert("shop.orders", 1)
		for i := 0; i < maxApplyOpsDepth+1; i++ {
			d = command("admin", bson.D{{Key: "applyOps", Value: applyOps(d)}})
		}
		if _, err := run(t, shopFilter(), entryAt(t, ts(1, 1), d)); !errors.Is(err, ErrMalformed) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestFilterBadRename(t *testing.T) {
	for _, to := range []string{"", "a.b", "admin", "has space", strings.Repeat("x", 64), "a$b"} {
		f := &Filter{Rename: func(string) string { return to }}
		for _, d := range []bson.D{
			insert("shop.orders", 1),
			command("shop", bson.D{{Key: "drop", Value: "orders"}}),
			command("shop", bson.D{{Key: "renameCollection", Value: "shop.a"}, {Key: "to", Value: "shop.b"}}),
			command("shop", bson.D{{Key: "commitIndexBuild", Value: "orders"}, {Key: "indexes", Value: bson.A{}}}),
			command("shop", bson.D{{Key: "applyOps", Value: applyOps(insert("crm.x", 1))}}),
		} {
			if _, err := run(t, f, entryAt(t, ts(1, 1), d)); !errors.Is(err, ErrBadRename) {
				t.Errorf("rename to %q, %v: err = %v, want ErrBadRename", to, d[3].Value, err)
			}
		}
	}
}

func TestFilterIndexBuild(t *testing.T) {
	specs := bson.A{
		bson.D{{Key: "v", Value: 2}, {Key: "key", Value: bson.D{{Key: "k", Value: 1}}}, {Key: "name", Value: "k_1"}},
		bson.D{{Key: "v", Value: 2}, {Key: "key", Value: bson.D{{Key: "v", Value: -1}}}, {Key: "name", Value: "v_-1"}, {Key: "sparse", Value: true}},
	}
	build := bson.Binary{Subtype: bson.TypeBinaryUUID, Data: bytes.Repeat([]byte{9}, 16)}
	f := shopFilter()
	out := mustRun(t, f,
		entryAt(t, ts(5, 1), command("shop", bson.D{{Key: "startIndexBuild", Value: "people"}, {Key: "indexBuildUUID", Value: build}, {Key: "indexes", Value: specs}})),
		entryAt(t, ts(5, 2), insert("config.system.indexBuilds", build)),
		entryAt(t, ts(5, 3), command("shop", bson.D{{Key: "commitIndexBuild", Value: "people"}, {Key: "indexBuildUUID", Value: build}, {Key: "indexes", Value: specs}})),
		entryAt(t, ts(5, 4), command("shop", bson.D{{Key: "abortIndexBuild", Value: "people"}, {Key: "indexBuildUUID", Value: build}, {Key: "indexes", Value: specs}})),
	)
	if len(out) != 2 {
		t.Fatalf("%d entries, want one createIndexes per index", len(out))
	}
	for i, e := range out {
		o := e.Lookup("o").Document()
		if got := strings.Join(keys(t, o), ","); !strings.HasPrefix(got, "createIndexes,v,key,name") {
			t.Errorf("entry %d: o fields %s", i, got)
		}
		if str(o, "createIndexes") != "people" || str(e, "ns") != "shop_rescue.$cmd" {
			t.Errorf("entry %d: %s", i, e)
		}
		if secs, inc, _ := e.Lookup("ts").TimestampOK(); secs != 5 || inc != 3 {
			t.Errorf("entry %d not at the commit's position: %v", i, e.Lookup("ts"))
		}
	}
	if str(out[1], "o", "name") != "v_-1" || !out[1].Lookup("o", "sparse").Boolean() {
		t.Errorf("second index: %s", out[1])
	}
	if f.Ops() != 2 {
		t.Errorf("ops = %d, want 2", f.Ops())
	}
}

func TestFilterIndexMoves(t *testing.T) {
	spec := func(name string, key bson.D) bson.D {
		return bson.D{{Key: "v", Value: 2}, {Key: "key", Value: key}, {Key: "name", Value: name}}
	}
	createIndex := func(coll string, s bson.D) bson.Raw {
		return entryAt(t, ts(1, 1), command("shop", append(bson.D{{Key: "createIndexes", Value: coll}}, s...)))
	}
	commit := func(coll string, specs ...bson.D) bson.Raw {
		a := bson.A{}
		for _, s := range specs {
			a = append(a, s)
		}
		return entryAt(t, ts(1, 2), command("shop", bson.D{{Key: "commitIndexBuild", Value: coll}, {Key: "indexes", Value: a}}))
	}
	rename := func(from, to string) bson.Raw {
		return entryAt(t, ts(2, 1), command("shop", bson.D{{Key: "renameCollection", Value: "shop." + from}, {Key: "to", Value: "shop." + to}, {Key: "dropTarget", Value: true}}))
	}
	// summary lists each entry as "<command> <collection> [<index>]".
	summary := func(out []bson.Raw) string {
		var s []string
		for _, e := range out {
			o := e.Lookup("o").Document()
			first := o.Index(0)
			line := first.Key() + " " + first.Value().StringValue()
			if first.Key() == "createIndexes" {
				line += " " + str(o, "name")
			}
			if first.Key() == "dropIndexes" {
				line += " " + str(o, "index")
			}
			s = append(s, line)
		}
		return strings.Join(s, "; ")
	}
	skuIdx, stIdx := spec("sku_1", bson.D{{Key: "sku", Value: 1}}), spec("st_1", bson.D{{Key: "st", Value: 1}})

	f := shopFilter()
	out := mustRun(t, f, createIndex("a", skuIdx), commit("a", stIdx), createIndex("b", skuIdx), rename("a", "b"))
	want := "createIndexes a sku_1; createIndexes a st_1; createIndexes b sku_1; " +
		"renameCollection shop_rescue.a; dropIndexes b *; createIndexes b sku_1; createIndexes b st_1; dropIndexes a *"
	if got := summary(out); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	for _, e := range out[3:] {
		if str(e, "ns") != "shop_rescue.$cmd" || e.Lookup("ts").String() != out[3].Lookup("ts").String() {
			t.Errorf("moved entry %s not at the rename", e)
		}
	}
	if f.Ops() != int64(len(out)) {
		t.Errorf("ops = %d, want %d", f.Ops(), len(out))
	}
	// The indexes now belong to b: renaming b moves them again.
	if got := summary(mustRun(t, f, rename("b", "c"))); got != "renameCollection shop_rescue.b; createIndexes c sku_1; createIndexes c st_1; dropIndexes b *" {
		t.Errorf("second rename: %s", got)
	}

	// Indexes that were dropped, or whose collection or database was, do not move.
	cases := map[string]bson.Raw{
		"drop":                 entryAt(t, ts(1, 3), command("shop", bson.D{{Key: "drop", Value: "a"}})),
		"dropDatabase":         entryAt(t, ts(1, 3), command("shop", bson.D{{Key: "dropDatabase", Value: 1}})),
		"dropIndexes *":        entryAt(t, ts(1, 3), command("shop", bson.D{{Key: "dropIndexes", Value: "a"}, {Key: "index", Value: "*"}})),
		"dropIndexes by name":  entryAt(t, ts(1, 3), command("shop", bson.D{{Key: "dropIndexes", Value: "a"}, {Key: "index", Value: "sku_1"}})),
		"deleteIndexes by key": entryAt(t, ts(1, 3), command("shop", bson.D{{Key: "deleteIndexes", Value: "a"}, {Key: "index", Value: bson.D{{Key: "sku", Value: 1}}}})),
	}
	for name, drop := range cases {
		got := mustRun(t, shopFilter(), createIndex("a", skuIdx), drop, rename("a", "b"))
		if last := summary(got[len(got)-1:]); last != "renameCollection shop_rescue.a" {
			t.Errorf("%s: rename followed by %s", name, summary(got))
		}
	}
	// Another collection's indexes stay.
	out = mustRun(t, shopFilter(), createIndex("x", skuIdx), entryAt(t, ts(1, 3), command("shop", bson.D{{Key: "dropIndexes", Value: "x"}, {Key: "index", Value: "other"}})), rename("x", "y"))
	if got := summary(out[2:]); got != "renameCollection shop_rescue.x; createIndexes y sku_1; dropIndexes x *" {
		t.Errorf("unrelated dropIndexes: %s", got)
	}
}

func TestFilterApplyOps(t *testing.T) {
	f := shopFilter()
	in := entryAt(t, ts(1, 1), command("admin", bson.D{{Key: "applyOps", Value: applyOps(
		insert("shop.a", 1),
		insert("crm.b", 2),
		append(insert("shop.a", 3), bson.E{Key: "stmtId", Value: int32(1)}),
		command("admin", bson.D{{Key: "applyOps", Value: applyOps(insert("shop.c", 4))}}),
	)}}))
	out := mustRun(t, f, in)
	if len(out) != 1 || str(out[0], "ns") != "admin.$cmd" {
		t.Fatalf("out = %v", out)
	}
	ops := nestedOps(t, out[0])
	if len(ops) != 3 || str(ops[0], "ns") != "shop_rescue.a" || str(ops[1], "ns") != "shop_rescue.a" {
		t.Fatalf("nested = %v", ops)
	}
	if _, err := ops[1].LookupErr("stmtId"); err == nil {
		t.Error("stmtId kept in a nested operation")
	}
	if inner := nestedOps(t, ops[2]); len(inner) != 1 || str(inner[0], "ns") != "shop_rescue.c" {
		t.Fatalf("doubly nested = %v", inner)
	}
	// mongorestore counts each applyOps and each operation in it: 1 + (1 + 1 + (1 + 1)).
	if f.Ops() != 5 {
		t.Errorf("ops = %d, want 5", f.Ops())
	}

	// An applyOps on a selected database is renamed with it.
	out = mustRun(t, shopFilter(), entryAt(t, ts(1, 2), command("shop", bson.D{{Key: "applyOps", Value: applyOps(insert("shop.a", 1))}})))
	if len(out) != 1 || str(out[0], "ns") != "shop_rescue.$cmd" {
		t.Fatalf("out = %v", out)
	}
	// A retryable batched insert (multiOpType 1) is no transaction: its session
	// fields go.
	vectored := append(txnEntry(3, bson.D{{Key: "applyOps", Value: applyOps(insert("shop.a", 1), insert("shop.a", 2))}}), bson.E{Key: "multiOpType", Value: int32(1)})
	f = shopFilter()
	out = mustRun(t, f, entryAt(t, ts(1, 3), vectored))
	if len(out) != 1 {
		t.Fatalf("out = %v", out)
	}
	if got := strings.Join(keys(t, out[0]), ","); got != "op,ns,o,ts,t,v,wall" {
		t.Errorf("fields = %s", got)
	}
	if f.Ops() != 3 {
		t.Errorf("ops = %d, want 3", f.Ops())
	}
}

func TestFilterTransactions(t *testing.T) {
	partial := func(n int64, ops ...bson.D) bson.D {
		return txnEntry(n, bson.D{{Key: "applyOps", Value: applyOps(ops...)}, {Key: "partialTxn", Value: true}})
	}
	final := func(n int64, count int64, ops ...bson.D) bson.D {
		return txnEntry(n, bson.D{{Key: "applyOps", Value: applyOps(ops...)}, {Key: "count", Value: count}})
	}

	t.Run("partial chain", func(t *testing.T) {
		f := shopFilter()
		out := mustRun(t, f,
			entryAt(t, ts(1, 1), partial(1, insert("crm.a", 1), insert("crm.a", 2))),
			entryAt(t, ts(1, 2), partial(1, insert("crm.a", 3), insert("shop.b", 1))),
			entryAt(t, ts(1, 3), partial(1, insert("crm.a", 4))),
			entryAt(t, ts(1, 4), final(1, 6, insert("crm.a", 5))),
		)
		if len(out) != 2 {
			t.Fatalf("%d entries, want the partial entry with shop and the commit", len(out))
		}
		if ops := nestedOps(t, out[0]); len(ops) != 1 || str(ops[0], "ns") != "shop_rescue.b" {
			t.Errorf("first entry ops = %v", ops)
		}
		if !out[0].Lookup("o", "partialTxn").Boolean() {
			t.Error("partialTxn lost")
		}
		if got := strings.Join(keys(t, out[0]), ","); got != "lsid,txnNumber,op,ns,o,prevOpTime,ts,t,v,wall" {
			t.Errorf("transaction fields = %s", got)
		}
		if ops := nestedOps(t, out[1]); len(ops) != 0 {
			t.Errorf("commit keeps %d operations, want 0", len(ops))
		}
		if got := out[1].Lookup("o", "count").Int64(); got != 1 {
			t.Errorf("count = %d, want 1", got)
		}
		if f.Ops() != 1 {
			t.Errorf("ops = %d, want 1", f.Ops())
		}
	})

	t.Run("nothing selected", func(t *testing.T) {
		f := shopFilter()
		out := mustRun(t, f,
			entryAt(t, ts(1, 1), partial(2, insert("crm.a", 1))),
			entryAt(t, ts(1, 2), final(2, 2, insert("crm.a", 2))),
		)
		if len(out) != 0 || f.Ops() != 0 || len(f.txns) != 0 {
			t.Fatalf("out %v ops %d open %d", out, f.Ops(), len(f.txns))
		}
	})

	t.Run("int32 count", func(t *testing.T) {
		out := mustRun(t, shopFilter(), entryAt(t, ts(1, 1), txnEntry(3, bson.D{{Key: "applyOps", Value: applyOps(insert("shop.a", 1), insert("crm.a", 1))}, {Key: "count", Value: int32(2)}})))
		if v := out[0].Lookup("o", "count"); v.Type != bson.TypeInt32 || v.Int32() != 1 {
			t.Errorf("count = %v", v)
		}
	})

	t.Run("unfinished", func(t *testing.T) {
		f := shopFilter()
		out := mustRun(t, f, entryAt(t, ts(1, 1), partial(4, insert("shop.a", 1))))
		if len(out) != 1 || f.Ops() != 0 {
			t.Fatalf("out %d ops %d: an uncommitted transaction counts nothing", len(out), f.Ops())
		}
	})

	t.Run("prepared", func(t *testing.T) {
		f := shopFilter()
		prepare := txnEntry(5, bson.D{{Key: "applyOps", Value: applyOps(insert("shop.a", 1), insert("shop.a", 2))}, {Key: "prepare", Value: true}})
		commit := txnEntry(5, bson.D{{Key: "commitTransaction", Value: 1}, {Key: "commitTimestamp", Value: ts(2, 1)}})
		out := mustRun(t, f, entryAt(t, ts(2, 1), prepare), entryAt(t, ts(2, 2), commit))
		if len(out) != 2 || str(out[1], "ns") != "admin.$cmd" {
			t.Fatalf("out = %v", out)
		}
		if _, err := out[1].LookupErr("lsid"); err != nil {
			t.Error("commitTransaction lost lsid")
		}
		if f.Ops() != 2 {
			t.Errorf("ops = %d, want 2", f.Ops())
		}

		f = shopFilter()
		abort := txnEntry(6, bson.D{{Key: "abortTransaction", Value: 1}})
		prepare = txnEntry(6, bson.D{{Key: "applyOps", Value: applyOps(insert("shop.a", 1))}, {Key: "prepare", Value: true}})
		out = mustRun(t, f, entryAt(t, ts(3, 1), prepare), entryAt(t, ts(3, 2), abort))
		if len(out) != 2 || f.Ops() != 0 {
			t.Fatalf("aborted: out %d ops %d", len(out), f.Ops())
		}

		f = shopFilter()
		prepare = txnEntry(7, bson.D{{Key: "applyOps", Value: applyOps(insert("crm.a", 1))}, {Key: "prepare", Value: true}})
		commit = txnEntry(7, bson.D{{Key: "commitTransaction", Value: 1}})
		if out = mustRun(t, f, entryAt(t, ts(4, 1), prepare), entryAt(t, ts(4, 2), commit)); len(out) != 0 {
			t.Fatalf("unselected prepared transaction written: %v", out)
		}
	})
}

func TestFilterLimit(t *testing.T) {
	var src bytes.Buffer
	for i := uint32(1); i <= 5; i++ {
		src.Write(entryAt(t, ts(100, i), insert("shop.a", int(i))))
	}
	f := &Filter{Limit: ts(100, 4)}
	var dst collect
	// Nothing after the entry at the limit may be read.
	if err := f.Copy(context.Background(), &dst, io.MultiReader(bytes.NewReader(src.Bytes()[:src.Len()/5*4]), failReader{})); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if len(dst.entries) != 3 || !f.LimitReached() {
		t.Fatalf("%d entries (limit reached %v), want 3", len(dst.entries), f.LimitReached())
	}
	if stop, err := f.Apply(entryAt(t, ts(100, 1), insert("shop.a", 1)), dst.WriteEntry); !stop || err != nil {
		t.Errorf("apply after the limit: stop %v err %v", stop, err)
	}

	f = &Filter{Limit: ts(101, 0)}
	dst = collect{}
	if err := f.Copy(context.Background(), &dst, bytes.NewReader(src.Bytes())); err != nil {
		t.Fatal(err)
	}
	if len(dst.entries) != 5 || f.LimitReached() {
		t.Fatalf("%d entries, limit reached %v", len(dst.entries), f.LimitReached())
	}
}

func TestFilterCopyErrors(t *testing.T) {
	good := entryAt(t, ts(1, 1), insert("shop.a", 1))
	f := &Filter{}
	if err := f.Copy(context.Background(), &collect{}, bytes.NewReader(good[:len(good)-1])); !errors.Is(err, ErrTruncated) {
		t.Errorf("truncated: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.Copy(ctx, &collect{}, bytes.NewReader(good)); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}
	var buf bytes.Buffer
	aw, err := NewArchiveWriter(&buf, ArchiveOptions{ServerVersion: "8.0.32"})
	if err != nil {
		t.Fatal(err)
	}
	_ = aw.Close()
	if err := f.Copy(context.Background(), aw, bytes.NewReader(good)); !errors.Is(err, ErrArchiveClosed) {
		t.Errorf("closed archive: %v", err)
	}
}

// TestFilterFixtures runs every fixture through the Filter, keeping all of its
// databases under new names, and checks the rules on real entries of MongoDB 5.0 and
// 8.0.
func TestFilterFixtures(t *testing.T) {
	for _, v := range fixtureVersions {
		for scenario, dbs := range fixtureScenarios {
			t.Run(v+"/"+scenario, func(t *testing.T) {
				sel := map[string]bool{}
				var renamed []string
				for _, db := range dbs {
					sel[db] = true
					renamed = append(renamed, rescue(db))
				}
				f := &Filter{Select: sel, Rename: rescue}
				var c collect
				if err := f.Copy(context.Background(), &c, bytes.NewReader(readFixture(t, v, scenario))); err != nil {
					t.Fatalf("filter: %v", err)
				}
				if f.Ops() == 0 || f.Entries() != int64(len(c.entries)) {
					t.Errorf("ops %d, entries %d for %d written", f.Ops(), f.Entries(), len(c.entries))
				}
				checkFixtureOutput(t, renamed, c.entries)
			})
		}
	}
}

// filterFixture filters a fixture with Select sel and Rename rescue.
func filterFixture(t *testing.T, version, scenario string, sel map[string]bool) []bson.Raw {
	t.Helper()
	return mustRun(t, &Filter{Select: sel, Rename: rescue}, splitEntries(t, readFixture(t, version, scenario))...)
}

// checkFixtureOutput checks what every filtered fixture must satisfy.
func checkFixtureOutput(t *testing.T, renamed []string, out []bson.Raw) {
	t.Helper()
	if len(out) == 0 {
		t.Fatal("nothing written")
	}
	for _, e := range out {
		_, errLSID := e.LookupErr("lsid")
		switch name := commandName(e); {
		case str(e, "ns") == "admin.$cmd" && name != "applyOps" && name != "commitTransaction" && name != "abortTransaction":
			t.Errorf("%s written on admin.$cmd", name)
		case errLSID == nil && str(e, "ns") != "admin.$cmd":
			t.Errorf("session fields kept outside a transaction: %s", e)
		}
		walkNamespaces(t, e, func(ns string) {
			if ns != "admin.$cmd" && !hasDB(ns, renamed...) {
				t.Errorf("namespace %q outside %v in %s", ns, renamed, e)
			}
		})
		for _, k := range []string{"ui", "stmtId", "fromMigrate", "needsRetryImage", "preImageOpTime"} {
			if _, err := e.LookupErr(k); err == nil {
				t.Errorf("%s kept: %s", k, e)
			}
		}
		switch commandName(e) {
		case "startIndexBuild", "commitIndexBuild", "abortIndexBuild":
			t.Errorf("index build entry written: %s", e)
		}
		if strings.HasSuffix(str(e, "ns"), ".system.views") || str(e, "o", "create") == "system.views" {
			t.Errorf("system.views written: %s", e)
		}
	}
}

// TestFilterFixtureDetails checks scenario-specific results on the fixtures.
func TestFilterFixtureDetails(t *testing.T) {
	for _, v := range fixtureVersions {
		t.Run(v, func(t *testing.T) {
			count := func(out []bson.Raw, pred func(bson.Raw) bool) int {
				n := 0
				for _, e := range out {
					if pred(e) {
						n++
					}
				}
				return n
			}

			// Index builds: one createIndexes per index of the committed build, none
			// for the aborted one; the rename moves the index that was not dropped.
			out := filterFixture(t, v, "indexbuild", map[string]bool{"fx_idx": true})
			if n := count(out, func(e bson.Raw) bool { return str(e, "o", "createIndexes") == "people" }); n != 2 {
				t.Errorf("indexbuild: %d createIndexes on people, want 2", n)
			}
			if n := count(out, func(e bson.Raw) bool {
				return str(e, "o", "createIndexes") == "persons" && str(e, "o", "name") == "v_k"
			}); n != 1 {
				t.Errorf("indexbuild: %d createIndexes v_k on persons, want 1", n)
			}
			if last := out[len(out)-1]; str(last, "o", "dropIndexes") != "people" || str(last, "o", "index") != "*" {
				t.Errorf("indexbuild ends with %s", last)
			}

			// A rename into the selected database keeps the copy (fromMigrate on 5.0).
			out = filterFixture(t, v, "crossdb", map[string]bool{"fx_xb": true})
			var all []bson.Raw
			for _, e := range out {
				all = append(all, e)
				if commandName(e) == "applyOps" {
					all = append(all, nestedOps(t, e)...)
				}
			}
			copies := count(all, func(e bson.Raw) bool {
				return str(e, "op") == "i" && strings.HasPrefix(str(e, "ns"), "fx_xb_rescue.tmp")
			})
			if copies != 5 {
				t.Errorf("crossdb into fx_xb: %d copied documents, want 5", copies)
			}
			if n := count(out, func(e bson.Raw) bool { return str(e, "o", "to") == "fx_xb_rescue.dst" }); n != 1 {
				t.Errorf("crossdb into fx_xb: %d renames to fx_xb_rescue.dst", n)
			}
			// A rename out of the selected database leaves only its drop.
			out = filterFixture(t, v, "crossdb", map[string]bool{"fx_xa": true})
			if last := out[len(out)-1]; str(last, "o", "drop") != "src" || str(last, "ns") != "fx_xa_rescue.$cmd" {
				t.Errorf("crossdb out of fx_xa ends with %s", last)
			}

			// A transaction across two databases keeps the selected operations and its
			// commit.
			f := &Filter{Select: map[string]bool{"fx_txn2": true}, Rename: rescue}
			out = mustRun(t, f, splitEntries(t, readFixture(t, v, "txn"))...)
			last := out[len(out)-1]
			if commandName(last) != "applyOps" || len(nestedOps(t, last)) != 0 || last.Lookup("o", "count").Int64() != 1 {
				t.Errorf("txn: commit entry %s", last)
			}
			// create c, insert seed into c, and the cross-database insert at commit.
			if f.Ops() != 3 {
				t.Errorf("txn: ops = %d, want 3", f.Ops())
			}

			// A time-series collection is created as one, without its view.
			out = filterFixture(t, v, "timeseries", map[string]bool{"fx_ts": true})
			creates := 0
			for _, e := range out {
				if str(e, "o", "create") == "weather" {
					creates++
					if str(e, "o", "timeseries", "timeField") != "at" || str(e, "o", "timeseries", "granularity") != "hours" {
						t.Errorf("timeseries: create %s", e)
					}
				}
			}
			if creates != 1 {
				t.Errorf("timeseries: %d creates of weather, want 1", creates)
			}

			// A view is created with the create command.
			out = filterFixture(t, v, "ddl", map[string]bool{"fx_ddl": true})
			if n := count(out, func(e bson.Raw) bool {
				return str(e, "o", "create") == "v_events" && str(e, "o", "viewOn") == "events" && str(e, "ns") == "fx_ddl_rescue.$cmd"
			}); n != 1 {
				t.Errorf("ddl: %d creates of the view v_events, want 1", n)
			}
		})
	}
}
