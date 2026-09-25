// Package mongoconn is the adapter between the connections domain and the official
// MongoDB Go driver. It is the only production package that imports the driver: it
// tests connectivity and lists databases and collections. Dumps and restores still use
// mongodump/mongorestore.
package mongoconn

import (
	"context"
	"errors"
	"fmt"
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

// fallbackTimeout bounds server selection when ctx carries no deadline.
const fallbackTimeout = 10 * time.Second

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

// ListCollections returns the collections and views of database.
func (p *Prober) ListCollections(ctx context.Context, uri, database string) ([]connections.Collection, error) {
	var out []connections.Collection
	err := withClient(ctx, uri, func(c *mongo.Client) error {
		specs, err := c.Database(database).ListCollectionSpecifications(ctx, bson.D{},
			options.ListCollections().SetAuthorizedCollections(true))
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

// withClient runs fn with a client for uri and always disconnects it.
func withClient(ctx context.Context, uri string, fn func(*mongo.Client) error) (err error) {
	timeout := fallbackTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = max(time.Until(deadline), time.Millisecond)
	}
	opts := options.Client().ApplyURI(uri).
		SetAppName(appName).
		SetServerSelectionTimeout(timeout).
		SetConnectTimeout(timeout)
	client, err := mongo.Connect(opts)
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
