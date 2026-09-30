package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// multipartS3 is a minimal S3 endpoint for multipart uploads: it creates uploads,
// accepts parts and records aborts. Completion is never reached by these tests.
type multipartS3 struct {
	mu      sync.Mutex
	parts   int
	aborted []string
}

func (f *multipartS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	_, _ = io.Copy(io.Discard, r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>k</Key><UploadId>upload-1</UploadId></InitiateMultipartUploadResult>`, fakeBucket)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		f.parts++
		w.Header().Set("ETag", fmt.Sprintf(`"part-%d"`, f.parts))
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		f.aborted = append(f.aborted, q.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)
	default:
		writeS3Error(w, http.StatusNotImplemented, "NotImplemented")
	}
}

// cancelAfterReader yields limit bytes, then cancels the upload's context and blocks
// until it is done, like a backup cancelled while mongodump is still streaming.
type cancelAfterReader struct {
	ctx    context.Context
	cancel context.CancelFunc
	left   int
}

func (r *cancelAfterReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		r.cancel()
		<-r.ctx.Done()
		return 0, r.ctx.Err()
	}
	n := min(len(p), r.left)
	clear(p[:n])
	r.left -= n
	return n, nil
}

// TestS3SaveAbortsMultipartUploadAfterCancellation checks that a cancelled upload
// does not leave its parts behind: the abort must be sent although the upload's
// context is already cancelled.
func TestS3SaveAbortsMultipartUploadAfterCancellation(t *testing.T) {
	isolateAWSEnv(t)
	fake := &multipartS3{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	st, err := NewS3Storage(context.Background(), S3Config{
		Endpoint: srv.URL, Bucket: fakeBucket, AccessKey: "a", SecretKey: "s", UsePathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Two full parts, so the multipart upload is created before the cancellation.
	body := &cancelAfterReader{ctx: ctx, cancel: cancel, left: 11 << 20}
	if _, err := st.Save(ctx, "db/cancelled.archive", body); !errors.Is(err, context.Canceled) {
		t.Fatalf("Save error = %v; want context.Canceled", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.aborted) == 0 || fake.aborted[0] != "upload-1" {
		t.Fatalf("aborted uploads = %v; want upload-1 aborted", fake.aborted)
	}
}

// TestS3SaveReadErrorAbortsMultipartUpload covers a failing source stream (e.g. a
// failed dump) with a live context.
func TestS3SaveReadErrorAbortsMultipartUpload(t *testing.T) {
	isolateAWSEnv(t)
	fake := &multipartS3{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	st, err := NewS3Storage(context.Background(), S3Config{
		Endpoint: srv.URL, Bucket: fakeBucket, AccessKey: "a", SecretKey: "s", UsePathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("dump failed")
	body := io.MultiReader(bytes.NewReader(make([]byte, 11<<20)), &errReader{err: boom})
	if _, err := st.Save(context.Background(), "db/failed.archive", body); !errors.Is(err, boom) {
		t.Fatalf("Save error = %v; want %v", err, boom)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.aborted) == 0 {
		t.Fatal("the failed multipart upload was not aborted")
	}
}

type errReader struct{ err error }

func (r *errReader) Read([]byte) (int, error) { return 0, r.err }
