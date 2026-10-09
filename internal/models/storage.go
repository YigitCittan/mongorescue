// Package models defines domain models and data structures for MongoRescue.
// These models are shared across the application without coupling to external frameworks.
package models

import (
	"fmt"
	"slices"
	"time"
)

// StorageType represents the storage backend provider identifier.
type StorageType string

const (
	// StorageLocal indicates local disk or network mounted filesystem storage.
	StorageLocal StorageType = "local"
	// StorageS3 indicates Amazon S3 or S3-compatible object storage (MinIO, R2, Wasabi, Spaces).
	StorageS3 StorageType = "s3"
)

// StorageObject represents metadata about a stored backup object in any backend.
type StorageObject struct {
	// Key is the unique identifier or relative path of the stored artifact.
	Key string `json:"key"`

	// SizeBytes is the raw size of the object in bytes.
	SizeBytes int64 `json:"size_bytes"`

	// ModTime is the last modification or creation timestamp.
	ModTime time.Time `json:"mod_time"`

	// StorageType identifies whether this object lives in local or S3 storage.
	StorageType StorageType `json:"storage_type"`

	// ETag is an optional entity tag or checksum provided by the storage driver.
	ETag string `json:"etag,omitempty"`

	// VersionID is the object version the driver wrote, on a bucket with versioning
	// (S3 Object Lock); empty otherwise.
	VersionID string `json:"version_id,omitempty"`

	// RetainUntil is the end of the object's S3 Object Lock retention: until then
	// the version cannot be deleted. Nil for an object without retention.
	RetainUntil *time.Time `json:"retain_until,omitempty"`

	// ObjectLockMode is the Object Lock mode of the retention (with RetainUntil).
	ObjectLockMode ObjectLockMode `json:"object_lock_mode,omitempty"`
}

// StorageTarget is a configured destination for backup archives: a local directory
// or an S3-compatible bucket. Backup records remember the target they were written
// to, so restores, deletions and retention always use that target.
type StorageTarget struct {
	// ID is the unique, immutable identifier (e.g. "stg_1a2b3c4d5e6f7a8b").
	ID string `json:"id"`

	// Name is a human-readable label.
	Name string `json:"name"`

	// Type selects the driver: StorageLocal or StorageS3.
	Type StorageType `json:"type"`

	// IsDefault marks the target used when a job or manual backup names none.
	// Exactly one target is the default.
	IsDefault bool `json:"is_default"`

	// Local configures a StorageLocal target.
	Local *LocalTarget `json:"local,omitempty"`

	// S3 configures a StorageS3 target.
	S3 *S3Target `json:"s3,omitempty"`

	// Region is the region (failure domain) holding the target's data, for the
	// disaster recovery checks of the readiness report: a label the operator set
	// (such as "dc-frankfurt"), or, with RegionDetected, the location AWS S3
	// reported for the bucket. See DRRegion.
	Region string `json:"region,omitempty"`

	// RegionDetected reports that Region was detected (GetBucketLocation on AWS
	// S3), not set by the operator; it is cleared when the endpoint, bucket or S3
	// region changes.
	RegionDetected bool `json:"region_detected,omitempty"`

	// CreatedAt is when the target was added.
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt is when the target was last modified.
	UpdatedAt time.Time `json:"updated_at"`

	// LastTestAt is when the target was last tested.
	LastTestAt *time.Time `json:"last_test_at,omitempty"`

	// LastTestOK reports whether the last test succeeded.
	LastTestOK bool `json:"last_test_ok"`

	// LastTestError is the failure reason of the last test.
	LastTestError string `json:"last_test_error,omitempty"`

	// Hints are notes on how well the target protects backups against deletion
	// (see ImmutabilityHint). They are computed for API responses (Redacted), never
	// stored.
	Hints []TargetHint `json:"hints,omitempty"`
}

