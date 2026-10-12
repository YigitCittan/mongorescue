package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// Compile-time check that S3Storage supports conditional writes.
var _ LeaseStore = (*S3Storage)(nil)

// ReadVersioned returns the content of key and its ETag.
func (s *S3Storage) ReadVersioned(ctx context.Context, key string) ([]byte, string, error) {
	objKey, err := s.objectKey(key)
	if err != nil {
		return nil, "", err
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(objKey)})
	if err != nil {
		if isS3NotFound(err) {
			return nil, "", ErrNotFound
		}
		return nil, "", fmt.Errorf("s3 get object: %w", err)
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(out.Body, MaxLeaseObjectSize+1))
	if err != nil {
		return nil, "", fmt.Errorf("s3 read object: %w", err)
	}
	if len(data) > MaxLeaseObjectSize {
		return nil, "", ErrObjectTooLarge
	}
	etag := aws.ToString(out.ETag)
	if etag == "" {
		return nil, "", fmt.Errorf("%w: the storage returned no ETag", ErrConditionalWritesUnsupported)
	}
	return data, etag, nil
}

// CreateIfAbsent stores data at key with If-None-Match: *.
func (s *S3Storage) CreateIfAbsent(ctx context.Context, key string, data []byte) (string, error) {
	return s.putConditional(ctx, key, data, func(in *s3.PutObjectInput) { in.IfNoneMatch = aws.String("*") })
}

// ReplaceIfMatch stores data at key with If-Match: version.
func (s *S3Storage) ReplaceIfMatch(ctx context.Context, key string, data []byte, version string) (string, error) {
	if version == "" {
		return "", ErrPreconditionFailed
	}
	return s.putConditional(ctx, key, data, func(in *s3.PutObjectInput) { in.IfMatch = aws.String(version) })
}

// putConditional is a single-request PutObject of data with the condition set by
// cond. Object Lock headers are never sent: a lease must not leave locked versions.
func (s *S3Storage) putConditional(ctx context.Context, key string, data []byte, cond func(*s3.PutObjectInput)) (string, error) {
	if len(data) > MaxLeaseObjectSize {
		return "", ErrObjectTooLarge
	}
	objKey, err := s.objectKey(key)
	if err != nil {
		return "", err
	}
	in := &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(objKey),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
		ContentType:   aws.String("application/json"),
	}
	cond(in)
	// One attempt: a retry after a lost response would fail its own condition, and
	// the caller (the lease manager) retries on its own schedule.
	out, err := s.client.PutObject(ctx, in, func(o *s3.Options) { o.RetryMaxAttempts = 1 })
	if err != nil {
		if isS3PreconditionFailed(err) {
			return "", ErrPreconditionFailed
		}
		if isS3NotImplemented(err) {
			return "", fmt.Errorf("%w: %w", ErrConditionalWritesUnsupported, err)
		}
		return "", fmt.Errorf("s3 conditional put object: %w", err)
	}
	etag := aws.ToString(out.ETag)
	if etag == "" {
		return "", fmt.Errorf("%w: the storage returned no ETag", ErrConditionalWritesUnsupported)
	}
	return etag, nil
}

// isS3PreconditionFailed reports a failed condition: 412 Precondition Failed, 409
// ConditionalRequestConflict (a concurrent conditional write won), or NoSuchKey for
// an If-Match on an object that no longer exists.
func isS3PreconditionFailed(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "PreconditionFailed", "ConditionalRequestConflict", "NoSuchKey":
			return true
		case "NoSuchBucket", "AccessDenied":
			return false
		}
	}
	var resp *awshttp.ResponseError
	if errors.As(err, &resp) {
		switch resp.HTTPStatusCode() {
		case http.StatusPreconditionFailed, http.StatusConflict:
			return true
		}
	}
	return false
}

// isS3NotImplemented reports a service that refuses conditional headers.
func isS3NotImplemented(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "NotImplemented" {
		return true
	}
	var resp *awshttp.ResponseError
	return errors.As(err, &resp) && resp.HTTPStatusCode() == http.StatusNotImplemented
}
