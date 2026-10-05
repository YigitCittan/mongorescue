package operations

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/restore"
)

// ErrPreflightFailed is returned by StartRestore when a preflight check failed and
// the request does not set "force": true (adapters answer 409 Conflict with the
// checks). The returned error is a *PreflightError.
var ErrPreflightFailed = errors.New("operations: restore preflight failed")

// PreflightError carries the preflight that refused a restore.
type PreflightError struct {
	// Result is the preflight, with at least one failed check.
	Result *models.PreflightResult
}

// Error lists the failed checks; the messages never contain credentials.
func (e *PreflightError) Error() string {
	failed := e.Result.Failed()
	parts := make([]string, 0, len(failed))
	for _, c := range failed {
		parts = append(parts, c.ID+": "+c.Message)
	}
	return fmt.Sprintf("restore refused by its preflight (%s); fix the cause or set \"force\": true to restore anyway", strings.Join(parts, "; "))
}

// Unwrap returns ErrPreflightFailed.
func (e *PreflightError) Unwrap() error { return ErrPreflightFailed }

// RestoreInspector inspects the target server of a restore for preflights and
// verifications (implemented by *mongoconn.Prober). Implementations must never
// include the URI's credentials in errors.
type RestoreInspector interface {
	// OpenTarget opens one client to the server at uri, shared by all the server
	// checks of a preflight; ctx's deadline bounds it.
	OpenTarget(ctx context.Context, uri string) (connections.RestoreTarget, error)
	// Manifest returns the counts and indexes of every collection of database.
	Manifest(ctx context.Context, uri, database string) (*models.Manifest, error)
}

// manifestStore is the part of the metadata store that keeps backup manifests
// (implemented by *store.SQLiteStore).
type manifestStore interface {
	// GetManifest returns the manifest of backup id, or store.ErrNotFound.
	GetManifest(ctx context.Context, id string) (*models.Manifest, error)
}

// Preflight limits.
const (
	// preflightTimeout bounds all the checks of one preflight, which share one
	// connection to the target.
	preflightTimeout = 15 * time.Second
	// maxListedCollections caps the collection names a check message lists.
	maxListedCollections = 20
)

// restoreActions are the privileges mongorestore needs on the target database: it
// creates collections and indexes and inserts documents.
var restoreActions = []string{"createCollection", "createIndex", "insert"}

// usersAndRolesActions are the privileges restoring the users and roles of a
// database needs on it (the userAdmin role grants them).
var usersAndRolesActions = []string{"createRole", "createUser", "dropRole", "dropUser", "grantRole", "revokeRole"}

// PreflightRestore runs the checks StartRestore runs before it starts a restore and
// returns the go/no-go summary, without starting or writing anything. req is the body
// of a restore request; an in-place restore does not need confirm_in_place here (it
// is still needed to start it), but it needs the admin scope exactly like
// StartRestore, so the checks never reveal more than the caller may restore into.
// Users-and-roles and decryption problems are reported as failed checks. Expected
// failures: ErrInvalid, ErrNotFound, ErrConnectionRequired, ErrUnknownConnection and
// auth.ErrForbidden.
//
// A point-in-time request (req.PITR) is checked like StartRestore checks it (admin
// only, safe clones only); a target no plan reaches fails the pitr_chain check, and
// the result carries the plan (PreflightResult.PITR) with an RTO estimate.
func (s *Service) PreflightRestore(ctx context.Context, req models.RestoreRequest) (*models.PreflightResult, error) {
	if req.PITR != nil || len(req.Databases) > 0 {
		if req.PITR == nil {
			return nil, invalid(req.ValidatePITR())
		}
		return s.preflightPITREndpoint(ctx, req)
	}
	if !req.IsSafeClone() {
		// Asking what would happen is no consent; StartRestore still needs it.
		req.ConfirmInPlace = true
	}
	plan, err := s.planRestore(ctx, req, true)
	if err != nil {
		return nil, err
	}
	// The users-and-roles check reports a request the backup cannot satisfy; the
	// namespace is the same without the option.
	probe := plan.req
	probe.RestoreUsersAndRoles = false
	record, err := s.cfg.Restore.Prepare(probe, plan.source)
	if err != nil {
		return nil, public(redact.Text(err.Error()), ErrInvalid, err)
	}
	return s.preflight(ctx, plan.req, plan.source, record.TargetDatabase), nil
}

