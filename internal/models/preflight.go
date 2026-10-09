package models

import (
	"cmp"
	"strconv"
	"strings"
	"time"
)

// PreflightStatus is the outcome of one restore preflight check.
type PreflightStatus string

// Preflight check outcomes.
const (
	// PreflightPass means the check found nothing in the way of the restore.
	PreflightPass PreflightStatus = "pass"
	// PreflightWarn means the restore can run, but something deserves attention
	// (data that will be replaced, a fact that could not be checked). Warnings never
	// block a restore.
	PreflightWarn PreflightStatus = "warn"
	// PreflightFail means the restore is expected to fail or to do harm; it is refused
	// unless the request sets "force": true.
	PreflightFail PreflightStatus = "fail"
)

// Preflight check IDs, in the order the checks run.
const (
	// PreflightCheckConnection checks that the target server answers.
	PreflightCheckConnection = "connection"
	// PreflightCheckEncryption checks that an encrypted backup can be decrypted.
	PreflightCheckEncryption = "encryption"
	// PreflightCheckServerVersion compares the target server's version with the
	// version of the server the backup was taken from.
	PreflightCheckServerVersion = "server_version"
	// PreflightCheckTargetDatabase checks the target database: whether it exists and,
	// for a safe clone, that the clone name is free.
	PreflightCheckTargetDatabase = "target_database"
	// PreflightCheckPrivileges checks that the target connection's user may restore.
	PreflightCheckPrivileges = "privileges"
	// PreflightCheckDiskSpace compares the archive size with the free disk space of
	// the target server, where it is known.
	PreflightCheckDiskSpace = "disk_space"
	// PreflightCheckCollections lists the existing collections an in-place restore
	// writes into or replaces.
	PreflightCheckCollections = "collections"
	// PreflightCheckUsersAndRoles checks the users-and-roles option against the backup.
	PreflightCheckUsersAndRoles = "users_and_roles"
	// PreflightCheckPITRChain checks that a point-in-time target has a base and an
	// unbroken, verified chain of oplog chunks up to it, with keys for their
	// encryption.
	PreflightCheckPITRChain = "pitr_chain"
	// PreflightCheckToolsVersion checks that mongorestore is recent enough for
	// point-in-time restores (mongotools.MinPITRToolsVersion).
	PreflightCheckToolsVersion = "tools_version"
	// PreflightCheckPostRestore lists the post-restore commands of the target
	// connection the restore runs against its clones (only when it has some).
	PreflightCheckPostRestore = "post_restore"
	// PreflightCheckSource checks that the archive the restore reads exists on the
	// storage target chosen for it (the primary or a copy). It runs only for
	// backups with copies.
	PreflightCheckSource = "source"
)

// PITRPreflight summarises the plan of a point-in-time restore for its preflight.
type PITRPreflight struct {
	// BaseID is the base backup the plan chose, BaseStartedAt when it was taken and
	// BaseConsistentAt the wall-clock time of its consistent point (T_after).
	BaseID           string    `json:"base_id"`
	BaseStartedAt    time.Time `json:"base_started_at"`
	BaseConsistentAt time.Time `json:"base_consistent_at"`
	// BaseBytes and OplogBytes are the stored sizes of the base and of the chunks.
	BaseBytes  int64 `json:"base_bytes"`
	OplogBytes int64 `json:"oplog_bytes"`
	// Chunks counts the chunks replayed and UnverifiedChunks those never verified.
	Chunks           int `json:"chunks"`
	UnverifiedChunks int `json:"unverified_chunks"`
	// TargetTime is the moment restored to and Limit the --oplogLimit position, as
	// "<t>:<i>".
	TargetTime time.Time `json:"target_time"`
	Limit      string    `json:"limit"`
	// CloneSuffix is appended to every restored database's name.
	CloneSuffix string `json:"clone_suffix"`
	// EstimatedSeconds is the estimated duration of the restore (RTO); EstimateFrom
	// says where its rates come from: the stream's recent restores and chain tests
	// ("measured"), a chain test of an earlier release ("chain_test") or default
	// rates ("default"); see PITREstimate.
	EstimatedSeconds float64 `json:"estimated_seconds"`
	EstimateFrom     string  `json:"estimate_from"`
}

// PreflightCheck is the result of one restore preflight check.
type PreflightCheck struct {
	// ID identifies the check (see the PreflightCheck* constants).
	ID string `json:"id"`
	// Status is pass, warn or fail.
	Status PreflightStatus `json:"status"`
	// Message explains the outcome; it never contains credentials.
	Message string `json:"message"`
}

