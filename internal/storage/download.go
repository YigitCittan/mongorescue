package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/redact"
)

// Resume bounds of an S3 download (see S3Storage.Retrieve).
const (
	// DefaultResumeAttempts is the number of consecutive failed attempts, without a
	// byte of progress in between, after which a download gives up.
	DefaultResumeAttempts = 5
	// resumeBackoffBase is the wait before the first resume; it doubles per
	// consecutive failure up to resumeBackoffMax.
	resumeBackoffBase = 500 * time.Millisecond
	// resumeBackoffMax caps the wait between two resume attempts.
	resumeBackoffMax = 10 * time.Second
)

// resumePolicy bounds the resumption of an interrupted download.
type resumePolicy struct {
	// attempts is the number of consecutive failures tolerated.
	attempts int
	// backoff returns the wait before resume attempt n (1 is the first).
	backoff func(n int) time.Duration
}

// defaultResumePolicy is the policy of the drivers NewS3Storage builds.
func defaultResumePolicy() resumePolicy {
	return resumePolicy{attempts: DefaultResumeAttempts, backoff: func(n int) time.Duration {
		return min(resumeBackoffBase<<min(max(n-1, 0), 5), resumeBackoffMax)
	}}
}

// s3Download is the reader Retrieve returns: the body of a GetObject that
//
//   - is watched for progress: a Read that receives no byte for the stall timeout
//     fails with an error wrapping ErrStorageStalled (time the consumer spends
//     between Reads, such as mongorestore inserting, is never a stall), and
//   - resumes after a transient failure (connection reset, truncated body, 5xx) with
//     a ranged GetObject that continues at the first byte not yet delivered and
//     carries If-Match with the ETag of the first response, so the consumer sees
//     one uninterrupted stream and its SHA-256 check is unaffected. An object that
//     was replaced in the meantime fails with ErrObjectChanged; two versions are
//     never spliced.
//
// It is read by one goroutine; Close may be called from another.
type s3Download struct {
	s      *S3Storage
	bucket string
	key    string
	// versionID, when set, selects the object version to read.
	versionID string
	parent    context.Context
	// etag is the ETag of the first response; empty (a backend that sends none)
	// means the download cannot be resumed safely.
	etag string
	// size is the object size, or -1 when unknown.
	size    int64
	timeout time.Duration

	// Owned by the reading goroutine.
	offset   int64
	failures int
	lastErr  error
	err      error // sticky terminal error

	mu     sync.Mutex
	body   io.ReadCloser
	cancel context.CancelCauseFunc
	closed bool

	// Progress watchdog.
	start    time.Time
	last     atomic.Int64
	reading  atomic.Bool
	paused   atomic.Bool
	fired    atomic.Bool
	done     chan struct{}
	wg       sync.WaitGroup
	stopOnce sync.Once
}

// Retrieve returns a streaming ReadCloser from S3. The stream is watched: a read
// that makes no progress for the storage target's stall timeout fails with an error
// wrapping ErrStorageStalled. A transient failure in the middle of the object (a
// connection reset, a truncated body, a 5xx answer) is retried with a ranged
// request that continues where the stream stopped, up to DefaultResumeAttempts
// failures in a row without progress, with an increasing, context-aware backoff;
// the caller sees one continuous stream. Every resumed request carries If-Match with
// the object's ETag: an object replaced in the meantime fails the read with
// ErrObjectChanged instead of mixing two versions. Backends that report no ETag are
// not resumed. The caller must Close the stream.
func (s *S3Storage) Retrieve(ctx context.Context, key string) (io.ReadCloser, error) {
	return s.download(ctx, key, "")
}

// download is Retrieve of the current version of key, or of version versionID when
// it is set.
func (s *S3Storage) download(ctx context.Context, key, versionID string) (io.ReadCloser, error) {
	objKey, err := s.objectKey(key)
	if err != nil {
		return nil, err
	}
	d := &s3Download{s: s, bucket: s.bucket, key: objKey, versionID: versionID, parent: ctx, size: -1,
		timeout: s.stall.timeout(), start: time.Now(), done: make(chan struct{})}
	d.startWatch()

	d.begin()
	resp, err := d.get(0)
	d.end()
	if err != nil {
		d.stop()
		if d.fired.Load() {
			return nil, d.stalledErr()
		}
		if isS3NotFound(err) || (versionID != "" && isNoSuchVersion(err)) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("s3 get object: %w", err)
	}
	d.etag = aws.ToString(resp.ETag)
	if resp.ContentLength != nil {
		d.size = *resp.ContentLength
	}
	return d, nil
}

