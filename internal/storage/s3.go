package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
)

// S3Config configures an S3Storage driver.
type S3Config struct {
	// Endpoint is the custom S3 endpoint URL (required for MinIO, Cloudflare R2,
	// Backblaze B2, Wasabi, Spaces). Leave blank for AWS S3.
	Endpoint string

	// Bucket is the name of the target bucket.
	Bucket string

	// Region is the bucket region (default "us-east-1").
	Region string

	// Prefix is prepended to every object key; a trailing "/" is added when missing.
	Prefix string

	// AccessKey and SecretKey are static credentials. When either is empty the AWS
	// default credential chain applies (environment, shared config, instance role).
	AccessKey string
	// SecretKey is the secret access key paired with AccessKey.
	SecretKey string

	// UsePathStyle enables path-style URLs (e.g. http://s3.host/bucket), required for MinIO.
	UsePathStyle bool

	// PartSizeMB is the multipart part size in MiB; 0 means
	// models.DefaultS3PartSizeMB. It bounds the largest archive (see MaxArchiveSize)
	// and the upload buffers (part size × models.S3UploadConcurrency).
	PartSizeMB int

	// Logger receives warnings the driver cannot return (a failed cleanup of an
	// aborted upload); nil means slog.Default().
	Logger *slog.Logger
}

// S3Storage provides a stream-first storage driver for AWS S3 and S3-compatible backends
// (MinIO, Cloudflare R2, Wasabi, DigitalOcean Spaces).
type S3Storage struct {
	client   *s3.Client
	uploader *manager.Uploader //nolint:staticcheck // SA1019: migrating to feature/s3/transfermanager requires a new module dependency; tracked separately.
	bucket   string
	// prefix is prepended to every key ("" or ending in "/"); List strips it again.
	prefix string
	logger *slog.Logger
	// partSize is the multipart part size in bytes.
	partSize int64
}

// Compile-time check that S3Storage reports its archive size limit.
var _ ArchiveLimiter = (*S3Storage)(nil)

// NormalizePrefix trims surrounding whitespace and slashes from prefix and appends a
// single "/" unless it is empty.
func NormalizePrefix(prefix string) string {
	p := strings.Trim(strings.TrimSpace(prefix), "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

// objectKey maps a storage key to the object key in the bucket.
func (s *S3Storage) objectKey(key string) (string, error) {
	clean := strings.TrimPrefix(strings.TrimSpace(key), "/")
	if clean == "" {
		return "", ErrInvalidKey
	}
	return s.prefix + clean, nil
}

// NewS3Storage initializes an S3Storage driver using the provided configuration.
func NewS3Storage(ctx context.Context, cfg S3Config) (*S3Storage, error) {
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, fmt.Errorf("%w: s3 bucket name cannot be empty", ErrInvalidKey)
	}

	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// 1. Build AWS configuration options
	var loadOpts []func(*awsconfig.LoadOptions) error
	loadOpts = append(loadOpts, awsconfig.WithRegion(region))

	if cfg.AccessKey != "" && cfg.SecretKey != "" {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	// 2. Build S3 Client options
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.UsePathStyle

		// aws-sdk-go-v2 (since early 2025) adds CRC32 integrity checksums to every request
		// and validates them on responses by default. Several S3-compatible providers
		// (Cloudflare R2, Backblaze B2, older MinIO releases, some gateways) reject those
		// headers or trailers, so only send and validate checksums when an operation
		// requires them. Archive integrity is still verified end-to-end via the SHA-256
		// computed while streaming.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})

	// 3. Configure memory-efficient streaming uploader. S3 allows at most 10,000 parts,
	// so the part size bounds the largest archive: the default 16 MiB parts allow about
	// 156 GiB, and with a concurrency of 2 the uploader holds about 32 MiB in memory.
	partSizeMB := (&models.S3Target{PartSizeMB: cfg.PartSizeMB}).EffectivePartSizeMB()
	if partSizeMB < models.MinS3PartSizeMB || partSizeMB > models.MaxS3PartSizeMB {
		return nil, fmt.Errorf("%w: s3 part size must be %d to %d MiB", ErrInvalidConfig, models.MinS3PartSizeMB, models.MaxS3PartSizeMB)
	}
	partSize := int64(partSizeMB) << 20
	uploader := manager.NewUploader(client, func(u *manager.Uploader) { //nolint:staticcheck // SA1019: see S3Storage.uploader.
		u.PartSize = partSize
		u.Concurrency = models.S3UploadConcurrency
	})

	return &S3Storage{
		client:   client,
		uploader: uploader,
		bucket:   cfg.Bucket,
		prefix:   NormalizePrefix(cfg.Prefix),
		logger:   logger,
		partSize: partSize,
	}, nil
}

// MaxArchiveSize returns the largest archive one upload can store: the part size
// times manager.MaxUploadParts.
func (s *S3Storage) MaxArchiveSize() int64 {
	return s.partSize * int64(manager.MaxUploadParts)
}

// Save streams data from the reader directly to S3 using multipart upload without buffering into memory.
func (s *S3Storage) Save(ctx context.Context, key string, r io.Reader) (*models.StorageObject, error) {
	objKey, err := s.objectKey(key)
	if err != nil {
		return nil, err
	}
	cleanKey := strings.TrimPrefix(objKey, s.prefix)

	startTime := time.Now()
	uploadOutput, err := s.uploader.Upload(ctx, &s3.PutObjectInput{ //nolint:staticcheck // SA1019: see S3Storage.uploader.
		Bucket: aws.String(s.bucket),
		Key:    aws.String(objKey),
		Body:   r,
	})
	if err != nil {
		s.abortFailedUpload(ctx, objKey, err)
		if strings.Contains(err.Error(), "MaxUploadParts") {
			return nil, fmt.Errorf("s3 multipart upload failed: the archive is larger than %d GiB, the most %d parts of %d MiB can hold; raise the storage target's part_size_mb (up to %d): %w",
				s.MaxArchiveSize()>>30, manager.MaxUploadParts, s.partSize>>20, models.MaxS3PartSizeMB, err)
		}
		return nil, fmt.Errorf("s3 multipart upload failed: %w", err)
	}

	// Retrieve object head to obtain definitive size and modtime
	head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(objKey),
	})
	if err != nil {
		// Fallback to estimated values if HeadObject fails
		return &models.StorageObject{
			Key:         cleanKey,
			ModTime:     startTime,
			StorageType: models.StorageS3,
		}, nil
	}

	var size int64
	if head.ContentLength != nil {
		size = *head.ContentLength
	}

	modTime := startTime
	if head.LastModified != nil {
		modTime = *head.LastModified
	}

	var etag string
	if uploadOutput.ETag != nil {
		etag = *uploadOutput.ETag
	}

	return &models.StorageObject{
		Key:         cleanKey,
		SizeBytes:   size,
		ModTime:     modTime,
		StorageType: models.StorageS3,
		ETag:        etag,
	}, nil
}