// PreflightResult is the go/no-go summary of a restore preflight: OK is false when at
// least one check failed.
type PreflightResult struct {
	// OK reports that no check failed (warnings allowed).
	OK bool `json:"ok"`
	// Checks lists every check in a stable order.
	Checks []PreflightCheck `json:"checks"`
	// PITR summarises the plan of a point-in-time restore; nil for the restore of a
	// backup or when no plan was found (see the pitr_chain check).
	PITR *PITRPreflight `json:"pitr,omitempty"`
	// PostRestore lists the post-restore commands the restore runs (status
	// "planned"), per restored database; empty when the target connection has
	// none or the restore runs none (see the post_restore check).
	PostRestore []PostRestoreResult `json:"post_restore,omitempty"`
}

// Add appends a check and keeps OK in step with it.
func (r *PreflightResult) Add(id string, status PreflightStatus, message string) {
	r.Checks = append(r.Checks, PreflightCheck{ID: id, Status: status, Message: message})
	r.OK = r.Failed() == nil
}

// Failed returns the failed checks.
func (r *PreflightResult) Failed() []PreflightCheck {
	if r == nil {
		return nil
	}
	var out []PreflightCheck
	for _, c := range r.Checks {
		if c.Status == PreflightFail {
			out = append(out, c)
		}
	}
	return out
}

// Warnings returns the checks that warned.
func (r *PreflightResult) Warnings() []PreflightCheck {
	if r == nil {
		return nil
	}
	var out []PreflightCheck
	for _, c := range r.Checks {
		if c.Status == PreflightWarn {
			out = append(out, c)
		}
	}
	return out
}

// Check returns the check id, or nil.
func (r *PreflightResult) Check(id string) *PreflightCheck {
	if r == nil {
		return nil
	}
	for i := range r.Checks {
		if r.Checks[i].ID == id {
			return &r.Checks[i]
		}
	}
	return nil
}

// RestoreVerificationStatus is the outcome of comparing a restored database with the
// manifest captured at backup time.
type RestoreVerificationStatus string

// Restore verification outcomes.
const (
	// RestoreVerificationPassed means every restored collection matches the manifest.
	RestoreVerificationPassed RestoreVerificationStatus = "passed"
	// RestoreVerificationFailed means a collection is missing, holds a document count
	// outside the captured range or lacks an index of the backup.
	RestoreVerificationFailed RestoreVerificationStatus = "failed"
	// RestoreVerificationSkipped means the comparison could not run: a dry run, a
	// backup without a manifest or a target that could not be inspected (see Notes).
	RestoreVerificationSkipped RestoreVerificationStatus = "skipped"
)

// RestoreVerification records the comparison of a restored database with its backup's
// manifest (RestoreRequest.VerifyRestore).
type RestoreVerification struct {
	// Status is passed, failed or skipped.
	Status RestoreVerificationStatus `json:"status"`
	// Mismatches lists the differences that make the restore untrustworthy.
	Mismatches []string `json:"mismatches,omitempty"`
	// Notes lists harmless differences and why a verification was skipped.
	Notes []string `json:"notes,omitempty"`
	// Collections counts the collections compared.
	Collections int `json:"collections"`
	// CheckedAt is when the comparison ran.
	CheckedAt time.Time `json:"checked_at"`
}

// ServerVersion is a parsed MongoDB server version ("7.0.12").
type ServerVersion struct {
	// Major, Minor and Patch are the numeric components; missing ones are 0.
	Major, Minor, Patch int
}

// ParseServerVersion parses the leading numeric components of a buildInfo version
// such as "8.0.4" or "7.0.12-rc0". ok is false when the major component is missing.
func ParseServerVersion(s string) (v ServerVersion, ok bool) {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "-+ "); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	nums := make([]int, 3)
	for i := 0; i < len(parts) && i < 3; i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			if i == 0 {
				return ServerVersion{}, false
			}
			break
		}
		nums[i] = n
	}
	return ServerVersion{Major: nums[0], Minor: nums[1], Patch: nums[2]}, true
}

// Compare returns -1, 0 or 1 when v is older than, equal to or newer than o, by
// major and minor version (patch releases are compatible).
func (v ServerVersion) Compare(o ServerVersion) int {
	if v.Major != o.Major {
		return cmp.Compare(v.Major, o.Major)
	}
	return cmp.Compare(v.Minor, o.Minor)
}
