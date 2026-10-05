package mongoconn

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"slices"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/diskspace"
)

// Compile-time check that Target implements the domain port.
var _ connections.RestoreTarget = (*Target)(nil)

// Target is one client to the target server of a restore, shared by the checks of a
// preflight. It is safe for concurrent use; Close disconnects it.
type Target struct {
	client *mongo.Client
	uri    string
}

// OpenTarget returns a client for uri. The connection string's own timeouts apply,
// capped by ctx's deadline, which also bounds every call made with it.
func (p *Prober) OpenTarget(ctx context.Context, uri string) (connections.RestoreTarget, error) {
	client, err := mongo.Connect(clientOptions(ctx, uri))
	if err != nil {
		// Parse errors may quote parts of the URI; never return them verbatim.
		return nil, errors.New("invalid connection string")
	}
	return &Target{client: client, uri: uri}, nil
}

// Close disconnects the client.
func (t *Target) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = t.client.Disconnect(ctx)
}

// Ping authenticates (via the ping command) and reports the server version.
func (t *Target) Ping(ctx context.Context) (connections.ServerInfo, error) {
	admin := t.client.Database("admin")
	if err := admin.RunCommand(ctx, bson.D{{Key: "ping", Value: 1}}).Err(); err != nil {
		return connections.ServerInfo{}, fmt.Errorf("ping: %w", err)
	}
	return connections.ServerInfo{Version: serverVersion(ctx, t.client)}, nil
}

// DatabaseExists reports whether database exists (among the authorized databases).
func (t *Target) DatabaseExists(ctx context.Context, database string) (bool, error) {
	names, err := t.client.ListDatabaseNames(ctx, bson.D{{Key: "name", Value: database}},
		options.ListDatabases().SetAuthorizedDatabases(true))
	if err != nil {
		return false, fmt.Errorf("listDatabases: %w", err)
	}
	return len(names) > 0, nil
}

// ListCollections returns the collections and views of database.
func (t *Target) ListCollections(ctx context.Context, database string) ([]connections.Collection, error) {
	specs, err := t.client.Database(database).ListCollectionSpecifications(ctx, bson.D{},
		options.ListCollections().SetNameOnly(true).SetAuthorizedCollections(true))
	if err != nil {
		return nil, fmt.Errorf("listCollections: %w", err)
	}
	out := make([]connections.Collection, 0, len(specs))
	for _, s := range specs {
		out = append(out, connections.Collection{Name: s.Name, Type: s.Type})
	}
	return out, nil
}

// Compile-time check that Target reports its user's roles.
var _ connections.RoleReporter = (*Target)(nil)

// UserRoles returns the roles of the connection's user (connectionStatus) and
// whether a privilege grants anyAction on any resource.
func (t *Target) UserRoles(ctx context.Context) (connections.UserRoles, error) {
	var res struct {
		AuthInfo struct {
			Users      []bson.Raw  `bson:"authenticatedUsers"`
			Roles      []roleRef   `bson:"authenticatedUserRoles"`
			Privileges []privilege `bson:"authenticatedUserPrivileges"`
		} `bson:"authInfo"`
	}
	cmd := bson.D{{Key: "connectionStatus", Value: 1}, {Key: "showPrivileges", Value: true}}
	if err := t.client.Database("admin").RunCommand(ctx, cmd).Decode(&res); err != nil {
		return connections.UserRoles{}, fmt.Errorf("connectionStatus: %w", err)
	}
	out := connections.UserRoles{AuthEnabled: len(res.AuthInfo.Users) > 0}
	for _, r := range res.AuthInfo.Roles {
		out.Roles = append(out.Roles, connections.Role{Role: r.Role, DB: r.DB})
	}
	for _, p := range res.AuthInfo.Privileges {
		if p.Resource.AnyResource && slices.Contains(p.Actions, "anyAction") {
			out.AnyAction = true
		}
	}
	return out, nil
}

// roleRef is one entry of connectionStatus' authenticatedUserRoles.
type roleRef struct {
	Role string `bson:"role"`
	DB   string `bson:"db"`
}

// Privileges reports which of actions the connection's user lacks on database (see
// privilegeReport). A server without access control grants everything.
func (t *Target) Privileges(ctx context.Context, database string, actions, collections []string) (connections.PrivilegeReport, error) {
	var res struct {
		AuthInfo struct {
			Users      []bson.Raw  `bson:"authenticatedUsers"`
			Roles      []roleRef   `bson:"authenticatedUserRoles"`
			Privileges []privilege `bson:"authenticatedUserPrivileges"`
		} `bson:"authInfo"`
	}
	cmd := bson.D{{Key: "connectionStatus", Value: 1}, {Key: "showPrivileges", Value: true}}
	if err := t.client.Database("admin").RunCommand(ctx, cmd).Decode(&res); err != nil {
		return connections.PrivilegeReport{}, fmt.Errorf("connectionStatus: %w", err)
	}
	if len(res.AuthInfo.Users) == 0 {
		return connections.PrivilegeReport{Certain: true}, nil
	}
	return privilegeReport(res.AuthInfo.Privileges, res.AuthInfo.Roles, database, actions, collections), nil
}

