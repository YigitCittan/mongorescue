package mongoconn

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/yigitcittan/mongorescue/internal/pitr"
)

func TestGrantsOplogFind(t *testing.T) {
	str := func(s string) *string { return &s }
	priv := func(db, coll *string, anyResource bool, actions ...string) privilege {
		var p privilege
		p.Resource.DB, p.Resource.Collection, p.Resource.AnyResource = db, coll, anyResource
		p.Actions = actions
		return p
	}
	for _, tc := range []struct {
		name  string
		privs []privilege
		want  bool
	}{
		{"anyResource", []privilege{priv(nil, nil, true, "find")}, true},
		{"read on local", []privilege{priv(str("local"), str(""), false, "find", "listCollections")}, true},
		{"find on local.oplog.rs", []privilege{priv(str("local"), str("oplog.rs"), false, "find")}, true},
		{"readAnyDatabase (database wildcard)", []privilege{priv(str(""), str(""), false, "find")}, false},
		{"collection wildcard oplog.rs", []privilege{priv(str(""), str("oplog.rs"), false, "find")}, false},
		{"another local collection", []privilege{priv(str("local"), str("startup_log"), false, "find")}, false},
		{"no find", []privilege{priv(str("local"), str(""), false, "listCollections")}, false},
		{"cluster", []privilege{func() privilege { p := priv(nil, nil, false, "find"); p.Resource.Cluster = true; return p }()}, false},
		{"none", nil, false},
	} {
		if got := grantsOplogFind(tc.privs); got != tc.want {
			t.Errorf("%s: grantsOplogFind = %v; want %v", tc.name, got, tc.want)
		}
	}
}

func TestRedactErr(t *testing.T) {
	if redactErr(nil) != nil {
		t.Fatal("redactErr(nil) must be nil")
	}
	plain := errors.New("connection refused")
	if redactErr(plain) != plain {
		t.Error("an error without credentials must be returned unchanged")
	}
	leaky := errors.Join(pitr.ErrOplogBehind, errors.New("dial mongodb://u:leak-pw-7@db1:27017/ failed"))
	got := redactErr(leaky)
	if strings.Contains(got.Error(), "leak-pw-7") {
		t.Errorf("redacted error leaks the password: %v", got)
	}
	if !errors.Is(got, pitr.ErrOplogBehind) {
		t.Error("the redacted error must still unwrap to its cause")
	}
}

