package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// ErrObjectLockUnavailable indicates a bucket without S3 Object Lock or versioning,
// which a target with an Object Lock mode cannot use.
var ErrObjectLockUnavailable = errors.New("storage: the bucket does not have S3 Object Lock enabled")

// ObjectLock configures the S3 Object Lock retention of uploaded objects.
type ObjectLock struct {
	// Mode is models.ObjectLockGovernance or models.ObjectLockCompliance; empty or
	// models.ObjectLockNone uploads without a lock.
	Mode models.ObjectLockMode
	// RetentionDays is how long each object is locked from its upload.
	RetentionDays int
	// Now is the clock of the retention dates; nil means time.Now.
	Now func() time.Time
}

// Enabled reports whether uploads are locked.
func (l ObjectLock) Enabled() bool {
	return l.Mode == models.ObjectLockGovernance || l.Mode == models.ObjectLockCompliance
}

// validate checks the mode and the retention.
func (l ObjectLock) validate() error {
	switch l.Mode {
	case "", models.ObjectLockNone:
		return nil
	case models.ObjectLockGovernance, models.ObjectLockCompliance:
	default:
		return fmt.Errorf("%w: unknown object lock mode %q", ErrInvalidConfig, l.Mode)
	}
	if l.RetentionDays < models.MinObjectLockRetentionDays || l.RetentionDays > models.MaxObjectLockRetentionDays {
		return fmt.Errorf("%w: object lock retention must be %d to %d days", ErrInvalidConfig,
			models.MinObjectLockRetentionDays, models.MaxObjectLockRetentionDays)
	}
	return nil
}

// s3Mode returns the S3 API value of the mode.
func (l ObjectLock) s3Mode() types.ObjectLockMode {
	if l.Mode == models.ObjectLockCompliance {
		return types.ObjectLockModeCompliance
	}
	return types.ObjectLockModeGovernance
}

// apply sets the lock headers on an upload and returns its retain-until date (nil
// without a lock). S3 requires an integrity checksum (Content-MD5 or an
// x-amz-checksum header) on every put with a retention, and the client otherwise
// only sends checksums when an operation requires them (see NewS3Storage), so a
// locked upload asks for CRC32 checksums: the SDK computes them for the single
// request or for each part of a multipart upload while streaming.
func (l ObjectLock) apply(in *s3.PutObjectInput) *time.Time {
	if !l.Enabled() {
		return nil
	}
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	// S3 keeps retain-until dates with second precision.
	until := now().UTC().Add(time.Duration(l.RetentionDays) * 24 * time.Hour).Truncate(time.Second)
	in.ObjectLockMode = l.s3Mode()
	in.ObjectLockRetainUntilDate = aws.Time(until)
	in.ChecksumAlgorithm = types.ChecksumAlgorithmCrc32
	return &until
}

// describe records the retention until (nil without a lock) on obj.
func (l ObjectLock) describe(obj *models.StorageObject, until *time.Time) *models.StorageObject {
	if until != nil {
		obj.RetainUntil, obj.ObjectLockMode = until, l.Mode
	}
	return obj
}

// Versioned is implemented by drivers that can address one version of an object
// (an S3 bucket with versioning, as Object Lock requires).
type Versioned interface {
	// RetrieveVersion streams version versionID of key.
	RetrieveVersion(ctx context.Context, key, versionID string) (io.ReadCloser, error)
	// DeleteVersion permanently deletes version versionID of key. Unlike Delete on
	// a versioned bucket, which only adds a delete marker, it frees the space; it
	// fails while the version is under an Object Lock retention or legal hold.
	DeleteVersion(ctx context.Context, key, versionID string) error
}

// ObjectLocker is implemented by drivers that support S3 Object Lock.
type ObjectLocker interface {
	// CheckObjectLock returns an error wrapping ErrObjectLockUnavailable when the
	// bucket does not have Object Lock and versioning enabled.
	CheckObjectLock(ctx context.Context) error
	// SetLegalHold turns the legal hold of version versionID of key (the current
	// version for "") on or off.
	SetLegalHold(ctx context.Context, key, versionID string, on bool) error
}

// Compile-time checks that S3Storage addresses versions and supports Object Lock.
var (
	_ Versioned    = (*S3Storage)(nil)
	_ ObjectLocker = (*S3Storage)(nil)
)

// RetrieveVersion streams version versionID of key from s when it is set and s
// addresses versions, and the current object otherwise.
func RetrieveVersion(ctx context.Context, s Storage, key, versionID string) (io.ReadCloser, error) {
	if v, ok := s.(Versioned); ok && versionID != "" {
		return v.RetrieveVersion(ctx, key, versionID)
	}
	return s.Retrieve(ctx, key)
}

// DeleteVersion permanently deletes version versionID of key from s when it is set
// and s addresses versions, and deletes key otherwise.
func DeleteVersion(ctx context.Context, s Storage, key, versionID string) error {
	if v, ok := s.(Versioned); ok && versionID != "" {
		return v.DeleteVersion(ctx, key, versionID)
	}
	return s.Delete(ctx, key)
}

