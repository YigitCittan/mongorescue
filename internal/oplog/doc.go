// Package oplog reads, rewrites and packages MongoDB oplog entries for point-in-time
// recovery (docs/design/pitr.md).
//
// It has three parts, all streaming one entry at a time:
//
//   - Scanner walks the length-prefixed BSON documents of an oplog chunk as it is
//     written, passing the bytes through unchanged, and records the entry count and
//     the first and last (ts, t). The collector puts it in the chunk pipeline, and the
//     integrity sweep uses it to walk stored chunks.
//   - Filter keeps the entries of the selected databases, renames their namespaces,
//     drops what must not be replayed and stops before the --oplogLimit position.
//   - ArchiveWriter wraps the filtered entries in a synthetic mongodump archive that
//     holds only the "oplog" namespace (database ""), which
//     mongorestore --archive --oplogReplay reads from stdin.
//
// # Why the oplog is rewritten
//
// Restores go into a safe clone (<db>_rescue_<timestamp>_<id>) by default, so a restore
// never overwrites the data it is meant to rescue. mongorestore refuses --nsFrom/--nsTo,
// --nsInclude and --nsExclude together with --oplogReplay, so replaying an unmodified
// oplog after a safe-clone restore would apply every write to the source databases,
// the very data the clone protects. The Filter therefore rewrites every namespace an
// entry carries (ns, the arguments of DDL commands, the operations nested in applyOps
// and transactions) to the clone, drops the entries of databases that were not
// selected and of admin, config and local, and refuses what it cannot rewrite safely:
// a rename across the selection boundary or a command it does not know. A refused
// stream fails the restore instead of touching a database the user did not choose.
//
// The package may import go.mongodb.org/mongo-driver/v2/bson, the driver's codec,
// and nothing else from the driver: it never opens a connection (AGENTS.md).
package oplog
