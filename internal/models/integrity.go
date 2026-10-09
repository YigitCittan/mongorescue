package models

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// VerificationStatus is the outcome of re-reading a stored archive and comparing its
// SHA-256 with the checksum recorded while it was written.
type VerificationStatus string

// Verification outcomes.
const (
	// VerificationOK means the stored bytes match the recorded checksum (and, when a
	// decrypt check was requested, the age stream authenticated to its end).
	VerificationOK VerificationStatus = "ok"
	// VerificationMismatch means the stored bytes differ from the recorded checksum or
	// size, or the age stream failed to authenticate: the archive is damaged.
	VerificationMismatch VerificationStatus = "mismatch"
	// VerificationError means the archive could not be read to the end (storage
	// unreachable, object missing, cancelled); nothing is known about its integrity.
	VerificationError VerificationStatus = "error"
)

// VerifyOverride is a job's choice for post-backup verification.
type VerifyOverride string

// Post-backup verification overrides.
const (
	// VerifyInherit follows the integrity.verify_after_backup setting (the default).
	VerifyInherit VerifyOverride = ""
	// VerifyOn always verifies the job's backups after upload.
	VerifyOn VerifyOverride = "on"
	// VerifyOff never verifies the job's backups after upload.
	VerifyOff VerifyOverride = "off"
)

// Valid reports whether v is one of the defined overrides.
func (v VerifyOverride) Valid() bool {
	return v == VerifyInherit || v == VerifyOn || v == VerifyOff
}

// Resolve returns whether to verify, given the global default def.
func (v VerifyOverride) Resolve(def bool) bool {
	switch v {
	case VerifyOn:
		return true
	case VerifyOff:
		return false
	default:
		return def
	}
}

// Manifest is a lightweight description of a backed-up database, captured while the
// backup runs: the document count and the index specifications of every collection.
// Restore tests compare a restored copy against it.
type Manifest struct {
	// CapturedAt is when the manifest was captured.
	CapturedAt time.Time `json:"captured_at"`
	// Collections lists the collections of the backup, sorted by name.
	Collections []CollectionManifest `json:"collections"`
	// ServerVersion is the MongoDB version (buildInfo) of the server the manifest was
	// read from; empty when it could not be read. It never takes part in comparisons.
	ServerVersion string `json:"server_version,omitempty"`
}

// CollectionManifest describes one collection of a Manifest.
type CollectionManifest struct {
	// Name is the collection name.
	Name string `json:"name"`
	// DocumentsMin and DocumentsMax bound the estimated document count: the count is
	// read before and after the dump, so a collection written to meanwhile has a
	// range. A restored copy matches when its count lies within it.
	DocumentsMin int64 `json:"documents_min"`
	DocumentsMax int64 `json:"documents_max"`
	// Indexes lists the index specifications, sorted by name.
	Indexes []IndexSpec `json:"indexes,omitempty"`
}

// IndexSpec is the comparable part of an index specification.
type IndexSpec struct {
	// Name is the index name.
	Name string `json:"name"`
	// Keys is the canonical key pattern, e.g. "a:1,b:-1" or "loc:2dsphere".
	Keys string `json:"keys"`
	// Unique reports a unique index.
	Unique bool `json:"unique,omitempty"`
	// Sparse reports a sparse index.
	Sparse bool `json:"sparse,omitempty"`
	// ExpireAfterSeconds is the TTL of a TTL index.
	ExpireAfterSeconds *int64 `json:"expire_after_seconds,omitempty"`
}

// String formats the specification for mismatch details.
func (s IndexSpec) String() string {
	out := s.Keys
	if s.Unique {
		out += " unique"
	}
	if s.Sparse {
		out += " sparse"
	}
	if s.ExpireAfterSeconds != nil {
		out += fmt.Sprintf(" ttl=%ds", *s.ExpireAfterSeconds)
	}
	return out
}

// equal reports whether s and o describe the same index.
func (s IndexSpec) equal(o IndexSpec) bool {
	ttlEqual := (s.ExpireAfterSeconds == nil) == (o.ExpireAfterSeconds == nil) &&
		(s.ExpireAfterSeconds == nil || *s.ExpireAfterSeconds == *o.ExpireAfterSeconds)
	return s.Keys == o.Keys && s.Unique == o.Unique && s.Sparse == o.Sparse && ttlEqual
}

// Collection returns the manifest of collection name, or nil.
func (m *Manifest) Collection(name string) *CollectionManifest {
	if m == nil {
		return nil
	}
	for i := range m.Collections {
		if m.Collections[i].Name == name {
			return &m.Collections[i]
		}
	}
	return nil
}

// Documents returns the sum of the upper document bounds of all collections.
func (m *Manifest) Documents() int64 {
	if m == nil {
		return 0
	}
	var n int64
	for _, c := range m.Collections {
		n += c.DocumentsMax
	}
	return n
}

