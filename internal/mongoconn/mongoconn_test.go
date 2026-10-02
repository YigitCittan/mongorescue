package mongoconn

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestPingUnreachableFailsWithinDeadline uses a local closed port, so it needs no
// network and no MongoDB.
func TestPingUnreachableFailsWithinDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := New().Ping(ctx, "mongodb://user:unreachable-pw@127.0.0.1:1/?connectTimeoutMS=200")
	if err == nil {
		t.Fatal("ping of a closed port must fail")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("ping took %v; want it bounded by the context deadline", elapsed)
	}
	if strings.Contains(err.Error(), "unreachable-pw") {
		t.Fatalf("error leaks the password: %v", err)
	}
	for _, call := range []func() error{
		func() error { _, err := New().ListDatabases(ctx, "mongodb://127.0.0.1:1/"); return err },
		func() error { _, err := New().ListCollections(ctx, "mongodb://127.0.0.1:1/", "db"); return err },
	} {
		if call() == nil {
			t.Fatal("discovery against a closed port must fail")
		}
	}
}

func TestInvalidURIIsNotEchoed(t *testing.T) {
	_, err := New().Ping(context.Background(), "mongodb://u:bad-uri-secret@h:notaport/")
	if err == nil || strings.Contains(err.Error(), "bad-uri-secret") {
		t.Fatalf("invalid uri error = %v", err)
	}
}

// TestClientOptionsTimeouts checks that the connection string's own timeouts and
// topology options survive, the fallback applies otherwise and a context deadline
// caps both.
func TestClientOptionsTimeouts(t *testing.T) {
	opts := clientOptions(context.Background(), "mongodb://127.0.0.1:27017/?replicaSet=rs0")
	if *opts.ServerSelectionTimeout != fallbackTimeout || *opts.ConnectTimeout != fallbackTimeout {
		t.Fatalf("defaults = %v / %v; want %v", *opts.ServerSelectionTimeout, *opts.ConnectTimeout, fallbackTimeout)
	}
	if opts.ReplicaSet == nil || *opts.ReplicaSet != "rs0" {
		t.Fatalf("replicaSet = %v; want rs0", opts.ReplicaSet)
	}

	opts = clientOptions(context.Background(), "mongodb://127.0.0.1:27017/?directConnection=true&serverSelectionTimeoutMS=45000&connectTimeoutMS=5000")
	if *opts.ServerSelectionTimeout != 45*time.Second || *opts.ConnectTimeout != 5*time.Second {
		t.Fatalf("URI timeouts = %v / %v; want 45s / 5s", *opts.ServerSelectionTimeout, *opts.ConnectTimeout)
	}
	if opts.Direct == nil || !*opts.Direct {
		t.Fatal("directConnection was dropped")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	opts = clientOptions(ctx, "mongodb://127.0.0.1:27017/?serverSelectionTimeoutMS=45000")
	if *opts.ServerSelectionTimeout > 2*time.Second || *opts.ConnectTimeout > 2*time.Second {
		t.Fatalf("deadline caps = %v / %v; want at most 2s", *opts.ServerSelectionTimeout, *opts.ConnectTimeout)
	}
}
