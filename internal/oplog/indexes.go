package oplog

import (
	"bytes"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// mongorestore does not build the indexes of a replayed createIndexes (or
// commitIndexBuild) when it reads them: it collects them in a catalog keyed by
// database and collection name and builds them once the replay ends. The catalog
// follows drop, dropDatabase and dropIndexes, but not renameCollection, so an index
// built before a rename would be built at the end on the old name, recreating an
// empty collection there. The Filter therefore remembers the indexes it has written
// per collection, and after a rename writes what moves them in mongorestore's
// catalog: dropIndexes "*" on the target (whose indexes a dropTarget rename removed),
// a createIndexes on the target for each index of the source, and dropIndexes "*"
// on the source. dropIndexes entries only change the catalog; mongorestore never sends
// them to the server.

// indexKey is the key of a collection in Filter.indexes.
func indexKey(db, coll string) string { return db + "\x00" + coll }

// indexSpec is an index specification the Filter wrote in a createIndexes.
type indexSpec struct {
	name string
	key  []byte // the raw key pattern, to match dropIndexes by key
	spec bson.Raw
}

// trackIndexes updates the indexes written for db.coll (the names written, after
// Rename) after a command o named name.
func (f *Filter) trackIndexes(name string, o bson.Raw, db string) error {
	coll, _ := o.Lookup(name).StringValueOK()
	switch name {
	case "createIndexes":
		spec, err := rebuild(o, func(k string, v bson.RawValue) (any, bool, error) {
			return v, k != "createIndexes", nil
		})
		if err != nil {
			return err
		}
		return f.addIndex(db, coll, spec)
	case "drop":
		delete(f.indexes, indexKey(db, coll))
	case "dropDatabase":
		for k := range f.indexes {
			if strings.HasPrefix(k, db+"\x00") {
				delete(f.indexes, k)
			}
		}
	case "dropIndexes", "dropIndex", "deleteIndexes", "deleteIndex":
		k := indexKey(db, coll)
		if len(f.indexes[k]) == 0 {
			return nil
		}
		index := o.Lookup("index")
		if s, ok := index.StringValueOK(); ok && s == "*" {
			delete(f.indexes, k)
			return nil
		}
		kept := f.indexes[k][:0]
		for _, spec := range f.indexes[k] {
			s, isName := index.StringValueOK()
			byName := isName && spec.name == s
			byKey := index.Type == bson.TypeEmbeddedDocument && bytes.Equal(spec.key, index.Value)
			if !byName && !byKey {
				kept = append(kept, spec)
			}
		}
		f.indexes[k] = kept
	}
	return nil
}

// addIndex remembers an index specification written for db.coll.
func (f *Filter) addIndex(db, coll string, spec bson.D) error {
	raw, err := bson.Marshal(spec)
	if err != nil {
		return fmt.Errorf("%w: encode index specification: %w", ErrMalformed, err)
	}
	name, _ := bson.Raw(raw).Lookup("name").StringValueOK()
	if f.indexes == nil {
		f.indexes = map[string][]indexSpec{}
	}
	k := indexKey(db, coll)
	f.indexes[k] = append(f.indexes[k], indexSpec{name: name, key: bson.Raw(raw).Lookup("key").Value, spec: raw})
	return nil
}

// moveIndexes returns the entries that move the indexes written for
// fromDB.fromColl to toDB.toColl in mongorestore's catalog after a rename entry.
func (f *Filter) moveIndexes(entry bson.Raw, fromDB, fromColl, toDB, toColl string) ([]bson.D, error) {
	from, to := indexKey(fromDB, fromColl), indexKey(toDB, toColl)
	specs := f.indexes[from]
	var out []bson.D
	if len(f.indexes[to]) > 0 {
		d, err := f.commandEntry(entry, toDB, bson.D{{Key: "dropIndexes", Value: toColl}, {Key: "index", Value: "*"}})
		if err != nil {
			return nil, err
		}
		out = append(out, d)
		delete(f.indexes, to)
	}
	if len(specs) == 0 {
		return out, nil
	}
	for _, s := range specs {
		cmd, err := rebuild(s.spec, func(_ string, v bson.RawValue) (any, bool, error) { return v, true, nil })
		if err != nil {
			return nil, err
		}
		d, err := f.commandEntry(entry, toDB, append(bson.D{{Key: "createIndexes", Value: toColl}}, cmd...))
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	d, err := f.commandEntry(entry, fromDB, bson.D{{Key: "dropIndexes", Value: fromColl}, {Key: "index", Value: "*"}})
	if err != nil {
		return nil, err
	}
	out = append(out, d)
	f.indexes[to] = specs
	delete(f.indexes, from)
	return out, nil
}
