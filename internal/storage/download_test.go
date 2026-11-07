package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// downloadS3 is a fake S3 endpoint serving one object with Range and If-Match
// support. behave decides, per GET (counted from 1), how a request is answered.
type downloadS3 struct {
	mu       sync.Mutex
	data     []byte
	etag     string
	gets     int
	ranges   []string
	ifMatch  []string
	behave   func(n int, w http.ResponseWriter, r *http.Request) bool
	released chan struct{}
}

func (f *downloadS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeS3Error(w, http.StatusNotImplemented, "NotImplemented")
		return
	}
	f.mu.Lock()
	f.gets++
	n := f.gets
	f.ranges = append(f.ranges, r.Header.Get("Range"))
	f.ifMatch = append(f.ifMatch, r.Header.Get("If-Match"))
	etag, data := f.etag, f.data
	f.mu.Unlock()

	if f.behave != nil && f.behave(n, w, r) {
		return
	}
	if im := r.Header.Get("If-Match"); im != "" && im != etag {
		writeS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
		return
	}
	w.Header().Set("ETag", etag)
	body := data
	if rg := r.Header.Get("Range"); rg != "" {
		var start int
		if _, err := fmt.Sscanf(rg, "bytes=%d-", &start); err != nil || start >= len(data) {
			writeS3Error(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange")
			return
		}
		body = data[start:]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(data)-1, len(data)))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}

// partialThenAbort answers with the full headers and the first half of the object,
// then drops the connection, like a reset in the middle of the transfer.
func (f *downloadS3) partialThenAbort(w http.ResponseWriter) {
	f.mu.Lock()
	etag := f.etag
	f.mu.Unlock()
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Length", strconv.Itoa(len(f.data)))
	_, _ = w.Write(f.data[:len(f.data)/2])
	w.(http.Flusher).Flush()
	panic(http.ErrAbortHandler)
}

func newDownloadS3(t *testing.T, size int, stall time.Duration) (*downloadS3, *S3Storage) {
	t.Helper()
	isolateAWSEnv(t)
	f := &downloadS3{data: make([]byte, size), etag: `"v1"`, released: make(chan struct{})}
	for i := range f.data {
		f.data[i] = byte(i * 7)
	}
	srv := httptest.NewServer(f)
	t.Cleanup(func() {
		close(f.released)
		srv.Close()
	})
	st, err := NewS3Storage(context.Background(), S3Config{
		Endpoint: srv.URL, Bucket: fakeBucket, AccessKey: "a", SecretKey: "s", UsePathStyle: true,
		Stall: fixedStall("test-target", stall),
	})
	if err != nil {
		t.Fatal(err)
	}
	st.resume = resumePolicy{attempts: 3, backoff: func(int) time.Duration { return time.Millisecond }}
	return f, st
}

