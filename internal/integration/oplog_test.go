//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/yigitcittan/mongorescue/internal/mongotools"
	"github.com/yigitcittan/mongorescue/internal/oplog"
)

// requireReplicaSet skips the test unless the server is a replica set member, the
// only topology with an oplog.
func requireReplicaSet(t *testing.T, m *mongoEnv) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	var hello struct {
		SetName string `bson:"setName"`
	}
	if err := m.Client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if hello.SetName == "" {
		t.Skip("not a replica set (MONGO_TOPOLOGY=replset); the oplog tests need one")
	}
}

// archiveOptions returns the options of a synthetic archive of entries from the
// server under test.
func (m *mongoEnv) archiveOptions(t *testing.T) oplog.ArchiveOptions {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	var info struct {
		Version string `bson:"version"`
	}
	if err := m.Client.Database("admin").RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&info); err != nil {
		t.Fatalf("buildInfo: %v", err)
	}
	return oplog.ArchiveOptions{ServerVersion: info.Version}
}

// lastOplogTS returns the timestamp of the newest oplog entry.
func (m *mongoEnv) lastOplogTS(t *testing.T) bson.Timestamp {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	raw, err := m.Client.Database("local").Collection("oplog.rs").FindOne(ctx, bson.D{},
		options.FindOne().SetSort(bson.D{{Key: "$natural", Value: -1}})).Raw()
	if err != nil {
		t.Fatalf("newest oplog entry: %v", err)
	}
	secs, inc, ok := raw.Lookup("ts").TimestampOK()
	if !ok {
		t.Fatal("newest oplog entry has no ts")
	}
	return bson.Timestamp{T: secs, I: inc}
}

// readOplog returns the oplog entries after from, as the collector reads them.
func (m *mongoEnv) readOplog(t *testing.T, from bson.Timestamp) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	cur, err := m.Client.Database("local").Collection("oplog.rs").Find(ctx,
		bson.D{{Key: "ts", Value: bson.D{{Key: "$gt", Value: from}}}},
		options.Find().SetSort(bson.D{{Key: "$natural", Value: 1}}))
	if err != nil {
		t.Fatalf("read oplog: %v", err)
	}
	defer func() { _ = cur.Close(ctx) }()
	var buf bytes.Buffer
	for cur.Next(ctx) {
		buf.Write(cur.Current)
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("read oplog: %v", err)
	}
	return buf.Bytes()
}