// get issues the GetObject of the download from offset and installs its body as
// the current one. From offset 0 on, the request is the plain first one; later
// requests are ranged and conditional on the ETag.
func (d *s3Download) get(offset int64) (*s3.GetObjectOutput, error) {
	ctx, cancel := context.WithCancelCause(d.parent)
	in := &s3.GetObjectInput{Bucket: aws.String(d.bucket), Key: aws.String(d.key)}
	if d.versionID != "" {
		in.VersionId = aws.String(d.versionID)
	}
	if offset > 0 {
		in.Range = aws.String(fmt.Sprintf("bytes=%d-", offset))
		in.IfMatch = aws.String(d.etag)
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		cancel(nil)
		return nil, io.ErrClosedPipe
	}
	d.cancel = cancel
	d.mu.Unlock()

	resp, err := d.s.client.GetObject(ctx, in)
	if err != nil {
		cancel(nil)
		return nil, err
	}
	if offset > 0 {
		if err := d.checkResumed(resp, offset); err != nil {
			_ = resp.Body.Close()
			cancel(nil)
			return nil, err
		}
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		_ = resp.Body.Close()
		cancel(nil)
		return nil, io.ErrClosedPipe
	}
	d.body = resp.Body
	d.mu.Unlock()
	return resp, nil
}

// errRangeIgnored is returned when a ranged request is answered with another range.
var errRangeIgnored = errors.New("the storage did not honour the resume range")

// checkResumed verifies that resp continues the object at offset and is still the
// version the download started with.
func (d *s3Download) checkResumed(resp *s3.GetObjectOutput, offset int64) error {
	if got := aws.ToString(resp.ETag); got != d.etag {
		return fmt.Errorf("%w: ETag is %s, was %s", ErrObjectChanged, got, d.etag)
	}
	if cr := aws.ToString(resp.ContentRange); !strings.HasPrefix(cr, fmt.Sprintf("bytes %d-", offset)) {
		return fmt.Errorf("%w: asked for offset %d, got %q", errRangeIgnored, offset, cr)
	}
	return nil
}

// Read reads the next bytes of the object, resuming after transient failures.
func (d *s3Download) Read(p []byte) (int, error) {
	if d.err != nil {
		return 0, d.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	d.begin()
	defer d.end()
	for {
		d.mu.Lock()
		body, closed := d.body, d.closed
		d.mu.Unlock()
		if closed {
			return 0, io.ErrClosedPipe
		}
		if body == nil {
			if err := d.resume(); err != nil {
				d.err = err
				return 0, err
			}
			continue
		}
		n, err := body.Read(p)
		if n > 0 {
			d.offset += int64(n)
			d.failures = 0
			d.touch()
		}
		if err == nil {
			return n, nil
		}
		if errors.Is(err, io.EOF) {
			if d.size < 0 || d.offset >= d.size {
				d.stop()
				d.err = io.EOF
				return n, io.EOF
			}
			err = io.ErrUnexpectedEOF
		}
		if rerr := d.readFailed(err); rerr != nil {
			d.err = rerr
			return n, rerr
		}
		if n > 0 {
			return n, nil
		}
	}
}

// readFailed handles a failed body read: it returns the terminal error, or nil when
// the download resumes (the failed body is dropped and the next attempt opens a
// new one).
func (d *s3Download) readFailed(err error) error {
	switch {
	case d.fired.Load():
		return d.stalledErr()
	case d.parent.Err() != nil:
		return fmt.Errorf("s3 read object: %w", context.Cause(d.parent))
	case d.etag == "":
		return fmt.Errorf("s3 read object: %w", err)
	}
	d.dropBody()
	d.failures++
	d.lastErr = err
	d.s.logger.Warn("s3 download interrupted; resuming with a ranged request",
		slog.String("bucket", d.bucket),
		logsafe.Attr("key", d.key),
		slog.Int64("offset", d.offset),
		slog.Int("failures", d.failures),
		slog.String("error", redact.Text(err.Error())))
	return nil
}

// resume opens the ranged request that continues the object at the current offset.
// It returns nil once a body is installed or after a transient failure of the
// request (counted; the caller loops), and an error when the download must stop.
func (d *s3Download) resume() error {
	pol := d.s.resume
	if d.failures > pol.attempts {
		return fmt.Errorf("s3 read object: giving up after %d resume attempts without progress: %w", pol.attempts, d.lastErr)
	}
	if err := d.wait(pol.backoff(d.failures)); err != nil {
		return err
	}
	_, err := d.get(d.offset)
	switch {
	case err == nil:
		return nil
	case d.fired.Load():
		return d.stalledErr()
	case d.parent.Err() != nil:
		return fmt.Errorf("s3 read object: %w", context.Cause(d.parent))
	case errors.Is(err, io.ErrClosedPipe), errors.Is(err, ErrObjectChanged), errors.Is(err, errRangeIgnored):
		return err
	case isS3NotFound(err) || resumePermanent(err):
		return fmt.Errorf("%w: resuming at offset %d: %w", ErrObjectChanged, d.offset, err)
	}
	d.failures++
	d.lastErr = err
	d.s.logger.Warn("resuming an s3 download failed",
		slog.String("bucket", d.bucket),
		logsafe.Attr("key", d.key),
		slog.Int64("offset", d.offset),
		slog.Int("failures", d.failures),
		slog.String("error", redact.Text(err.Error())))
	return nil
}

// resumePermanent reports whether a failed ranged request will not succeed on a
// retry: a failed If-Match precondition (the object changed) or a 4xx answer other
// than throttling.
func resumePermanent(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "PreconditionFailed" {
		return true
	}
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		code := re.HTTPStatusCode()
		return code == http.StatusPreconditionFailed ||
			(code >= 400 && code < 500 && code != http.StatusRequestTimeout && code != http.StatusTooManyRequests)
	}
	return false
}