// TestS3RetrieveStalledDownloadFails is a peer that sends a few bytes and then goes
// silent without closing the connection: the read fails with ErrStorageStalled
// within the stall timeout instead of hanging.
func TestS3RetrieveStalledDownloadFails(t *testing.T) {
	f, st := newDownloadS3(t, 1<<16, 300*time.Millisecond)
	f.behave = func(_ int, w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("ETag", f.etag)
		w.Header().Set("Content-Length", strconv.Itoa(len(f.data)))
		_, _ = w.Write(f.data[:10])
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-f.released:
		}
		return true
	}
	rc, err := st.Retrieve(context.Background(), "db/a.archive")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	start := time.Now()
	_, err = io.ReadAll(rc)
	if !errors.Is(err, ErrStorageStalled) {
		t.Fatalf("ReadAll error = %v; want ErrStorageStalled", err)
	}
	if !strings.Contains(err.Error(), "test-target") {
		t.Errorf("error %q does not name the target", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("stall detected after %s", d)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gets != 1 {
		t.Errorf("GETs = %d; a stall must fail, not resume", f.gets)
	}
}

// TestS3RetrieveSlowConsumerIsNotAStall checks that time spent between Reads (the
// consumer inserting into MongoDB) never counts as a storage stall.
func TestS3RetrieveSlowConsumerIsNotAStall(t *testing.T) {
	_, st := newDownloadS3(t, 1<<16, 100*time.Millisecond)
	rc, err := st.Retrieve(context.Background(), "db/a.archive")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	buf := make([]byte, 1024)
	if _, err := io.ReadFull(rc, buf); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := io.Copy(io.Discard, rc); err != nil {
		t.Fatalf("read after a slow consumer: %v", err)
	}
}

// TestS3RetrieveResumesAfterReset is a connection reset in the middle of the
// object: the download continues with a ranged, conditional request and the stream
// the consumer hashes is byte-identical to the object.
func TestS3RetrieveResumesAfterReset(t *testing.T) {
	f, st := newDownloadS3(t, 1<<20, time.Minute)
	f.behave = func(n int, w http.ResponseWriter, _ *http.Request) bool {
		if n == 1 {
			f.partialThenAbort(w)
		}
		return false
	}
	rc, err := st.Retrieve(context.Background(), "db/a.archive")
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if want := sha256.Sum256(f.data); !bytes.Equal(h.Sum(nil), want[:]) {
		t.Fatal("the resumed stream differs from the object")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gets != 2 {
		t.Fatalf("GETs = %d; want 2", f.gets)
	}
	if !strings.HasPrefix(f.ranges[1], "bytes=") || f.ranges[1] == "bytes=0-" || !strings.HasSuffix(f.ranges[1], "-") {
		t.Errorf("resume Range = %q; want bytes=N- with N > 0", f.ranges[1])
	}
	if f.ifMatch[1] != f.etag {
		t.Errorf("resume If-Match = %q; want %q", f.ifMatch[1], f.etag)
	}
	if f.ranges[0] != "" || f.ifMatch[0] != "" {
		t.Errorf("first request had Range %q, If-Match %q; want none", f.ranges[0], f.ifMatch[0])
	}
}

// TestS3RetrieveETagChangeFails is an object replaced while a download is
// interrupted: the resume must fail with ErrObjectChanged and never deliver a byte
// of the new version.
func TestS3RetrieveETagChangeFails(t *testing.T) {
	for name, ignoresIfMatch := range map[string]bool{
		"precondition failed": false,
		"if-match ignored":    true,
	} {
		t.Run(name, func(t *testing.T) {
			f, st := newDownloadS3(t, 1<<20, time.Minute)
			f.behave = func(n int, w http.ResponseWriter, _ *http.Request) bool {
				switch {
				case n == 1:
					f.partialThenAbort(w)
				case ignoresIfMatch:
					// A backend that ignores If-Match answers with the new version.
					w.Header().Set("ETag", `"v2"`)
					w.Header().Set("Content-Range", "bytes 1-2/3")
					w.WriteHeader(http.StatusPartialContent)
					_, _ = w.Write([]byte("zz"))
					return true
				default:
					// The object was replaced: its ETag no longer matches If-Match.
					writeS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
					return true
				}
				return false
			}
			rc, err := st.Retrieve(context.Background(), "db/a.archive")
			if err != nil {
				t.Fatal(err)
			}
			defer rc.Close()
			got, err := io.ReadAll(rc)
			if !errors.Is(err, ErrObjectChanged) {
				t.Fatalf("ReadAll error = %v; want ErrObjectChanged", err)
			}
			if len(got) == 0 || len(got) >= len(f.data) || !bytes.Equal(got, f.data[:len(got)]) {
				t.Fatalf("delivered %d bytes; want a strict prefix of the first version", len(got))
			}
		})
	}
}

// TestS3RetrieveGivesUpWithoutProgress checks the bound on resume attempts: a
// storage that keeps resetting without delivering a byte ends the download.
func TestS3RetrieveGivesUpWithoutProgress(t *testing.T) {
	f, st := newDownloadS3(t, 1<<16, time.Minute)
	f.behave = func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n == 1 {
			f.partialThenAbort(w)
		}
		var start int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start); err != nil {
			t.Errorf("Range %q: %v", r.Header.Get("Range"), err)
		}
		w.Header().Set("ETag", f.etag)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(f.data)-1, len(f.data)))
		w.Header().Set("Content-Length", strconv.Itoa(len(f.data)))
		w.WriteHeader(http.StatusPartialContent)
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}
	rc, err := st.Retrieve(context.Background(), "db/a.archive")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	_, err = io.ReadAll(rc)
	if err == nil || !strings.Contains(err.Error(), "giving up") {
		t.Fatalf("ReadAll error = %v; want giving up", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if want := 1 + 1 + st.resume.attempts; f.gets > want+1 {
		t.Errorf("GETs = %d; the attempts are not bounded (%d)", f.gets, want)
	}
}

// TestS3RetrieveCancelDuringBackoff checks that a cancelled context ends a pending
// resume at once.
func TestS3RetrieveCancelDuringBackoff(t *testing.T) {
	f, st := newDownloadS3(t, 1<<16, time.Minute)
	st.resume.backoff = func(int) time.Duration { return time.Hour }
	f.behave = func(n int, w http.ResponseWriter, _ *http.Request) bool {
		if n == 1 {
			f.partialThenAbort(w)
		}
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rc, err := st.Retrieve(ctx, "db/a.archive")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	time.AfterFunc(200*time.Millisecond, cancel)
	if _, err := io.ReadAll(rc); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadAll error = %v; want context.Canceled", err)
	}
}

// TestS3RetrieveVersionResumesTheSameVersion checks that the versioned read of an
// Object Lock backup is watched and resumed like Retrieve, on the same version.
func TestS3RetrieveVersionResumesTheSameVersion(t *testing.T) {
	f, st := newDownloadS3(t, 1<<18, time.Minute)
	var mu sync.Mutex
	var versions []string
	f.behave = func(n int, w http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		versions = append(versions, r.URL.Query().Get("versionId"))
		mu.Unlock()
		if n == 1 {
			f.partialThenAbort(w)
		}
		return false
	}
	rc, err := st.RetrieveVersion(context.Background(), "db/a.archive", "ver-7")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil || !bytes.Equal(got, f.data) {
		t.Fatalf("ReadAll = %d bytes, %v; want the whole object", len(got), err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(versions) != 2 || versions[0] != "ver-7" || versions[1] != "ver-7" {
		t.Fatalf("versionId of the requests = %q; want ver-7 twice", versions)
	}
}

// TestS3RetrieveNotFound keeps the sentinel of a missing object.
func TestS3RetrieveNotFound(t *testing.T) {
	f, st := newDownloadS3(t, 16, time.Minute)
	f.behave = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		writeS3Error(w, http.StatusNotFound, "NoSuchKey")
		return true
	}
	if _, err := st.Retrieve(context.Background(), "db/missing.archive"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Retrieve error = %v; want ErrNotFound", err)
	}
}
