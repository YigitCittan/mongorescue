// Package models defines domain models and data structures for MongoRescue.
// These models are shared across the application without coupling to external frameworks.
package models

import (
	"fmt"
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
	return &clone
}

// Redacted returns a copy that is safe to serialize to API clients or logs: the S3
// secret access key is replaced by SecretMask, and an unset S3 part size shows the
// default.
func (t *StorageTarget) Redacted() *StorageTarget {
	clone := t.Clone()
	if clone != nil && clone.S3 != nil && clone.S3.SecretAccessKey != "" {
		clone.S3.SecretAccessKey = SecretMask
	}
	if clone != nil && clone.S3 != nil {
		clone.S3.PartSizeMB = clone.S3.EffectivePartSizeMB()
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
