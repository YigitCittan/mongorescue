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

// now returns the time on the lock's clock.
func (l ObjectLock) now() time.Time {
	if l.Now != nil {
		return l.Now().UTC()
	}
	return time.Now().UTC()
}

// retainUntil returns the retain-until date of an object finished at t: t plus the
// retention, rounded up to the second S3 keeps, so it is never shorter.
func (l ObjectLock) retainUntil(t time.Time) time.Time {
	until := t.UTC().Add(time.Duration(l.RetentionDays) * 24 * time.Hour)
	if r := until.Truncate(time.Second); !r.Equal(until) {
		return r.Add(time.Second)
	}
	return until
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
	until := l.retainUntil(l.now())
	in.ObjectLockMode = l.s3Mode()
	in.ObjectLockRetainUntilDate = aws.Time(until)
	in.ChecksumAlgorithm = types.ChecksumAlgorithmCrc32
	return &until
}

// describe records the version and the retention (nil without a lock) on obj.
// Versions are recorded only on a locked target: other targets keep addressing
// objects by key, as before, even on a bucket that returns versions.
func (l ObjectLock) describe(obj *models.StorageObject, versionID string, until *time.Time) *models.StorageObject {
	if !l.Enabled() {
		return obj
	}
	obj.VersionID = versionID
	if until != nil {
		obj.RetainUntil, obj.ObjectLockMode = until, l.Mode
	}
	return obj
}

// extendRetention moves the retain-until date of a finished locked upload to the
// upload's end plus the retention when that is later than the date set when it
// began (a long upload), so no object is locked for less than the retention.
// Extending a retention is allowed in both modes.
func (s *S3Storage) extendRetention(ctx context.Context, objKey, versionID string, until *time.Time) (*time.Time, error) {
	if until == nil || !s.lock.Enabled() {
		return until, nil
	}
	want := s.lock.retainUntil(s.lock.now())
	if !want.After(*until) {
		return until, nil
	}
	in := &s3.PutObjectRetentionInput{
		Bucket:    aws.String(s.bucket),
		Key:       aws.String(objKey),
		Retention: &types.ObjectLockRetention{Mode: types.ObjectLockRetentionMode(s.lock.s3Mode()), RetainUntilDate: aws.Time(want)},
	}
	if versionID != "" {
		in.VersionId = aws.String(versionID)
	}
	if _, err := s.client.PutObjectRetention(ctx, in); err != nil {
		return until, fmt.Errorf("s3 extend the object lock to the end of the upload (needs s3:PutObjectRetention): %w", err)
	}
	return &want, nil
}

// Versioned is implemented by drivers that can read one version of an object (an
// S3 bucket with versioning, as Object Lock requires).
type Versioned interface {
	// RetrieveVersion streams version versionID of key.
	RetrieveVersion(ctx context.Context, key, versionID string) (io.ReadCloser, error)
}

// ObjectLocker is implemented by drivers that support S3 Object Lock.
type ObjectLocker interface {
	// ObjectLockEnabled reports whether the driver locks its uploads.
	ObjectLockEnabled() bool
	// CheckObjectLock returns an error wrapping ErrObjectLockUnavailable when the
	// bucket does not have Object Lock and versioning enabled.
	CheckObjectLock(ctx context.Context) error
	// SetLegalHold turns the legal hold of version versionID of key (the current
	// version for "") on or off.
	SetLegalHold(ctx context.Context, key, versionID string, on bool) error
	// PurgeVersions deletes every version and delete marker of key once none is
	// locked at now; see Purge.
	PurgeVersions(ctx context.Context, key string, now time.Time) (*time.Time, error)
}

// ErrObjectHeld indicates an object version under an S3 legal hold, which cannot
// be deleted until the hold is lifted.
var ErrObjectHeld = errors.New("storage: an object version is under a legal hold")

// Compile-time checks that S3Storage reads versions and supports Object Lock.
var (
	_ Versioned    = (*S3Storage)(nil)
	_ ObjectLocker = (*S3Storage)(nil)
)

// RetrieveVersion streams version versionID of key from s when it is set (it is
// recorded only for objects of a locked target) and s reads versions, and the
// current object otherwise.
func RetrieveVersion(ctx context.Context, s Storage, key, versionID string) (io.ReadCloser, error) {
	if v, ok := s.(Versioned); ok && versionID != "" {
		return v.RetrieveVersion(ctx, key, versionID)
	}
	return s.Retrieve(ctx, key)
}

// Purge removes key from s for good. On a target with Object Lock, or for an object
// recorded with a version (uploaded while its target locked objects), it lists
// every version and delete marker of key, and deletes them all once none is locked
// at now; while one is, it deletes nothing and returns the latest end of their
// retentions for the caller to retry then. A version under a legal hold fails with
// ErrObjectHeld, and a key without any version with ErrNotFound. Every other target
// deletes key exactly like Delete (on a versioned bucket that leaves a delete
// marker, as before).
func Purge(ctx context.Context, s Storage, key, versionID string, now time.Time) (*time.Time, error) {
	if l, ok := s.(ObjectLocker); ok && (l.ObjectLockEnabled() || versionID != "") {
		return l.PurgeVersions(ctx, key, now)
	}
	return nil, s.Delete(ctx, key)
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

// ObjectLockEnabled reports whether the driver locks its uploads.
func (s *S3Storage) ObjectLockEnabled() bool { return s.lock.Enabled() }

// objectVersion is one version or delete marker of a key.
type objectVersion struct {
	id     string
	marker bool
}

// PurgeVersions deletes every version and delete marker of key once none of its
// versions is under a retention at now or under a legal hold (see Purge).
func (s *S3Storage) PurgeVersions(ctx context.Context, key string, now time.Time) (*time.Time, error) {
	objKey, err := s.objectKey(key)
	if err != nil {
		return nil, err
	}
	var versions []objectVersion
	pages := s3.NewListObjectVersionsPaginator(s.client, &s3.ListObjectVersionsInput{Bucket: aws.String(s.bucket), Prefix: aws.String(objKey)})
	for pages.HasMorePages() {
		page, pageErr := pages.NextPage(ctx)
		if pageErr != nil {
			return nil, fmt.Errorf("s3 list object versions: %w", pageErr)
		}
		for _, v := range page.Versions {
			if aws.ToString(v.Key) == objKey {
				versions = append(versions, objectVersion{id: aws.ToString(v.VersionId)})
			}
		}
		for _, m := range page.DeleteMarkers {
			if aws.ToString(m.Key) == objKey {
				versions = append(versions, objectVersion{id: aws.ToString(m.VersionId), marker: true})
			}
		}
	}
	if len(versions) == 0 {
		return nil, ErrNotFound
	}
	var wait *time.Time
	for _, v := range versions {
		if v.marker {
			continue
		}
		head, headErr := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(objKey), VersionId: aws.String(v.id)})
		switch {
		case headErr != nil && (isS3NotFound(headErr) || isNoSuchVersion(headErr)):
			continue
		case headErr != nil:
			return nil, fmt.Errorf("s3 head object version: %w", headErr)
		case head.ObjectLockLegalHoldStatus == types.ObjectLockLegalHoldStatusOn:
			return nil, fmt.Errorf("%w: %s version %s", ErrObjectHeld, key, v.id)
		}
		if u := head.ObjectLockRetainUntilDate; u != nil && now.Before(*u) && (wait == nil || u.After(*wait)) {
			until := u.UTC()
			wait = &until
		}
	}
	if wait != nil {
		return wait, nil
	}
	for _, v := range versions {
		if err = s.DeleteVersion(ctx, key, v.id); err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	return nil, nil
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
