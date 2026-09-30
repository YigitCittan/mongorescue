//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// collSnapshot describes one collection or view in a form that can be compared across
// databases: its type and options, its index specifications and its documents.
type collSnapshot struct {
	Type    string
	Options string
	Indexes []string
	Count   int
	// Hash is the SHA-256 of the canonical extended JSON of every document, sorted
	// by _id. Views are compared by definition only (Count and Hash stay empty).
	Hash string
}

// dbSnapshot maps collection and view names to their snapshots. System collections
// (system.views, system.buckets.*) are left out: they are compared through the
// views and time-series collections they belong to.
type dbSnapshot map[string]collSnapshot

// only returns the part of s for names.
func (s dbSnapshot) only(names ...string) dbSnapshot {
	out := dbSnapshot{}
	for _, n := range names {
		if c, ok := s[n]; ok {
			out[n] = c
		}
	}
	return out
}

// without returns s minus names.
func (s dbSnapshot) without(names ...string) dbSnapshot {
	out := maps.Clone(s)
	for _, n := range names {
		delete(out, n)
	}
	return out
}

// snapshotDB reads collections, options, indexes and document hashes of db.
func (m *mongoEnv) snapshotDB(t *testing.T, db string) dbSnapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	specs, err := m.Client.Database(db).ListCollectionSpecifications(ctx, bson.D{})
	if err != nil {
		t.Fatalf("list collections of %s: %v", db, err)
	}
	snap := dbSnapshot{}
	for _, spec := range specs {
		if strings.HasPrefix(spec.Name, "system.") {
			continue
		}
		c := collSnapshot{Type: spec.Type, Options: canonicalJSON(t, spec.Options, false)}
		if spec.Type != "view" {
			c.Indexes = m.indexSpecs(ctx, t, db, spec.Name)
			c.Count, c.Hash = m.documentHash(ctx, t, db, spec.Name)
		}
		snap[spec.Name] = c
	}
	return snap
}

// indexSpecs returns the normalised index specifications of db.coll sorted by name.
func (m *mongoEnv) indexSpecs(ctx context.Context, t *testing.T, db, coll string) []string {
	t.Helper()
	cur, err := m.Client.Database(db).Collection(coll).Indexes().List(ctx)
	if err != nil {
		t.Fatalf("list indexes of %s.%s: %v", db, coll, err)
	}
	defer cur.Close(ctx)
	var out []string
	for cur.Next(ctx) {
		out = append(out, canonicalJSON(t, cur.Current, false, "ns"))
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("iterate indexes of %s.%s: %v", db, coll, err)
	}
	slices.Sort(out)
	return out
}

// documentHash returns the number of documents of db.coll and the SHA-256 of their
// canonical extended JSON in _id order.
func (m *mongoEnv) documentHash(ctx context.Context, t *testing.T, db, coll string) (int, string) {
	t.Helper()
	cur, err := m.Client.Database(db).Collection(coll).Find(ctx, bson.D{},
		options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		t.Fatalf("find %s.%s: %v", db, coll, err)
	}
	defer cur.Close(ctx)
	h := sha256.New()
	n := 0
	for cur.Next(ctx) {
		js, err := bson.MarshalExtJSON(cur.Current, true, false)
		if err != nil {
			t.Fatalf("extended JSON of a %s.%s document: %v", db, coll, err)
		}
		h.Write(js)
		h.Write([]byte{'\n'})
		n++
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("iterate %s.%s: %v", db, coll, err)
	}
	return n, hex.EncodeToString(h.Sum(nil))
}

// canonicalJSON renders raw as canonical extended JSON with the fields of every
// document sorted, except the (ordered) key pattern of an index, and without the
// top-level fields in drop.
func canonicalJSON(t *testing.T, raw bson.Raw, keepOrder bool, drop ...string) string {
	t.Helper()
	if len(raw) == 0 {
		return "{}"
	}
	doc, err := sortedDoc(raw, keepOrder, drop)
	if err != nil {
		t.Fatalf("normalise %s: %v", raw, err)
	}
	js, err := bson.MarshalExtJSON(doc, true, false)
	if err != nil {
		t.Fatalf("extended JSON: %v", err)
	}
	return string(js)
}

func sortedDoc(raw bson.Raw, keepOrder bool, drop []string) (bson.D, error) {
	elems, err := raw.Elements()
	if err != nil {
		return nil, err
	}
	out := make(bson.D, 0, len(elems))
	for _, e := range elems {
		if slices.Contains(drop, e.Key()) {
			continue
		}
		v, err := sortedValue(e.Value(), e.Key() == "key")
		if err != nil {
			return nil, err
		}
		out = append(out, bson.E{Key: e.Key(), Value: v})
	}
	if !keepOrder {
		slices.SortFunc(out, func(a, b bson.E) int { return strings.Compare(a.Key, b.Key) })
	}
	return out, nil
}

func sortedValue(v bson.RawValue, keepOrder bool) (any, error) {
	switch v.Type {
	case bson.TypeEmbeddedDocument:
		return sortedDoc(v.Document(), keepOrder, nil)
	case bson.TypeArray:
		values, err := v.Array().Values()
		if err != nil {
			return nil, err
		}
		out := make(bson.A, 0, len(values))
		for _, item := range values {
			s, err := sortedValue(item, false)
			if err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return v, nil
	}
}

// assertSnapshotsEqual fails the test with every difference between want and got.
func assertSnapshotsEqual(t *testing.T, want, got dbSnapshot) {
	t.Helper()
	var diffs []string
	for _, name := range slices.Sorted(maps.Keys(want)) {
		w := want[name]
		g, ok := got[name]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("%s: missing after restore", name))
			continue
		}
		if g.Type != w.Type {
			diffs = append(diffs, fmt.Sprintf("%s: type %s, want %s", name, g.Type, w.Type))
		}
		if g.Options != w.Options {
			diffs = append(diffs, fmt.Sprintf("%s: options\n\tgot  %s\n\twant %s", name, g.Options, w.Options))
		}
		if !slices.Equal(g.Indexes, w.Indexes) {
			diffs = append(diffs, fmt.Sprintf("%s: indexes\n\tgot  %v\n\twant %v", name, g.Indexes, w.Indexes))
		}
		if g.Count != w.Count || g.Hash != w.Hash {
			diffs = append(diffs, fmt.Sprintf("%s: %d documents (hash %.12s), want %d (hash %.12s)", name, g.Count, g.Hash, w.Count, w.Hash))
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			diffs = append(diffs, fmt.Sprintf("%s: unexpected after restore", name))
		}
	}
	if len(diffs) > 0 {
		t.Fatalf("restored database differs from the source:\n%s", strings.Join(diffs, "\n"))
	}
}
