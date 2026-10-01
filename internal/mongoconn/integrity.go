package mongoconn

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// Manifest returns the manifest of database: every collection (views and system
// collections excluded) with its estimated document count and index
// specifications. The count is metadata-based (estimatedDocumentCount), so it is
// cheap on large collections.
func (p *Prober) Manifest(ctx context.Context, uri, database string) (*models.Manifest, error) {
	var out *models.Manifest
	err := withClient(ctx, uri, func(c *mongo.Client) error {
		db := c.Database(database)
		specs, err := db.ListCollectionSpecifications(ctx, bson.D{}, options.ListCollections().SetAuthorizedCollections(true))
		if err != nil {
			return fmt.Errorf("listCollections: %w", err)
		}
		m := &models.Manifest{CapturedAt: time.Now().UTC(), Collections: make([]models.CollectionManifest, 0, len(specs))}
		for _, s := range specs {
			if s.Type != "collection" || strings.HasPrefix(s.Name, "system.") {
				continue
			}
			coll := db.Collection(s.Name)
			n, err := coll.EstimatedDocumentCount(ctx)
			if err != nil {
				return fmt.Errorf("count %s: %w", s.Name, err)
			}
			indexes, err := indexSpecs(ctx, coll)
			if err != nil {
				return fmt.Errorf("listIndexes %s: %w", s.Name, err)
			}
			m.Collections = append(m.Collections, models.CollectionManifest{Name: s.Name, DocumentsMin: n, DocumentsMax: n, Indexes: indexes})
		}
		m.Normalize()
		out = m
		return nil
	})
	return out, err
}

// indexDoc is the part of a listIndexes document a manifest records.
type indexDoc struct {
	Name               string        `bson:"name"`
	Key                bson.D        `bson:"key"`
	Unique             bool          `bson:"unique"`
	Sparse             bool          `bson:"sparse"`
	ExpireAfterSeconds bson.RawValue `bson:"expireAfterSeconds"`
}

// indexSpecs lists the index specifications of coll.
func indexSpecs(ctx context.Context, coll *mongo.Collection) ([]models.IndexSpec, error) {
	cur, err := coll.Indexes().List(ctx)
	if err != nil {
		return nil, err
	}
	var docs []indexDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	out := make([]models.IndexSpec, 0, len(docs))
	for _, d := range docs {
		spec := models.IndexSpec{Name: d.Name, Keys: canonicalKeys(d.Key), Unique: d.Unique, Sparse: d.Sparse}
		if ttl, ok := numberOf(d.ExpireAfterSeconds); ok {
			v := int64(ttl)
			spec.ExpireAfterSeconds = &v
		}
		out = append(out, spec)
	}
	return out, nil
}

// canonicalKeys formats an index key pattern as "field:value,..." with numbers in
// their shortest form, so 1, 1.0 and NumberLong(1) compare equal.
func canonicalKeys(keys bson.D) string {
	parts := make([]string, 0, len(keys))
	for _, e := range keys {
		parts = append(parts, e.Key+":"+keyValue(e.Value))
	}
	return strings.Join(parts, ",")
}

// keyValue formats one index key value.
func keyValue(v any) string {
	switch x := v.(type) {
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

// numberOf returns a BSON number as a float64.
func numberOf(v bson.RawValue) (float64, bool) {
	switch v.Type {
	case bson.TypeInt32:
		return float64(v.Int32()), true
	case bson.TypeInt64:
		return float64(v.Int64()), true
	case bson.TypeDouble:
		return v.Double(), true
	default:
		return 0, false
	}
}

// restoreTestActions are the privileges a restore test needs on its temporary
// database: mongorestore creates collections and indexes and inserts documents,
// the comparison counts and lists them, and the database is dropped afterwards.
var restoreTestActions = []string{"createCollection", "insert", "createIndex", "find", "listCollections", "listIndexes"}

// RestoreTestPrivileges returns the actions the connection's user lacks on the
// whole of database to run a restore test into it (restoreTestActions plus
// dropDatabase or dropCollection), or none. A server without access control
// grants everything.
func (p *Prober) RestoreTestPrivileges(ctx context.Context, uri, database string) ([]string, error) {
	var missing []string
	err := withClient(ctx, uri, func(c *mongo.Client) error {
		privs, users, err := userPrivileges(ctx, c)
		if err != nil {
			return err
		}
		if users == 0 {
			return nil
		}
		missing = missingActions(privs, database)
		return nil
	})
	return missing, err
}

// userPrivileges returns the authenticated users' privileges and the number of
// authenticated users.
func userPrivileges(ctx context.Context, c *mongo.Client) ([]privilege, int, error) {
	var res struct {
		AuthInfo struct {
			Users      []bson.Raw  `bson:"authenticatedUsers"`
			Privileges []privilege `bson:"authenticatedUserPrivileges"`
		} `bson:"authInfo"`
	}
	cmd := bson.D{{Key: "connectionStatus", Value: 1}, {Key: "showPrivileges", Value: true}}
	if err := c.Database("admin").RunCommand(ctx, cmd).Decode(&res); err != nil {
		return nil, 0, fmt.Errorf("connectionStatus: %w", err)
	}
	return res.AuthInfo.Privileges, len(res.AuthInfo.Users), nil
}

// missingActions returns the restore test actions privs do not grant on every
// collection of database.
func missingActions(privs []privilege, database string) []string {
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
	var missing []string
	for _, a := range restoreTestActions {
		if !granted[a] {
			missing = append(missing, a)
		}
	}
	if !granted["dropDatabase"] && !granted["dropCollection"] {
		missing = append(missing, "dropDatabase")
	}
	slices.Sort(missing)
	return missing
}