// abortTimeout bounds the abort of a failed multipart upload.
const abortTimeout = 30 * time.Second

// abortFailedUpload aborts the multipart upload behind a failed Upload. The uploader
// aborts it itself, but with the upload's context: after a cancellation (a cancelled
// or timed-out backup) that request is never sent and the uploaded parts would stay
// in the bucket, invisible and billed, until a lifecycle rule removes them. The abort
// therefore runs detached from ctx cancellation; aborting an upload the uploader
// already aborted is harmless.
func (s *S3Storage) abortFailedUpload(ctx context.Context, objKey string, uploadErr error) {
	var failure manager.MultiUploadFailure //nolint:staticcheck // SA1019: see S3Storage.uploader.
	if !errors.As(uploadErr, &failure) || failure.UploadID() == "" {
		return
	}
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortTimeout)
	defer cancel()
	_, err := s.client.AbortMultipartUpload(abortCtx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(objKey),
		UploadId: aws.String(failure.UploadID()),
	})
	var apiErr smithy.APIError
	if err != nil && (!errors.As(err, &apiErr) || apiErr.ErrorCode() != "NoSuchUpload") {
		s.logger.Warn("failed to abort the multipart upload of a failed backup; its parts stay in the bucket until a lifecycle rule removes them",
			slog.String("bucket", s.bucket),
			logsafe.Attr("key", objKey),
			slog.String("error", redact.Text(err.Error())),
		)
	}
}

// Retrieve returns a streaming ReadCloser from S3.
func (s *S3Storage) Retrieve(ctx context.Context, key string) (io.ReadCloser, error) {
	objKey, err := s.objectKey(key)
	if err != nil {
		return nil, err
	}

	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(objKey),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("s3 get object: %w", err)
	}

	return resp.Body, nil
}

// Delete removes an object from S3.
func (s *S3Storage) Delete(ctx context.Context, key string) error {
	objKey, err := s.objectKey(key)
	if err != nil {
		return err
	}

	_, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(objKey),
	})
	if err != nil {
		if isS3NotFound(err) {
			return ErrNotFound
		}
		return fmt.Errorf("s3 delete object: %w", err)
	}

	return nil
}

// List returns all S3 objects matching the specified prefix.
func (s *S3Storage) List(ctx context.Context, prefix string) ([]*models.StorageObject, error) {
	cleanPrefix := s.prefix + strings.TrimPrefix(strings.TrimSpace(prefix), "/")

	var objects []*models.StorageObject
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(cleanPrefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3 list objects page: %w", err)
		}

		for _, item := range page.Contents {
			if item.Key == nil {
				continue
			}

			var size int64
			if item.Size != nil {
				size = *item.Size
			}

			modTime := time.Time{}
			if item.LastModified != nil {
				modTime = *item.LastModified
			}

			var etag string
			if item.ETag != nil {
				etag = *item.ETag
			}

			objects = append(objects, &models.StorageObject{
				Key:         strings.TrimPrefix(*item.Key, s.prefix),
				SizeBytes:   size,
				ModTime:     modTime,
				StorageType: models.StorageS3,
				ETag:        etag,
			})
		}
	}

	return objects, nil
}

// Stat retrieves metadata for a specific key in S3.
func (s *S3Storage) Stat(ctx context.Context, key string) (*models.StorageObject, error) {
	objKey, err := s.objectKey(key)
	if err != nil {
		return nil, err
	}
	cleanKey := strings.TrimPrefix(objKey, s.prefix)

	head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(objKey),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("s3 head object: %w", err)
	}

	var size int64
	if head.ContentLength != nil {
		size = *head.ContentLength
	}

	modTime := time.Time{}
	if head.LastModified != nil {
		modTime = *head.LastModified
	}

	var etag string
	if head.ETag != nil {
		etag = *head.ETag
	}

	return &models.StorageObject{
		Key:         cleanKey,
		SizeBytes:   size,
		ModTime:     modTime,
		StorageType: models.StorageS3,
		ETag:        etag,
	}, nil
}

// isS3NotFound tests if an AWS error corresponds to a 404 Not Found condition.
func isS3NotFound(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return true
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		return code == "NoSuchKey" || code == "NotFound"
	}

	return false
}
