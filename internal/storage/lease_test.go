package storage

import (
	"context"
	"crypto/md5" //nolint:gosec // G501: S3 ETags of single-part uploads are MD5 digests; the fake mimics them.
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// leaseFakeS3 is a path-style S3 endpoint with conditional PutObject (If-None-Match:
// * and If-Match), GetObject with ETags and DeleteObject. With ignoreConditions it
// accepts every write, like a service that ignores the headers.
type leaseFakeS3 struct {
	mu               sync.Mutex
	objects          map[string][]byte
	ignoreConditions bool
	notImplemented   bool
	puts             int
}

func etagOf(data []byte) string {
	sum := md5.Sum(data) //nolint:gosec // G401: see the import.
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func (f *leaseFakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != fakeBucket {
		writeS3Error(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	data, exists := f.objects[key]
	switch r.Method {
	case http.MethodPut:
		f.puts++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeS3Error(w, http.StatusBadRequest, "IncompleteBody")
			return
		}
		inm, im := r.Header.Get("If-None-Match"), r.Header.Get("If-Match")
		if f.notImplemented && (inm != "" || im != "") {
			writeS3Error(w, http.StatusNotImplemented, "NotImplemented")
			return
		}
		if !f.ignoreConditions {
			if inm == "*" && exists {
				writeS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
				return
			}
			if im != "" && !exists {
				writeS3Error(w, http.StatusNotFound, "NoSuchKey")
				return
			}
			if im != "" && im != etagOf(data) {
				writeS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
				return
			}
		}
		f.objects[key] = body
		w.Header().Set("ETag", etagOf(body))
	case http.MethodGet:
		if !exists {
			writeS3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("ETag", etagOf(data))
		_, _ = w.Write(data)
	case http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func newLeaseFakeS3(t *testing.T, f *leaseFakeS3) *S3Storage {
	t.Helper()
	isolateAWSEnv(t)
	f.objects = map[string][]byte{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	st, err := NewS3Storage(context.Background(), S3Config{
		Endpoint: srv.URL, Bucket: fakeBucket, Region: "us-east-1", Prefix: "pfx",
		AccessKey: "test-access-key", SecretKey: "test-secret-key", UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Storage: %v", err)
	}
	return st
}

// leaseStores returns the local driver and the S3 driver against the conditional
// fake.
func leaseStores(t *testing.T) map[string]LeaseStore {
	t.Helper()
	local, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return map[string]LeaseStore{"local": local, "s3": newLeaseFakeS3(t, &leaseFakeS3{})}
}

func TestLeaseStoreContract(t *testing.T) {
	for name, ls := range leaseStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			key := "_mongorescue/ha/test/lease.json"
			if _, _, err := ls.ReadVersioned(ctx, key); !errors.Is(err, ErrNotFound) {
				t.Fatalf("ReadVersioned of a missing key = %v; want ErrNotFound", err)
			}
			if _, err := ls.ReplaceIfMatch(ctx, key, []byte(`{"a":0}`), "v"); !errors.Is(err, ErrPreconditionFailed) {
				t.Fatalf("ReplaceIfMatch of a missing key = %v; want ErrPreconditionFailed", err)
			}
			v1, err := ls.CreateIfAbsent(ctx, key, []byte(`{"a":1}`))
			if err != nil || v1 == "" {
				t.Fatalf("CreateIfAbsent = %q, %v", v1, err)
			}
			if _, err = ls.CreateIfAbsent(ctx, key, []byte(`{"a":2}`)); !errors.Is(err, ErrPreconditionFailed) {
				t.Fatalf("second CreateIfAbsent = %v; want ErrPreconditionFailed", err)
			}
			data, v, err := ls.ReadVersioned(ctx, key)
			if err != nil || string(data) != `{"a":1}` || v != v1 {
				t.Fatalf("ReadVersioned = %q, %q, %v; want the first write with version %q", data, v, err, v1)
			}
			if _, err = ls.ReplaceIfMatch(ctx, key, []byte(`{"a":3}`), wrongVersion(v1)); !errors.Is(err, ErrPreconditionFailed) {
				t.Fatalf("ReplaceIfMatch with a wrong version = %v; want ErrPreconditionFailed", err)
			}
			v2, err := ls.ReplaceIfMatch(ctx, key, []byte(`{"a":4}`), v1)
			if err != nil || v2 == "" || v2 == v1 {
				t.Fatalf("ReplaceIfMatch = %q, %v; want a new version", v2, err)
			}
			if _, err = ls.ReplaceIfMatch(ctx, key, []byte(`{"a":5}`), v1); !errors.Is(err, ErrPreconditionFailed) {
				t.Fatalf("ReplaceIfMatch with a stale version = %v; want ErrPreconditionFailed", err)
			}
			data, v, err = ls.ReadVersioned(ctx, key)
			if err != nil || string(data) != `{"a":4}` || v != v2 {
				t.Fatalf("ReadVersioned = %q, %q, %v; want the replaced content", data, v, err)
			}
			big := make([]byte, MaxLeaseObjectSize+1)
			if _, err = ls.CreateIfAbsent(ctx, "big.json", big); !errors.Is(err, ErrObjectTooLarge) {
				t.Fatalf("CreateIfAbsent of a large object = %v; want ErrObjectTooLarge", err)
			}
			if _, err = ls.CreateIfAbsent(ctx, "../escape.json", []byte("x")); name == "local" && err == nil {
				t.Fatal("CreateIfAbsent outside the root succeeded")
			}
		})
	}
}

func TestLeaseStoreConcurrentReplaceHasOneWinner(t *testing.T) {
	for name, ls := range leaseStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			v0, err := ls.CreateIfAbsent(ctx, "lease.json", []byte(`{"seq":0}`))
			if err != nil {
				t.Fatal(err)
			}
			const writers = 8
			var wg sync.WaitGroup
			var mu sync.Mutex
			wins := 0
			for i := range writers {
				wg.Go(func() {
					_, err := ls.ReplaceIfMatch(ctx, "lease.json", []byte(`{"seq":`+string(rune('1'+i))+`}`), v0)
					if err == nil {
						mu.Lock()
						wins++
						mu.Unlock()
					} else if !errors.Is(err, ErrPreconditionFailed) {
						t.Errorf("ReplaceIfMatch: %v", err)
					}
				})
			}
			wg.Wait()
			if wins != 1 {
				t.Fatalf("%d concurrent replaces of one version succeeded; want exactly 1", wins)
			}
		})
	}
}

func TestProbeLeaseStore(t *testing.T) {
	for name, ls := range leaseStores(t) {
		t.Run(name, func(t *testing.T) {
			del := ls.(Storage).Delete
			if err := ProbeLeaseStore(context.Background(), ls, "_mongorescue/ha/c/probe.json", del); err != nil {
				t.Fatalf("ProbeLeaseStore: %v", err)
			}
			if _, _, err := ls.ReadVersioned(context.Background(), "_mongorescue/ha/c/probe.json"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("the probe object was not deleted: %v", err)
			}
		})
	}
}

func TestProbeLeaseStoreCatchesIgnoredConditions(t *testing.T) {
	st := newLeaseFakeS3(t, &leaseFakeS3{ignoreConditions: true})
	err := ProbeLeaseStore(context.Background(), st, "probe.json", st.Delete)
	if !errors.Is(err, ErrConditionalWritesUnsupported) {
		t.Fatalf("ProbeLeaseStore against a service that ignores conditions = %v; want ErrConditionalWritesUnsupported", err)
	}
}

func TestS3ConditionalNotImplemented(t *testing.T) {
	st := newLeaseFakeS3(t, &leaseFakeS3{notImplemented: true})
	if _, err := st.CreateIfAbsent(context.Background(), "lease.json", []byte("{}")); !errors.Is(err, ErrConditionalWritesUnsupported) {
		t.Fatalf("CreateIfAbsent against a service without conditional writes = %v; want ErrConditionalWritesUnsupported", err)
	}
}

func TestS3ConditionalPutIsNotRetried(t *testing.T) {
	f := &leaseFakeS3{}
	st := newLeaseFakeS3(t, f)
	if _, err := st.CreateIfAbsent(context.Background(), "lease.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateIfAbsent(context.Background(), "lease.json", []byte("{}")); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.puts != 2 {
		t.Fatalf("%d PUT requests; want 2 (one per call)", f.puts)
	}
	if _, ok := f.objects["pfx/lease.json"]; !ok {
		t.Fatalf("the target prefix was not applied: %v", f.objects)
	}
}

func TestAsLeaseStore(t *testing.T) {
	local, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = AsLeaseStore(local); err != nil {
		t.Fatalf("AsLeaseStore(local) = %v", err)
	}
	if _, err = AsLeaseStore(NewMockStorage()); !errors.Is(err, ErrConditionalWritesUnsupported) {
		t.Fatalf("AsLeaseStore(mock) = %v; want ErrConditionalWritesUnsupported", err)
	}
}

func TestLocalLeaseLockWaitsForContext(t *testing.T) {
	dir := t.TempDir()
	local, err := NewLocalStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Hold the sidecar lock through a second open file description.
	lock, err := os.OpenFile(filepath.Join(dir, "lease.json"+leaseLockSuffix), os.O_RDWR|os.O_CREATE, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if ok, lockErr := tryFileLock(lock); !ok || lockErr != nil {
		t.Fatalf("tryFileLock = %v, %v", ok, lockErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err = local.CreateIfAbsent(ctx, "lease.json", []byte("{}")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CreateIfAbsent under a held lock = %v; want context.DeadlineExceeded", err)
	}
	if err = fileUnlock(lock); err != nil {
		t.Fatal(err)
	}
	if _, err = local.CreateIfAbsent(context.Background(), "lease.json", []byte("{}")); err != nil {
		t.Fatalf("CreateIfAbsent after unlock: %v", err)
	}
}

func TestWrongVersion(t *testing.T) {
	for _, v := range []string{`"abc"`, "abc", `"00000000000000000000000000000000"`, "00000000000000000000000000000000"} {
		if w := wrongVersion(v); w == v || w == "" {
			t.Fatalf("wrongVersion(%q) = %q", v, w)
		}
	}
}