// wait sleeps for d, ending early with an error when the context ends or the
// download is closed. The wait is not a stall.
func (d *s3Download) wait(dur time.Duration) error {
	d.paused.Store(true)
	defer func() {
		d.touch()
		d.paused.Store(false)
	}()
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-d.parent.Done():
		return fmt.Errorf("s3 read object: %w", context.Cause(d.parent))
	case <-d.done:
		return io.ErrClosedPipe
	}
}

// dropBody closes the current body and cancels its request.
func (d *s3Download) dropBody() {
	d.mu.Lock()
	body, cancel := d.body, d.cancel
	d.body, d.cancel = nil, nil
	d.mu.Unlock()
	if cancel != nil {
		cancel(nil)
	}
	if body != nil {
		_ = body.Close()
	}
}

// Close stops the watchdog and releases the connection.
func (d *s3Download) Close() error {
	d.mu.Lock()
	d.closed = true
	body, cancel := d.body, d.cancel
	d.body, d.cancel = nil, nil
	d.mu.Unlock()
	if cancel != nil {
		cancel(nil)
	}
	d.stop()
	if body != nil {
		return body.Close()
	}
	return nil
}

// stalledErr is the error of a download the watchdog cancelled.
func (d *s3Download) stalledErr() error {
	return fmt.Errorf("%w: no download progress for %s from target %q", ErrStorageStalled, d.timeout, d.s.stall.Target)
}

// touch records progress on the monotonic clock.
func (d *s3Download) touch() { d.last.Store(int64(time.Since(d.start))) }

// begin marks a wait for the storage (a Read or a request) as started.
func (d *s3Download) begin() {
	d.touch()
	d.reading.Store(true)
}

// end marks the wait for the storage as over.
func (d *s3Download) end() {
	d.touch()
	d.reading.Store(false)
}

// startWatch starts the watchdog goroutine, which lives until stop or the end of
// the context.
func (d *s3Download) startWatch() {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		ticker := time.NewTicker(min(max(d.timeout/10, 10*time.Millisecond), 5*time.Second))
		defer ticker.Stop()
		for {
			select {
			case <-d.done:
				return
			case <-d.parent.Done():
				return
			case <-ticker.C:
				if !d.reading.Load() || d.paused.Load() {
					continue
				}
				if time.Since(d.start)-time.Duration(d.last.Load()) > d.timeout {
					d.fired.Store(true)
					d.mu.Lock()
					cancel := d.cancel
					d.mu.Unlock()
					if cancel != nil {
						cancel(d.stalledErr())
					}
					return
				}
			}
		}
	}()
}

// stop ends the watchdog and waits for its goroutine.
func (d *s3Download) stop() {
	d.stopOnce.Do(func() { close(d.done) })
	d.wg.Wait()
}
