//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"fmt"
	"math"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	// fidelityOrders exceeds 10k documents so the dump spans many archive blocks.
	fidelityOrders = 10_500
	// fidelityBlobs documents of fidelityBlobSize random bytes sit near the 16 MiB
	// BSON document limit.
	fidelityBlobs    = 3
	fidelityBlobSize = 15 << 20
	// fidelityCappedMax bounds the capped collection, which receives more inserts.
	fidelityCappedMax = 100
)

// fidelityDB describes what seedFidelity created.
type fidelityDB struct {
	Name string
	// TimeSeries reports whether the metrics time-series collection exists (5.0+).
	TimeSeries bool
}

// seedFidelity creates a database exercising collection options, index types, every
// BSON type and large documents. The returned names are the collections and views.
func seedFidelity(t *testing.T, env *mongoEnv, tag string) fidelityDB {
	t.Helper()
	major, _ := env.serverVersion(t)
	f := fidelityDB{Name: env.uniqueDB(t, tag), TimeSeries: major >= 5}
	ctx, cancel := context.WithTimeout(context.Background(), 2*opTimeout)
	defer cancel()
	db := env.Client.Database(f.Name)

	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed %s: %s: %v", f.Name, what, err)
		}
	}
	run := func(what string, cmd bson.D) {
		t.Helper()
		must(what, db.RunCommand(ctx, cmd).Err())
	}
	insert := func(coll string, docs []any) {
		t.Helper()
		_, err := db.Collection(coll).InsertMany(ctx, docs)
		must("insert into "+coll, err)
	}
	indexes := func(coll string, models ...mongo.IndexModel) {
		t.Helper()
		_, err := db.Collection(coll).Indexes().CreateMany(ctx, models)
		must("indexes of "+coll, err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)

	// orders: 10k+ documents with compound, unique, partial and hidden indexes.
	for start := 0; start < fidelityOrders; start += 1000 {
		docs := make([]any, 0, 1000)
		for i := start; i < min(start+1000, fidelityOrders); i++ {
			docs = append(docs, bson.D{
				{Key: "_id", Value: i},
				{Key: "seq", Value: i},
				{Key: "sku", Value: fmt.Sprintf("SKU-%05d", i%977)},
				{Key: "ref", Value: fmt.Sprintf("REF-%06d", i)},
				{Key: "amount", Value: float64(i) * 1.25},
				{Key: "createdAt", Value: bson.NewDateTimeFromTime(now.Add(-time.Duration(i) * time.Minute))},
				{Key: "tags", Value: bson.A{"t" + fmt.Sprint(i%7), "t" + fmt.Sprint(i%11)}},
			})
		}
		insert("orders", docs)
	}
	indexes("orders",
		mongo.IndexModel{Keys: bson.D{{Key: "sku", Value: 1}, {Key: "amount", Value: -1}}, Options: options.Index().SetName("sku_amount")},
		mongo.IndexModel{Keys: bson.D{{Key: "ref", Value: 1}}, Options: options.Index().SetUnique(true)},
		mongo.IndexModel{Keys: bson.D{{Key: "amount", Value: 1}}, Options: options.Index().
			SetPartialFilterExpression(bson.D{{Key: "amount", Value: bson.D{{Key: "$gt", Value: 100}}}})},
		mongo.IndexModel{Keys: bson.D{{Key: "seq", Value: 1}}, Options: options.Index().SetHidden(true)},
		mongo.IndexModel{Keys: bson.D{{Key: "tags", Value: 1}, {Key: "createdAt", Value: -1}}},
	)

	// types: every BSON type, nested arrays, unicode and dotted keys, mixed _id types.
	oid := bson.NewObjectID()
	dec, err := bson.ParseDecimal128("1234567890.123456789012345678901234")
	must("decimal", err)
	uuid := make([]byte, 16)
	_, _ = rand.Read(uuid)
	insert("types", []any{
		bson.D{
			{Key: "_id", Value: 1},
			{Key: "double", Value: 3.141592653589793},
			{Key: "negZero", Value: math.Copysign(0, -1)},
			{Key: "nan", Value: math.NaN()},
			{Key: "inf", Value: math.Inf(-1)},
			{Key: "string", Value: "héllo wörld ✓ \u0000 embedded nul"},
			{Key: "doc", Value: bson.D{{Key: "a", Value: bson.D{{Key: "b", Value: bson.D{{Key: "c", Value: int32(1)}}}}}}},
			{Key: "nested", Value: bson.A{int32(1), "two", bson.A{3.0, bson.A{int64(4), bson.A{bson.D{{Key: "five", Value: 5}}}}}, bson.A{}}},
			{Key: "binary", Value: bson.Binary{Subtype: 0x00, Data: []byte{0, 1, 2, 0xfe, 0xff}}},
			{Key: "uuid", Value: bson.Binary{Subtype: 0x04, Data: uuid}},
			{Key: "userBinary", Value: bson.Binary{Subtype: 0x80, Data: []byte("custom")}},
			{Key: "oid", Value: oid},
			{Key: "bool", Value: true},
			{Key: "date", Value: bson.NewDateTimeFromTime(now)},
			{Key: "before1970", Value: bson.DateTime(-2208988800000)},
			{Key: "year3000", Value: bson.DateTime(32503680000000)},
			{Key: "null", Value: nil},
			{Key: "regex", Value: bson.Regex{Pattern: `^a.*b\d+$`, Options: "imsx"}},
			{Key: "js", Value: bson.JavaScript("function () { return 1; }")},
			{Key: "jsScope", Value: bson.CodeWithScope{Code: "function () { return x; }", Scope: bson.D{{Key: "x", Value: int32(1)}}}},
			{Key: "int32", Value: int32(-42)},
			{Key: "int64", Value: int64(1) << 62},
			{Key: "timestamp", Value: bson.Timestamp{T: 1_700_000_000, I: 7}},
			{Key: "decimal", Value: dec},
			{Key: "minKey", Value: bson.MinKey{}},
			{Key: "maxKey", Value: bson.MaxKey{}},
			{Key: "symbol", Value: bson.Symbol("sym")},
			{Key: "undefined", Value: bson.Undefined{}},
			{Key: "dbPointer", Value: bson.DBPointer{DB: "other.coll", Pointer: oid}},
			{Key: "ключ", Value: "значение"},
			{Key: "键", Value: "值"},
			{Key: "🔑", Value: "🔒"},
			{Key: "a.b", Value: "dotted key"},
			{Key: "emptyString", Value: ""},
			{Key: "emptyDoc", Value: bson.D{}},
		},
		bson.D{{Key: "_id", Value: "string id ü"}, {Key: "v", Value: 2}},
		bson.D{{Key: "_id", Value: oid}, {Key: "v", Value: 3}},
		bson.D{{Key: "_id", Value: bson.D{{Key: "compound", Value: 1}, {Key: "id", Value: "x"}}}, {Key: "v", Value: 4}},
		bson.D{{Key: "_id", Value: dec}, {Key: "v", Value: 5}},
		bson.D{{Key: "_id", Value: bson.NewDateTimeFromTime(now)}, {Key: "v", Value: 6}},
	})

	// blobs: documents near the 16 MiB limit (random, so gzip cannot shrink them).
	for i := 0; i < fidelityBlobs; i++ {
		payload := make([]byte, fidelityBlobSize)
		_, _ = rand.Read(payload)
		_, err := db.Collection("blobs").InsertOne(ctx, bson.D{{Key: "_id", Value: i}, {Key: "payload", Value: bson.Binary{Data: payload}}})
		must("insert blob", err)
	}

	// places: text (weights, language), 2dsphere and collation indexes.
	var places []any
	cities := []string{"İstanbul", "Izmir", "ısparta", "Çanakkale", "Şanlıurfa", "Ağrı"}
	for i := 0; i < 200; i++ {
		places = append(places, bson.D{
			{Key: "_id", Value: i},
			{Key: "name", Value: fmt.Sprintf("%s %d", cities[i%len(cities)], i)},
			{Key: "title", Value: fmt.Sprintf("place number %d", i)},
			{Key: "body", Value: "a quiet street with trees and cafés"},
			{Key: "loc", Value: bson.D{{Key: "type", Value: "Point"}, {Key: "coordinates", Value: bson.A{28.9 + float64(i)/1000, 41.0 + float64(i)/1000}}}},
		})
	}
	insert("places", places)
	indexes("places",
		mongo.IndexModel{Keys: bson.D{{Key: "title", Value: "text"}, {Key: "body", Value: "text"}}, Options: options.Index().
			SetName("fulltext").SetWeights(bson.D{{Key: "title", Value: 10}, {Key: "body", Value: 2}}).SetDefaultLanguage("english")},
		mongo.IndexModel{Keys: bson.D{{Key: "loc", Value: "2dsphere"}}},
		mongo.IndexModel{Keys: bson.D{{Key: "name", Value: 1}}, Options: options.Index().SetName("name_tr").
			SetCollation(&options.Collation{Locale: "tr", Strength: 2})},
	)

	// sessions: a TTL index (far in the future, so nothing expires during the test).
	var sessions []any
	for i := 0; i < 50; i++ {
		sessions = append(sessions, bson.D{{Key: "_id", Value: i}, {Key: "createdAt", Value: bson.NewDateTimeFromTime(now)}})
	}
	insert("sessions", sessions)
	indexes("sessions", mongo.IndexModel{Keys: bson.D{{Key: "createdAt", Value: 1}}, Options: options.Index().SetExpireAfterSeconds(10 * 365 * 24 * 3600)})

	// validated: a JSON schema validator added after a non-conforming document was
	// written (validationLevel moderate keeps it); a restore must keep that document.
	insert("validated", []any{
		bson.D{{Key: "_id", Value: 1}, {Key: "name", Value: "ok"}, {Key: "qty", Value: 1}},
		bson.D{{Key: "_id", Value: 2}, {Key: "qty", Value: "not a number"}},
	})
	run("validator", bson.D{
		{Key: "collMod", Value: "validated"},
		{Key: "validator", Value: bson.D{{Key: "$jsonSchema", Value: bson.D{
			{Key: "bsonType", Value: "object"},
			{Key: "required", Value: bson.A{"name", "qty"}},
			{Key: "properties", Value: bson.D{
				{Key: "name", Value: bson.D{{Key: "bsonType", Value: "string"}}},
				{Key: "qty", Value: bson.D{{Key: "bsonType", Value: "int"}, {Key: "minimum", Value: 0}}},
			}},
		}}}},
		{Key: "validationLevel", Value: "moderate"},
		{Key: "validationAction", Value: "error"},
	})
	insert("validated", []any{bson.D{{Key: "_id", Value: 3}, {Key: "name", Value: "later"}, {Key: "qty", Value: int32(3)}}})

	// capped: size and max; more inserts than max, so the oldest were evicted.
	must("capped", db.CreateCollection(ctx, "capped", options.CreateCollection().SetCapped(true).SetSizeInBytes(1<<20).SetMaxDocuments(fidelityCappedMax)))
	var capped []any
	for i := 0; i < fidelityCappedMax+50; i++ {
		capped = append(capped, bson.D{{Key: "_id", Value: i}, {Key: "line", Value: fmt.Sprintf("log line %d", i)}})
	}
	insert("capped", capped)

	// collated: a collection default collation.
	must("collated", db.CreateCollection(ctx, "collated", options.CreateCollection().
		SetCollation(&options.Collation{Locale: "fr", Strength: 2, CaseLevel: true})))
	var collated []any
	for i, w := range []string{"côte", "cote", "Côte", "coté", "côté", "Cote"} {
		collated = append(collated, bson.D{{Key: "_id", Value: i}, {Key: "word", Value: w}})
	}
	insert("collated", collated)
	indexes("collated", mongo.IndexModel{Keys: bson.D{{Key: "word", Value: 1}}})

	// metrics: a time-series collection with a secondary index (5.0+).
	if f.TimeSeries {
		must("metrics", db.CreateCollection(ctx, "metrics", options.CreateCollection().SetTimeSeriesOptions(
			options.TimeSeries().SetTimeField("ts").SetMetaField("sensor").SetGranularity("minutes"))))
		var points []any
		for i := 0; i < 500; i++ {
			points = append(points, bson.D{
				{Key: "_id", Value: bson.NewObjectID()},
				{Key: "ts", Value: bson.NewDateTimeFromTime(now.Add(-time.Duration(i) * time.Minute))},
				{Key: "sensor", Value: bson.D{{Key: "id", Value: i % 5}, {Key: "site", Value: "north"}}},
				{Key: "value", Value: float64(i) / 3},
			})
		}
		insert("metrics", points)
		indexes("metrics", mongo.IndexModel{Keys: bson.D{{Key: "sensor.id", Value: 1}, {Key: "ts", Value: -1}}})
	}

	// Views, one with its own collation.
	must("view big_orders", db.CreateView(ctx, "big_orders", "orders", mongo.Pipeline{
		{{Key: "$match", Value: bson.D{{Key: "amount", Value: bson.D{{Key: "$gt", Value: 10000}}}}}},
		{{Key: "$project", Value: bson.D{{Key: "sku", Value: 1}, {Key: "amount", Value: 1}}}},
	}))
	must("view places_tr", db.CreateView(ctx, "places_tr", "places", mongo.Pipeline{
		{{Key: "$sort", Value: bson.D{{Key: "name", Value: 1}}}},
	}, options.CreateView().SetCollation(&options.Collation{Locale: "tr"})))

	return f
}