// TargetHint is a note on a storage target shown in the dashboard and the
// readiness report.
type TargetHint struct {
	// Level is HintInfo or HintWarn.
	Level string `json:"level"`
	// Code identifies the hint (HintNoObjectLock, HintGovernanceBypass,
	// HintLocalNotImmutable).
	Code string `json:"code"`
	// Message explains it in English.
	Message string `json:"message"`
}

// Hint levels and codes.
const (
	// HintInfo is a suggestion.
	HintInfo = "info"
	// HintWarn is a weakness worth fixing.
	HintWarn = "warn"
	// HintNoObjectLock: an S3 target without Object Lock.
	HintNoObjectLock = "no_object_lock"
	// HintGovernanceBypass: an S3 target in governance mode.
	HintGovernanceBypass = "governance_bypass"
	// HintLocalNotImmutable: a local target, which has no immutability option.
	HintLocalNotImmutable = "local_not_immutable"
)

// ImmutabilityHint returns the note on how t protects backups against deletion
// outside MongoRescue, or nil for a target in compliance mode.
func (t *StorageTarget) ImmutabilityHint() *TargetHint {
	switch {
	case t == nil:
		return nil
	case t.Type == StorageLocal:
		return &TargetHint{Level: HintInfo, Code: HintLocalNotImmutable,
			Message: "local storage has no immutability option: anyone with access to the directory can delete backups; the delete grace period protects only against deletions through MongoRescue. Use an S3 target with Object Lock for immutable backups"}
	case t.S3 == nil:
		return nil
	case t.S3.ObjectLock == ObjectLockGovernance:
		return &TargetHint{Level: HintWarn, Code: HintGovernanceBypass,
			Message: "governance mode: principals with s3:BypassGovernanceRetention can still delete locked backups or shorten their lock; use compliance mode to stop everyone, including the bucket owner"}
	case !t.S3.Locked():
		return &TargetHint{Level: HintInfo, Code: HintNoObjectLock,
			Message: "backups can be deleted with the bucket credentials; enable S3 Object Lock (a bucket created with Object Lock and an object lock mode on this target) to make them immutable"}
	}
	return nil
}

// RetentionLockWarning returns a warning when a job's retention (days and count)
// deletes backups on t before their S3 Object Lock ends, and "" otherwise: the
// deletion is allowed, but storage keeps the objects, and their cost, until then.
func (t *StorageTarget) RetentionLockWarning(retentionDays, retentionCount int) string {
	if !t.ObjectLocked() {
		return ""
	}
	lock := t.S3.RetentionDays
	switch {
	case retentionDays > 0 && retentionDays < lock:
		return fmt.Sprintf("retention_days %d is shorter than the %d-day object lock of storage target %s: deleted backups stay in the bucket, and keep costing storage, until their lock ends",
			retentionDays, lock, t.Name)
	case retentionDays == 0 && retentionCount > 0:
		return fmt.Sprintf("retention_count may delete backups before the %d-day object lock of storage target %s ends: they then stay in the bucket, and keep costing storage, until it does",
			lock, t.Name)
	}
	return ""
}

// LocalTarget configures a directory on the MongoRescue host (or a mounted volume).
type LocalTarget struct {
	// Path is an absolute path, or a path relative to the parent of the data
	// directory. It must not contain "..".
	Path string `json:"path"`
}

