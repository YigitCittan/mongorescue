package mongoconn

import (
	"context"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/oplog"
	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// CountsAt returns the exact document count of each collection of namespaces
// ("<database>.<collection>") at oplog position at, a majority-committed write of
// the replica set at uri (a PITR base's T_after), read on the primary, and which of
// them did not exist there (ExactCounts.Absent).
//
// It counts with a snapshot read at that position (readConcern snapshot with
// atClusterTime, MongoDB 5.0 and later). Whether a collection existed at at follows
// from whether it exists now and the oplog commands since at: a collection created,
// dropped or renamed since (or whose database was dropped) is unverified, which
// also covers snapshot reads before 7.0 resolving a name in today's catalog. When
// the snapshot read of a collection fails, for instance because the server no
// longer keeps the history back to at or majority reads are disabled, its count is
// derived from the oplog instead (see OplogCountsAt). A collection whose count
// neither method gives is unverified, with the reason. It returns an error only
// when ctx ends or no session can be opened. Errors and reasons are redacted.
func (p *Prober) CountsAt(ctx context.Context, uri string, namespaces []string, at pitr.Timestamp) (*models.ExactCounts, error) {
	s, err := p.OpenOplogSession(ctx, uri, readpref.PrimaryMode.String())
	if err != nil {
		return nil, err
	}
	defer s.Close()
	out := newExactCounts(at, models.CountMethodSnapshot)
	var failed []string
	var snapErr error
	for _, ns := range namespaces {
		n, countErr := snapshotCount(ctx, s.client, ns, at)
		if countErr != nil {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
			failed = append(failed, ns)
			if snapErr == nil {
				snapErr = countErr
			}
			continue
		}
		out.Counts[ns] = n
	}
	if len(out.Counts) > 0 {
		if err = s.settleSnapshotCounts(ctx, out); err != nil {
			return nil, err
		}
	}
	if len(failed) == 0 {
		return out, nil
	}
	derived, err := s.oplogCounts(ctx, failed, at)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		reason := fmt.Sprintf("no count at the consistent point: the snapshot read failed (%v) and so did the oplog (%v)", redactErr(snapErr), redactErr(err))
		for _, ns := range failed {
			out.Unverified[ns] = reason
		}
		return out, nil
	}
	out.Method = models.CountMethodOplog
	if len(out.Counts) > 0 {
		out.Method = models.CountMethodMixed
	}
	for ns, n := range derived.Counts {
		out.Counts[ns] = n
	}
	for ns, why := range derived.Unverified {
		out.Unverified[ns] = why
	}
	for ns := range derived.Absent {
		out.Absent[ns] = true
	}
	return out, nil
}

// newExactCounts returns empty counts at at taken by method.
func newExactCounts(at pitr.Timestamp, method models.CountMethod) *models.ExactCounts {
	return &models.ExactCounts{At: at, Method: method, Counts: map[string]int64{}, Unverified: map[string]string{}, Absent: map[string]bool{}}
}

// settleSnapshotCounts keeps the snapshot counts of out whose collection's
// existence at out.At is known: it lists the collections now, then reads the oplog
// commands since out.At. A collection without such a command existed at out.At
// exactly when it exists now; one with a command is unverified, as is every count
// when the listing or the oplog cannot be read. It returns an error only when ctx
// ends.
func (s *OplogSession) settleSnapshotCounts(ctx context.Context, out *models.ExactCounts) error {
	names := make([]string, 0, len(out.Counts))
	for ns := range out.Counts {
		names = append(names, ns)
	}
	// Listed before the commands are read, so a change after the listing is seen.
	infos, listErrs := listNamespaces(ctx, s.client, names)
	ddl, err := s.commandsSince(ctx, out.At)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	for _, ns := range names {
		db, _, _ := strings.Cut(ns, ".")
		why := ""
		switch {
		case err != nil:
			why = fmt.Sprintf("the oplog since the consistent point could not be checked for created, dropped or renamed collections: %v", redactErr(err))
		case ddl.DDL(ns) != "":
			why = ddl.DDL(ns)
		case listErrs[db] != nil:
			why = fmt.Sprintf("the collections of %s could not be listed: %v", db, redactErr(listErrs[db]))
		}
		if why != "" {
			out.Unverified[ns] = why
			delete(out.Counts, ns)
			continue
		}
		if _, exists := infos[ns]; !exists {
			out.Absent[ns], out.Counts[ns] = true, 0
		}
	}
	return nil
}

// collectionInfo is what a count needs to know about an existing collection.
type collectionInfo struct {
	// Type is "collection", "view" or "timeseries".
	Type string
	// Capped reports a capped collection.
	Capped bool
}

