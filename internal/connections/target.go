package connections

import "context"

// RestoreTarget is one open connection to the target server of a restore, shared by
// the checks of a restore preflight (implemented by *mongoconn.Target). Close it
// when done. Implementations must honour ctx deadlines and never include the URI's
// credentials in errors.
type RestoreTarget interface {
	// Ping authenticates and returns the server version.
	Ping(ctx context.Context) (ServerInfo, error)
	// DatabaseExists reports whether database exists.
	DatabaseExists(ctx context.Context, database string) (bool, error)
	// ListCollections returns the collections and views of database.
	ListCollections(ctx context.Context, database string) ([]Collection, error)
	// Privileges reports which of actions the user lacks on database, or on every
	// one of collections when collections is not empty.
	Privileges(ctx context.Context, database string, actions, collections []string) (PrivilegeReport, error)
	// FreeSpace returns the free disk space of the server's data filesystem.
	FreeSpace(ctx context.Context, database string) (DiskSpace, error)
	// Close releases the connection.
	Close()
}

// PrivilegeReport is the outcome of a privilege check.
type PrivilegeReport struct {
	// Missing lists the actions the user was not found to hold, sorted.
	Missing []string
	// Certain reports that Missing is definite: the user's roles are all built-in
	// and every privilege was understood. A report that is not certain may list
	// actions a custom role or a partial grant would still allow.
	Certain bool
}

// Disk space sources.
const (
	// DiskSpaceDBStats is free space reported by the server itself (dbStats).
	DiskSpaceDBStats = "dbstats"
	// DiskSpaceLocal is free space of the local filesystem holding the server's
	// dbPath, for a server on a loopback address. A loopback address may be an SSH
	// tunnel or a Docker host network, so it is only indicative.
	DiskSpaceLocal = "local"
)

// DiskSpace is the free disk space of a server's data filesystem.
type DiskSpace struct {
	// Known reports that Free was determined.
	Known bool
	// Free is the free space in bytes.
	Free int64
	// Source is DiskSpaceDBStats or DiskSpaceLocal.
	Source string
}

// Role is a role granted to a user: its name and the database it is defined in.
type Role struct {
	Role string `json:"role"`
	DB   string `json:"db"`
}

// UserRoles describes the authenticated user of a connection.
type UserRoles struct {
	// AuthEnabled is false on a server without access control, which allows
	// everything.
	AuthEnabled bool
	// Roles are the user's roles, including inherited ones.
	Roles []Role
	// AnyAction reports a grant of the anyAction action on any resource.
	AnyAction bool
}

// RoleReporter is implemented by restore targets that can report the roles of their
// user; point-in-time restores check them, since replaying the oplog checks
// privileges per operation.
type RoleReporter interface {
	// UserRoles returns the roles of the connection's user.
	UserRoles(ctx context.Context) (UserRoles, error)
}