// S3Target configures an S3-compatible bucket.
type S3Target struct {
	// Endpoint is the S3 API URL; empty means AWS S3.
	Endpoint string `json:"endpoint,omitempty"`

	// Region is the bucket region ("auto" for Cloudflare R2).
	Region string `json:"region,omitempty"`

	// Bucket is the bucket name.
	Bucket string `json:"bucket"`

	// Prefix is prepended to every object key (e.g. "mongorescue/").
	Prefix string `json:"prefix,omitempty"`

	// AccessKeyID is the access key; empty uses the AWS default credential chain
	// (environment, shared config, instance role).
	AccessKeyID string `json:"access_key_id,omitempty"`

	// SecretAccessKey is the secret key. It is encrypted at rest and masked in API
	// responses (see Redacted).
	SecretAccessKey string `json:"secret_access_key,omitempty"`

	// UsePathStyle selects path-style URLs (required by MinIO and some gateways).
	UsePathStyle bool `json:"use_path_style"`

	// PartSizeMB is the multipart upload part size in MiB (MinS3PartSizeMB to
	// MaxS3PartSizeMB). S3 allows at most S3MaxUploadParts parts, so it bounds the
	// largest archive (see MaxArchiveBytes); the uploader holds part size × 2 bytes
	// in memory per running upload. Zero means DefaultS3PartSizeMB.
	PartSizeMB int `json:"part_size_mb,omitempty"`

	// ObjectLock is the S3 Object Lock retention mode set on every uploaded object:
	// ObjectLockNone (empty), ObjectLockGovernance or ObjectLockCompliance. A locked
	// target needs a bucket created with Object Lock (and so versioning) enabled.
	ObjectLock ObjectLockMode `json:"object_lock,omitempty"`

	// RetentionDays is how long every uploaded object is locked
	// (MinObjectLockRetentionDays to MaxObjectLockRetentionDays) when ObjectLock is
	// set; zero otherwise.
	RetentionDays int `json:"retention_days,omitempty"`

	// LegalHoldOnPin sets an S3 legal hold on a backup's object while the backup is
	// pinned (needs ObjectLock).
	LegalHoldOnPin bool `json:"legal_hold_on_pin,omitempty"`
}

// ObjectLockMode is the S3 Object Lock retention mode of a storage target.
type ObjectLockMode string

// S3 Object Lock modes.
const (
	// ObjectLockNone (stored as "", shown as "none") uploads objects without a lock.
	ObjectLockNone ObjectLockMode = "none"
	// ObjectLockGovernance locks objects so that only principals with
	// s3:BypassGovernanceRetention can delete them or shorten the lock.
	ObjectLockGovernance ObjectLockMode = "governance"
	// ObjectLockCompliance locks objects so that nobody, not even the bucket owner
	// or the root account, can delete them before their retention ends.
	ObjectLockCompliance ObjectLockMode = "compliance"
)

// S3 Object Lock retention bounds, in days.
const (
	// MinObjectLockRetentionDays is the shortest retention of a locked target.
	MinObjectLockRetentionDays = 1
	// MaxObjectLockRetentionDays is the longest retention of a locked target
	// (about ten years).
	MaxObjectLockRetentionDays = 3650
)

// Locked reports whether the target uploads objects with an S3 Object Lock.
func (t *S3Target) Locked() bool {
	return t != nil && (t.ObjectLock == ObjectLockGovernance || t.ObjectLock == ObjectLockCompliance)
}

// ObjectLocked reports whether t is an S3 target that locks uploaded objects.
func (t *StorageTarget) ObjectLocked() bool {
	return t != nil && t.Type == StorageS3 && t.S3.Locked()
}

// RetentionDuration returns the lock retention of t as a duration (zero when the
// target does not lock objects).
func (t *S3Target) RetentionDuration() time.Duration {
	if !t.Locked() {
		return 0
	}
	return time.Duration(t.RetentionDays) * 24 * time.Hour
}

// S3 multipart upload limits.
const (
	// DefaultS3PartSizeMB is the part size of a target that sets none: 16 MiB parts
	// allow archives of about 156 GiB.
	DefaultS3PartSizeMB = 16
	// MinS3PartSizeMB is the smallest part size S3 accepts.
	MinS3PartSizeMB = 5
	// MaxS3PartSizeMB is the largest part size a target may set (about 4.9 TiB per
	// archive, 1 GiB of upload buffers).
	MaxS3PartSizeMB = 512
	// S3MaxUploadParts is the most parts one S3 multipart upload may have.
	S3MaxUploadParts = 10000
	// S3UploadConcurrency is the number of parts uploaded in parallel; the uploader
	// buffers one part per concurrent upload.
	S3UploadConcurrency = 2
	// ArchiveSizeWarnPercent is the share of a target's largest archive above which
	// a backup's expected size is warned about.
	ArchiveSizeWarnPercent = 80
)