// preflight runs every check of a restore of source described by req into targetDB.
// It never fails: what cannot be checked is a warning. The server checks share one
// client, bounded by preflightTimeout and by ctx (so a shutdown or a client that
// goes away ends them).
//
// Only these can fail: an unreachable target, a missing decryption key, an existing
// safe clone name, privileges certainly missing, free space reported by the server
// smaller than the archive, and a users-and-roles request the backup cannot satisfy.
// Everything uncertain warns.
func (s *Service) preflight(ctx context.Context, req models.RestoreRequest, source *models.BackupRecord, targetDB string) *models.PreflightResult {
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()
	p := &preflightRun{svc: s, ctx: ctx, req: req, source: source, targetDB: targetDB, res: &models.PreflightResult{OK: true}}
	defer p.close()
	p.connection()
	p.encryption()
	p.serverVersion()
	p.targetDatabase()
	p.privileges()
	p.diskSpace()
	p.collections()
	p.usersAndRoles()
	return p.res
}

// preflightRun holds the state of one preflight.
type preflightRun struct {
	svc      *Service
	ctx      context.Context
	req      models.RestoreRequest
	source   *models.BackupRecord
	targetDB string
	res      *models.PreflightResult

	// target is the open client to the target server; reachable is set when it
	// answered, version is its version.
	target    connections.RestoreTarget
	reachable bool
	version   string

	// extraBytes is written on top of the archive: the oplog a point-in-time
	// restore replays.
	extraBytes int64
}

// inspector returns the inspector, or nil.
func (p *preflightRun) inspector() RestoreInspector { return p.svc.cfg.Inspector }

// close releases the client to the target.
func (p *preflightRun) close() {
	if p.target != nil {
		p.target.Close()
	}
}

// connectionName names the target connection in messages.
func (p *preflightRun) connectionName() string {
	if p.req.TargetConnectionName != "" {
		return p.req.TargetConnectionName
	}
	return p.req.TargetConnectionID
}

// skipServer records check id as not checked when the target cannot be inspected and
// reports whether it did.
func (p *preflightRun) skipServer(id string) bool {
	switch {
	case p.inspector() == nil:
		p.res.Add(id, models.PreflightWarn, "not checked: the target server cannot be inspected in this setup")
	case !p.reachable:
		p.res.Add(id, models.PreflightWarn, "not checked: the target server did not answer")
	default:
		return false
	}
	return true
}

// errText is the redacted text of err for a check message.
func errText(err error) string {
	return redact.Text(err.Error())
}

func (p *preflightRun) connection() {
	if p.inspector() == nil {
		p.res.Add(models.PreflightCheckConnection, models.PreflightWarn, "not checked: the target server cannot be inspected in this setup")
		return
	}
	target, err := p.inspector().OpenTarget(p.ctx, p.req.MongoURI)
	if err != nil {
		p.res.Add(models.PreflightCheckConnection, models.PreflightFail,
			fmt.Sprintf("the target connection %s cannot be opened: %s", p.connectionName(), errText(err)))
		return
	}
	p.target = target
	info, err := target.Ping(p.ctx)
	if err != nil {
		p.res.Add(models.PreflightCheckConnection, models.PreflightFail,
			fmt.Sprintf("the target connection %s does not answer: %s", p.connectionName(), errText(err)))
		return
	}
	p.reachable, p.version = true, info.Version
	p.res.Add(models.PreflightCheckConnection, models.PreflightPass,
		fmt.Sprintf("connected to %s (MongoDB %s)", p.connectionName(), orUnknown(info.Version)))
}

func (p *preflightRun) encryption() {
	encrypted := p.source.Encrypted || strings.HasSuffix(p.source.StorageKey, encryption.FileExtension)
	switch {
	case !encrypted:
		p.res.Add(models.PreflightCheckEncryption, models.PreflightPass, "the backup is not encrypted")
	case p.svc.cfg.Restore.CanDecrypt():
		p.res.Add(models.PreflightCheckEncryption, models.PreflightPass, "the backup is encrypted and a decryption key is configured")
	default:
		p.res.Add(models.PreflightCheckEncryption, models.PreflightFail, "the backup is encrypted and no decryption key is configured: "+restore.KeyRequiredHint)
	}
}

