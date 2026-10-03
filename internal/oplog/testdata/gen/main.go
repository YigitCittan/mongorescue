//go:build integration

// Command gen writes the oplog fixtures of internal/oplog: it runs one scenario per
// kind of oplog entry (CRUD, DDL, transactions, index builds, time series and a
// cross-database rename) against a single-node replica set and saves the entries each
// scenario produced as concatenated BSON in <out>/<scenario>.bson.
//
// Start the server with a small transaction entry limit, so multi-entry
// transactions (partialTxn) appear without megabytes of data:
//
//	docker run -d --name fx -p 127.0.0.1:27099:27017 mongo:8.0 mongod --replSet rs0 \
//	  --bind_ip_all --setParameter maxNumberOfTransactionOperationsInSingleOplogEntry=2
//	docker exec fx mongosh --eval 'rs.initiate({_id:"rs0",members:[{_id:0,host:"127.0.0.1:27017"}]})'
//	go run -tags integration ./internal/oplog/testdata/gen \
//	  -uri 'mongodb://127.0.0.1:27099/?directConnection=true' -out internal/oplog/testdata/fixtures/8.0
//
// The directory is ignored by "go build ./...", so the driver never reaches a binary.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// scenario is one fixture: the databases it uses and the operations it runs.
type scenario struct {
	name string
	dbs  []string
	run  func(ctx context.Context, c *mongo.Client) error
}

func main() {
	uri := flag.String("uri", "", "MongoDB URI of a single-node replica set")
	out := flag.String("out", "", "output directory")
	flag.Parse()
	if *uri == "" || *out == "" {
		log.Fatal("-uri and -out are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(*uri))
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	if err := os.MkdirAll(*out, 0o750); err != nil {
		log.Fatal(err)
	}
	for _, s := range scenarios() {
		n, err := record(ctx, client, s, filepath.Join(*out, s.name+".bson"))
		if err != nil {
			log.Fatalf("%s: %v", s.name, err)
		}
		log.Printf("%s: %d entries", s.name, n)
	}
}

// record runs s and writes the oplog entries it produced to path.
func record(ctx context.Context, c *mongo.Client, s scenario, path string) (int, error) {
	for _, db := range s.dbs {
		if err := c.Database(db).Drop(ctx); err != nil {
			return 0, err
		}
	}
	start, err := lastTS(ctx, c)
	if err != nil {
		return 0, err
	}
	marker := bson.D{{Key: "appendOplogNote", Value: 1}, {Key: "data", Value: bson.D{{Key: "fixture", Value: s.name}}}}
	if err := c.Database("admin").RunCommand(ctx, marker).Err(); err != nil {
		return 0, err
	}
	if err := s.run(ctx, c); err != nil {
		return 0, err
	}
	time.Sleep(time.Second)
	cur, err := c.Database("local").Collection("oplog.rs").Find(ctx,
		bson.D{{Key: "ts", Value: bson.D{{Key: "$gt", Value: start}}}},
		options.Find().SetSort(bson.D{{Key: "$natural", Value: 1}}))
	if err != nil {
		return 0, err
	}
	defer func() { _ = cur.Close(ctx) }()
	var buf []byte
	n := 0
	for cur.Next(ctx) {
		if keep(cur.Current, s.dbs) {
			buf = append(buf, cur.Current...)
			n++
		}
	}
	if err := cur.Err(); err != nil {
		return 0, err
	}
	return n, os.WriteFile(path, buf, 0o600)
}

// keep reports whether entry belongs to the scenario: its databases, transactions on
// admin.$cmd, index build bookkeeping and the scenario's marker note. Periodic no-ops
// and other system writes are left out.
func keep(entry bson.Raw, dbs []string) bool {
	op, _ := entry.Lookup("op").StringValueOK()
	ns, _ := entry.Lookup("ns").StringValueOK()
	if _, ok := entry.Lookup("o", "fixture").StringValueOK(); ok && op == "n" {
		return true
	}
	if ns == "admin.$cmd" || strings.HasPrefix(ns, "config.system.indexBuilds") {
		return true
	}
	db, _, _ := strings.Cut(ns, ".")
	for _, d := range dbs {
		if d == db {
			return true
		}
	}
	return false
}

// lastTS returns the timestamp of the newest oplog entry.
func lastTS(ctx context.Context, c *mongo.Client) (bson.Timestamp, error) {
	raw, err := c.Database("local").Collection("oplog.rs").FindOne(ctx, bson.D{},
		options.FindOne().SetSort(bson.D{{Key: "$natural", Value: -1}})).Raw()
	if err != nil {
		return bson.Timestamp{}, err
	}
	t, i, ok := raw.Lookup("ts").TimestampOK()
	if !ok {
		return bson.Timestamp{}, errors.New("newest oplog entry has no ts")
	}
	return bson.Timestamp{T: t, I: i}, nil
}

// cmd runs a command on db.
func cmd(ctx context.Context, c *mongo.Client, db string, command bson.D) error {
	if err := c.Database(db).RunCommand(ctx, command).Err(); err != nil {
		return fmt.Errorf("%s %v: %w", db, command[0].Key, err)
	}
	return nil
}

// docs returns n small documents with _id from first.
func docs(first, n int) []any {
	out := make([]any, 0, n)
	for i := first; i < first+n; i++ {
		out = append(out, bson.D{{Key: "_id", Value: i}, {Key: "k", Value: i % 3}, {Key: "v", Value: fmt.Sprintf("v%d", i)}})
	}
	return out
}

func scenarios() []scenario {
	return []scenario{
		{name: "crud", dbs: []string{"fx_crud"}, run: crudScenario},
		{name: "ddl", dbs: []string{"fx_ddl", "fx_ddl2"}, run: ddlScenario},
		{name: "txn", dbs: []string{"fx_txn", "fx_txn2"}, run: txnScenario},
		{name: "indexbuild", dbs: []string{"fx_idx"}, run: indexBuildScenario},
		{name: "timeseries", dbs: []string{"fx_ts"}, run: timeseriesScenario},
		{name: "crossdb", dbs: []string{"fx_xa", "fx_xb"}, run: crossDBScenario},
	}
}

func crudScenario(ctx context.Context, c *mongo.Client) error {
	coll := c.Database("fx_crud").Collection("items")
	if _, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: 0}, {Key: "k", Value: 0}}); err != nil {
		return err
	}
	if _, err := coll.InsertMany(ctx, docs(1, 5)); err != nil {
		return err
	}
	if _, err := coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: 1}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "updated"}}}}); err != nil {
		return err
	}
	if _, err := coll.ReplaceOne(ctx, bson.D{{Key: "_id", Value: 2}}, bson.D{{Key: "replaced", Value: true}}); err != nil {
		return err
	}
	if _, err := coll.UpdateMany(ctx, bson.D{}, bson.D{{Key: "$inc", Value: bson.D{{Key: "n", Value: 1}}}}); err != nil {
		return err
	}
	if _, err := coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: 10}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "upserted"}}}}, options.UpdateOne().SetUpsert(true)); err != nil {
		return err
	}
	if err := coll.FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: 4}}, bson.D{{Key: "$set", Value: bson.D{{Key: "fam", Value: true}}}}).Err(); err != nil {
		return err
	}
	if _, err := coll.DeleteOne(ctx, bson.D{{Key: "_id", Value: 3}}); err != nil {
		return err
	}
	_, err := coll.DeleteMany(ctx, bson.D{{Key: "k", Value: 2}})
	return err
}

