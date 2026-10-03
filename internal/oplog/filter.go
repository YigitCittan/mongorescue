package oplog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// maxApplyOpsDepth bounds the nesting of applyOps commands.
const maxApplyOpsDepth = 8

// maxDatabaseName is MongoDB's limit on database name length.
const maxDatabaseName = 63

// strippedFields are dropped from every entry and nested operation: collection UUIDs
// (the clone's collections have other UUIDs), the retryable-write fields and the
// references to retryable findAndModify images, which only the source can resolve.
// Transaction entries keep lsid, txnNumber and prevOpTime, which mongorestore needs
// to group them (see txnFields).
var strippedFields = map[string]bool{
	"ui":              true,
	"lsid":            true,
	"txnNumber":       true,
	"stmtId":          true,
	"prevOpTime":      true,
	"preImageOpTime":  true,
	"postImageOpTime": true,
	"needsRetryImage": true,
	"multiOpType":     true,
	"fromMigrate":     true,
}

// txnFields are the fields a transaction entry keeps.
var txnFields = map[string]bool{"lsid": true, "txnNumber": true, "prevOpTime": true}

// ignoredOps are operation types that are not replayed: no-ops and the bookkeeping
// of replicated record IDs, which mongorestore skips as well.
var ignoredOps = map[string]bool{"n": true, "cd": true, "ci": true, "cu": true, "km": true}

// Filter rewrites a stream of oplog entries for a filtered, renamed replay. It reads
// one entry at a time and writes the entries to replay:
//
//   - Entries of the selected databases are kept, with every namespace they carry
//     rewritten through Rename: ns, the arguments of create, drop, renameCollection
//     (renameCollection and to), createIndexes, collMod, dropDatabase, dropIndexes,
//     convertToCapped and emptycapped, and the operations nested in applyOps,
//     including transactions (partialTxn chains with their count, and prepared
//     transactions with commitTransaction and abortTransaction). Writes to
//     system.views and the creation of time-series buckets collections become the
//     user-level create, collMod and drop commands, which applyOps accepts
//     (views.go).
//   - Entries of other databases, of admin, config (config.system.indexBuilds
//     included) and local, no-ops and dbCheck entries are dropped. Collection UUIDs
//     (ui), fromMigrate and the retryable-write fields (lsid, txnNumber, stmtId,
//     prevOpTime and the findAndModify image references) are removed. fromMigrate
//     entries are kept: on a replica set they are the copies made by
//     convertToCapped, cloneCollectionAsCapped and (5.0) renames across databases,
//     and dropping them would leave those collections empty.
//   - Each commitIndexBuild becomes one createIndexes entry per index at the commit's
//     position; startIndexBuild and abortIndexBuild are dropped. mongorestore builds
//     replayed indexes only at the end, by collection name, so after a rename the
//     Filter also writes the entries that move the indexes it wrote to the new name
//     (indexes.go).
//   - A rename over an existing collection (dropTarget, also written by $out and
//     convertToCapped) becomes a drop of the target followed by the rename without
//     dropTarget: applyOps would move the clone's target aside to a tmpXXXXX.rename
//     collection instead of dropping it, as dropTarget names the source's UUID.
//   - A renameCollection between a selected and an unselected database returns
//     ErrCrossSelectionRename, an unknown command ErrUnknownCommand and an unknown
//     operation type ErrUnknownOp: such a stream cannot be replayed safely.
//   - The stream ends before the first entry at or after Limit, so mongorestore never
//     sees an entry past --oplogLimit.
//
// Ops counts the operations written the way mongorestore counts "applied N oplog
// entries", so the two can be compared after a replay. A Filter needs either Rename
// or InPlace: the zero Filter refuses to run (ErrNoTarget), so a caller cannot replay
// into the source databases by accident. A Filter is not safe for concurrent use.
type Filter struct {
	// Select lists the databases to keep; nil keeps every database except admin,
	// config and local, which are always dropped.
	Select map[string]bool
	// Rename maps a kept database to the database to replay it into. It must return
	// a valid database name other than db, or the Filter fails with ErrBadRename.
	// Exactly one of Rename and InPlace must be set.
	Rename func(db string) string
	// InPlace replays the kept databases under their own names, over the source:
	// the destructive mode, which must be chosen explicitly.
	InPlace bool
	// Limit is the exclusive end of the replay, the --oplogLimit position; the zero
	// Timestamp sets none.
	Limit bson.Timestamp

	ops          int64
	entries      int64
	limitReached bool
	txns         map[string]*txnState
	// indexes are the index specifications written per collection (see indexes.go).
	indexes map[string][]indexSpec
}

