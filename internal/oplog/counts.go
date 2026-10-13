package oplog

import (
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// CountDelta sums, per namespace ("<database>.<collection>"), how the document
// count changed over a run of oplog entries: inserts minus deletes, including the
// operations nested in applyOps and those of transactions, which count at their
// commit. Given the count of a collection at one end of the run, the count at the
// other end follows from it.
//
// A namespace whose count cannot be derived is unknown, with a reason: one that was
// created, dropped, renamed, emptied or converted to capped in the run, every
// collection of a database dropped in it or hit by a command CountDelta does not
// know, and everything when a transaction committed in the run began before it (its
// earlier entries, and so the namespaces it wrote, were not seen). Entries after
// TaintAfter make the namespaces they change unknown instead of counting.
//
// The oplog does not hold every change of every count: a full capped collection
// evicts documents without a delete entry, and a time-series collection's writes
// land on its system.buckets namespace (bucket documents, not measurements).
// Callers must not derive the counts of such collections (internal/mongoconn marks
// them unverified).
//
// Feed the entries in oplog order with Add, then read the result with Delta.
type CountDelta struct {
	// TaintAfter, when set, ends the counted part of the run: an entry after it
	// (a transaction by its commit) makes the namespaces whose count it changes
	// unknown. Callers set it to the position up to which counts read during the
	// run are known to be settled.
	TaintAfter bson.Timestamp

	delta   map[string]int64
	unknown map[string]string // namespace → reason
	ddl     map[string]string // namespace → DDL that made it unknown
	dbs     map[string]string // database → reason
	all     string
	txns    map[string]*countTxn
}

// countTxn holds the operations of a transaction until it commits.
type countTxn struct {
	ops []countOp
	// incomplete is set when an entry of the transaction before the first one seen
	// exists (prevOpTime): its operations are unknown.
	incomplete bool
}

// countOp is a count-changing operation: n documents added to ns, or, with what
// set, a change that makes ns unknown (the whole database ns names when db is set).
type countOp struct {
	ns   string
	n    int64
	what string
	db   bool
}

// countNeutral are commands that change no document count.
var countNeutral = map[string]bool{
	"collMod": true, "createIndexes": true, "dropIndexes": true, "dropIndex": true,
	"deleteIndexes": true, "deleteIndex": true, "commitIndexBuild": true,
	"startIndexBuild": true, "abortIndexBuild": true, "dbCheck": true,
}

// Add accounts for one oplog entry.
func (d *CountDelta) Add(entry bson.Raw) error {
	if err := entry.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	t, i, ok := entry.Lookup("ts").TimestampOK()
	if !ok {
		return fmt.Errorf("%w: no timestamp field ts", ErrMalformed)
	}
	at := bson.Timestamp{T: t, I: i}
	if id, ok := txnID(entry); ok {
		return d.transaction(entry, id, at)
	}
	ops, err := countOps(entry, 0)
	if err != nil {
		return err
	}
	d.apply(ops, at)
	return nil
}

// Delta returns how the count of namespace ns changed over the entries added, or
// the reason it cannot be derived (n is 0 then).
func (d *CountDelta) Delta(ns string) (n int64, unknown string) {
	if d.all != "" {
		return 0, d.all
	}
	db, _, _ := strings.Cut(ns, ".")
	if r := d.dbs[db]; r != "" {
		return 0, r
	}
	if r := d.unknown[ns]; r != "" {
		return 0, r
	}
	return d.delta[ns], ""
}

// DDL returns why the entries added created, dropped, renamed, emptied or
// converted to capped namespace ns (or dropped or ran an unknown command on its
// database), or "" when none did. Unlike Delta it ignores what was counted, what
// TaintAfter tainted and transactions whose start was not seen (a transaction
// cannot drop or rename a collection).
func (d *CountDelta) DDL(ns string) string {
	db, _, _ := strings.Cut(ns, ".")
	if r := d.dbs[db]; r != "" {
		return r
	}
	return d.ddl[ns]
}

// transaction accounts for an entry of transaction id at position at.
func (d *CountDelta) transaction(entry bson.Raw, id string, at bson.Timestamp) error {
	if d.txns == nil {
		d.txns = map[string]*countTxn{}
	}
	st := d.txns[id]
	switch commandName(entry) {
	case "abortTransaction":
		delete(d.txns, id)
		return nil
	case "commitTransaction":
		// A prepared transaction commits here; its operations were prepared earlier.
		delete(d.txns, id)
		d.commit(st == nil || st.incomplete, st, at)
		return nil
	}
	o, _ := entry.Lookup("o").DocumentOK()
	ops, err := nestedCountOps(o, 0)
	if err != nil {
		return err
	}
	if st == nil {
		st = &countTxn{incomplete: hasPrevOpTime(entry)}
		d.txns[id] = st
	}
	st.ops = append(st.ops, ops...)
	if isTrue(o, "partialTxn") || isTrue(o, "prepare") {
		return nil
	}
	delete(d.txns, id)
	d.commit(st.incomplete, st, at)
	return nil
}

// commit applies the operations of transaction st, committed at at; missing says
// that some of its entries were not seen.
func (d *CountDelta) commit(missing bool, st *countTxn, at bson.Timestamp) {
	if missing {
		if d.all == "" {
			d.all = fmt.Sprintf("a transaction committed at %s began before the oplog entries read", tsString(at))
		}
		return
	}
	d.apply(st.ops, at)
}

// apply accounts for ops, visible from position at.
func (d *CountDelta) apply(ops []countOp, at bson.Timestamp) {
	taint := d.TaintAfter != (bson.Timestamp{}) && Before(d.TaintAfter, at)
	for _, op := range ops {
		switch {
		case op.what != "" && op.db:
			setFirst(&d.dbs, op.ns, fmt.Sprintf("database %s: %s at %s", op.ns, op.what, tsString(at)))
		case op.what != "":
			why := fmt.Sprintf("%s at %s", op.what, tsString(at))
			setFirst(&d.unknown, op.ns, why)
			setFirst(&d.ddl, op.ns, why)
		case taint:
			setFirst(&d.unknown, op.ns, fmt.Sprintf("written at %s, while the counts were read", tsString(at)))
		default:
			if d.delta == nil {
				d.delta = map[string]int64{}
			}
			d.delta[op.ns] += op.n
		}
	}
}

// setFirst sets (*m)[key] to reason unless it holds one already.
func setFirst(m *map[string]string, key, reason string) {
	if *m == nil {
		*m = map[string]string{}
	}
	if _, ok := (*m)[key]; !ok {
		(*m)[key] = reason
	}
}

// countOps returns the count-changing operations of one operation, a top-level
// entry (depth 0) or one nested in applyOps.
func countOps(doc bson.Raw, depth int) ([]countOp, error) {
	opType, ok := doc.Lookup("op").StringValueOK()
	if !ok {
		return nil, fmt.Errorf("%w: no operation type op", ErrMalformed)
	}
	if ignoredOps[opType] || opType == "u" {
		return nil, nil
	}
	ns, ok := doc.Lookup("ns").StringValueOK()
	if !ok {
		return nil, fmt.Errorf("%w: no namespace ns", ErrMalformed)
	}
	db, _, ok := strings.Cut(ns, ".")
	if !ok || db == "" {
		return nil, fmt.Errorf("%w: namespace %q has no database", ErrMalformed, ns)
	}
	switch opType {
	case "i":
		return []countOp{{ns: ns, n: 1}}, nil
	case "d":
		return []countOp{{ns: ns, n: -1}}, nil
	case "c":
		return commandCountOps(doc, db, depth)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownOp, opType)
	}
}