// runTool runs a database tool with the URI passed through a --config file. When
// feed is not nil it writes the tool's stdin and closes it. It returns stderr.
func (m *mongoEnv) runTool(t *testing.T, name string, feed func(stdin io.Writer) error, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	configArg, cleanup, err := mongotools.WriteURIConfig(t.TempDir(), m.URI)
	if err != nil {
		t.Fatalf("tools config: %v", err)
	}
	defer cleanup()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, path, append([]string{configArg}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	var stdin io.WriteCloser
	if feed != nil {
		if stdin, err = cmd.StdinPipe(); err != nil {
			t.Fatal(err)
		}
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	var feedErr error
	if feed != nil {
		feedErr = feed(stdin)
		if err := stdin.Close(); err != nil && feedErr == nil {
			feedErr = err
		}
	}
	waitErr := cmd.Wait()
	out := stderr.String()
	assertNoSecret(t, m.Password, name+" stderr", out)
	if feedErr != nil || waitErr != nil {
		t.Fatalf("%s: feed %v, exit %v\n%s", name, feedErr, waitErr, out)
	}
	return out
}

// appliedRE matches mongorestore's count of replayed operations.
var appliedRE = regexp.MustCompile(`applied (\d+) oplog entries`)

// applied returns the count in mongorestore's "applied N oplog entries".
func applied(t *testing.T, stderr string) int64 {
	t.Helper()
	m := appliedRE.FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("no \"applied N oplog entries\" in mongorestore output:\n%s", stderr)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// limitArg formats ts as an --oplogLimit value.
func limitArg(ts bson.Timestamp) string { return fmt.Sprintf("--oplogLimit=%d:%d", ts.T, ts.I) }

// dbState is the contents of a database: every collection's documents in _id order
// and index names, and every view's definition.
type dbState struct {
	Collections map[string][]bson.Raw
	Indexes     map[string][]string
	Views       map[string]string
}

// state captures the contents of db.
func (m *mongoEnv) state(t *testing.T, db string) dbState {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	s := dbState{Collections: map[string][]bson.Raw{}, Indexes: map[string][]string{}, Views: map[string]string{}}
	specs, err := m.Client.Database(db).ListCollectionSpecifications(ctx, bson.D{})
	if err != nil {
		t.Fatalf("list collections of %s: %v", db, err)
	}
	for _, spec := range specs {
		if strings.HasPrefix(spec.Name, "system.") {
			continue
		}
		if spec.Type == "view" {
			s.Views[spec.Name] = spec.Options.String()
			continue
		}
		coll := m.Client.Database(db).Collection(spec.Name)
		cur, err := coll.Find(ctx, bson.D{}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
		if err != nil {
			t.Fatalf("read %s.%s: %v", db, spec.Name, err)
		}
		docs := []bson.Raw{}
		for cur.Next(ctx) {
			docs = append(docs, append(bson.Raw(nil), cur.Current...))
		}
		if err = cur.Err(); err != nil {
			t.Fatal(err)
		}
		_ = cur.Close(ctx)
		s.Collections[spec.Name] = docs
		idx, err := coll.Indexes().ListSpecifications(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range idx {
			s.Indexes[spec.Name] = append(s.Indexes[spec.Name], i.Name)
		}
		slices.Sort(s.Indexes[spec.Name])
	}
	return s
}

// assertSameState fails the test unless got equals want.
func assertSameState(t *testing.T, what string, got, want dbState) {
	t.Helper()
	names := func(m map[string][]bson.Raw) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		slices.Sort(out)
		return out
	}
	if g, w := names(got.Collections), names(want.Collections); !slices.Equal(g, w) {
		t.Fatalf("%s: collections %v, want %v", what, g, w)
	}
	for name, docs := range want.Collections {
		g := got.Collections[name]
		if len(g) != len(docs) {
			t.Fatalf("%s: %s has %d documents, want %d", what, name, len(g), len(docs))
		}
		for i := range docs {
			if !bytes.Equal(g[i], docs[i]) {
				t.Fatalf("%s: %s document %d is %s, want %s", what, name, i, g[i], docs[i])
			}
		}
		if !slices.Equal(got.Indexes[name], want.Indexes[name]) {
			t.Errorf("%s: %s indexes %v, want %v", what, name, got.Indexes[name], want.Indexes[name])
		}
	}
	if len(got.Views) != len(want.Views) {
		t.Errorf("%s: views %v, want %v", what, got.Views, want.Views)
	}
	for name, def := range want.Views {
		if got.Views[name] != def {
			t.Errorf("%s: view %s is %s, want %s", what, name, got.Views[name], def)
		}
	}
}

// TestOplogSyntheticArchiveReplay replays a synthetic oplog-only archive, written by
// oplog.ArchiveWriter, with mongorestore --archive --oplogReplay from stdin, with and
// without an --oplogLimit the archive ends before.
func TestOplogSyntheticArchiveReplay(t *testing.T) {
	env := requireMongo(t)
	requireReplicaSet(t, env)
	opts := env.archiveOptions(t)
	db := env.uniqueDB(t, "oplogar")
	at := env.lastOplogTS(t)
	next := func() bson.Timestamp {
		at.I++
		return at
	}
	entry := func(op string, o bson.D, o2 bson.D) bson.Raw {
		ns := db + ".items"
		if op == "c" {
			ns = db + ".$cmd"
		}
		d := bson.D{{Key: "op", Value: op}, {Key: "ns", Value: ns}, {Key: "o", Value: o}}
		if o2 != nil {
			d = append(d, bson.E{Key: "o2", Value: o2})
		}
		d = append(d, bson.E{Key: "ts", Value: next()}, bson.E{Key: "t", Value: int64(1)}, bson.E{Key: "v", Value: int64(2)})
		b, err := bson.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	entries := []bson.Raw{entry("c", bson.D{{Key: "create", Value: "items"}}, nil)}
	for i := 0; i < 10; i++ {
		entries = append(entries, entry("i", bson.D{{Key: "_id", Value: i}, {Key: "n", Value: i}}, nil))
	}
	entries = append(entries,
		entry("u", bson.D{{Key: "$v", Value: 2}, {Key: "diff", Value: bson.D{{Key: "u", Value: bson.D{{Key: "n", Value: 100}}}}}}, bson.D{{Key: "_id", Value: 3}}),
		entry("d", bson.D{{Key: "_id", Value: 9}}, nil),
	)
	limit := next()
	feed := func(stdin io.Writer) error {
		a, err := oplog.NewArchiveWriter(stdin, opts)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := a.WriteEntry(e); err != nil {
				return err
			}
		}
		return a.Close()
	}

	for _, args := range [][]string{{"--archive", "--oplogReplay"}, {"--archive", "--oplogReplay", limitArg(limit)}} {
		stderr := env.runTool(t, "mongorestore", feed, args...)
		if got := applied(t, stderr); got != int64(len(entries)) {
			t.Errorf("%v: applied %d, want %d", args, got, len(entries))
		}
		if n := env.count(t, db, "items"); n != 9 {
			t.Fatalf("%v: %d documents, want 9", args, n)
		}
		var doc struct{ N int }
		if err := env.Client.Database(db).Collection("items").FindOne(context.Background(), bson.D{{Key: "_id", Value: 3}}).Decode(&doc); err != nil || doc.N != 100 {
			t.Fatalf("%v: updated document n=%d err=%v", args, doc.N, err)
		}
	}

	// An empty archive replays nothing.
	stderr := env.runTool(t, "mongorestore", func(stdin io.Writer) error {
		a, err := oplog.NewArchiveWriter(stdin, opts)
		if err != nil {
			return err
		}
		return a.Close()
	}, "--archive", "--oplogReplay")
	if got := applied(t, stderr); got != 0 {
		t.Errorf("empty archive: applied %d", got)
	}
}

// TestOplogFilterReplaysIntoSafeClone is a point-in-time restore of one database into
// a safe clone: a base backup (mongodump --oplog), then writes, a rename, an index
// build, a drop, a transaction, a rename from another database and a view; the oplog
// since the base, filtered and renamed to <db>_rescue, is replayed through a synthetic
// archive onto the restored base. The clone must equal the source, and the source and
// the other database must be untouched by the replay.
func TestOplogFilterReplaysIntoSafeClone(t *testing.T) {
	env := requireMongo(t)
	requireReplicaSet(t, env)
	opts := env.archiveOptions(t)
	src := env.uniqueDB(t, "pitr")
	other := src + "_other"
	clone := src + "_rescue"
	ctx := context.Background()
	sdb, odb := env.Client.Database(src), env.Client.Database(other)

	env.seed(t, src, "orders", 50)
	env.seed(t, src, "customers", 10)
	env.seed(t, other, "moved", 5)
	env.seed(t, other, "keep", 3)

	// The base backup.
	before := env.lastOplogTS(t)
	base := filepath.Join(t.TempDir(), "base.archive")
	env.runTool(t, "mongodump", nil, "--oplog", "--archive="+base)

	// Changes after the base.
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	orders := sdb.Collection("orders")
	var more []any
	for i := 50; i < 60; i++ {
		more = append(more, bson.D{{Key: "seq", Value: i}, {Key: "sku", Value: fmt.Sprintf("SKU-%05d", i)}})
	}
	_, err := orders.InsertMany(ctx, more)
	must("insert", err)
	_, err = orders.UpdateMany(ctx, bson.D{{Key: "seq", Value: bson.D{{Key: "$lt", Value: 5}}}}, bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "paid"}}}})
	must("update", err)
	_, err = orders.DeleteOne(ctx, bson.D{{Key: "seq", Value: 7}})
	must("delete", err)
	must("rename", env.Client.Database("admin").RunCommand(ctx, bson.D{{Key: "renameCollection", Value: src + ".customers"}, {Key: "to", Value: src + ".clients"}}).Err())
	_, err = orders.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "sku", Value: 1}}},
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "seq", Value: -1}}, Options: options.Index().SetName("status_seq").SetSparse(true)},
	})
	must("index build", err)
	_, err = sdb.Collection("scratch").InsertOne(ctx, bson.D{{Key: "x", Value: 1}})
	must("scratch", err)
	must("drop", sdb.Collection("scratch").Drop(ctx))
	sess, err := env.Client.StartSession()
	must("session", err)
	_, err = sess.WithTransaction(ctx, func(ctx context.Context) (any, error) {
		if _, ierr := orders.InsertOne(ctx, bson.D{{Key: "seq", Value: 1000}, {Key: "txn", Value: true}}); ierr != nil {
			return nil, ierr
		}
		if _, uerr := sdb.Collection("clients").UpdateOne(ctx, bson.D{{Key: "seq", Value: 1}}, bson.D{{Key: "$set", Value: bson.D{{Key: "txn", Value: true}}}}); uerr != nil {
			return nil, uerr
		}
		return odb.Collection("keep").InsertOne(ctx, bson.D{{Key: "seq", Value: 1000}})
	})
	sess.EndSession(ctx)
	must("transaction", err)
	must("cross-database rename", env.Client.Database("admin").RunCommand(ctx, bson.D{{Key: "renameCollection", Value: other + ".moved"}, {Key: "to", Value: src + ".moved"}}).Err())
	must("view", sdb.CreateView(ctx, "v_paid", "orders", bson.A{bson.D{{Key: "$match", Value: bson.D{{Key: "status", Value: "paid"}}}}}))
	must("collMod view", sdb.RunCommand(ctx, bson.D{{Key: "collMod", Value: "v_paid"}, {Key: "viewOn", Value: "orders"}, {Key: "pipeline", Value: bson.A{bson.D{{Key: "$match", Value: bson.D{{Key: "txn", Value: true}}}}}}}).Err())
	must("view to drop", sdb.CreateView(ctx, "v_gone", "orders", bson.A{}))
	must("drop view", sdb.Collection("v_gone").Drop(ctx))
	tsOpts := options.TimeSeries().SetTimeField("at").SetMetaField("meta").SetGranularity("minutes")
	must("time series", sdb.CreateCollection(ctx, "metrics", options.CreateCollection().SetTimeSeriesOptions(tsOpts).SetExpireAfterSeconds(86400*365*10)))
	var points []any
	for i := 0; i < 20; i++ {
		points = append(points, bson.D{{Key: "at", Value: time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC)}, {Key: "meta", Value: bson.D{{Key: "host", Value: i % 3}}}, {Key: "v", Value: i}})
	}
	_, err = sdb.Collection("metrics").InsertMany(ctx, points)
	must("time-series writes", err)
	must("time-series collMod", sdb.RunCommand(ctx, bson.D{{Key: "collMod", Value: "metrics"}, {Key: "expireAfterSeconds", Value: 86400 * 365 * 20}}).Err())
	must("time series to drop", sdb.CreateCollection(ctx, "tsgone", options.CreateCollection().SetTimeSeriesOptions(options.TimeSeries().SetTimeField("at"))))
	_, err = sdb.Collection("tsgone").InsertOne(ctx, bson.D{{Key: "at", Value: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}})
	must("time-series write to drop", err)
	must("drop time series", sdb.Collection("tsgone").Drop(ctx))
	// An index build followed by a rename: the index must follow the collection.
	staging := sdb.Collection("staging")
	_, err = staging.InsertMany(ctx, more[:5])
	must("staging", err)
	_, err = staging.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "sku", Value: -1}}})
	must("staging index", err)
	must("rename after index build", env.Client.Database("admin").RunCommand(ctx, bson.D{{Key: "renameCollection", Value: src + ".staging"}, {Key: "to", Value: src + ".final"}}).Err())
	_, err = odb.Collection("keep").InsertOne(ctx, bson.D{{Key: "seq", Value: 2000}})
	must("write to the other database", err)
	end := env.lastOplogTS(t)

	wantSrc := env.state(t, src)
	wantOther := env.state(t, other)

	// Pass 1: the base, restored into the clone.
	env.runTool(t, "mongorestore", nil, "--archive="+base, "--nsInclude="+src+".*", "--nsFrom="+src+".*", "--nsTo="+clone+".*")

	// Pass 2: the filtered oplog through a synthetic archive.
	limit := bson.Timestamp{T: end.T, I: end.I + 1}
	filter := &oplog.Filter{
		Select: map[string]bool{src: true},
		Rename: func(string) string { return clone },
		Limit:  limit,
	}
	chunk := env.readOplog(t, before)
	stderr := env.runTool(t, "mongorestore", func(stdin io.Writer) error {
		a, err := oplog.NewArchiveWriter(stdin, opts)
		if err != nil {
			return err
		}
		if err := filter.Copy(ctx, a, bytes.NewReader(chunk)); err != nil {
			return err
		}
		return a.Close()
	}, "--archive", "--oplogReplay", limitArg(limit))

	assertSameState(t, "clone", env.state(t, clone), wantSrc)
	assertSameState(t, "source after the replay", env.state(t, src), wantSrc)
	assertSameState(t, "other database after the replay", env.state(t, other), wantOther)
	if got := applied(t, stderr); got != filter.Ops() {
		t.Errorf("mongorestore applied %d operations, the filter counted %d", got, filter.Ops())
	}
	// The oplog was read after pass 1, whose writes into the clone come after the
	// limit: the filter must have stopped there, or the clone would not match.
	if !filter.LimitReached() {
		t.Error("the filter did not stop at the limit")
	}
}