// DeleteUnlocked deletes the current version of key from s unless it is under an
// Object Lock retention at now, in which case it returns the end of the retention
// and deletes nothing (the caller retries later). It reads the object's version and
// retention first (Stat), so on a versioned bucket the version itself is deleted
// and its space freed, not hidden behind a delete marker.
func DeleteUnlocked(ctx context.Context, s Storage, key string, now time.Time) (*time.Time, error) {
	obj, err := s.Stat(ctx, key)
	if err != nil {
		return nil, err
	}
	if obj.RetainUntil != nil && now.Before(*obj.RetainUntil) {
		return obj.RetainUntil, nil
	}
	return nil, DeleteVersion(ctx, s, key, obj.VersionID)
}

// SetLegalHold turns the legal hold of version versionID of key on s on or off; it
// returns errors.ErrUnsupported when s does not support Object Lock.
func SetLegalHold(ctx context.Context, s Storage, key, versionID string, on bool) error {
	l, ok := s.(ObjectLocker)
	if !ok {
		return fmt.Errorf("storage: legal holds: %w", errors.ErrUnsupported)
	}
	return l.SetLegalHold(ctx, key, versionID, on)
}

// CheckObjectLock verifies that the bucket has Object Lock enabled
// (GetObjectLockConfiguration) and versioning on (GetBucketVersioning). It never
// changes the bucket: Object Lock can only be enabled when a bucket is created (or
// by its owner, irreversibly).
func (s *S3Storage) CheckObjectLock(ctx context.Context) error {
	out, err := s.client.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: aws.String(s.bucket)})
	var apiErr smithy.APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.ErrorCode() == "ObjectLockConfigurationNotFoundError":
		return fmt.Errorf("%w: bucket %s has no Object Lock configuration; create a bucket with Object Lock enabled (see docs/configuration.md) or set the object lock mode to none", ErrObjectLockUnavailable, s.bucket)
	case err != nil:
		return fmt.Errorf("read the object lock configuration of bucket %s (needs s3:GetBucketObjectLockConfiguration): %w", s.bucket, err)
	case out.ObjectLockConfiguration == nil || out.ObjectLockConfiguration.ObjectLockEnabled != types.ObjectLockEnabledEnabled:
		return fmt.Errorf("%w: Object Lock is not enabled on bucket %s; create a bucket with Object Lock enabled (see docs/configuration.md) or set the object lock mode to none", ErrObjectLockUnavailable, s.bucket)
	}
	ver, err := s.client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(s.bucket)})
	switch {
	case err != nil:
		return fmt.Errorf("read the versioning state of bucket %s (needs s3:GetBucketVersioning): %w", s.bucket, err)
	case ver.Status != types.BucketVersioningStatusEnabled:
		return fmt.Errorf("%w: versioning is not enabled on bucket %s (Object Lock needs it)", ErrObjectLockUnavailable, s.bucket)
	}
	return nil
}

// RetrieveVersion streams version versionID of key.
func (s *S3Storage) RetrieveVersion(ctx context.Context, key, versionID string) (io.ReadCloser, error) {
	objKey, err := s.objectKey(key)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket:    aws.String(s.bucket),
		Key:       aws.String(objKey),
		VersionId: aws.String(versionID),
	})
	if err != nil {
		if isS3NotFound(err) || isNoSuchVersion(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("s3 get object version: %w", err)
	}
	return resp.Body, nil
}

// DeleteVersion permanently deletes version versionID of key. It never bypasses a
// governance retention.
func (s *S3Storage) DeleteVersion(ctx context.Context, key, versionID string) error {
	objKey, err := s.objectKey(key)
	if err != nil {
		return err
	}
	_, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket:    aws.String(s.bucket),
		Key:       aws.String(objKey),
		VersionId: aws.String(versionID),
	})
	if err != nil {
		if isS3NotFound(err) || isNoSuchVersion(err) {
			return ErrNotFound
		}
		return fmt.Errorf("s3 delete object version: %w", err)
	}
	return nil
}

// SetLegalHold turns the legal hold of version versionID of key (the current
// version for "") on or off.
func (s *S3Storage) SetLegalHold(ctx context.Context, key, versionID string, on bool) error {
	objKey, err := s.objectKey(key)
	if err != nil {
		return err
	}
	status := types.ObjectLockLegalHoldStatusOff
	if on {
		status = types.ObjectLockLegalHoldStatusOn
	}
	in := &s3.PutObjectLegalHoldInput{
		Bucket:    aws.String(s.bucket),
		Key:       aws.String(objKey),
		LegalHold: &types.ObjectLockLegalHold{Status: status},
	}
	if versionID != "" {
		in.VersionId = aws.String(versionID)
	}
	if _, err = s.client.PutObjectLegalHold(ctx, in); err != nil {
		if isS3NotFound(err) || isNoSuchVersion(err) {
			return ErrNotFound
		}
		return fmt.Errorf("s3 put object legal hold: %w", err)
	}
	return nil
}

// isNoSuchVersion reports a missing object version.
func isNoSuchVersion(err error) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchVersion"
}