// txnState tracks a transaction whose entries span several oplog entries.
type txnState struct {
	// written counts the transaction's entries written, ops the nested operations in
	// them, and pending the operations mongorestore applies when it commits.
	written, ops, pending int64
}

// EntryWriter receives the entries a Filter writes; *ArchiveWriter implements it.
type EntryWriter interface {
	// WriteEntry writes one complete BSON document.
	WriteEntry(entry []byte) error
}

// Ops returns the number of operations written so far, counted like mongorestore's
// "applied N oplog entries": a non-transaction applyOps counts itself and its nested
// operations, a transaction counts its operations once it is committed in the
// stream, and an aborted or unfinished transaction counts nothing.
func (f *Filter) Ops() int64 { return f.ops }

// Entries returns the number of entries written so far.
func (f *Filter) Entries() int64 { return f.entries }

// LimitReached reports whether the stream reached Limit.
func (f *Filter) LimitReached() bool { return f.limitReached }

// Copy reads the entries of src one at a time, filters them and writes the result to
// dst, until src ends or an entry at or after Limit is read (the rest of src is not
// read). It stops when ctx is done.
func (f *Filter) Copy(ctx context.Context, dst EntryWriter, src io.Reader) error {
	r := NewReader(src)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		doc, err := r.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		stop, err := f.Apply(doc, dst.WriteEntry)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
	}
}

// Apply filters one entry and calls emit for each entry to write (none, one, or one
// per index of a commitIndexBuild). It returns stop = true, without calling emit, for
// an entry at or after Limit: the stream must end there.
func (f *Filter) Apply(entry bson.Raw, emit func([]byte) error) (stop bool, err error) {
	if (f.Rename == nil) == !f.InPlace {
		return false, ErrNoTarget
	}
	if f.limitReached {
		return true, nil
	}
	if verr := entry.Validate(); verr != nil {
		return false, fmt.Errorf("%w: %w", ErrMalformed, verr)
	}
	t, i, ok := entry.Lookup("ts").TimestampOK()
	if !ok {
		return false, fmt.Errorf("%w: no timestamp field ts", ErrMalformed)
	}
	if f.Limit != (bson.Timestamp{}) && !Before(bson.Timestamp{T: t, I: i}, f.Limit) {
		f.limitReached = true
		return true, nil
	}

	var out []bson.D
	var ops int64
	if id, ok := txnID(entry); ok {
		out, ops, err = f.transaction(entry, id)
	} else {
		out, ops, err = f.op(entry, 0)
	}
	if err != nil {
		return false, err
	}
	for _, d := range out {
		b, err := bson.Marshal(d)
		if err != nil {
			return false, fmt.Errorf("%w: encode rewritten entry: %w", ErrMalformed, err)
		}
		if err := emit(b); err != nil {
			return false, err
		}
		f.entries++
	}
	f.ops += ops
	return false, nil
}

// txnID returns the transaction an entry belongs to: a command entry with lsid and
// txnNumber whose command is applyOps, commitTransaction or abortTransaction.
// Retryable batched inserts (multiOpType 1) carry the same fields but are no
// transaction, as for mongorestore.
func txnID(entry bson.Raw) (string, bool) {
	if op, _ := entry.Lookup("op").StringValueOK(); op != "c" {
		return "", false
	}
	lsid, err := entry.LookupErr("lsid")
	if err != nil {
		return "", false
	}
	txn, err := entry.LookupErr("txnNumber")
	if err != nil {
		return "", false
	}
	if m, ok := entry.Lookup("multiOpType").AsInt64OK(); ok && m == 1 {
		return "", false
	}
	switch commandName(entry) {
	case "applyOps", "commitTransaction", "abortTransaction":
		return string(lsid.Value) + "\x00" + string(txn.Value), true
	}
	return "", false
}

// commandName returns the first field name of an entry's o document.
func commandName(entry bson.Raw) string {
	o, ok := entry.Lookup("o").DocumentOK()
	if !ok {
		return ""
	}
	first, err := o.IndexErr(0)
	if err != nil {
		return ""
	}
	return first.Key()
}

