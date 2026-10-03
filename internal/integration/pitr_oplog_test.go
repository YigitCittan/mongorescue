//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/url"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/yigitcittan/mongorescue/internal/mongoconn"
	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// requireReplicaSet skips the test unless the server under test is a replica set
// member (MONGO_TOPOLOGY=replset in scripts/test-integration-docker.sh).
func requireReplicaSet(t *testing.T, env *mongoEnv) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	var hello struct {
		SetName string `bson:"setName"`
	}
	if err := env.Client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if hello.SetName == "" {
		t.Skip("needs a replica set member (MONGO_TOPOLOGY=replset)")
	}
}

// oplogEntry is the part of an oplog entry the tests check.
type oplogEntry struct {
	TS pitr.Timestamp
	NS string
	Op string
	ID int32
}

// splitOplog parses the concatenated raw BSON documents ReadOplog wrote.
func splitOplog(t *testing.T, raw []byte) []oplogEntry {
	t.Helper()
	var out []oplogEntry
	for len(raw) > 0 {
		if len(raw) < 4 {
			t.Fatalf("%d trailing bytes after the last document", len(raw))
		}
		n := int(binary.LittleEndian.Uint32(raw))
		if n < 5 || n > len(raw) {
			t.Fatalf("document length %d with %d bytes left", n, len(raw))
		}
		doc := bson.Raw(raw[:n])
		if err := doc.Validate(); err != nil {
			t.Fatalf("invalid oplog document: %v", err)
		}
		sec, ord, ok := doc.Lookup("ts").TimestampOK()
		if !ok {
			t.Fatal("oplog document without ts")
		}
		e := oplogEntry{TS: pitr.Timestamp{T: sec, I: ord}, NS: doc.Lookup("ns").StringValue(), Op: doc.Lookup("op").StringValue()}
		if e.Op == "i" {
			e.ID, _ = doc.Lookup("o", "_id").Int32OK()
		}
		out = append(out, e)
		raw = raw[n:]
	}
	return out
}

// majorityWindow waits until every oplog entry is majority-committed and returns
// the window, so a read up to MajorityOpTime covers all writes made before.
func majorityWindow(ctx context.Context, t *testing.T, s *mongoconn.OplogSession) pitr.OplogWindow {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		w, err := s.OplogWindow(ctx)
		if err != nil {
			t.Fatalf("OplogWindow: %v", err)
		}
		if w.MajorityOpTime.TS == w.Newest {
			return w
		}
		if time.Now().After(deadline) {
			t.Fatalf("majority optime %s never reached the newest entry %s", w.MajorityOpTime.TS, w.Newest)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// readRange reads (from, to] and returns the parsed entries and the stats.
func readRange(ctx context.Context, t *testing.T, s *mongoconn.OplogSession, from, to pitr.Timestamp) ([]oplogEntry, pitr.OplogStats) {
	t.Helper()
	var buf bytes.Buffer
	stats, err := s.ReadOplog(ctx, from, to, &buf)
	if err != nil {
		t.Fatalf("ReadOplog(%s, %s): %v", from, to, err)
	}
	entries := splitOplog(t, buf.Bytes())
	if len(entries) != stats.Entries {
		t.Fatalf("ReadOplog reported %d entries and wrote %d", stats.Entries, len(entries))
	}
	if len(entries) > 0 && (stats.First.TS != entries[0].TS || stats.Last.TS != entries[len(entries)-1].TS || stats.Last.TS != to) {
		t.Fatalf("stats %+v do not match the entries written (to %s)", stats, to)
	}
	return entries, stats
}

// TestOplogWindowAndRangeReads checks the oplog adapter the PITR collector uses: the
// window's shape, range reads that return exactly the inserted entries with no gap
// or duplicate across consecutive ranges, the divergence lookup and a read past the
// member's newest entry.
func TestOplogWindowAndRangeReads(t *testing.T) {
	env := requireMongo(t)
	requireReplicaSet(t, env)
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	s, err := mongoconn.New().OpenOplogSession(ctx, env.URI, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	w0 := majorityWindow(ctx, t, s)
	if w0.ReplicaSet == "" || w0.Oldest.IsZero() || w0.Oldest.Compare(w0.Newest) > 0 || w0.MajorityOpTime.Term < 1 {
		t.Fatalf("window = %+v", w0)
	}
	if len(w0.ReplicaSetID) != 24 {
		t.Errorf("replica set ID = %q; want a hex ObjectID", w0.ReplicaSetID)
	}
	if one, oneErr := mongoconn.New().OplogWindow(ctx, env.URI); oneErr != nil || one.ReplicaSet != w0.ReplicaSet || one.ReplicaSetID != w0.ReplicaSetID {
		t.Errorf("OplogWindow through the prober = %+v, %v", one, oneErr)
	}

	db := env.uniqueDB(t, "oplog")
	ns := db + ".events"
	coll := env.Client.Database(db).Collection("events")
	insert := func(from, to int) {
		t.Helper()
		for i := from; i < to; i++ {
			if _, insErr := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(i)}}); insErr != nil {
				t.Fatal(insErr)
			}
		}
	}

	insert(0, 15)
	w1 := majorityWindow(ctx, t, s)
	insert(15, 40)
	w2 := majorityWindow(ctx, t, s)

	A, B1, B2 := w0.MajorityOpTime.TS, w1.MajorityOpTime.TS, w2.MajorityOpTime.TS
	first, _ := readRange(ctx, t, s, A, B1)
	second, _ := readRange(ctx, t, s, B1, B2)
	whole, _ := readRange(ctx, t, s, A, B2)

	joined := append(append([]oplogEntry{}, first...), second...)
	if len(joined) != len(whole) {
		t.Fatalf("consecutive ranges returned %d entries, one read %d", len(joined), len(whole))
	}
	for i := range whole {
		if joined[i] != whole[i] {
			t.Fatalf("entry %d differs: %+v vs %+v", i, joined[i], whole[i])
		}
		if i > 0 && whole[i].TS.Compare(whole[i-1].TS) <= 0 {
			t.Fatalf("entries out of order at %d: %s after %s", i, whole[i].TS, whole[i-1].TS)
		}
	}
	inserted := func(entries []oplogEntry) []int32 {
		var ids []int32
		for _, e := range entries {
			if e.NS == ns && e.Op == "i" {
				ids = append(ids, e.ID)
			}
		}
		return ids
	}
	for _, tc := range []struct {
		name    string
		entries []oplogEntry
		from    int
		to      int
	}{{"first range", first, 0, 15}, {"second range", second, 15, 40}} {
		ids := inserted(tc.entries)
		if len(ids) != tc.to-tc.from {
			t.Fatalf("%s holds %d inserts; want %d", tc.name, len(ids), tc.to-tc.from)
		}
		for i, id := range ids {
			if id != int32(tc.from+i) {
				t.Fatalf("%s insert %d has _id %d; want %d", tc.name, i, id, tc.from+i)
			}
		}
	}

	// An empty range writes nothing.
	if empty, _ := readRange(ctx, t, s, B2, B2); len(empty) != 0 {
		t.Errorf("empty range returned %d entries", len(empty))
	}
	// A bound past the member's newest entry is reported, never silently cut short.
	var sink bytes.Buffer
	if _, err = s.ReadOplog(ctx, B2, pitr.Timestamp{T: w2.Newest.T + 3600}, &sink); !errors.Is(err, pitr.ErrOplogBehind) {
		t.Errorf("read past the newest entry = %v; want ErrOplogBehind", err)
	}

	// Divergence check: the entry at the stored position and its term.
	term, found, err := s.EntryAt(ctx, B2)
	if err != nil || !found || term != w2.MajorityOpTime.Term {
		t.Errorf("EntryAt(%s) = %d, %v, %v; want term %d", B2, term, found, err, w2.MajorityOpTime.Term)
	}
	if _, found, err = mongoconn.New().EntryAt(ctx, env.URI, pitr.Timestamp{T: B2.T, I: 1 << 30}); err != nil || found {
		t.Errorf("EntryAt of a missing position = %v, %v; want not found", found, err)
	}
}

