package mongoconn

import (
	"context"
	"fmt"
	"math"
	"net"
	"slices"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/yigitcittan/mongorescue/internal/diskspace"
)

// MissingPrivileges returns the entries of actions the connection's user is not
// granted on the whole of database (a privilege on every collection of it, on any
// database, or on any resource), sorted, or none. A server without access control
// grants everything. Restore preflights use it with the actions mongorestore needs.
func (p *Prober) MissingPrivileges(ctx context.Context, uri, database string, actions []string) ([]string, error) {
	var missing []string
	err := withClient(ctx, uri, func(c *mongo.Client) error {
		privs, users, err := userPrivileges(ctx, c)
		if err != nil {
			return err
		}
		if users == 0 {
			return nil
		}
		missing = missingFrom(privs, database, actions)
		return nil
	})
	return missing, err
}

// grantedOn returns the actions privs grant on every collection of database.
func grantedOn(privs []privilege, database string) map[string]bool {
	granted := map[string]bool{}
	for _, p := range privs {
		r := p.Resource
		covers := r.AnyResource ||
			(r.DB != nil && r.Collection != nil && *r.Collection == "" && (*r.DB == "" || *r.DB == database))
		if !covers {
			continue
		}
		for _, a := range p.Actions {
			granted[a] = true
		}
	}
	return granted
}

// missingFrom returns the entries of actions privs do not grant on every collection
// of database, sorted.
func missingFrom(privs []privilege, database string, actions []string) []string {
	granted := grantedOn(privs, database)
	var missing []string
	for _, a := range actions {
		if !granted[a] && !slices.Contains(missing, a) {
			missing = append(missing, a)
		}
	}
	slices.Sort(missing)
	return missing
}

// FreeSpace returns the free disk space of the filesystem the server at uri stores
// database on, when it is knowable: from dbStats (fsTotalSize minus fsUsedSize, on
// database or else on admin) or, for a server on this host (every host of uri is a
// loopback address) whose dbPath getCmdLineOpts reveals, from the local filesystem.
// ok is false when neither works; err is only returned when the server cannot be
// reached at all.
func (p *Prober) FreeSpace(ctx context.Context, uri, database string) (free int64, ok bool, err error) {
	err = withClient(ctx, uri, func(c *mongo.Client) error {
		if pingErr := c.Database("admin").RunCommand(ctx, bson.D{{Key: "ping", Value: 1}}).Err(); pingErr != nil {
			return fmt.Errorf("ping: %w", pingErr)
		}
		for _, db := range []string{database, "admin"} {
			var stats struct {
				Used  bson.RawValue `bson:"fsUsedSize"`
				Total bson.RawValue `bson:"fsTotalSize"`
			}
			if db == "" || c.Database(db).RunCommand(ctx, bson.D{{Key: "dbStats", Value: 1}}).Decode(&stats) != nil {
				continue
			}
			if n, known := fsFree(stats.Used, stats.Total); known {
				free, ok = n, true
				return nil
			}
		}
		if !loopbackURI(uri) {
			return nil
		}
		path, known := dbPath(ctx, c)
		if !known {
			return nil
		}
		n, statErr := diskspace.Free(path)
		if statErr != nil {
			return nil
		}
		free, ok = int64(min(n, math.MaxInt64)), true
		return nil
	})
	return free, ok, err
}

// fsFree returns total minus used of dbStats' filesystem fields, when both are
// numbers and total is positive.
func fsFree(used, total bson.RawValue) (int64, bool) {
	u, okUsed := numberOf(used)
	t, okTotal := numberOf(total)
	if !okUsed || !okTotal || t <= 0 || u < 0 {
		return 0, false
	}
	return int64(max(t-u, 0)), true
}

// dbPath returns the server's storage.dbPath from getCmdLineOpts (which needs a
// cluster-wide privilege), or false.
func dbPath(ctx context.Context, c *mongo.Client) (string, bool) {
	var res struct {
		Parsed struct {
			Storage struct {
				DBPath string `bson:"dbPath"`
			} `bson:"storage"`
		} `bson:"parsed"`
	}
	if err := c.Database("admin").RunCommand(ctx, bson.D{{Key: "getCmdLineOpts", Value: 1}}).Decode(&res); err != nil {
		return "", false
	}
	path := strings.TrimSpace(res.Parsed.Storage.DBPath)
	return path, path != ""
}

// loopbackURI reports whether every host of the connection string uri is on this
// host: a loopback address (localhost, 127.0.0.0/8, ::1) or a Unix domain socket. SRV
// connection strings never are.
func loopbackURI(uri string) bool {
	if strings.HasPrefix(uri, "mongodb+srv://") {
		return false
	}
	opts := options.Client().ApplyURI(uri)
	if len(opts.Hosts) == 0 {
		return false
	}
	for _, h := range opts.Hosts {
		if strings.HasSuffix(h, ".sock") {
			continue
		}
		host := h
		if hostOnly, _, err := net.SplitHostPort(h); err == nil {
			host = hostOnly
		}
		if strings.EqualFold(host, "localhost") {
			continue
		}
		ip := net.ParseIP(strings.Trim(host, "[]"))
		if ip == nil || !ip.IsLoopback() {
			return false
		}
	}
	return true
}