func ddlScenario(ctx context.Context, c *mongo.Client) error {
	db := c.Database("fx_ddl")
	validator := bson.D{{Key: "$jsonSchema", Value: bson.D{{Key: "bsonType", Value: "object"}}}}
	if err := db.CreateCollection(ctx, "events", options.CreateCollection().SetValidator(validator)); err != nil {
		return err
	}
	if err := cmd(ctx, c, "fx_ddl", bson.D{{Key: "collMod", Value: "events"}, {Key: "validationLevel", Value: "moderate"}}); err != nil {
		return err
	}
	ev := db.Collection("events")
	if _, err := ev.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "x", Value: 1}}},
		{Keys: bson.D{{Key: "y", Value: 1}}, Options: options.Index().SetName("y_1")},
	}); err != nil {
		return err
	}
	if _, err := ev.InsertMany(ctx, docs(0, 4)); err != nil {
		return err
	}
	if err := cmd(ctx, c, "fx_ddl", bson.D{{Key: "collMod", Value: "events"}, {Key: "index", Value: bson.D{{Key: "name", Value: "y_1"}, {Key: "hidden", Value: true}}}}); err != nil {
		return err
	}
	if err := ev.Indexes().DropOne(ctx, "x_1"); err != nil {
		return err
	}
	if _, err := db.Collection("tmp").InsertMany(ctx, docs(0, 2)); err != nil {
		return err
	}
	if err := cmd(ctx, c, "admin", bson.D{{Key: "renameCollection", Value: "fx_ddl.tmp"}, {Key: "to", Value: "fx_ddl.renamed"}}); err != nil {
		return err
	}
	if err := db.CreateView(ctx, "v_events", "events", bson.A{bson.D{{Key: "$match", Value: bson.D{{Key: "k", Value: 1}}}}}); err != nil {
		return err
	}
	if err := cmd(ctx, c, "fx_ddl", bson.D{{Key: "collMod", Value: "v_events"}, {Key: "viewOn", Value: "events"}, {Key: "pipeline", Value: bson.A{}}}); err != nil {
		return err
	}
	if err := db.CreateView(ctx, "v_gone", "events", bson.A{}); err != nil {
		return err
	}
	if err := db.Collection("v_gone").Drop(ctx); err != nil {
		return err
	}
	if err := cmd(ctx, c, "fx_ddl", bson.D{{Key: "create", Value: "capped"}, {Key: "capped", Value: true}, {Key: "size", Value: 4096}}); err != nil {
		return err
	}
	if err := db.Collection("renamed").Drop(ctx); err != nil {
		return err
	}
	if _, err := c.Database("fx_ddl2").Collection("gone").InsertMany(ctx, docs(0, 2)); err != nil {
		return err
	}
	return c.Database("fx_ddl2").Drop(ctx)
}