// Normalize sorts collections and indexes by name, so manifests compare and
// serialise deterministically.
func (m *Manifest) Normalize() {
	if m == nil {
		return
	}
	slices.SortFunc(m.Collections, func(a, b CollectionManifest) int { return strings.Compare(a.Name, b.Name) })
	for i := range m.Collections {
		c := &m.Collections[i]
		if c.DocumentsMax < c.DocumentsMin {
			c.DocumentsMin, c.DocumentsMax = c.DocumentsMax, c.DocumentsMin
		}
		slices.SortFunc(c.Indexes, func(a, b IndexSpec) int { return strings.Compare(a.Name, b.Name) })
	}
}

// MergeCounts widens the document ranges of m with the counts of later, a manifest
// of the same database captured afterwards (collections only in later are ignored).
func (m *Manifest) MergeCounts(later *Manifest) {
	if m == nil || later == nil {
		return
	}
	for i := range m.Collections {
		c := &m.Collections[i]
		if l := later.Collection(c.Name); l != nil {
			c.DocumentsMin = min(c.DocumentsMin, l.DocumentsMin)
			c.DocumentsMax = max(c.DocumentsMax, l.DocumentsMax)
		}
	}
}

// CompareManifests compares the manifest of a restored database (actual) with the
// manifest captured at backup time (expected). Mismatches are differences that make
// the restore untrustworthy: a missing collection, a document count outside the
// captured range, a missing or different index. Notes are differences that do not:
// collections in actual only (created while the dump ran).
func CompareManifests(expected, actual *Manifest) (mismatches, notes []string) {
	if expected == nil {
		return nil, nil
	}
	if actual == nil {
		actual = &Manifest{}
	}
	for _, want := range expected.Collections {
		got := actual.Collection(want.Name)
		if got == nil {
			mismatches = append(mismatches, fmt.Sprintf("collection %s is missing from the restored copy", want.Name))
			continue
		}
		if got.DocumentsMax < want.DocumentsMin || got.DocumentsMin > want.DocumentsMax {
			expectText := fmt.Sprintf("%d", want.DocumentsMin)
			if want.DocumentsMax != want.DocumentsMin {
				expectText = fmt.Sprintf("%d to %d", want.DocumentsMin, want.DocumentsMax)
			}
			mismatches = append(mismatches, fmt.Sprintf("collection %s: %d documents restored, %s expected", want.Name, got.DocumentsMax, expectText))
		}
		for _, idx := range want.Indexes {
			i := slices.IndexFunc(got.Indexes, func(g IndexSpec) bool { return g.Name == idx.Name })
			switch {
			case i < 0:
				mismatches = append(mismatches, fmt.Sprintf("collection %s: index %s (%s) is missing", want.Name, idx.Name, idx))
			case !got.Indexes[i].equal(idx):
				mismatches = append(mismatches, fmt.Sprintf("collection %s: index %s is %s, expected %s", want.Name, idx.Name, got.Indexes[i], idx))
			}
		}
	}
	for _, got := range actual.Collections {
		if expected.Collection(got.Name) == nil {
			notes = append(notes, fmt.Sprintf("collection %s was restored but is not in the manifest (created during the backup?)", got.Name))
		}
	}
	return mismatches, notes
}

// RestoreTestFrequency says how often a job's restore test runs.
type RestoreTestFrequency string

// Restore test frequencies.
const (
	// RestoreTestDaily tests at most once a day.
	RestoreTestDaily RestoreTestFrequency = "daily"
	// RestoreTestWeekly tests at most once a week.
	RestoreTestWeekly RestoreTestFrequency = "weekly"
	// RestoreTestMonthly tests at most once every 30 days.
	RestoreTestMonthly RestoreTestFrequency = "monthly"
	// RestoreTestEveryN tests after every EveryN successful backups.
	RestoreTestEveryN RestoreTestFrequency = "every_n"
)

// MaxRestoreTestEveryN caps RestoreTestPolicy.EveryN.
const MaxRestoreTestEveryN = 1000

// ErrInvalidRestoreTest is returned for an invalid restore test policy.
var ErrInvalidRestoreTest = errors.New("invalid restore_test")