// transaction filters an entry of transaction id.
func (f *Filter) transaction(entry bson.Raw, id string) ([]bson.D, int64, error) {
	if f.txns == nil {
		f.txns = map[string]*txnState{}
	}
	st := f.txns[id]
	switch commandName(entry) {
	case "commitTransaction", "abortTransaction":
		// Prepared transactions end with one of these; they only matter when an
		// entry of the transaction was written.
		delete(f.txns, id)
		if st == nil || st.written == 0 {
			return nil, 0, nil
		}
		d, err := rebuild(entry, func(key string, v bson.RawValue) (any, bool, error) {
			return v, !strippedFields[key] || txnFields[key], nil
		})
		if err != nil {
			return nil, 0, err
		}
		if commandName(entry) == "abortTransaction" {
			return []bson.D{d}, 0, nil
		}
		return []bson.D{d}, st.pending, nil
	}

	o, _ := entry.Lookup("o").DocumentOK()
	commit := !isTrue(o, "partialTxn") && !isTrue(o, "prepare")

	nested, ops, err := f.nested(o, 0)
	if err != nil {
		return nil, 0, err
	}
	if st == nil {
		st = &txnState{}
		f.txns[id] = st
	}
	st.ops += int64(len(nested))
	st.pending += ops
	if commit {
		delete(f.txns, id)
	}
	// An entry with nothing left is dropped, unless it commits a transaction whose
	// earlier entries were written.
	if len(nested) == 0 && (!commit || st.written == 0) {
		return nil, 0, nil
	}
	st.written++
	d, err := rebuild(entry, func(key string, v bson.RawValue) (any, bool, error) {
		if key == "o" {
			od, oerr := rebuild(o, func(k string, ov bson.RawValue) (any, bool, error) {
				switch k {
				case "applyOps":
					return nestedArray(nested), true, nil
				case "count":
					return sameIntType(ov, st.ops), true, nil
				}
				return ov, true, nil
			})
			return od, true, oerr
		}
		return v, !strippedFields[key] || txnFields[key], nil
	})
	if err != nil {
		return nil, 0, err
	}
	if commit {
		return []bson.D{d}, st.pending, nil
	}
	return []bson.D{d}, 0, nil
}

// nested filters the operations of an applyOps command document o, returning the
// operations to keep and their mongorestore count.
func (f *Filter) nested(o bson.Raw, depth int) ([]bson.D, int64, error) {
	if depth >= maxApplyOpsDepth {
		return nil, 0, fmt.Errorf("%w: applyOps nested more than %d levels", ErrMalformed, maxApplyOpsDepth)
	}
	arr, ok := o.Lookup("applyOps").ArrayOK()
	if !ok {
		return nil, 0, fmt.Errorf("%w: applyOps is not an array", ErrMalformed)
	}
	values, err := arr.Values()
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	var out []bson.D
	var ops int64
	for _, v := range values {
		doc, ok := v.DocumentOK()
		if !ok {
			return nil, 0, fmt.Errorf("%w: applyOps operation is not a document", ErrMalformed)
		}
		d, n, err := f.op(doc, depth+1)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, d...)
		ops += n
	}
	return out, ops, nil
}

// op filters one operation, a top-level entry (depth 0) or an operation nested in
// applyOps, and returns what replaces it and its mongorestore count.
func (f *Filter) op(doc bson.Raw, depth int) ([]bson.D, int64, error) {
	opType, ok := doc.Lookup("op").StringValueOK()
	if !ok {
		return nil, 0, fmt.Errorf("%w: no operation type op", ErrMalformed)
	}
	if ignoredOps[opType] {
		return nil, 0, nil
	}
	ns, ok := doc.Lookup("ns").StringValueOK()
	if !ok {
		return nil, 0, fmt.Errorf("%w: no namespace ns", ErrMalformed)
	}
	db, coll, ok := strings.Cut(ns, ".")
	if !ok || db == "" {
		return nil, 0, fmt.Errorf("%w: namespace %q has no database", ErrMalformed, ns)
	}
	switch opType {
	case "i", "u", "d":
		if !f.selected(db) {
			return nil, 0, nil
		}
		if coll == viewsCollection {
			return f.viewOp(doc, opType, db)
		}
		d, err := f.crud(doc, db, coll)
		if err != nil {
			return nil, 0, err
		}
		return []bson.D{d}, 1, nil
	case "c":
		return f.command(doc, db, depth)
	default:
		return nil, 0, fmt.Errorf("%w: %q", ErrUnknownOp, opType)
	}
}

