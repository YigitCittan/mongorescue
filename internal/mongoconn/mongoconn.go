// Package mongoconn is the adapter between the connections domain and the official
// MongoDB Go driver. It is the only production package that imports the driver: it
// tests connectivity, lists databases and collections (also for backups of several
// collections), checks the bypassDocumentValidation privilege for restores and runs
// the post-restore commands of a connection against restored clones. Dumps and
// restores still use mongodump/mongorestore.
package mongoconn

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/yigitcittan/mongorescue/internal/connections"
)

// Compile-time check that Prober implements the domain port.
var _ connections.Prober = (*Prober)(nil)

// appName identifies MongoRescue in server logs and currentOp.
const appName = "mongorescue"

// fallbackTimeout bounds server selection and connecting when neither ctx nor the
// connection string sets a limit. It is the driver's default: ten seconds made
// restore tests fail on busy replica set members whose first handshake after a
// large mongorestore took longer, and overrode the URI's own timeouts.
const fallbackTimeout = 30 * time.Second

// Prober opens a short-lived client per call. It is safe for concurrent use.
type Prober struct{}

// New returns a Prober.
func New() *Prober { return &Prober{} }

// Ping connects, authenticates (via the ping command) and reports the server version.
func (p *Prober) Ping(ctx context.Context, uri string) (connections.ServerInfo, error) {
	var info connections.ServerInfo
	err := withClient(ctx, uri, func(c *mongo.Client) error {
		admin := c.Database("admin")
		if err := admin.RunCommand(ctx, bson.D{{Key: "ping", Value: 1}}).Err(); err != nil {
			return fmt.Errorf("ping: %w", err)
		}
		var build struct {
			Version string `bson:"version"`
		}
		if err := admin.RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&build); err != nil {
			return fmt.Errorf("buildInfo: %w", err)
		}
		info.Version = build.Version
		return nil
	})
	return info, err
}

// ListDatabases returns the databases the connection's user may see.
func (p *Prober) ListDatabases(ctx context.Context, uri string) ([]connections.Database, error) {
	var out []connections.Database
	err := withClient(ctx, uri, func(c *mongo.Client) error {
		res, err := c.ListDatabases(ctx, bson.D{}, options.ListDatabases().SetAuthorizedDatabases(true))
		if err != nil {
			return fmt.Errorf("listDatabases: %w", err)
		}
		out = make([]connections.Database, 0, len(res.Databases))
		for _, d := range res.Databases {
			out = append(out, connections.Database{Name: d.Name, SizeBytes: d.SizeOnDisk, Empty: d.Empty})
		}
		return nil
	})
	return out, err
}

// DatabaseExists reports whether database exists on the server at uri (among the
// databases the user is authorized for).
func (p *Prober) DatabaseExists(ctx context.Context, uri, database string) (bool, error) {
	var exists bool
	err := withClient(ctx, uri, func(c *mongo.Client) error {
		names, err := c.ListDatabaseNames(ctx, bson.D{{Key: "name", Value: database}},
			options.ListDatabases().SetAuthorizedDatabases(true))
		if err != nil {
			return fmt.Errorf("listDatabases: %w", err)
		}
		exists = len(names) > 0
		return nil
	})
	return exists, err
}

// unauthorizedCode is the server error code for a missing privilege.
const unauthorizedCode = 13

// DropDatabase drops database on the server at uri. A user without the dropDatabase
// privilege (e.g. readWrite) drops its collections one by one instead.
func (p *Prober) DropDatabase(ctx context.Context, uri, database string) error {
	return withClient(ctx, uri, func(c *mongo.Client) error {
		db := c.Database(database)
		err := db.Drop(ctx)
		if err == nil {
			return nil
		}
		var cmdErr mongo.CommandError
		if !errors.As(err, &cmdErr) || cmdErr.Code != unauthorizedCode {
			return fmt.Errorf("dropDatabase: %w", err)
		}
		names, err := db.ListCollectionNames(ctx, bson.D{}, options.ListCollections().SetAuthorizedCollections(true))
		if err != nil {
			return fmt.Errorf("listCollections: %w", err)
		}
		for _, name := range names {
			if strings.HasPrefix(name, "system.") {
				continue
			}
			if err := db.Collection(name).Drop(ctx); err != nil {
				return fmt.Errorf("drop %s: %w", name, err)
			}
		}
		return nil
	})
}

