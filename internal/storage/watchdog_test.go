package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/throttle"
)

// testStallTimeout is the stall timeout of these tests.
const testStallTimeout = 150 * time.Millisecond

// fixedStall returns a StallWatch with timeout d.
func fixedStall(target string, d time.Duration) StallWatch {
	return StallWatch{Target: target, Timeout: func() time.Duration { return d }}
}

// TestStallWatchCancelsBlockedUpload is an upload whose writer blocks forever after
// the first chunk (a storage partition): it fails with ErrStorageStalled within the
// stall timeout, not with the cancellation the watchdog caused.
func TestStallWatchCancelsBlockedUpload(t *testing.T) {
	start := time.Now()
	ctx, r, finish := fixedStall("offsite", testStallTimeout).Start(context.Background(), strings.NewReader("archive bytes"))
	if _, err := r.Read(make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the watchdog never cancelled the blocked upload")
	}
	err := finish(ctx.Err())
	if !errors.Is(err, ErrStorageStalled) || errors.Is(err, context.Canceled) {
		t.Fatalf("finish = %v; want ErrStorageStalled only", err)
	}
	if !strings.Contains(err.Error(), `no upload progress for 150ms to target "offsite"`) {
		t.Fatalf("error %q does not name the timeout and target", err)
	}
	if elapsed := time.Since(start); elapsed < testStallTimeout || elapsed > 5*time.Second {
		t.Fatalf("stall detected after %v; want about %v", elapsed, testStallTimeout)
	}
}

// slowSource pauses before each Read, like a throttled or slow dump.
type slowSource struct {
	r     io.Reader
	pause time.Duration
}

func (s *slowSource) Read(p []byte) (int, error) {
	time.Sleep(s.pause)
	return s.r.Read(p)
}

// TestStallWatchIgnoresSlowSource checks that time spent waiting for the source is
// not a stall: every Read takes longer than the stall timeout.
func TestStallWatchIgnoresSlowSource(t *testing.T) {
	src := &slowSource{r: strings.NewReader("abcdef"), pause: 2 * testStallTimeout}
	ctx, r, finish := fixedStall("t", testStallTimeout).Start(context.Background(), src)
	buf := make([]byte, 2)
	for {
		if _, err := r.Read(buf); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if ctx.Err() != nil {
		t.Fatalf("a slow source stalled the upload: %v", context.Cause(ctx))
	}
	if err := finish(nil); err != nil {
		t.Fatal(err)
	}
}

// TestStallWatchKeepsOtherErrors checks that finish passes on errors the watchdog
// did not cause, a cancelled parent included, and that it is idempotent.
func TestStallWatchKeepsOtherErrors(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ctx, _, finish := fixedStall("t", time.Hour).Start(parent, strings.NewReader(""))
	cancel()
	<-ctx.Done()
	if err := finish(ctx.Err()); !errors.Is(err, context.Canceled) || errors.Is(err, ErrStorageStalled) {
		t.Fatalf("finish = %v; want context.Canceled", err)
	}
	boom := errors.New("boom")
	if err := finish(boom); !errors.Is(err, boom) {
		t.Fatalf("finish = %v; want %v", err, boom)
	}
	if got := (StallWatch{}).timeout(); got != DefaultStallTimeout {
		t.Fatalf("default timeout = %v", got)
	}
}

// partitionS3 is an S3 endpoint for complete multipart uploads. With hang set, part
// uploads neither read their body nor answer until release is closed, like a
// network partition.
type partitionS3 struct {
	hang    atomic.Bool
	release chan struct{}
	mu      sync.Mutex
	parts   int
	size    int64
	aborted []string
}

func (f *partitionS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if f.hang.Load() && r.Method == http.MethodPut && q.Has("partNumber") {
		<-f.release
		return
	}
	n, _ := io.Copy(io.Discard, r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>k</Key><UploadId>upload-1</UploadId></InitiateMultipartUploadResult>`, fakeBucket)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		f.parts++
		f.size += n
		w.Header().Set("ETag", fmt.Sprintf(`"part-%d"`, f.parts))
	case r.Method == http.MethodPost && q.Has("uploadId"):
		_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><CompleteMultipartUploadResult><Bucket>%s</Bucket><Key>k</Key><ETag>"done"</ETag></CompleteMultipartUploadResult>`, fakeBucket)
	case r.Method == http.MethodHead:
		w.Header().Set("Content-Length", fmt.Sprint(f.size))
		w.Header().Set("Last-Modified", fakeModTime.Format(http.TimeFormat))
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		f.aborted = append(f.aborted, q.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)
	default:
		writeS3Error(w, http.StatusNotImplemented, "NotImplemented")
	}
}