// listNamespaces returns the collections of namespaces that exist now on the
// primary of c, and the error of each database whose collections could not be
// listed.
func listNamespaces(ctx context.Context, c *mongo.Client, namespaces []string) (map[string]collectionInfo, map[string]error) {
	byDB := map[string][]string{}
	for _, ns := range namespaces {
		if db, coll, ok := strings.Cut(ns, "."); ok {
			byDB[db] = append(byDB[db], coll)
		}
	}
	infos, errs := map[string]collectionInfo{}, map[string]error{}
	for db, colls := range byDB {
		cur, err := c.Database(db).RunCommandCursor(ctx, bson.D{
			{Key: "listCollections", Value: 1},
			{Key: "filter", Value: bson.D{{Key: "name", Value: bson.D{{Key: "$in", Value: colls}}}}},
		}, options.RunCmd().SetReadPreference(readpref.Primary()))
		if err != nil {
			errs[db] = fmt.Errorf("listCollections: %w", err)
			continue
		}
		var specs []struct {
			Name    string `bson:"name"`
			Type    string `bson:"type"`
			Options struct {
				Capped bool `bson:"capped"`
			} `bson:"options"`
		}
		if err = cur.All(ctx, &specs); err != nil {
			errs[db] = fmt.Errorf("listCollections: %w", err)
			continue
		}
		for _, sp := range specs {
			infos[db+"."+sp.Name] = collectionInfo{Type: sp.Type, Capped: sp.Options.Capped}
		}
	}
	return infos, errs
}

// commandsSince returns the command entries of the primary's oplog after at, up to
// its newest write, in a CountDelta. The oplog must still hold the entry at at.
func (s *OplogSession) commandsSince(ctx context.Context, at pitr.Timestamp) (*oplog.CountDelta, error) {
	h, err := s.hello(ctx, readpref.Primary())
	if err != nil {
		return nil, err
	}
	_, found, err := s.EntryAt(ctx, at)
	switch {
	case err != nil:
		return nil, err
	case !found:
		return nil, fmt.Errorf("the oplog no longer holds the entry at %s", at)
	}
	filter := bson.D{
		{Key: "ts", Value: bson.D{{Key: "$gt", Value: bson.Timestamp{T: at.T, I: at.I}}, {Key: "$lte", Value: h.LastWrite.OpTime.TS}}},
		{Key: "op", Value: "c"},
	}
	cur, err := s.oplog().Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "$natural", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("find oplog commands: %w", err)
	}
	defer func() { _ = cur.Close(context.WithoutCancel(ctx)) }()
	d := &oplog.CountDelta{}
	for cur.Next(ctx) {
		if err = d.Add(cur.Current); err != nil {
			return nil, err
		}
	}
	if err = cur.Err(); err != nil {
		return nil, fmt.Errorf("read oplog commands: %w", err)
	}
	return d, nil
}

// OplogCountsAt derives the document count of each collection of namespaces at
// oplog position at, a majority-committed write of the replica set at uri, from
// the oplog: it reads the fast (metadata) counts on the primary, bracketed by the
// primary's majority-committed and newest write, and subtracts what the oplog
// entries after at changed (see oplog.CountDelta). A collection written to while
// its count was read, created, dropped or renamed since at, a capped collection
// (its evictions write no oplog entry), a time-series collection or a view (its
// writes land on another namespace), and one whose count could not be read are
// unverified, with the reason. CountsAt uses it when a snapshot read is not
// possible. Errors are redacted.
func (p *Prober) OplogCountsAt(ctx context.Context, uri string, namespaces []string, at pitr.Timestamp) (*models.ExactCounts, error) {
	s, err := p.OpenOplogSession(ctx, uri, readpref.PrimaryMode.String())
	if err != nil {
		return nil, err
	}
	defer s.Close()
	out, err := s.oplogCounts(ctx, namespaces, at)
	return out, redactErr(err)
}

// snapshotCount counts the documents of collection ns with a snapshot read at at on
// the primary of c.
func snapshotCount(ctx context.Context, c *mongo.Client, ns string, at pitr.Timestamp) (int64, error) {
	db, coll, ok := strings.Cut(ns, ".")
	if !ok || db == "" || coll == "" {
		return 0, fmt.Errorf("invalid namespace %q", ns)
	}
	var res struct {
		Cursor struct {
			FirstBatch []struct {
				N int64 `bson:"n"`
			} `bson:"firstBatch"`
		} `bson:"cursor"`
	}
	cmd := bson.D{
		{Key: "aggregate", Value: coll},
		{Key: "pipeline", Value: bson.A{bson.D{{Key: "$count", Value: "n"}}}},
		{Key: "cursor", Value: bson.D{}},
		{Key: "readConcern", Value: bson.D{{Key: "level", Value: "snapshot"}, {Key: "atClusterTime", Value: bson.Timestamp{T: at.T, I: at.I}}}},
	}
	if err := c.Database(db).RunCommand(ctx, cmd, options.RunCmd().SetReadPreference(readpref.Primary())).Decode(&res); err != nil {
		return 0, fmt.Errorf("count %s at %s: %w", ns, at, err)
	}
	if len(res.Cursor.FirstBatch) == 0 {
		return 0, nil
	}
	return res.Cursor.FirstBatch[0].N, nil
}