// crud rewrites an insert, update or delete in db.coll.
func (f *Filter) crud(doc bson.Raw, db, coll string) (bson.D, error) {
	to, err := f.rename(db)
	if err != nil {
		return nil, err
	}
	return rebuild(doc, func(key string, v bson.RawValue) (any, bool, error) {
		switch {
		case strippedFields[key]:
			return nil, false, nil
		case key == "ns":
			return to + "." + coll, true, nil
		}
		return v, true, nil
	})
}

// command filters a command entry on db.$cmd.
func (f *Filter) command(doc bson.Raw, db string, depth int) ([]bson.D, int64, error) {
	o, ok := doc.Lookup("o").DocumentOK()
	if !ok {
		return nil, 0, fmt.Errorf("%w: command without a document o", ErrMalformed)
	}
	first, err := o.IndexErr(0)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: empty command", ErrMalformed)
	}
	name := first.Key()
	if name == "renameCollection" {
		// Checked before the database: a rename out of admin into a selected
		// database must be refused, not dropped.
		return f.renameCollection(doc, o)
	}
	if systemDB(db) && (db != "admin" || name != "applyOps") {
		// Commands on admin, config and local are not replayed, except applyOps on
		// admin, which carries operations of other databases.
		return nil, 0, nil
	}
	switch name {
	case "applyOps":
		return f.applyOps(doc, o, db, depth)
	case "create", "drop", "collMod", "createIndexes", "dropDatabase",
		"dropIndexes", "dropIndex", "deleteIndexes", "deleteIndex",
		"convertToCapped", "emptycapped":
		if !f.selected(db) {
			return nil, 0, nil
		}
		if coll, _ := o.Lookup("create").StringValueOK(); name == "create" && coll == viewsCollection {
			return nil, 0, nil
		}
		if name == "create" {
			cmd, ok, err := timeseriesCreate(o)
			if err != nil {
				return nil, 0, err
			}
			if ok {
				d, err := f.asCommand(doc, db, cmd)
				if err != nil {
					return nil, 0, err
				}
				return []bson.D{d}, 1, nil
			}
		}
		d, err := f.ddl(doc, o, db)
		if err != nil {
			return nil, 0, err
		}
		to, err := f.rename(db)
		if err != nil {
			return nil, 0, err
		}
		if err := f.trackIndexes(name, o, to); err != nil {
			return nil, 0, err
		}
		return []bson.D{d}, 1, nil
	case "commitIndexBuild":
		if !f.selected(db) {
			return nil, 0, nil
		}
		return f.commitIndexBuild(doc, o, db)
	case "startIndexBuild", "abortIndexBuild", "dbCheck":
		// Index builds are replayed as createIndexes at their commit; dbCheck only
		// checks data.
		return nil, 0, nil
	default:
		return nil, 0, fmt.Errorf("%w: %q on %s", ErrUnknownCommand, name, db)
	}
}

// applyOps filters a non-transaction applyOps command and the operations in it.
func (f *Filter) applyOps(doc, o bson.Raw, db string, depth int) ([]bson.D, int64, error) {
	nested, ops, err := f.nested(o, depth)
	if err != nil {
		return nil, 0, err
	}
	if len(nested) == 0 {
		return nil, 0, nil
	}
	ns := "admin.$cmd"
	if f.selected(db) {
		to, rerr := f.rename(db)
		if rerr != nil {
			return nil, 0, rerr
		}
		ns = to + ".$cmd"
	}
	d, err := rebuild(doc, func(key string, v bson.RawValue) (any, bool, error) {
		switch {
		case strippedFields[key]:
			return nil, false, nil
		case key == "ns":
			return ns, true, nil
		case key == "o":
			od, oerr := rebuild(o, func(k string, ov bson.RawValue) (any, bool, error) {
				if k == "applyOps" {
					return nestedArray(nested), true, nil
				}
				return ov, true, nil
			})
			return od, true, oerr
		}
		return v, true, nil
	})
	if err != nil {
		return nil, 0, err
	}
	// mongorestore counts the applyOps entry and each operation in it.
	return []bson.D{d}, 1 + ops, nil
}