func (p *preflightRun) serverVersion() {
	const id = models.PreflightCheckServerVersion
	if p.skipServer(id) {
		return
	}
	src, srcOK := models.ParseServerVersion(p.source.ServerVersion)
	tgt, tgtOK := models.ParseServerVersion(p.version)
	switch {
	case !srcOK:
		p.res.Add(id, models.PreflightWarn, fmt.Sprintf(
			"the backup's server version is unknown (backups of earlier releases do not record it); the target runs MongoDB %s", orUnknown(p.version)))
		return
	case !tgtOK:
		p.res.Add(id, models.PreflightWarn, fmt.Sprintf("the target server's version is unknown; the backup is from MongoDB %s", p.source.ServerVersion))
		return
	}
	versions := fmt.Sprintf("backup from MongoDB %s, target runs %s", p.source.ServerVersion, p.version)
	// Version differences only warn: mongorestore often restores across versions, and
	// what it rejects depends on the data (index types, collection options).
	switch {
	case tgt.Major < src.Major:
		p.res.Add(id, models.PreflightWarn, versions+": the target is an older major version; mongorestore may still work, but data, indexes or options newer than the target can be rejected, so try a dry run or a safe clone first")
	case tgt.Major > src.Major:
		p.res.Add(id, models.PreflightWarn, versions+": mongorestore supports restoring into the same major version; check the upgrade notes of the versions in between")
	case tgt.Compare(src) < 0:
		p.res.Add(id, models.PreflightWarn, versions+": the target is an older release of the same major version")
	default:
		p.res.Add(id, models.PreflightPass, versions)
	}
}

func (p *preflightRun) targetDatabase() {
	const id = models.PreflightCheckTargetDatabase
	if p.skipServer(id) {
		return
	}
	exists, err := p.target.DatabaseExists(p.ctx, p.targetDB)
	if err != nil {
		p.res.Add(id, models.PreflightWarn, fmt.Sprintf("could not check whether database %s exists: %s", p.targetDB, errText(err)))
		return
	}
	switch {
	case p.req.DryRun:
		p.res.Add(id, models.PreflightPass, fmt.Sprintf("dry run: nothing is written to %s", p.targetDB))
	case !p.req.InPlace() && exists:
		p.res.Add(id, models.PreflightFail, fmt.Sprintf("the safe clone database %s already exists; retry in a moment", p.targetDB))
	case !p.req.InPlace():
		p.res.Add(id, models.PreflightPass, fmt.Sprintf("restores into the new database %s; existing data is untouched", p.targetDB))
	case exists:
		p.res.Add(id, models.PreflightPass, fmt.Sprintf("restores in place into the existing database %s", p.targetDB))
	default:
		p.res.Add(id, models.PreflightPass, fmt.Sprintf("database %s does not exist yet and is created", p.targetDB))
	}
}

func (p *preflightRun) privileges() {
	const id = models.PreflightCheckPrivileges
	if p.skipServer(id) {
		return
	}
	if p.req.DryRun {
		p.res.Add(id, models.PreflightPass, "dry run: no write privileges are needed")
		return
	}
	actions := slices.Clone(restoreActions)
	if p.req.DropTarget && p.req.InPlace() {
		actions = append(actions, "dropCollection")
	}
	if p.req.RestoreUsersAndRoles {
		actions = append(actions, usersAndRolesActions...)
	}
	// Collection-level grants count for the collections the restore writes, when
	// they are known.
	collections, _ := p.restoredCollections()
	report, err := p.target.Privileges(p.ctx, p.targetDB, actions, collections)
	if err != nil {
		p.res.Add(id, models.PreflightWarn, "could not read the privileges of the target connection's user: "+errText(err))
		return
	}
	var core, users []string
	for _, a := range report.Missing {
		if slices.Contains(usersAndRolesActions, a) {
			users = append(users, a)
		} else {
			core = append(core, a)
		}
	}
	switch {
	case len(core) > 0 && report.Certain:
		p.res.Add(id, models.PreflightFail, fmt.Sprintf(
			"the user of connection %s may not %s on database %s; grant e.g. readWrite on it, readWriteAnyDatabase or restore",
			p.connectionName(), strings.Join(core, ", "), p.targetDB))
	case len(core) > 0:
		p.res.Add(id, models.PreflightWarn, fmt.Sprintf(
			"the user of connection %s was not found to hold %s on database %s; custom roles or collection-level grants may still allow it",
			p.connectionName(), strings.Join(core, ", "), p.targetDB))
	case len(users) > 0:
		p.res.Add(id, models.PreflightWarn, fmt.Sprintf(
			"the user of connection %s may not %s on database %s, which restoring users and roles may need (e.g. the restore or userAdmin role)",
			p.connectionName(), strings.Join(users, ", "), p.targetDB))
	default:
		p.res.Add(id, models.PreflightPass, fmt.Sprintf("the user of connection %s may restore into %s", p.connectionName(), p.targetDB))
	}
}