// TestCanReadOplog checks the collector's privilege check against real users: read
// on local grants find on local.oplog.rs, readAnyDatabase does not.
func TestCanReadOplog(t *testing.T) {
	env := requireMongo(t)
	requireReplicaSet(t, env)
	if env.Password == "" {
		t.Skip("needs a server with access control (a URI with credentials)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	p := mongoconn.New()

	if ok, err := p.CanReadOplog(ctx, env.URI); err != nil || !ok {
		t.Fatalf("CanReadOplog as root = %v, %v; want true", ok, err)
	}

	admin := env.Client.Database("admin")
	password := randomHex(t, 12)
	userURI := func(user string) string {
		u, err := url.Parse(env.URI)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(user, password)
		q := u.Query()
		q.Set("authSource", "admin")
		u.RawQuery = q.Encode()
		return u.String()
	}
	suffix := randomHex(t, 3)
	for _, tc := range []struct {
		user string
		role bson.D
		want bool
	}{
		{"it_oplog_local_" + suffix, bson.D{{Key: "role", Value: "read"}, {Key: "db", Value: "local"}}, true},
		{"it_oplog_any_" + suffix, bson.D{{Key: "role", Value: "readAnyDatabase"}, {Key: "db", Value: "admin"}}, false},
	} {
		if err := admin.RunCommand(ctx, bson.D{
			{Key: "createUser", Value: tc.user},
			{Key: "pwd", Value: password},
			{Key: "roles", Value: bson.A{tc.role}},
		}).Err(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cctx, ccancel := context.WithTimeout(context.Background(), opTimeout)
			defer ccancel()
			_ = admin.RunCommand(cctx, bson.D{{Key: "dropUser", Value: tc.user}}).Err()
		})
		uri := userURI(tc.user)
		ok, err := p.CanReadOplog(ctx, uri)
		if err != nil || ok != tc.want {
			t.Errorf("CanReadOplog as %s = %v, %v; want %v", tc.user, ok, err, tc.want)
		}
		// The check agrees with the server: the read works exactly when it says so.
		w, err := p.OplogWindow(ctx, uri)
		if tc.want {
			if err != nil {
				t.Fatalf("OplogWindow as %s: %v", tc.user, err)
			}
			var buf bytes.Buffer
			from := pitr.Timestamp{T: w.MajorityOpTime.TS.T - 60}
			if _, err = p.ReadOplog(ctx, uri, from, w.MajorityOpTime.TS, &buf); err != nil {
				t.Errorf("ReadOplog as %s: %v", tc.user, err)
			}
		} else if err == nil {
			t.Errorf("OplogWindow as %s succeeded without find on local.oplog.rs", tc.user)
		}
		if err != nil {
			assertNoSecret(t, password, "oplog error", err.Error())
		}
	}
}