// nowCounts are the collections' state read now by oplogCounts: their fast counts,
// the ones that do not exist, and the ones whose count cannot be derived (with the
// reason).
type nowCounts struct {
	counts map[string]int64
	absent map[string]bool
	skip   map[string]string
}

// oplogCounts derives the counts at at from the oplog (see OplogCountsAt).
func (s *OplogSession) oplogCounts(ctx context.Context, namespaces []string, at pitr.Timestamp) (*models.ExactCounts, error) {
	// Every write up to the majority-committed optime before the counts is in them
	// (no write before it is still in flight); one after the newest write after
	// them is not. A collection written to in between has no exact count.
	before, err := s.hello(ctx, readpref.Primary())
	if err != nil {
		return nil, err
	}
	now := s.readNow(ctx, namespaces)
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	after, err := s.hello(ctx, readpref.Primary())
	if err != nil {
		return nil, err
	}
	settled := before.LastWrite.MajorityOpTime.TS
	last := pitr.Timestamp{T: after.LastWrite.OpTime.TS.T, I: after.LastWrite.OpTime.TS.I}
	if last.Compare(at) < 0 {
		return nil, fmt.Errorf("the primary's newest write %s is before %s", last, at)
	}
	if (pitr.Timestamp{T: settled.T, I: settled.I}).Compare(at) < 0 {
		settled = bson.Timestamp{T: at.T, I: at.I}
	}
	delta := &oplog.CountDelta{TaintAfter: settled}
	if _, err = s.ReadOplog(ctx, pitr.OplogRange{From: at, To: last}, deltaWriter{delta}); err != nil {
		return nil, fmt.Errorf("read the oplog after %s: %w", at, err)
	}
	return deriveCounts(at, namespaces, now, delta), nil
}

// readNow lists namespaces and reads the fast count of each existing collection
// whose count the oplog can derive. A failure only concerns its collection (or
// database).
func (s *OplogSession) readNow(ctx context.Context, namespaces []string) nowCounts {
	now := nowCounts{counts: map[string]int64{}, absent: map[string]bool{}, skip: map[string]string{}}
	infos, listErrs := listNamespaces(ctx, s.client, namespaces)
	for _, ns := range namespaces {
		db, coll, ok := strings.Cut(ns, ".")
		if !ok || db == "" || coll == "" {
			now.skip[ns] = "invalid namespace"
			continue
		}
		if listErrs[db] != nil {
			now.skip[ns] = fmt.Sprintf("the collections of %s could not be listed: %v", db, redactErr(listErrs[db]))
			continue
		}
		info, exists := infos[ns]
		switch {
		case !exists:
			now.absent[ns] = true
			continue
		case info.Type == "timeseries":
			now.skip[ns] = "a time-series collection: its writes land on system.buckets, so the oplog cannot derive its count"
			continue
		case info.Type != "" && info.Type != "collection":
			now.skip[ns] = "a " + info.Type + ", not a collection"
			continue
		case info.Capped:
			now.skip[ns] = "a capped collection: the documents it evicts leave no oplog entry, so the oplog cannot derive its count"
			continue
		}
		n, err := s.client.Database(db).Collection(coll, options.Collection().SetReadPreference(readpref.Primary())).EstimatedDocumentCount(ctx)
		if err != nil {
			now.skip[ns] = fmt.Sprintf("its count could not be read: %v", redactErr(err))
			continue
		}
		now.counts[ns] = n
	}
	return now
}

// deltaWriter feeds the oplog entries written to it, one per Write as ReadOplog
// writes them, to a CountDelta.
type deltaWriter struct{ d *oplog.CountDelta }

// Write adds the entry p.
func (w deltaWriter) Write(p []byte) (int, error) {
	if err := w.d.Add(bson.Raw(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// deriveCounts returns the counts at at of namespaces: their counts read later
// (now) minus what the oplog entries since at changed (delta). A collection that
// does not exist now and saw no command since at did not exist at at either.
func deriveCounts(at pitr.Timestamp, namespaces []string, now nowCounts, delta *oplog.CountDelta) *models.ExactCounts {
	out := newExactCounts(at, models.CountMethodOplog)
	for _, ns := range namespaces {
		if why := now.skip[ns]; why != "" {
			out.Unverified[ns] = why
			continue
		}
		d, unknown := delta.Delta(ns)
		switch n := now.counts[ns] - d; {
		case unknown != "":
			out.Unverified[ns] = unknown
		case now.absent[ns] && d != 0:
			out.Unverified[ns] = fmt.Sprintf("it does not exist now, but the oplog since the consistent point changed its count by %+d", d)
		case now.absent[ns]:
			out.Counts[ns], out.Absent[ns] = 0, true
		case n < 0:
			out.Unverified[ns] = fmt.Sprintf("the count derived from the oplog is negative (%d documents now, %+d since)", now.counts[ns], d)
		default:
			out.Counts[ns] = n
		}
	}
	return out
}