// renameCollection filters a rename. Both namespaces must be on the same side of the
// selection.
func (f *Filter) renameCollection(doc, o bson.Raw) ([]bson.D, int64, error) {
	from, ok1 := o.Lookup("renameCollection").StringValueOK()
	to, ok2 := o.Lookup("to").StringValueOK()
	fromDB, fromColl, okFrom := strings.Cut(from, ".")
	toDB, toColl, okTo := strings.Cut(to, ".")
	if !ok1 || !ok2 || !okFrom || !okTo {
		return nil, 0, fmt.Errorf("%w: renameCollection without source and target namespaces", ErrMalformed)
	}
	fromSel, toSel := f.selected(fromDB), f.selected(toDB)
	if fromSel != toSel {
		return nil, 0, fmt.Errorf("%w: %s to %s", ErrCrossSelectionRename, from, to)
	}
	if !fromSel {
		return nil, 0, nil
	}
	newFrom, err := f.rename(fromDB)
	if err != nil {
		return nil, 0, err
	}
	newTo, err := f.rename(toDB)
	if err != nil {
		return nil, 0, err
	}
	d, err := rebuild(doc, func(key string, v bson.RawValue) (any, bool, error) {
		switch {
		case strippedFields[key]:
			return nil, false, nil
		case key == "ns":
			return newFrom + ".$cmd", true, nil
		case key == "o":
			od, oerr := rebuild(o, func(k string, ov bson.RawValue) (any, bool, error) {
				switch k {
				case "renameCollection":
					return renameNS(from, fromDB, newFrom), true, nil
				case "to":
					return renameNS(to, toDB, newTo), true, nil
				case "dropTarget":
					return false, true, nil
				}
				return ov, true, nil
			})
			return od, true, oerr
		}
		return v, true, nil
	})
	if err != nil {
		return nil, 0, err
	}
	var out []bson.D
	// dropTarget is a UUID or true when set; false or absent when not.
	if dt := o.Lookup("dropTarget"); dt.Type != 0 && (dt.Type != bson.TypeBoolean || dt.Boolean()) {
		drop := bson.D{{Key: "drop", Value: toColl}}
		dd, derr := f.commandEntry(doc, newTo, drop)
		if derr != nil {
			return nil, 0, derr
		}
		out = append(out, dd)
		if err = f.trackIndexes("drop", mustRaw(drop), newTo); err != nil {
			return nil, 0, err
		}
	}
	moves, err := f.moveIndexes(doc, newFrom, fromColl, newTo, toColl)
	if err != nil {
		return nil, 0, err
	}
	out = append(append(out, d), moves...)
	return out, int64(len(out)), nil
}

// ddl rewrites a DDL command on db: its ns, and the "ns" fields older servers put in
// index specifications (o.ns for createIndexes, o.idIndex.ns for create, o2.ns for
// dropIndexes).
func (f *Filter) ddl(doc, o bson.Raw, db string) (bson.D, error) {
	to, err := f.rename(db)
	if err != nil {
		return nil, err
	}
	return rebuild(doc, func(key string, v bson.RawValue) (any, bool, error) {
		switch {
		case strippedFields[key]:
			return nil, false, nil
		case key == "ns":
			return to + ".$cmd", true, nil
		case key == "o":
			od, oerr := rebuild(o, func(k string, ov bson.RawValue) (any, bool, error) {
				switch k {
				case "ns":
					if s, ok := ov.StringValueOK(); ok {
						return renameNS(s, db, to), true, nil
					}
				case "idIndex":
					if spec, ok := ov.DocumentOK(); ok {
						d, err := renameSpecNS(spec, db, to)
						return d, true, err
					}
				}
				return ov, true, nil
			})
			return od, true, oerr
		case key == "o2":
			if spec, ok := v.DocumentOK(); ok {
				d, err := renameSpecNS(spec, db, to)
				return d, true, err
			}
		}
		return v, true, nil
	})
}