// Free space headroom: restored data takes more room than a compressed archive.
const (
	// plainHeadroom is the warning threshold, in archive sizes, for an uncompressed
	// archive (indexes are built on top of the data).
	plainHeadroom = 2
	// compressedHeadroom is the threshold for a compressed or encrypted archive whose
	// compression is unknown.
	compressedHeadroom = 4
)

func (p *preflightRun) diskSpace() {
	const id = models.PreflightCheckDiskSpace
	if p.req.DryRun {
		p.res.Add(id, models.PreflightPass, "dry run: nothing is written")
		return
	}
	if p.skipServer(id) {
		return
	}
	size := p.source.SizeBytes + p.extraBytes
	space, err := p.target.FreeSpace(p.ctx, p.targetDB)
	free := space.Free
	switch {
	case err != nil:
		p.res.Add(id, models.PreflightWarn, "the free disk space of the target server is unknown: "+errText(err))
		return
	case !space.Known:
		p.res.Add(id, models.PreflightWarn, fmt.Sprintf("the free disk space of the target server is unknown (the archive holds %s)", formatBytes(size)))
		return
	case size <= 0:
		p.res.Add(id, models.PreflightWarn, fmt.Sprintf("the archive size is unknown; the target has %s free", formatBytes(free)))
		return
	}
	headroom := int64(compressedHeadroom)
	if gzip, ok := gzipFromKey(p.source.StorageKey); ok && !gzip {
		headroom = plainHeadroom
	}
	// Only free space the server reports itself can fail the check: the local file
	// system behind a loopback address may belong to a tunnel or a Docker host, and
	// an in-place restore with drop_target frees the space of what it replaces.
	certain := space.Source == connections.DiskSpaceDBStats && (!p.req.InPlace() || !p.req.DropTarget)
	switch {
	case free < size && certain:
		p.res.Add(id, models.PreflightFail, fmt.Sprintf("the target has %s free, less than the %s archive", formatBytes(free), formatBytes(size)))
	case free < size:
		p.res.Add(id, models.PreflightWarn, fmt.Sprintf("the target seems to have %s free, less than the %s archive (%s)",
			formatBytes(free), formatBytes(size), diskSpaceCaveat(space.Source)))
	case (free-p.extraBytes)/headroom < p.source.SizeBytes:
		p.res.Add(id, models.PreflightWarn, fmt.Sprintf(
			"the target has %s free for a %s archive; restored data and indexes usually take more room than the archive", formatBytes(free), formatBytes(size)))
	default:
		p.res.Add(id, models.PreflightPass, fmt.Sprintf("the target has %s free for a %s archive", formatBytes(free), formatBytes(size)))
	}
}

