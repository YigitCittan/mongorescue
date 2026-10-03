package oplog

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// fixtureVersions are the MongoDB versions with fixtures in testdata/fixtures.
var fixtureVersions = []string{"5.0", "8.0"}

// fixtureScenarios are the scenarios of testdata/gen, with the databases each uses.
var fixtureScenarios = map[string][]string{
	"crud":       {"fx_crud"},
	"ddl":        {"fx_ddl", "fx_ddl2"},
	"txn":        {"fx_txn", "fx_txn2"},
	"indexbuild": {"fx_idx"},
	"timeseries": {"fx_ts"},
	"crossdb":    {"fx_xa", "fx_xb"},
	"replace":    {"fx_rep"},
}

// readFixture returns the raw bytes of a fixture.
func readFixture(t testing.TB, version, scenario string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "fixtures", version, scenario+".bson"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

// splitEntries splits concatenated BSON documents.
func splitEntries(t testing.TB, b []byte) []bson.Raw {
	t.Helper()
	r := NewReader(bytes.NewReader(b))
	var out []bson.Raw
	for {
		doc, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("split entries: %v", err)
		}
		out = append(out, doc)
	}
}

// mustMarshal marshals d.
func mustMarshal(t testing.TB, d any) bson.Raw {
	t.Helper()
	b, err := bson.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// ts returns the timestamp (secs, inc).
func ts(secs, inc uint32) bson.Timestamp { return bson.Timestamp{T: secs, I: inc} }

// entryAt builds a top-level oplog entry at timestamp at: the given fields, then ts,
// t, v and wall as the server writes them.
func entryAt(t testing.TB, at bson.Timestamp, fields bson.D) bson.Raw {
	t.Helper()
	d := append(bson.D{}, fields...)
	d = append(d,
		bson.E{Key: "ts", Value: at},
		bson.E{Key: "t", Value: int64(1)},
		bson.E{Key: "v", Value: int64(2)},
		bson.E{Key: "wall", Value: bson.DateTime(1700000000000)},
	)
	return mustMarshal(t, d)
}

// collect is an EntryWriter that keeps what it is given.
type collect struct{ entries []bson.Raw }

func (c *collect) WriteEntry(b []byte) error {
	c.entries = append(c.entries, append(bson.Raw(nil), b...))
	return nil
}

// run filters entries and returns the written ones.
func run(t testing.TB, f *Filter, entries ...bson.Raw) ([]bson.Raw, error) {
	t.Helper()
	var c collect
	for _, e := range entries {
		stop, err := f.Apply(e, c.WriteEntry)
		if err != nil {
			return c.entries, err
		}
		if stop {
			break
		}
	}
	return c.entries, nil
}

// mustRun is run that fails the test on an error.
func mustRun(t testing.TB, f *Filter, entries ...bson.Raw) []bson.Raw {
	t.Helper()
	out, err := run(t, f, entries...)
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	return out
}

// str returns the string at path in doc ("" when absent).
func str(doc bson.Raw, path ...string) string {
	s, _ := doc.Lookup(path...).StringValueOK()
	return s
}

// nestedOps returns the operations of an applyOps entry.
func nestedOps(t testing.TB, entry bson.Raw) []bson.Raw {
	t.Helper()
	arr, ok := entry.Lookup("o", "applyOps").ArrayOK()
	if !ok {
		t.Fatalf("no applyOps array in %s", entry)
	}
	vals, err := arr.Values()
	if err != nil {
		t.Fatal(err)
	}
	out := make([]bson.Raw, 0, len(vals))
	for _, v := range vals {
		out = append(out, v.Document())
	}
	return out
}

// walkNamespaces calls fn for the ns of entry and of every operation nested in it,
// with the namespaces the command arguments name.
func walkNamespaces(t testing.TB, entry bson.Raw, fn func(ns string)) {
	t.Helper()
	fn(str(entry, "ns"))
	o, _ := entry.Lookup("o").DocumentOK()
	if str(entry, "op") != "c" || o == nil {
		return
	}
	if s := str(o, "renameCollection"); s != "" {
		fn(s)
		fn(str(o, "to"))
	}
	if _, ok := o.Lookup("applyOps").ArrayOK(); ok {
		for _, op := range nestedOps(t, entry) {
			walkNamespaces(t, op, fn)
		}
	}
}

// hasDB reports whether ns is in one of dbs.
func hasDB(ns string, dbs ...string) bool {
	for _, db := range dbs {
		if strings.HasPrefix(ns, db+".") {
			return true
		}
	}
	return false
}
