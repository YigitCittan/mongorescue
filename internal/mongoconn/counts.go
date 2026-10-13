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
// the replica set at uri (a PITR base's T_after), read on the primary.
//
// It counts with a snapshot read at that position (readConcern snapshot with
// atClusterTime, MongoDB 5.0 and later). Before 7.0 such a read finds a collection
// by its current name, so a collection dropped or renamed since at (an oplog
// command after it) is unverified. When the snapshot read of a collection fails,
// for instance because the server no longer keeps the history back to at or
// majority reads are disabled, its count is derived from the oplog instead (see
// OplogCountsAt). A collection whose count neither method gives is unverified, with
// the reason. It returns an error only when ctx ends or no session can be opened.
// Errors and reasons are redacted.
func (p *Prober) CountsAt(ctx context.Context, uri string, namespaces []string, at pitr.Timestamp) (*models.ExactCounts, error) {
	s, err := p.OpenOplogSession(ctx, uri, readpref.PrimaryMode.String())
	if err != nil {
		return nil, err
	}
	defer s.Close()
	out := &models.ExactCounts{At: at, Method: models.CountMethodSnapshot, Counts: map[string]int64{}, Unverified: map[string]string{}}
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
		if err = s.uncountRenamed(ctx, out); err != nil {
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
	return out, nil
}

// uncountRenamed moves the counts of collections created, dropped or renamed
// after out.At (or whose database was dropped since) to the unverified ones: a
// snapshot read before MongoDB 7.0 resolves a name in today's catalog, not in the
// one at out.At. When the oplog since out.At cannot be read, every count moves.
// It returns an error only when ctx ends.
func (s *OplogSession) uncountRenamed(ctx context.Context, out *models.ExactCounts) error {
	ddl, err := s.commandsSince(ctx, out.At)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		why := fmt.Sprintf("the oplog since the consistent point could not be checked for dropped or renamed collections: %v", redactErr(err))
		for ns := range out.Counts {
			out.Unverified[ns] = why
			delete(out.Counts, ns)
		}
		return nil
	}
	for ns := range out.Counts {
		if why := ddl.DDL(ns); why != "" {
			out.Unverified[ns] = why
			delete(out.Counts, ns)
		}
	}
	return nil
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
// its count was read, or created, dropped or renamed since at, is unverified, with
// the reason. CountsAt uses it when a snapshot read is not possible. Errors are
// redacted.
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

// oplogCounts derives the counts at at from the oplog (see OplogCountsAt).
func (s *OplogSession) oplogCounts(ctx context.Context, namespaces []string, at pitr.Timestamp) (*models.ExactCounts, error) {
	// Every write up to the majority-committed optime before the counts is in them
	// (no write before it is still in flight); one after the newest write after
	// them is not. A collection written to in between has no exact count.
	before, err := s.hello(ctx, readpref.Primary())
	if err != nil {
		return nil, err
	}
	fast := make(map[string]int64, len(namespaces))
	for _, ns := range namespaces {
		db, coll, ok := strings.Cut(ns, ".")
		if !ok || db == "" || coll == "" {
			return nil, fmt.Errorf("invalid namespace %q", ns)
		}
		n, countErr := s.client.Database(db).Collection(coll, options.Collection().SetReadPreference(readpref.Primary())).EstimatedDocumentCount(ctx)
		if countErr != nil {
			return nil, fmt.Errorf("count %s: %w", ns, countErr)
		}
		fast[ns] = n
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
	return deriveCounts(at, namespaces, fast, delta), nil
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
// (now) minus what the oplog entries since at changed (delta).
func deriveCounts(at pitr.Timestamp, namespaces []string, now map[string]int64, delta *oplog.CountDelta) *models.ExactCounts {
	out := &models.ExactCounts{At: at, Method: models.CountMethodOplog, Counts: map[string]int64{}, Unverified: map[string]string{}}
	for _, ns := range namespaces {
		d, unknown := delta.Delta(ns)
		switch n := now[ns] - d; {
		case unknown != "":
			out.Unverified[ns] = unknown
		case n < 0:
			out.Unverified[ns] = fmt.Sprintf("the count derived from the oplog is negative (%d documents now, %+d since)", now[ns], d)
		default:
			out.Counts[ns] = n
		}
	}
	return out
}