// commitIndexBuild turns a commitIndexBuild into one createIndexes entry per index,
// at the commit's position. mongorestore defers commitIndexBuild to the end of the
// replay, where a later rename or drop of the collection no longer applies.
func (f *Filter) commitIndexBuild(doc, o bson.Raw, db string) ([]bson.D, int64, error) {
	coll, ok := o.Lookup("commitIndexBuild").StringValueOK()
	if !ok {
		return nil, 0, fmt.Errorf("%w: commitIndexBuild without a collection", ErrMalformed)
	}
	arr, ok := o.Lookup("indexes").ArrayOK()
	if !ok {
		return nil, 0, fmt.Errorf("%w: commitIndexBuild without indexes", ErrMalformed)
	}
	specs, err := arr.Values()
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	to, err := f.rename(db)
	if err != nil {
		return nil, 0, err
	}
	out := make([]bson.D, 0, len(specs))
	for _, s := range specs {
		spec, ok := s.DocumentOK()
		if !ok {
			return nil, 0, fmt.Errorf("%w: index specification is not a document", ErrMalformed)
		}
		renamed, err := renameSpecNS(spec, db, to)
		if err != nil {
			return nil, 0, err
		}
		if err = f.addIndex(to, coll, renamed); err != nil {
			return nil, 0, err
		}
		create := append(bson.D{{Key: "createIndexes", Value: coll}}, renamed...)
		d, err := rebuild(doc, func(key string, v bson.RawValue) (any, bool, error) {
			switch {
			case strippedFields[key]:
				return nil, false, nil
			case key == "ns":
				return to + ".$cmd", true, nil
			case key == "o":
				return create, true, nil
			}
			return v, true, nil
		})
		if err != nil {
			return nil, 0, err
		}
		out = append(out, d)
	}
	return out, int64(len(out)), nil
}

// selected reports whether database db is replayed.
func (f *Filter) selected(db string) bool {
	if systemDB(db) {
		return false
	}
	if f.Select == nil {
		return true
	}
	return f.Select[db]
}

// rename returns the database db is replayed into.
func (f *Filter) rename(db string) (string, error) {
	if f.InPlace {
		return db, nil
	}
	to := f.Rename(db)
	if to == db || to == "" || len(to) > maxDatabaseName || strings.ContainsAny(to, "./\\ \"$\x00") || systemDB(to) {
		return "", fmt.Errorf("%w: %q", ErrBadRename, to)
	}
	return to, nil
}

// systemDB reports whether db is admin, config or local, which are never replayed.
func systemDB(db string) bool {
	return db == "admin" || db == "config" || db == "local"
}

// mustRaw encodes a small command document built here; it cannot fail.
func mustRaw(d bson.D) bson.Raw {
	b, err := bson.Marshal(d)
	if err != nil {
		panic(err)
	}
	return b
}

// isTrue reports whether field key of doc is the boolean true.
func isTrue(doc bson.Raw, key string) bool {
	b, ok := doc.Lookup(key).BooleanOK()
	return ok && b
}

// renameNS replaces the database of namespace ns when it is from.
func renameNS(ns, from, to string) string {
	if rest, ok := strings.CutPrefix(ns, from+"."); ok {
		return to + "." + rest
	}
	return ns
}

// renameSpecNS copies an index specification with its "ns" field renamed.
func renameSpecNS(spec bson.Raw, from, to string) (bson.D, error) {
	return rebuild(spec, func(k string, v bson.RawValue) (any, bool, error) {
		if s, ok := v.StringValueOK(); ok && k == "ns" {
			return renameNS(s, from, to), true, nil
		}
		return v, true, nil
	})
}

// nestedArray returns operations as an applyOps array.
func nestedArray(ops []bson.D) bson.A {
	a := make(bson.A, len(ops))
	for i, op := range ops {
		a[i] = op
	}
	return a
}

// sameIntType returns n in the integer type of v (int64 unless v is an int32).
func sameIntType(v bson.RawValue, n int64) any {
	if v.Type == bson.TypeInt32 && n <= 1<<31-1 {
		return int32(n) //nolint:gosec // G115: n is within the int32 range here.
	}
	return n
}

// rebuild copies the elements of doc in order through edit, which returns each
// element's new value (a bson.RawValue to keep it as it is) and whether to keep it.
func rebuild(doc bson.Raw, edit func(key string, v bson.RawValue) (any, bool, error)) (bson.D, error) {
	elems, err := doc.Elements()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	out := make(bson.D, 0, len(elems))
	for _, e := range elems {
		v, keep, err := edit(e.Key(), e.Value())
		if err != nil {
			return nil, err
		}
		if keep {
			out = append(out, bson.E{Key: e.Key(), Value: v})
		}
	}
	return out, nil
}