func txnScenario(ctx context.Context, c *mongo.Client) error {
	a := c.Database("fx_txn").Collection("a")
	b := c.Database("fx_txn").Collection("b")
	other := c.Database("fx_txn2").Collection("c")
	for _, coll := range []*mongo.Collection{a, b, other} {
		if _, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: "seed"}}); err != nil {
			return err
		}
	}
	sess, err := c.StartSession()
	if err != nil {
		return err
	}
	defer sess.EndSession(ctx)
	// A transaction of one entry.
	if _, err := sess.WithTransaction(ctx, func(ctx context.Context) (any, error) {
		if _, err := a.InsertOne(ctx, bson.D{{Key: "_id", Value: "small"}}); err != nil {
			return nil, err
		}
		return b.UpdateOne(ctx, bson.D{{Key: "_id", Value: "seed"}}, bson.D{{Key: "$set", Value: bson.D{{Key: "t", Value: 1}}}})
	}); err != nil {
		return err
	}
	// A transaction over two databases, split into partialTxn entries (two
	// operations per entry), that also creates a collection.
	if _, err := sess.WithTransaction(ctx, func(ctx context.Context) (any, error) {
		if _, err := a.InsertMany(ctx, docs(0, 3)); err != nil {
			return nil, err
		}
		if _, err := other.InsertOne(ctx, bson.D{{Key: "_id", Value: "cross"}}); err != nil {
			return nil, err
		}
		if _, err := c.Database("fx_txn").Collection("created").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}}); err != nil {
			return nil, err
		}
		return b.DeleteOne(ctx, bson.D{{Key: "_id", Value: "seed"}})
	}); err != nil {
		return err
	}
	// An aborted transaction leaves no entry.
	errAbort := errors.New("abort")
	_, err = sess.WithTransaction(ctx, func(ctx context.Context) (any, error) {
		if _, err := a.InsertOne(ctx, bson.D{{Key: "_id", Value: "aborted"}}); err != nil {
			return nil, err
		}
		return nil, errAbort
	})
	if !errors.Is(err, errAbort) {
		return fmt.Errorf("aborted transaction: %w", err)
	}
	return nil
}

func indexBuildScenario(ctx context.Context, c *mongo.Client) error {
	coll := c.Database("fx_idx").Collection("people")
	if _, err := coll.InsertMany(ctx, docs(0, 50)); err != nil {
		return err
	}
	if _, err := coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "k", Value: 1}}},
		{Keys: bson.D{{Key: "v", Value: -1}, {Key: "k", Value: 1}}, Options: options.Index().SetName("v_k").SetSparse(true)},
	}); err != nil {
		return err
	}
	// A unique index over duplicate keys fails and is aborted.
	if _, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "k", Value: -1}}, Options: options.Index().SetUnique(true)}); err == nil {
		return errors.New("unique index over duplicates succeeded")
	}
	if err := coll.Indexes().DropOne(ctx, "k_1"); err != nil {
		return err
	}
	// An index build followed by a rename.
	return cmd(ctx, c, "admin", bson.D{{Key: "renameCollection", Value: "fx_idx.people"}, {Key: "to", Value: "fx_idx.persons"}})
}

func timeseriesScenario(ctx context.Context, c *mongo.Client) error {
	db := c.Database("fx_ts")
	ts := options.TimeSeries().SetTimeField("at").SetMetaField("meta").SetGranularity("hours")
	if err := db.CreateCollection(ctx, "weather", options.CreateCollection().SetTimeSeriesOptions(ts).SetExpireAfterSeconds(86400*365*10)); err != nil {
		return err
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var rows []any
	for i := 0; i < 6; i++ {
		rows = append(rows, bson.D{{Key: "at", Value: now.Add(time.Duration(i) * time.Hour)}, {Key: "meta", Value: bson.D{{Key: "site", Value: i % 2}}}, {Key: "temp", Value: 20 + i}})
	}
	if _, err := db.Collection("weather").InsertMany(ctx, rows); err != nil {
		return err
	}
	if err := cmd(ctx, c, "fx_ts", bson.D{{Key: "collMod", Value: "weather"}, {Key: "expireAfterSeconds", Value: 86400 * 365 * 20}}); err != nil {
		return err
	}
	if err := db.CreateCollection(ctx, "gone", options.CreateCollection().SetTimeSeriesOptions(options.TimeSeries().SetTimeField("at"))); err != nil {
		return err
	}
	return db.Collection("gone").Drop(ctx)
}

func crossDBScenario(ctx context.Context, c *mongo.Client) error {
	src := c.Database("fx_xa").Collection("src")
	if _, err := src.InsertMany(ctx, docs(0, 5)); err != nil {
		return err
	}
	if _, err := src.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "k", Value: 1}}}); err != nil {
		return err
	}
	if _, err := c.Database("fx_xb").Collection("keep").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}}); err != nil {
		return err
	}
	return cmd(ctx, c, "admin", bson.D{{Key: "renameCollection", Value: "fx_xa.src"}, {Key: "to", Value: "fx_xb.dst"}})
}