func TestOplogSessionOptions(t *testing.T) {
	ctx := context.Background()
	if _, err := New().OpenOplogSession(ctx, "mongodb://127.0.0.1:1/", "nearestish"); !errors.Is(err, ErrInvalidReadPreference) {
		t.Errorf("unknown read preference = %v; want ErrInvalidReadPreference", err)
	}
	if _, err := New().OpenOplogSession(ctx, "mongodb://u:bad-uri-secret@h:notaport/", ""); err == nil || strings.Contains(err.Error(), "bad-uri-secret") {
		t.Errorf("invalid uri error = %v", err)
	}
	s, err := New().OpenOplogSession(ctx, "mongodb://127.0.0.1:1/", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := s.rp.Mode().String(); got != DefaultOplogReadPreference {
		t.Errorf("default read preference = %s; want %s", got, DefaultOplogReadPreference)
	}
	// A range that ends before it starts never reaches the server.
	if _, err = s.ReadOplog(ctx, pitr.OplogRange{From: pitr.Timestamp{T: 5}, To: pitr.Timestamp{T: 4}}, io.Discard); !errors.Is(err, ErrInvalidRange) {
		t.Errorf("backwards range = %v; want ErrInvalidRange", err)
	}
}

// fakeCursor serves documents to copyOplog without a server.
type fakeCursor struct {
	docs []bson.Raw
	pos  int
	err  error
}

func (c *fakeCursor) Next(context.Context) bool {
	if c.pos >= len(c.docs) {
		return false
	}
	c.pos++
	return true
}
func (c *fakeCursor) Doc() bson.Raw { return c.docs[c.pos-1] }
func (c *fakeCursor) Err() error    { return c.err }

// entry returns a raw oplog entry at (sec, ord) in term.
func entry(t *testing.T, sec, ord uint32, term int64) bson.Raw {
	t.Helper()
	raw, err := bson.Marshal(bson.D{
		{Key: "op", Value: "n"}, {Key: "ns", Value: ""},
		{Key: "ts", Value: bson.Timestamp{T: sec, I: ord}}, {Key: "t", Value: term},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestCopyOplogContinuity checks that every range read proves its start on the
// member that served it and writes only (From, To], or [From, To] with
// StartInclusive.
func TestCopyOplogContinuity(t *testing.T) {
	at := func(sec, ord uint32) pitr.Timestamp { return pitr.Timestamp{T: sec, I: ord} }
	e10, e11, e12, e20 := entry(t, 10, 1, 2), entry(t, 11, 1, 2), entry(t, 12, 1, 2), entry(t, 20, 1, 3)
	for _, tc := range []struct {
		name    string
		docs    []bson.Raw
		cursErr error
		r       pitr.OplogRange
		want    []bson.Raw
		wantErr error
	}{
		{"start stripped", []bson.Raw{e10, e11, e12}, nil, pitr.OplogRange{From: at(10, 1), To: at(12, 1)}, []bson.Raw{e11, e12}, nil},
		{"term matches", []bson.Raw{e10, e11}, nil, pitr.OplogRange{From: at(10, 1), To: at(11, 1), CheckTerm: true, FromTerm: 2}, []bson.Raw{e11}, nil},
		{"start inclusive", []bson.Raw{e10, e11}, nil, pitr.OplogRange{From: at(10, 1), To: at(11, 1), StartInclusive: true}, []bson.Raw{e10, e11}, nil},
		{"empty range", []bson.Raw{e12}, nil, pitr.OplogRange{From: at(12, 1), To: at(12, 1)}, nil, nil},
		{"start truncated", []bson.Raw{e11, e12}, nil, pitr.OplogRange{From: at(10, 1), To: at(12, 1)}, nil, pitr.ErrOplogGap},
		{"start truncated, inclusive", []bson.Raw{e11, e12}, nil, pitr.OplogRange{From: at(10, 1), To: at(12, 1), StartInclusive: true}, nil, pitr.ErrOplogGap},
		{"nothing left", nil, nil, pitr.OplogRange{From: at(10, 1), To: at(12, 1)}, nil, pitr.ErrOplogGap},
		{"start has another term", []bson.Raw{e10, e11}, nil, pitr.OplogRange{From: at(10, 1), To: at(11, 1), CheckTerm: true, FromTerm: 1}, nil, pitr.ErrOplogGap},
		{"member behind", []bson.Raw{e10, e11}, nil, pitr.OplogRange{From: at(10, 1), To: at(20, 1)}, []bson.Raw{e11}, pitr.ErrOplogBehind},
		{"out of order", []bson.Raw{e10, e20, e12}, nil, pitr.OplogRange{From: at(10, 1), To: at(20, 1)}, []bson.Raw{e20}, nil},
		{"cursor error", []bson.Raw{e10, e11}, errors.New("cursor killed"), pitr.OplogRange{From: at(10, 1), To: at(11, 1)}, []bson.Raw{e11}, nil},
	} {
		var buf bytes.Buffer
		stats, err := copyOplog(context.Background(), &fakeCursor{docs: tc.docs, err: tc.cursErr}, tc.r, &buf)
		switch {
		case tc.wantErr != nil:
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("%s: err = %v; want %v", tc.name, err, tc.wantErr)
			}
		case tc.name == "out of order" || tc.name == "cursor error":
			if err == nil || errors.Is(err, pitr.ErrOplogGap) || errors.Is(err, pitr.ErrOplogBehind) {
				t.Errorf("%s: err = %v; want a read error", tc.name, err)
			}
		case err != nil:
			t.Errorf("%s: %v", tc.name, err)
		}
		var want []byte
		for _, d := range tc.want {
			want = append(want, d...)
		}
		if !bytes.Equal(buf.Bytes(), want) {
			t.Errorf("%s: wrote %d bytes; want the %d entries of the range (%d bytes)", tc.name, buf.Len(), len(tc.want), len(want))
		}
		if err == nil && (stats.Entries != len(tc.want) ||
			(len(tc.want) > 0 && stats.Last.TS != tc.r.To)) {
			t.Errorf("%s: stats = %+v", tc.name, stats)
		}
	}
}

// TestOplogUnreachableIsRedacted uses a local closed port, so it needs no network
// and no MongoDB.
func TestOplogUnreachableIsRedacted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	uri := "mongodb://user:oplog-pw-9@127.0.0.1:1/?connectTimeoutMS=200"
	p := New()
	for name, call := range map[string]func() error{
		"OplogWindow": func() error { _, err := p.OplogWindow(ctx, uri); return err },
		"ReadOplog": func() error {
			_, err := p.ReadOplog(ctx, uri, pitr.OplogRange{From: pitr.Timestamp{T: 1}, To: pitr.Timestamp{T: 2}}, io.Discard)
			return err
		},
		"EntryAt":      func() error { _, _, err := p.EntryAt(ctx, uri, pitr.Timestamp{T: 1}); return err },
		"CanReadOplog": func() error { _, err := p.CanReadOplog(ctx, uri); return err },
	} {
		err := call()
		if err == nil {
			t.Errorf("%s against a closed port must fail", name)
			continue
		}
		if strings.Contains(err.Error(), "oplog-pw-9") {
			t.Errorf("%s error leaks the password: %v", name, err)
		}
	}
}