func (p *preflightRun) collections() {
	const id = models.PreflightCheckCollections
	switch {
	case !p.req.InPlace():
		p.res.Add(id, models.PreflightPass, "a safe clone restores into a new database; no existing collection is replaced")
		return
	case p.req.DryRun:
		p.res.Add(id, models.PreflightPass, "dry run: no collection is replaced")
		return
	}
	if p.skipServer(id) {
		return
	}
	list, err := p.target.ListCollections(p.ctx, p.targetDB)
	if err != nil {
		p.res.Add(id, models.PreflightWarn, fmt.Sprintf("could not list the collections of %s: %s", p.targetDB, errText(err)))
		return
	}
	var existing []string
	for _, c := range list {
		if !strings.HasPrefix(c.Name, "system.") {
			existing = append(existing, c.Name)
		}
	}
	slices.Sort(existing)
	if len(existing) == 0 {
		p.res.Add(id, models.PreflightPass, fmt.Sprintf("database %s holds no collections; nothing is replaced", p.targetDB))
		return
	}
	restored, known := p.restoredCollections()
	affected := existing
	if known {
		affected = nil
		for _, name := range existing {
			if slices.Contains(restored, name) {
				affected = append(affected, name)
			}
		}
	}
	switch {
	case len(affected) == 0:
		p.res.Add(id, models.PreflightPass, fmt.Sprintf("none of the %d existing collection(s) of %s is in the restore; they are kept", len(existing), p.targetDB))
	case !known && p.req.DropTarget:
		p.res.Add(id, models.PreflightWarn, fmt.Sprintf(
			"the backup's collection list is unknown: any of the %d existing collection(s) of %s may be dropped and replaced: %s",
			len(affected), p.targetDB, listNames(affected)))
	case !known:
		p.res.Add(id, models.PreflightWarn, fmt.Sprintf(
			"the backup's collection list is unknown: any of the %d existing collection(s) of %s may receive its documents: %s",
			len(affected), p.targetDB, listNames(affected)))
	case p.req.DropTarget:
		p.res.Add(id, models.PreflightWarn, fmt.Sprintf("%d existing collection(s) of %s are dropped and replaced: %s",
			len(affected), p.targetDB, listNames(affected)))
	default:
		p.res.Add(id, models.PreflightWarn, fmt.Sprintf(
			"%d existing collection(s) of %s receive the backup's documents without being dropped (documents with the same _id fail the restore): %s",
			len(affected), p.targetDB, listNames(affected)))
	}
}

// restoredCollections returns the collections the restore writes: the selection, or
// else the backup's manifest, or else the collections its backup was filtered to.
// known is false when none of them is available.
func (p *preflightRun) restoredCollections() (names []string, known bool) {
	if selected := trimmedNonEmpty(p.req.SelectedCollections); len(selected) > 0 {
		return selected, true
	}
	if m := p.svc.manifest(p.ctx, p.source); m != nil {
		for _, c := range m.Collections {
			names = append(names, c.Name)
		}
		return names, true
	}
	if filtered := trimmedNonEmpty(p.source.Collections); len(filtered) > 0 {
		return filtered, true
	}
	return nil, false
}

func (p *preflightRun) usersAndRoles() {
	const id = models.PreflightCheckUsersAndRoles
	if !p.req.RestoreUsersAndRoles {
		p.res.Add(id, models.PreflightPass, "users and roles are not restored")
		return
	}
	if err := p.req.ValidateUsersAndRoles(p.source); err != nil {
		p.res.Add(id, models.PreflightFail, redact.Text(err.Error()))
		return
	}
	p.res.Add(id, models.PreflightWarn, fmt.Sprintf(
		"every user and role defined on %s is replaced with the ones in the backup; users and roles created since are removed", p.source.Database))
}

// diskSpaceCaveat says why a free space shortage only warns.
func diskSpaceCaveat(source string) string {
	if source == connections.DiskSpaceLocal {
		return "measured on this host's file system for a server on a loopback address, which may be a tunnel or a container network"
	}
	return "drop_target frees the space of the collections it replaces"
}

// manifest returns the manifest of backup source, or nil when it has none or the
// store does not keep manifests.
func (s *Service) manifest(ctx context.Context, source *models.BackupRecord) *models.Manifest {
	if source.Manifest != nil {
		return source.Manifest
	}
	ms, ok := s.cfg.Store.(manifestStore)
	if !ok || !source.HasManifest {
		return nil
	}
	m, err := ms.GetManifest(ctx, source.ID)
	if err != nil {
		return nil
	}
	return m
}

// trimmedNonEmpty returns the trimmed, non-blank entries of names.
func trimmedNonEmpty(names []string) []string {
	var out []string
	for _, n := range names {
		if t := strings.TrimSpace(n); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// listNames joins names, listing at most maxListedCollections of them.
func listNames(names []string) string {
	if len(names) <= maxListedCollections {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:maxListedCollections], ", ") + fmt.Sprintf(" (+%d more)", len(names)-maxListedCollections)
}

// orUnknown returns s, or "unknown" for "".
func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// formatBytes formats a byte count with binary units.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