// builtinRolesAnyDB are the built-in roles that exist in every database.
var builtinRolesAnyDB = []string{"read", "readWrite", "dbAdmin", "dbOwner", "userAdmin"}

// builtinRolesAdmin are the built-in roles that exist in the admin database only.
var builtinRolesAdmin = []string{
	"readAnyDatabase", "readWriteAnyDatabase", "userAdminAnyDatabase", "dbAdminAnyDatabase",
	"clusterAdmin", "clusterManager", "clusterMonitor", "hostManager", "backup", "restore",
	"root", "__system", "enableSharding", "directShardOperations", "__queryableBackup", "searchCoordinator",
}

// builtinRole reports whether r is a built-in MongoDB role.
func builtinRole(r roleRef) bool {
	return slices.Contains(builtinRolesAnyDB, r.Role) || (r.DB == "admin" && slices.Contains(builtinRolesAdmin, r.Role))
}

// privilegeReport returns the entries of actions privs do not grant on every
// collection of database, or, when collections is not empty, on each of those
// collections (a database-wide grant or a grant on the collection itself). The
// report is certain only when every role is built-in, every privilege resource is a
// database, collection or any-resource one, and no partial grant (a collection-level
// grant of a missing action, which the collection list may not fully cover) could
// still allow a missing action.
func privilegeReport(privs []privilege, roles []roleRef, database string, actions, collections []string) connections.PrivilegeReport {
	granted := grantedOn(privs, database)
	certain := true
	for _, r := range roles {
		if !builtinRole(r) {
			certain = false
		}
	}
	partial := map[string]bool{}
	for _, p := range privs {
		r := p.Resource
		switch {
		case r.AnyResource, r.Cluster:
		case r.DB == nil || r.Collection == nil:
			// system_buckets and other resources this check does not model.
			certain = false
		case *r.Collection != "" && (*r.DB == "" || *r.DB == database):
			for _, a := range p.Actions {
				partial[a] = true
			}
		}
	}
	var missing []string
	for _, a := range actions {
		if granted[a] || slices.Contains(missing, a) {
			continue
		}
		if len(collections) > 0 && coversCollections(privs, database, a, collections) {
			continue
		}
		missing = append(missing, a)
		if partial[a] {
			certain = false
		}
	}
	slices.Sort(missing)
	return connections.PrivilegeReport{Missing: missing, Certain: certain}
}

// coversCollections reports whether privs grant action on each of collections of
// database (each by a grant on the collection itself or on the whole database).
func coversCollections(privs []privilege, database, action string, collections []string) bool {
	for _, coll := range collections {
		ok := false
		for _, p := range privs {
			r := p.Resource
			if r.DB == nil || r.Collection == nil || (*r.DB != "" && *r.DB != database) {
				continue
			}
			if (*r.Collection == "" || *r.Collection == coll) && slices.Contains(p.Actions, action) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// FreeSpace returns the free disk space of the filesystem the server stores
// database on, when it is knowable: from dbStats (fsTotalSize minus fsUsedSize, on
// database or else on admin; Source DiskSpaceDBStats) or, for a server on a loopback
// address whose dbPath getCmdLineOpts reveals, from the local filesystem (Source
// DiskSpaceLocal, only indicative). Known is false when neither works.
func (t *Target) FreeSpace(ctx context.Context, database string) (connections.DiskSpace, error) {
	for _, db := range []string{database, "admin"} {
		var stats struct {
			Used  bson.RawValue `bson:"fsUsedSize"`
			Total bson.RawValue `bson:"fsTotalSize"`
		}
		if db == "" || t.client.Database(db).RunCommand(ctx, bson.D{{Key: "dbStats", Value: 1}}).Decode(&stats) != nil {
			continue
		}
		if n, known := fsFree(stats.Used, stats.Total); known {
			return connections.DiskSpace{Known: true, Free: n, Source: connections.DiskSpaceDBStats}, nil
		}
	}
	if !loopbackURI(t.uri) {
		return connections.DiskSpace{}, nil
	}
	path, known := dbPath(ctx, t.client)
	if !known {
		return connections.DiskSpace{}, nil
	}
	n, err := diskspace.Free(path)
	if err != nil {
		return connections.DiskSpace{}, nil
	}
	return connections.DiskSpace{Known: true, Free: int64(min(n, math.MaxInt64)), Source: connections.DiskSpaceLocal}, nil
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