// RestoreTestPolicy configures a job's automated restore test: after a successful
// scheduled backup, when a test is due, the latest backup is restored into a
// temporary <db>_rescue_verify_<timestamp> database, compared with its manifest and
// dropped again.
type RestoreTestPolicy struct {
	// Enabled turns the test on.
	Enabled bool `json:"enabled"`
	// Frequency says how often it runs.
	Frequency RestoreTestFrequency `json:"frequency"`
	// EveryN is the number of successful backups between tests (RestoreTestEveryN).
	EveryN int `json:"every_n,omitempty"`
	// ConnectionID selects a dedicated test server; empty restores into the server
	// the backup was taken from (into the temporary database only).
	ConnectionID string `json:"connection_id,omitempty"`
	// Databases says which databases of a multi-database run are tested when a test
	// is due: RestoreTestRotate (also when empty) tests one per run, taking turns,
	// RestoreTestAllDatabases tests each of them. Single-database jobs ignore it.
	Databases RestoreTestScope `json:"databases,omitempty"`
	// SourceTargetID, when set, makes the test a disaster recovery drill: it reads
	// the archive from the job's copy on this copy target (one of the job's copy
	// targets) instead of the primary, so it proves a restore works with the
	// primary's region gone. A drill tests the newest backup whose copy there is
	// complete.
	SourceTargetID string `json:"source_target_id,omitempty"`
}

// RestoreTestScope says which databases of a multi-database job run a restore test
// covers.
type RestoreTestScope string

// Restore test scopes.
const (
	// RestoreTestRotate tests one database per run, taking turns in name order.
	RestoreTestRotate RestoreTestScope = "rotate"
	// RestoreTestAllDatabases tests every database the run backed up.
	RestoreTestAllDatabases RestoreTestScope = "all"
)

// Validate normalises and checks p. Errors wrap ErrInvalidRestoreTest.
func (p *RestoreTestPolicy) Validate() error {
	if p == nil {
		return nil
	}
	p.ConnectionID = strings.TrimSpace(p.ConnectionID)
	p.SourceTargetID = strings.TrimSpace(p.SourceTargetID)
	switch p.Databases {
	case "", RestoreTestRotate, RestoreTestAllDatabases:
	default:
		return fmt.Errorf("%w: databases must be rotate or all", ErrInvalidRestoreTest)
	}
	if p.Frequency == "" {
		p.Frequency = RestoreTestWeekly
	}
	switch p.Frequency {
	case RestoreTestDaily, RestoreTestWeekly, RestoreTestMonthly:
		p.EveryN = 0
	case RestoreTestEveryN:
		if p.EveryN < 1 || p.EveryN > MaxRestoreTestEveryN {
			return fmt.Errorf("%w: every_n must be between 1 and %d", ErrInvalidRestoreTest, MaxRestoreTestEveryN)
		}
	default:
		return fmt.Errorf("%w: frequency must be daily, weekly, monthly or every_n", ErrInvalidRestoreTest)
	}
	return nil
}

// Interval returns the minimum time between tests of a time-based frequency, or 0.
func (p *RestoreTestPolicy) Interval() time.Duration {
	if p == nil {
		return 0
	}
	switch p.Frequency {
	case RestoreTestDaily:
		return 24 * time.Hour
	case RestoreTestWeekly:
		return 7 * 24 * time.Hour
	case RestoreTestMonthly:
		return 30 * 24 * time.Hour
	default:
		return 0
	}
}

// RestoreTestStatus is the outcome of a restore test.
type RestoreTestStatus string

// Restore test outcomes.
const (
	// RestoreTestOK means the backup restored and matched its manifest.
	RestoreTestOK RestoreTestStatus = "ok"
	// RestoreTestMismatch means the backup restored but differs from its manifest.
	RestoreTestMismatch RestoreTestStatus = "mismatch"
	// RestoreTestError means the test could not restore the backup (or not run).
	RestoreTestError RestoreTestStatus = "error"
)

// RestoreTestResult records one automated restore test.
type RestoreTestResult struct {
	// ID identifies the test ("rt_<db>_<timestamp>_<suffix>").
	ID string `json:"id"`
	// JobID is the job whose backup was tested.
	JobID string `json:"job_id"`
	// BackupID is the tested backup.
	BackupID string `json:"backup_id,omitempty"`
	// Database is the backed-up database.
	Database string `json:"database,omitempty"`
	// ConnectionID and ConnectionName name the server restored into.
	ConnectionID   string `json:"connection_id,omitempty"`
	ConnectionName string `json:"connection_name,omitempty"`
	// TempDatabase is the temporary database the backup was restored into.
	TempDatabase string `json:"temp_database,omitempty"`
	// SourceTargetID and SourceTargetName name the copy target a disaster recovery
	// drill read the archive from (RestoreTestPolicy.SourceTargetID); empty when the
	// test read the primary.
	SourceTargetID   string `json:"source_target_id,omitempty"`
	SourceTargetName string `json:"source_target_name,omitempty"`
	// Trigger is "scheduled" (after a scheduled backup) or "manual".
	Trigger string `json:"trigger"`
	// Status is the outcome.
	Status RestoreTestStatus `json:"status"`
	// StartedAt and CompletedAt bound the test.
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	// DurationSeconds is how long the test took, the drop included.
	DurationSeconds float64 `json:"duration_seconds"`
	// Collections and Documents describe the restored copy.
	Collections int   `json:"collections"`
	Documents   int64 `json:"documents"`
	// Mismatches lists the differences from the manifest (Status mismatch).
	Mismatches []string `json:"mismatches,omitempty"`
	// Notes lists harmless differences and remarks (e.g. no manifest recorded).
	Notes []string `json:"notes,omitempty"`
	// Error is the (redacted) reason of a failed test.
	Error string `json:"error,omitempty"`
	// Dropped reports that the temporary database was dropped afterwards; DropError
	// says why it could not be (it must then be dropped by hand).
	Dropped   bool   `json:"dropped"`
	DropError string `json:"drop_error,omitempty"`
	// PostRestore is what the connection's post-restore commands did on the
	// temporary database, after it was compared with the manifest.
	PostRestore *PostRestoreReport `json:"post_restore,omitempty"`
}