// newPartitionS3 returns an S3 driver with 5 MiB parts against f.
func newPartitionS3(t *testing.T, f *partitionS3) *S3Storage {
	t.Helper()
	isolateAWSEnv(t)
	f.release = make(chan struct{})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	// Runs before srv.Close: a handler that never read its body would block it.
	t.Cleanup(func() { close(f.release) })
	st, err := NewS3Storage(context.Background(), S3Config{
		Endpoint: srv.URL, Bucket: fakeBucket, AccessKey: "a", SecretKey: "s", UsePathStyle: true,
		PartSizeMB: 5, Stall: fixedStall("offsite", testStallTimeout),
	})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// TestS3SaveStalledUploadFailsAndAborts covers #136: parts that never reach the
// target fail the upload within the stall timeout, the multipart upload is aborted,
// and the next upload succeeds.
func TestS3SaveStalledUploadFailsAndAborts(t *testing.T) {
	fake := &partitionS3{}
	fake.hang.Store(true)
	st := newPartitionS3(t, fake)

	start := time.Now()
	body := io.MultiReader(bytes.NewReader(make([]byte, 11<<20))) // not seekable
	_, err := st.Save(context.Background(), "db/stalled.archive", body)
	if !errors.Is(err, ErrStorageStalled) {
		t.Fatalf("Save error = %v; want ErrStorageStalled", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the stall was detected after %v", elapsed)
	}
	fake.hang.Store(false)
	fake.mu.Lock()
	aborted := fake.aborted
	fake.mu.Unlock()
	if len(aborted) == 0 || aborted[0] != "upload-1" {
		t.Fatalf("aborted uploads = %v; want upload-1 aborted", aborted)
	}

	if _, err := st.Save(context.Background(), "db/next.archive", io.MultiReader(bytes.NewReader(make([]byte, 6<<20)))); err != nil {
		t.Fatalf("the next upload failed: %v", err)
	}
}

// TestS3SaveThrottledUploadIsNoStall checks that an upload capped below its natural
// speed (#100) is no stall: reading one part takes several stall timeouts.
func TestS3SaveThrottledUploadIsNoStall(t *testing.T) {
	st := newPartitionS3(t, &partitionS3{})
	const size = 6 << 20
	body := throttle.NewReader(context.Background(), bytes.NewReader(make([]byte, size)), 8<<20) // about 0.75 s
	obj, err := st.Save(context.Background(), "db/throttled.archive", body)
	if err != nil {
		t.Fatalf("throttled upload failed: %v", err)
	}
	if obj.SizeBytes != size {
		t.Fatalf("size = %d; want %d", obj.SizeBytes, size)
	}
}

// TestLocalSaveStallWatch checks the local driver runs uploads under the watchdog.
func TestLocalSaveStallWatch(t *testing.T) {
	st, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st.WithStallWatch(fixedStall("local", testStallTimeout))
	src := &slowSource{r: strings.NewReader("payload"), pause: 2 * testStallTimeout}
	if _, err := st.Save(context.Background(), "db/a.archive", src); err != nil {
		t.Fatalf("a slow source must not stall a local upload: %v", err)
	}
}
