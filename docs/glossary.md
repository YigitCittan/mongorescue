# Glossary

The terms MongoRescue uses, in alphabetical order, each with a short definition and a link to the page that defines it in full. Questions rather than words? See the [FAQ](faq.md).

### Active and standby

With [high availability](production.md#high-availability), the instance that holds the *lease* is **active** and does all the work; the others are **standbys** that serve only a health endpoint and take over when the lease expires. [Design](design/ha.md#roles).

### Age encryption

The optional encryption of backup archives with [age](https://age-encryption.org): to one or more public keys (**recipients**, X25519, recommended) or with a passphrase (scrypt). The private half is the **identity**. Archives encrypted this way end in `.age`. [Encryption](encryption.md).

### API key and scope

A token for the REST API, the CLI and MCP, shown once and stored as a SHA-256 hash. Its **scope** limits what it can do: `read`, `operator` (backups, verifications, safe-clone restores) or `admin` (in-place restores, settings, users). [Scopes](api.md#api-key-scopes), [CLI](cli.md).

### Archive

The file one backup writes to storage: the output of `mongodump --archive`, gzip-compressed by default, optionally wrapped in age. An ordinary `mongodump` archive that `mongorestore` can read without MongoRescue. [Encryption: behaviour](encryption.md#behaviour).

### Backup job

A schedule plus settings (which database, which storage target, retention, RPO, copies) that creates backups; a **run** is one execution. Backups can also be started by hand. [Features](features.md).

### Base backup (PITR)

A `mongodump --oplog` of the whole replica set that gives a *PITR chain* its starting point. Restoring to a point in time restores a base, then replays the oplog on top of it. [PITR](pitr.md).

### Chain (PITR)

A continuous run of oplog *chunks* with no gap, starting at a *base backup*. A gap (the oplog rolled past the collector, for example) breaks the chain and a new base is taken; **chain tests** replay a chain regularly to prove it works. [Chain tests](pitr.md#chain-tests), [design](design/pitr.md).

### Copy target

An additional storage target (up to three) a backup is replicated to after the primary write, optionally in another region or account, with its own verification. [Copies in another region](pitr.md#copies-in-another-region), [3-2-1 backups](production.md#3-2-1-backups).

### Delete protection and grace period

Backups deleted in MongoRescue are only **soft deleted** and kept for the grace period (default 7 days) before the purge; lowering a protection also waits out the grace period. It guards against a compromised account, not against someone with direct access to the storage; that is what *Object Lock* is for. [Delete protection](security.md#delete-protection).

### Epoch and fencing

In [high availability](design/ha.md), the **epoch** is a counter in the *lease* that increases at every takeover. **Fencing** is the rule that stops a paused or partitioned old active instance from doing damage: every run records the epoch it started under and checks that it still holds the lease before it starts, before it is marked completed and before background work writes or deletes. [Fencing](design/ha.md#fencing).

### Escrow

Keeping a copy of a secret apart from the system that uses it, so losing the system does not lose the secret. Here: the age identity, passphrase and `secret.key`, stored offline (the *recovery kit* does it for you). Without them, encrypted backups and stored credentials cannot be recovered. [Key management and loss](encryption.md#key-management-and-loss).

### Lease

The small JSON object on the HA storage target that names the active instance, the *epoch* and when it was last renewed. [Lease](design/ha.md#lease).

### Metadata snapshot (metadata backup)

An encrypted copy of MongoRescue's own database (`mongorescue.db`) written to a storage target, so a lost host does not lose your jobs, history and users. [Metadata backups](production.md#metadata-backups).

### Object Lock

An S3 feature that makes object versions undeletable until a retention date. MongoRescue can write every object with a lock in **compliance** mode (nobody can delete early, not even the account root) or **governance** mode (principals with a bypass permission can). [Immutable backups](security.md#immutable-backups-s3-object-lock).

### Oplog and oplog window

The **oplog** is the replica set's log of writes. The **oplog window** is the time between its oldest and newest entry: how far back a restart or a collector outage can reach before the log rolls over. A base backup that takes longer than the window never completes. [Sizing the oplog](pitr.md#sizing-the-oplog).

### PITR (point-in-time recovery) and stream

Restoring a replica set, or some of its databases, to a chosen second. A **PITR stream** is the collector configuration of one replica set connection: it reads the oplog into encrypted chunks and takes base backups on a schedule. The **window** of a stream is the range of times you can restore to. [PITR](pitr.md).

### Pin (legal hold)

Marks a backup as exempt from retention. On an *Object Lock* target a pin also places a legal hold on the archive. [Retention](verification.md#retention), [Object Lock](security.md#immutable-backups-s3-object-lock).

### Recovery kit

An age-encrypted download (Settings → Recovery) holding `secret.key`, the age identities, every storage target's credentials and the location of the latest *metadata snapshot*: everything needed to rebuild MongoRescue on a new host. Store it offline. [Recovery kit](production.md#recovery-kit).

### Recovery readiness

The per-database answer to "could we recover, and how fast?": last good, verified and restore-tested backups, *RPO* with its current age, estimated *RTO*, and whether the keys are escrowed. [API](api.md#recovery-readiness), [production](production.md#rpo-and-rto).

### Restore test

An automated restore of the newest backup into a temporary database, compared with the backup's manifest, then dropped. It proves the archive restores and measures the *RTO*. [Automated restore tests](verification.md#automated-restore-tests).

### RPO and RTO

The **recovery point objective** is how much data you can lose, expressed as how old the newest good backup may be. The **recovery time objective** is how long a restore may take. MongoRescue alerts when a job's RPO is missed and estimates the RTO from restore tests. [RPO and RTO](production.md#rpo-and-rto), [service level objectives](production.md#service-level-objectives).

### Safe clone

The default way to restore: into a **new database** named `<db>_rescue_<timestamp>_<id>` next to the original, which is never modified. You inspect it, then copy back what you need. Overwriting the original (*in place*) is always an explicit opt-in. [CLI restore](cli.md#restore), [FAQ](faq.md#why-does-a-restore-go-into-a-safe-clone-instead-of-the-original-database).

### secret.key

The AES-256 key that encrypts every credential stored in `mongorescue.db`. Lives in the data directory, in `MONGORESCUE_SECRET_KEY`, or wrapped by a KMS. The root of trust: losing it loses the stored credentials. [Data directory](production.md#data-directory), [key wrapping](security.md#wrapping-secretkey-with-a-kms), [FAQ](faq.md#where-does-secretkey-live-and-what-happens-if-i-lose-it).

### Setup code

The one-time code printed in the log on first start (in memory only, new at every restart) that proves you control the host when you create the first administrator. [Authentication](production.md#authentication).

### Storage target and storage scan

A **storage target** is where archives go: local disk or an S3-compatible bucket. A **scan** lists a target and compares its archives with the backup records, to import orphans or find missing archives. [Storage scans](verification.md#storage-scans), [configuration](configuration.md).

### Trusted proxies

The addresses (`security.trusted_proxies`) of the reverse proxies whose `X-Forwarded-*` headers MongoRescue believes. Needed behind any proxy for correct client addresses and `Secure` cookies. [TLS and the reverse proxy](production.md#tls-and-the-reverse-proxy), [Caddy recipe](deployment-recipes.md#caddy-with-automatic-https).

### Two-person rule

With `security.require_second_approver` on, destructive actions (deleting backups or storage targets, shortening retention, creating admin keys and users) become approval requests that a second administrator must approve. [The two-person rule](security.md#the-two-person-rule).

### Verification

Re-reading an archive from storage and comparing it with the checksum recorded when it was written; done after every backup and on a schedule (the *integrity sweep*). Different from a *restore test*, which proves the data loads. [Verification](verification.md).