// EffectivePartSizeMB returns PartSizeMB, or DefaultS3PartSizeMB when it is unset.
func (t *S3Target) EffectivePartSizeMB() int {
	if t == nil || t.PartSizeMB <= 0 {
		return DefaultS3PartSizeMB
	}
	return t.PartSizeMB
}

// MaxArchiveBytes is the largest archive the target can store: its part size times
// S3MaxUploadParts.
func (t *S3Target) MaxArchiveBytes() int64 {
	return int64(t.EffectivePartSizeMB()) << 20 * S3MaxUploadParts
}

// MaxArchiveBytes is the largest archive the target can store, or 0 when it has no
// limit of its own (a local directory).
func (t *StorageTarget) MaxArchiveBytes() int64 {
	if t == nil || t.Type != StorageS3 || t.S3 == nil {
		return 0
	}
	return t.S3.MaxArchiveBytes()
}

// ArchiveSizeWarning returns a warning when an archive of about estimate bytes
// exceeds ArchiveSizeWarnPercent of limit, the largest archive of a storage target
// (0 for no limit), and "" otherwise. source says where the estimate comes from.
func ArchiveSizeWarning(database string, estimate int64, source string, limit int64) string {
	if limit <= 0 || estimate <= 0 || estimate*100 <= limit*ArchiveSizeWarnPercent {
		return ""
	}
	return fmt.Sprintf("database %s is about %s (%s), %d%% of the %s the storage target can hold in one archive; raise the target's part_size_mb before the upload runs out of parts",
		database, formatBytes(estimate), source, estimate*100/limit, formatBytes(limit))
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

// SecretMask replaces stored secrets in API responses. Sending it back unchanged on an
// update keeps the stored value.
const SecretMask = "******"

// Clone returns a deep copy of the target.
func (t *StorageTarget) Clone() *StorageTarget {
	if t == nil {
		return nil
	}
	clone := *t
	if t.Local != nil {
		local := *t.Local
		clone.Local = &local
	}
	if t.S3 != nil {
		s3 := *t.S3
		clone.S3 = &s3
	}
	if t.LastTestAt != nil {
		at := *t.LastTestAt
		clone.LastTestAt = &at
	}
	clone.Hints = slices.Clone(t.Hints)
	return &clone
}

// Redacted returns a copy that is safe to serialize to API clients or logs: the S3
// secret access key is replaced by SecretMask, and an unset S3 part size shows the
// default and an unset object lock mode ObjectLockNone. Hints holds the target's
// ImmutabilityHint.
func (t *StorageTarget) Redacted() *StorageTarget {
	clone := t.Clone()
	if clone != nil && clone.S3 != nil && clone.S3.SecretAccessKey != "" {
		clone.S3.SecretAccessKey = SecretMask
	}
	if clone != nil && clone.S3 != nil {
		clone.S3.PartSizeMB = clone.S3.EffectivePartSizeMB()
		if clone.S3.ObjectLock == "" {
			clone.S3.ObjectLock = ObjectLockNone
		}
	}
	if h := t.ImmutabilityHint(); clone != nil && h != nil {
		clone.Hints = []TargetHint{*h}
	}
	return clone
}

// Location describes where the target stores archives ("/backups",
// "s3://bucket/prefix"), without credentials.
func (t *StorageTarget) Location() string {
	switch {
	case t == nil:
		return ""
	case t.Type == StorageLocal && t.Local != nil:
		return t.Local.Path
	case t.Type == StorageS3 && t.S3 != nil:
		return "s3://" + t.S3.Bucket + "/" + t.S3.Prefix
	}
	return ""
}