// commandCountOps returns the count-changing operations of a command entry on db.
func commandCountOps(doc bson.Raw, db string, depth int) ([]countOp, error) {
	o, ok := doc.Lookup("o").DocumentOK()
	if !ok {
		return nil, fmt.Errorf("%w: command without a document o", ErrMalformed)
	}
	first, err := o.IndexErr(0)
	if err != nil {
		return nil, fmt.Errorf("%w: empty command", ErrMalformed)
	}
	name := first.Key()
	switch name {
	case "applyOps":
		return nestedCountOps(o, depth)
	case "renameCollection":
		from, ok1 := o.Lookup("renameCollection").StringValueOK()
		to, ok2 := o.Lookup("to").StringValueOK()
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("%w: renameCollection without source and target namespaces", ErrMalformed)
		}
		return []countOp{{ns: from, what: "renamed to " + to}, {ns: to, what: "renamed from " + from}}, nil
	case "create", "drop", "convertToCapped", "emptycapped":
		coll, ok := first.Value().StringValueOK()
		if !ok {
			return nil, fmt.Errorf("%w: %s without a collection name", ErrMalformed, name)
		}
		return []countOp{{ns: db + "." + coll, what: ddlWhat[name]}}, nil
	case "dropDatabase":
		return []countOp{{ns: db, what: "dropped", db: true}}, nil
	}
	if countNeutral[name] {
		return nil, nil
	}
	return []countOp{{ns: db, what: "command " + name, db: true}}, nil
}

// ddlWhat describes the DDL commands that make a collection's count unknown.
var ddlWhat = map[string]string{
	"create": "created", "drop": "dropped", "convertToCapped": "converted to capped", "emptycapped": "emptied",
}

// nestedCountOps returns the count-changing operations of an applyOps command
// document o.
func nestedCountOps(o bson.Raw, depth int) ([]countOp, error) {
	if depth >= maxApplyOpsDepth {
		return nil, fmt.Errorf("%w: applyOps nested more than %d levels", ErrMalformed, maxApplyOpsDepth)
	}
	arr, ok := o.Lookup("applyOps").ArrayOK()
	if !ok {
		return nil, fmt.Errorf("%w: applyOps is not an array", ErrMalformed)
	}
	values, err := arr.Values()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	var out []countOp
	for _, v := range values {
		doc, ok := v.DocumentOK()
		if !ok {
			return nil, fmt.Errorf("%w: applyOps operation is not a document", ErrMalformed)
		}
		ops, err := countOps(doc, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, ops...)
	}
	return out, nil
}

// hasPrevOpTime reports whether a transaction entry links to an earlier entry of
// its transaction (prevOpTime.ts is set).
func hasPrevOpTime(entry bson.Raw) bool {
	t, i, ok := entry.Lookup("prevOpTime", "ts").TimestampOK()
	return ok && (t != 0 || i != 0)
}

// tsString formats an oplog timestamp like the shell.
func tsString(ts bson.Timestamp) string {
	return fmt.Sprintf("Timestamp(%d, %d)", ts.T, ts.I)
}
