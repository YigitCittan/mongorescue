package mongoconn

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

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
	// An empty range never reaches the server.
	stats, err := s.ReadOplog(ctx, pitr.Timestamp{T: 5}, pitr.Timestamp{T: 5}, io.Discard)
	if err != nil || stats.Entries != 0 {
		t.Errorf("empty range = %+v, %v", stats, err)
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
			_, err := p.ReadOplog(ctx, uri, pitr.Timestamp{T: 1}, pitr.Timestamp{T: 2}, io.Discard)
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
