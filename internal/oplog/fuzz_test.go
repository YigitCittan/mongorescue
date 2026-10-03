package oplog

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// fuzzSeeds adds the fixtures and a few hand-made streams to f.
func fuzzSeeds(f *testing.F, add func(data []byte)) {
	for _, v := range fixtureVersions {
		for scenario := range fixtureScenarios {
			add(readFixture(f, v, scenario))
		}
	}
	add(nil)
	add([]byte{5, 0, 0, 0, 0})
	add([]byte{0xff, 0xff, 0xff, 0xff})
	nested := entryAt(f, ts(1, 1), command("admin", bson.D{{Key: "applyOps", Value: applyOps(
		insert("a.x", 1), insert("b.x", 2),
		command("a", bson.D{{Key: "renameCollection", Value: "a.x"}, {Key: "to", Value: "a.y"}}),
	)}}))
	add(nested)
	add(entryAt(f, ts(1, 2), txnEntry(1, bson.D{{Key: "applyOps", Value: applyOps(insert("a.x", 1))}, {Key: "partialTxn", Value: true}})))
}

// FuzzScanner checks that the Scanner passes any stream through unchanged, whatever
// the write boundaries, and agrees with Reader and entryOpTime on where the stream is
// valid.
func FuzzScanner(f *testing.F) {
	fuzzSeeds(f, func(data []byte) { f.Add(data, uint64(1)) })
	f.Fuzz(func(t *testing.T, data []byte, seed uint64) {
		// The expected result: walk the documents with Reader.
		var want []OpTime
		var wantErr error
		r := NewReader(bytes.NewReader(data))
		for {
			doc, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err == nil {
				var pos OpTime
				if pos, err = entryOpTime(doc); err == nil {
					want = append(want, pos)
					continue
				}
			}
			wantErr = err
			break
		}

		var out bytes.Buffer
		s := NewScanner(&out)
		err := writeSplit(t, s, data, rand.New(rand.NewPCG(seed, seed^0x9e3779b9))) //nolint:gosec // G404: reproducible write splits, not secrets.
		if err == nil {
			err = s.Finish()
		}
		switch {
		case wantErr == nil && err != nil:
			t.Fatalf("scanner error %v on a valid stream", err)
		case wantErr != nil && err == nil:
			t.Fatalf("scanner accepted a stream the reader refuses (%v)", wantErr)
		case wantErr != nil && errors.Is(wantErr, ErrTruncated) != errors.Is(err, ErrTruncated):
			t.Fatalf("scanner error %v, reader error %v", err, wantErr)
		}
		if wantErr != nil {
			return
		}
		if !bytes.Equal(out.Bytes(), data) {
			t.Fatal("bytes changed on the way through")
		}
		if s.Count() != int64(len(want)) {
			t.Fatalf("count %d, want %d", s.Count(), len(want))
		}
		if len(want) > 0 && (s.First() != want[0] || s.Last() != want[len(want)-1]) {
			t.Fatalf("first %+v last %+v, want %+v %+v", s.First(), s.Last(), want[0], want[len(want)-1])
		}
	})
}

// FuzzFilter checks that the Filter never panics and that whatever it writes is valid
// BSON in the renamed selected database, before the limit.
func FuzzFilter(f *testing.F) {
	fuzzSeeds(f, func(data []byte) { f.Add(data, "fx_txn", uint32(0)) })
	f.Add(readFixture(f, "8.0", "crossdb"), "fx_xb", uint32(1790996222))
	f.Add(readFixture(f, "5.0", "ddl"), "fx_ddl", uint32(0))
	f.Fuzz(func(t *testing.T, data []byte, db string, limit uint32) {
		if db == "" || strings.ContainsAny(db, ".\x00") || systemDB(db) {
			db = "a"
		}
		filter := &Filter{
			Select: map[string]bool{db: true},
			Rename: func(name string) string { return name + "_r" },
			Limit:  bson.Timestamp{T: limit},
		}
		target := db + "_r"
		if len(target) > maxDatabaseName || strings.ContainsAny(target, "/\\ \"$") {
			target = ""
		}
		var c collect
		err := filter.Copy(t.Context(), &c, bytes.NewReader(data))
		if err != nil && target != "" && errors.Is(err, ErrBadRename) {
			t.Fatalf("valid rename refused: %v", err)
		}
		var entries int64
		for _, e := range c.entries {
			entries++
			if err := e.Validate(); err != nil {
				t.Fatalf("invalid entry written: %v", err)
			}
			secs, inc, ok := e.Lookup("ts").TimestampOK()
			if !ok {
				t.Fatalf("entry without ts written: %s", e)
			}
			if limit != 0 && !Before(bson.Timestamp{T: secs, I: inc}, filter.Limit) {
				t.Fatalf("entry at %d:%d written past the limit %d", secs, inc, limit)
			}
			checkFuzzNamespaces(t, e, target, 0)
		}
		if entries != filter.Entries() {
			t.Fatalf("%d entries written, Entries() = %d", entries, filter.Entries())
		}
		if filter.Ops() < 0 {
			t.Fatalf("ops = %d", filter.Ops())
		}
	})
}

// checkFuzzNamespaces checks that an operation written by the Filter and everything
// nested in it is in database target, apart from applyOps and transaction entries on
// admin.$cmd.
func checkFuzzNamespaces(t *testing.T, e bson.Raw, target string, depth int) {
	t.Helper()
	if depth > maxApplyOpsDepth {
		t.Fatalf("written entry nested deeper than %d levels", maxApplyOpsDepth)
	}
	ns := str(e, "ns")
	name := commandName(e)
	if ns == "admin.$cmd" {
		if name != "applyOps" && name != "commitTransaction" && name != "abortTransaction" {
			t.Fatalf("%q written on admin.$cmd", name)
		}
	} else if !strings.HasPrefix(ns, target+".") {
		t.Fatalf("namespace %q outside %s", ns, target)
	}
	if str(e, "op") != "c" {
		return
	}
	if name == "renameCollection" {
		for _, k := range []string{"renameCollection", "to"} {
			if s := str(e, "o", k); !strings.HasPrefix(s, target+".") {
				t.Fatalf("rename %s %q outside %s", k, s, target)
			}
		}
	}
	if arr, ok := e.Lookup("o", "applyOps").ArrayOK(); ok && name == "applyOps" {
		vals, err := arr.Values()
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range vals {
			checkFuzzNamespaces(t, v.Document(), target, depth+1)
		}
	}
}