// ListCollections returns the collections and views of database. nameOnly together
// with authorizedCollections lets users without the listCollections privilege list
// the collections they may read.
func (p *Prober) ListCollections(ctx context.Context, uri, database string) ([]connections.Collection, error) {
	var out []connections.Collection
	err := withClient(ctx, uri, func(c *mongo.Client) error {
		specs, err := c.Database(database).ListCollectionSpecifications(ctx, bson.D{},
			options.ListCollections().SetNameOnly(true).SetAuthorizedCollections(true))
		if err != nil {
			return fmt.Errorf("listCollections: %w", err)
		}
		out = make([]connections.Collection, 0, len(specs))
		for _, s := range specs {
			out = append(out, connections.Collection{Name: s.Name, Type: s.Type})
		}
		return nil
	})
	return out, err
}

// DatabaseSize returns the uncompressed data size of database from dbStats
// (dataSize), close to the size of an uncompressed mongodump archive of it.
func (p *Prober) DatabaseSize(ctx context.Context, uri, database string) (int64, error) {
	var size int64
	err := withClient(ctx, uri, func(c *mongo.Client) error {
		var stats struct {
			DataSize bson.RawValue `bson:"dataSize"`
		}
		if err := c.Database(database).RunCommand(ctx, bson.D{{Key: "dbStats", Value: 1}}).Decode(&stats); err != nil {
			return fmt.Errorf("dbStats: %w", err)
		}
		if n, ok := numberOf(stats.DataSize); ok && n > 0 && n < 1<<62 {
			size = int64(n)
		}
		return nil
	})
	return size, err
}

// privilege is one entry of connectionStatus' authenticatedUserPrivileges.
type privilege struct {
	Resource struct {
		DB          *string `bson:"db"`
		Collection  *string `bson:"collection"`
		AnyResource bool    `bson:"anyResource"`
		Cluster     bool    `bson:"cluster"`
	} `bson:"resource"`
	Actions []string `bson:"actions"`
}

// CanBypassDocumentValidation reports whether the connection's user may write to
// every collection of database without document validation (the
// bypassDocumentValidation action, granted e.g. by the built-in restore role).
// A server without access control allows it.
func (p *Prober) CanBypassDocumentValidation(ctx context.Context, uri, database string) (bool, error) {
	var ok bool
	err := withClient(ctx, uri, func(c *mongo.Client) error {
		var res struct {
			AuthInfo struct {
				Users      []bson.Raw  `bson:"authenticatedUsers"`
				Privileges []privilege `bson:"authenticatedUserPrivileges"`
			} `bson:"authInfo"`
		}
		cmd := bson.D{{Key: "connectionStatus", Value: 1}, {Key: "showPrivileges", Value: true}}
		if err := c.Database("admin").RunCommand(ctx, cmd).Decode(&res); err != nil {
			return fmt.Errorf("connectionStatus: %w", err)
		}
		ok = len(res.AuthInfo.Users) == 0 || grantsBypass(res.AuthInfo.Privileges, database)
		return nil
	})
	return ok, err
}

// grantsBypass reports whether privs allow bypassDocumentValidation on every
// collection of database.
func grantsBypass(privs []privilege, database string) bool {
	for _, p := range privs {
		r := p.Resource
		covers := r.AnyResource ||
			(r.DB != nil && r.Collection != nil && *r.Collection == "" && (*r.DB == "" || *r.DB == database))
		if covers && slices.Contains(p.Actions, "bypassDocumentValidation") {
			return true
		}
	}
	return false
}

// withClient runs fn with a client for uri and always disconnects it.
func withClient(ctx context.Context, uri string, fn func(*mongo.Client) error) (err error) {
	client, err := mongo.Connect(clientOptions(ctx, uri))
	if err != nil {
		// Parse errors may quote parts of the URI; never return them verbatim.
		return errors.New("invalid connection string")
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = client.Disconnect(dctx)
	}()
	return fn(client)
}

// clientOptions returns the options of a client for uri. The connection string's
// own serverSelectionTimeoutMS and connectTimeoutMS are kept, like every other
// option (replicaSet, directConnection, TLS); missing ones default to
// fallbackTimeout. A deadline on ctx caps both.
func clientOptions(ctx context.Context, uri string) *options.ClientOptions {
	opts := options.Client().ApplyURI(uri).SetAppName(appName)
	selection, connect := fallbackTimeout, fallbackTimeout
	if opts.ServerSelectionTimeout != nil && *opts.ServerSelectionTimeout > 0 {
		selection = *opts.ServerSelectionTimeout
	}
	if opts.ConnectTimeout != nil && *opts.ConnectTimeout > 0 {
		connect = *opts.ConnectTimeout
	}
	if deadline, ok := ctx.Deadline(); ok {
		left := max(time.Until(deadline), time.Millisecond)
		selection, connect = min(selection, left), min(connect, left)
	}
	return opts.SetServerSelectionTimeout(selection).SetConnectTimeout(connect)
}
