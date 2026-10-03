package oplog

import (
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Views and time-series collections. The oplog records a view as writes to
// <db>.system.views and a time-series collection as its buckets collection
// (system.buckets.<name>) plus such a view. Replaying those writes through applyOps
// needs privileges no built-in role grants (root is refused), so the Filter replays
// the user-level commands instead:
//
//	create system.views                      dropped (created with the first view)
//	insert into system.views                 create <view> {viewOn, pipeline, collation}
//	update of system.views                   collMod <view> {viewOn, pipeline}
//	delete from system.views                 drop <view>
//	create system.buckets.<ts> {timeseries}  create <ts> {timeseries, expireAfterSeconds}
//	views on system.buckets.*                dropped (part of the time-series collection)
//
// Writes to the buckets collection itself, and collMod and drop of it, are replayed
// as they are: applyOps accepts them, and refuses collMod <ts> ("not supported on a
// view").

const (
	// viewsCollection holds a database's view definitions.
	viewsCollection = "system.views"
	// bucketsPrefix starts the buckets collection of a time-series collection.
	bucketsPrefix = "system.buckets."
)

// viewOp turns a write to db.system.views into the command it stands for, or nothing.
func (f *Filter) viewOp(doc bson.Raw, opType, db string) ([]bson.D, int64, error) {
	o, ok := doc.Lookup("o").DocumentOK()
	if !ok {
		return nil, 0, fmt.Errorf("%w: %s write without a document o", ErrMalformed, viewsCollection)
	}
	id, ok := o.Lookup("_id").StringValueOK()
	if !ok {
		id, ok = doc.Lookup("o2", "_id").StringValueOK()
	}
	name, found := strings.CutPrefix(id, db+".")
	if !ok || !found || name == "" {
		return nil, 0, fmt.Errorf("%w: view _id %q is not in database %s", ErrMalformed, id, db)
	}
	var cmd bson.D
	switch opType {
	case "d":
		cmd = bson.D{{Key: "drop", Value: name}}
	default:
		viewOn, ok1 := o.Lookup("viewOn").StringValueOK()
		// Kept as a RawValue: a bson.RawArray would be encoded as binary.
		pipeline := o.Lookup("pipeline")
		if !ok1 || pipeline.Type != bson.TypeArray {
			// An update that is not a replacement of the definition.
			return nil, 0, fmt.Errorf("%w: %s %s of view %s without viewOn and pipeline", ErrUnknownCommand, viewsCollection, opType, name)
		}
		if strings.HasPrefix(viewOn, bucketsPrefix) {
			// The view of a time-series collection comes with its create and collMod.
			return nil, 0, nil
		}
		verb := "create"
		if opType == "u" {
			verb = "collMod"
		}
		cmd = bson.D{{Key: verb, Value: name}, {Key: "viewOn", Value: viewOn}, {Key: "pipeline", Value: pipeline}}
		if collation, err := o.LookupErr("collation"); err == nil && opType == "i" {
			cmd = append(cmd, bson.E{Key: "collation", Value: collation})
		}
	}
	d, err := f.asCommand(doc, db, cmd)
	if err != nil {
		return nil, 0, err
	}
	return []bson.D{d}, 1, nil
}

// timeseriesCreate turns the create of a time-series buckets collection into the
// create of the time-series collection. ok is false for other creates.
func timeseriesCreate(o bson.Raw) (cmd bson.D, ok bool, err error) {
	coll, _ := o.Lookup("create").StringValueOK()
	name, isBuckets := strings.CutPrefix(coll, bucketsPrefix)
	ts, hasTS := o.Lookup("timeseries").DocumentOK()
	if !isBuckets || !hasTS || name == "" {
		return nil, false, nil
	}
	_, hasGranularity := ts.Lookup("granularity").StringValueOK()
	options, err := rebuild(ts, func(k string, v bson.RawValue) (any, bool, error) {
		// The server derives the bucket span from the granularity and refuses both.
		if hasGranularity && (k == "bucketMaxSpanSeconds" || k == "bucketRoundingSeconds") {
			return nil, false, nil
		}
		return v, true, nil
	})
	if err != nil {
		return nil, false, err
	}
	cmd = bson.D{{Key: "create", Value: name}, {Key: "timeseries", Value: options}}
	if exp, err := o.LookupErr("expireAfterSeconds"); err == nil {
		cmd = append(cmd, bson.E{Key: "expireAfterSeconds", Value: exp})
	}
	return cmd, true, nil
}

// asCommand builds a command entry on db, renamed, from entry (see commandEntry).
func (f *Filter) asCommand(entry bson.Raw, db string, cmd bson.D) (bson.D, error) {
	to, err := f.rename(db)
	if err != nil {
		return nil, err
	}
	return f.commandEntry(entry, to, cmd)
}

// commandEntry builds a command entry on database to (as written) from entry, with o
// replaced by cmd: op becomes "c", ns <to>.$cmd, o2 and the stripped fields go, ts,
// t, v and wall stay.
func (f *Filter) commandEntry(entry bson.Raw, to string, cmd bson.D) (bson.D, error) {
	return rebuild(entry, func(key string, v bson.RawValue) (any, bool, error) {
		switch {
		case strippedFields[key] || key == "o2":
			return nil, false, nil
		case key == "op":
			return "c", true, nil
		case key == "ns":
			return to + ".$cmd", true, nil
		case key == "o":
			return cmd, true, nil
		}
		return v, true, nil
	})
}