// Summary returns the compact form stored on the job and the backup.
func (r *RestoreTestResult) Summary() *RestoreTestSummary {
	if r == nil {
		return nil
	}
	at := r.StartedAt
	if r.CompletedAt != nil {
		at = *r.CompletedAt
	}
	detail := r.Error
	if detail == "" && len(r.Mismatches) > 0 {
		detail = r.Mismatches[0]
		if len(r.Mismatches) > 1 {
			detail += fmt.Sprintf(" (+%d more)", len(r.Mismatches)-1)
		}
	}
	return &RestoreTestSummary{ID: r.ID, BackupID: r.BackupID, Database: r.Database, Status: r.Status, At: at, DurationSeconds: r.DurationSeconds, Detail: detail}
}

// RestoreTestSummary is the latest restore test of a job or a backup.
type RestoreTestSummary struct {
	// ID identifies the test.
	ID string `json:"id"`
	// BackupID is the tested backup.
	BackupID string `json:"backup_id,omitempty"`
	// Database is the tested backup's database (multi-database jobs test their
	// databases in turn).
	Database string `json:"database,omitempty"`
	// Status is the outcome.
	Status RestoreTestStatus `json:"status"`
	// At is when the test finished.
	At time.Time `json:"at"`
	// DurationSeconds is how long it took.
	DurationSeconds float64 `json:"duration_seconds"`
	// Detail is the first mismatch or the error, if any.
	Detail string `json:"detail,omitempty"`
}

// ArchiveReferences returns the records of all, other than rec itself, that name
// rec's archive (the same storage target and key), in any status. An archive may
// only be deleted from storage when this is empty; a record that shares it is
// deleted alone.
func ArchiveReferences(all []*BackupRecord, rec *BackupRecord) []*BackupRecord {
	if rec == nil || rec.StorageKey == "" {
		return nil
	}
	var out []*BackupRecord
	for _, r := range all {
		if r.ID != rec.ID && r.StorageKey == rec.StorageKey && r.StorageTargetID == rec.StorageTargetID {
			out = append(out, r)
		}
	}
	return out
}

// RetentionReason says why retention deleted a backup.
type RetentionReason string

// Retention reasons.
const (
	// RetentionMaxAge is a backup older than the job's retention_days.
	RetentionMaxAge RetentionReason = "max_age"
	// RetentionMaxCount is a backup beyond the job's retention_count newest ones.
	RetentionMaxCount RetentionReason = "max_count"
)

// RetentionLogEntry records one backup deleted by retention.
type RetentionLogEntry struct {
	// ID orders entries (assigned by the store).
	ID int64 `json:"id"`
	// Time is when the backup was deleted.
	Time time.Time `json:"time"`
	// JobID is the job whose policy deleted it.
	JobID string `json:"job_id"`
	// BackupID, Database, StorageTargetID and StorageKey identify the backup.
	BackupID        string `json:"backup_id"`
	Database        string `json:"database"`
	StorageTargetID string `json:"storage_target_id,omitempty"`
	StorageKey      string `json:"storage_key,omitempty"`
	// BackupStartedAt is when the deleted backup was taken.
	BackupStartedAt time.Time `json:"backup_started_at"`
	// SizeBytes is the size of the deleted archive.
	SizeBytes int64 `json:"size_bytes"`
	// Reason says which rule deleted it; Detail explains it ("older than 30 days").
	Reason RetentionReason `json:"reason"`
	Detail string          `json:"detail,omitempty"`
	// PurgeAfter is the end of the deletion's grace period: until then the backup
	// can be undeleted, afterwards the purge removes its archive. Entries written
	// before soft deletes have none (their archive was deleted at once).
	PurgeAfter *time.Time `json:"purge_after,omitempty"`
	// Error is set when the archive could not be deleted from storage (the record
	// is pruned anyway; a storage scan reports the leftover object). Only entries
	// written before soft deletes carry it.
	Error string `json:"error,omitempty"`
}
