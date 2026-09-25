package storage

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/yigitcittan/mongorescue/internal/models"
)

const (
	fakeBucket   = "mongo-backups"
	fakePageSize = 2
)

// fakeModTime is the Last-Modified value reported for every fake object.
var fakeModTime = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// fakeS3 is a minimal path-style S3 endpoint (PutObject, HeadObject, GetObject,
// DeleteObject, ListObjectsV2) backed by a map. Keys containing "denied" answer
// every request with 403 AccessDenied; keys containing "nohead" fail only HEAD.
type fakeS3 struct {
	mu       sync.Mutex
	objects  map[string][]byte
	requests []string
}

// isolateAWSEnv keeps the developer's AWS environment and shared config out of the test.
func isolateAWSEnv(t *testing.T) {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "missing")
	for k, v := range map[string]string{
		"AWS_CONFIG_FILE": missing, "AWS_SHARED_CREDENTIALS_FILE": missing, "AWS_PROFILE": "",
		"AWS_REGION": "", "AWS_DEFAULT_REGION": "", "AWS_ENDPOINT_URL": "", "AWS_ENDPOINT_URL_S3": "",
	} {
		t.Setenv(k, v)
	}
}

func newFakeS3(t *testing.T) (*fakeS3, *S3Storage) {
	t.Helper()
	isolateAWSEnv(t)

	f := &fakeS3{objects: make(map[string][]byte)}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	store, err := NewS3Storage(context.Background(), S3Config{
		Endpoint:     srv.URL,
		Bucket:       fakeBucket,
		Region:       "us-east-1",
		AccessKey:    "test-access-key",
		SecretKey:    "test-secret-key",
		UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Storage: %v", err)
	}
	return f, store
}

func (f *fakeS3) put(key string, data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = []byte(data)
}

func (f *fakeS3) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" /"+bucket+"/"+key)
	f.mu.Unlock()

	if bucket != fakeBucket {
		writeS3Error(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	if strings.Contains(key, "denied") || strings.Contains(r.URL.Query().Get("prefix"), "denied") {
		writeS3Error(w, http.StatusForbidden, "AccessDenied")
		return
	}

	if key == "" && r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
		f.list(w, r)
		return
	}

	f.mu.Lock()
	data, ok := f.objects[key]
	f.mu.Unlock()

	switch r.Method {
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeS3Error(w, http.StatusBadRequest, "IncompleteBody")
			return
		}
		f.put(key, string(body))
		w.Header().Set("ETag", `"etag-`+key+`"`)
	case http.MethodHead:
		if strings.Contains(key, "nohead") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeObjectHeaders(w, key, data)
	case http.MethodGet:
		if !ok {
			writeS3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		writeObjectHeaders(w, key, data)
		_, _ = w.Write(data)
	case http.MethodDelete:
		f.mu.Lock()
		delete(f.objects, key)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

func writeObjectHeaders(w http.ResponseWriter, key string, data []byte) {
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Last-Modified", fakeModTime.Format(http.TimeFormat))
	w.Header().Set("ETag", `"etag-`+key+`"`)
}

func writeS3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
}

type listResult struct {
	XMLName               xml.Name      `xml:"ListBucketResult"`
	Name                  string        `xml:"Name"`
	Prefix                string        `xml:"Prefix"`
	KeyCount              int           `xml:"KeyCount"`
	MaxKeys               int           `xml:"MaxKeys"`
	IsTruncated           bool          `xml:"IsTruncated"`
	NextContinuationToken string        `xml:"NextContinuationToken,omitempty"`
	Contents              []listContent `xml:"Contents"`
}

type listContent struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int    `xml:"Size"`
}

// list serves ListObjectsV2 with fakePageSize keys per page; the continuation token
// is the index of the next key.
func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	start, _ := strconv.Atoi(r.URL.Query().Get("continuation-token"))

	f.mu.Lock()
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	res := listResult{Name: fakeBucket, Prefix: prefix, MaxKeys: fakePageSize}
	end := min(start+fakePageSize, len(keys))
	for _, k := range keys[min(start, end):end] {
		res.Contents = append(res.Contents, listContent{
			Key:          k,
			LastModified: fakeModTime.Format("2006-01-02T15:04:05.000Z"),
			ETag:         `"etag-` + k + `"`,
			Size:         len(f.objects[k]),
		})
	}
	f.mu.Unlock()

	res.KeyCount = len(res.Contents)
	if end < len(keys) {
		res.IsTruncated = true
		res.NextContinuationToken = strconv.Itoa(end)
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(res)
}

func TestNewS3StorageConfig(t *testing.T) {
	isolateAWSEnv(t)

	if _, err := NewS3Storage(context.Background(), S3Config{Bucket: "  "}); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("empty bucket: expected ErrInvalidKey, got %v", err)
	}

	store, err := NewS3Storage(context.Background(), S3Config{
		Bucket:       "b",
		Endpoint:     "http://minio.internal:9000",
		UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Storage: %v", err)
	}
	opts := store.client.Options()
	if opts.Region != "us-east-1" {
		t.Errorf("region = %q, want us-east-1 default", opts.Region)
	}
	if opts.BaseEndpoint == nil || *opts.BaseEndpoint != "http://minio.internal:9000" {
		t.Errorf("base endpoint = %v", opts.BaseEndpoint)
	}
	if !opts.UsePathStyle {
		t.Error("path-style addressing not applied")
	}
	if store.bucket != "b" || store.uploader.PartSize != 5*1024*1024 || store.uploader.Concurrency != 2 {
		t.Errorf("unexpected uploader settings: bucket=%q part=%d conc=%d",
			store.bucket, store.uploader.PartSize, store.uploader.Concurrency)
	}

	awsStore, err := NewS3Storage(context.Background(), S3Config{Bucket: "b", Region: "eu-central-1"})
	if err != nil {
		t.Fatalf("NewS3Storage: %v", err)
	}
	if opts := awsStore.client.Options(); opts.Region != "eu-central-1" || opts.BaseEndpoint != nil || opts.UsePathStyle {
		t.Errorf("AWS defaults not kept: region=%q endpoint=%v pathStyle=%v", opts.Region, opts.BaseEndpoint, opts.UsePathStyle)
	}
}

func TestS3StorageRoundTrip(t *testing.T) {
	fake, store := newFakeS3(t)
	ctx := context.Background()

	obj, err := store.Save(ctx, " /orders/2026/09/a.gz ", strings.NewReader("archive-bytes"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if obj.Key != "orders/2026/09/a.gz" || obj.SizeBytes != 13 || obj.StorageType != models.StorageS3 ||
		!obj.ModTime.Equal(fakeModTime) || obj.ETag != `"etag-orders/2026/09/a.gz"` {
		t.Fatalf("unexpected saved object: %+v", obj)
	}

	stat, err := store.Stat(ctx, "/orders/2026/09/a.gz")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if stat.Key != "orders/2026/09/a.gz" || stat.SizeBytes != 13 || !stat.ModTime.Equal(fakeModTime) || stat.ETag == "" {
		t.Fatalf("unexpected stat: %+v", stat)
	}

	rc, err := store.Retrieve(ctx, "orders/2026/09/a.gz")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(data) != "archive-bytes" {
		t.Fatalf("content = %q, %v", data, err)
	}

	if err := store.Delete(ctx, "/orders/2026/09/a.gz"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Stat(ctx, "orders/2026/09/a.gz"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat after delete: expected ErrNotFound, got %v", err)
	}

	// Every request addressed the bucket path-style with the leading slash trimmed.
	for _, req := range fake.log() {
		if !strings.HasPrefix(req, "PUT /"+fakeBucket+"/orders/") && !strings.HasPrefix(req, "HEAD /"+fakeBucket+"/orders/") &&
			!strings.HasPrefix(req, "GET /"+fakeBucket+"/orders/") && !strings.HasPrefix(req, "DELETE /"+fakeBucket+"/orders/") {
			t.Errorf("unexpected request %q", req)
		}
	}
}

func TestS3StorageSaveWithoutHead(t *testing.T) {
	_, store := newFakeS3(t)
	before := time.Now()

	obj, err := store.Save(context.Background(), "db/nohead.gz", strings.NewReader("data"))
	if err != nil {
		t.Fatalf("Save must succeed when only HeadObject fails: %v", err)
	}
	if obj.Key != "db/nohead.gz" || obj.SizeBytes != 0 || obj.ModTime.Before(before) || obj.StorageType != models.StorageS3 {
		t.Fatalf("unexpected fallback object: %+v", obj)
	}
}

func TestS3StorageInvalidKey(t *testing.T) {
	_, store := newFakeS3(t)
	ctx := context.Background()

	ops := map[string]func() error{
		"Save":     func() error { _, err := store.Save(ctx, "  ", strings.NewReader("x")); return err },
		"Retrieve": func() error { _, err := store.Retrieve(ctx, ""); return err },
		"Stat":     func() error { _, err := store.Stat(ctx, " "); return err },
		"Delete":   func() error { return store.Delete(ctx, "") },
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			if err := op(); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("expected ErrInvalidKey, got %v", err)
			}
		})
	}
}

func TestS3StorageErrorMapping(t *testing.T) {
	_, store := newFakeS3(t)
	ctx := context.Background()

	tests := []struct {
		name         string
		op           func() error
		wantNotFound bool
		wantMsg      string
	}{
		{"Retrieve missing", func() error { _, err := store.Retrieve(ctx, "db/missing.gz"); return err }, true, ""},
		{"Stat missing", func() error { _, err := store.Stat(ctx, "db/missing.gz"); return err }, true, ""},
		{"Retrieve denied", func() error { _, err := store.Retrieve(ctx, "db/denied.gz"); return err }, false, "s3 get object"},
		{"Stat denied", func() error { _, err := store.Stat(ctx, "db/denied.gz"); return err }, false, "s3 head object"},
		{"Delete denied", func() error { return store.Delete(ctx, "db/denied.gz") }, false, "s3 delete object"},
		{"Save denied", func() error {
			_, err := store.Save(ctx, "db/denied.gz", strings.NewReader("x"))
			return err
		}, false, "s3 multipart upload failed"},
		{"List denied", func() error { _, err := store.List(ctx, "denied/"); return err }, false, "s3 list objects page"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.op()
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := errors.Is(err, ErrNotFound); got != tt.wantNotFound {
				t.Fatalf("errors.Is(err, ErrNotFound) = %v, want %v (err: %v)", got, tt.wantNotFound, err)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("error %q does not contain %q", err, tt.wantMsg)
			}
			if strings.Contains(err.Error(), "test-secret-key") {
				t.Fatalf("error leaks the secret key: %v", err)
			}
		})
	}
}

func TestS3StorageListPaginates(t *testing.T) {
	fake, store := newFakeS3(t)
	keys := []string{"orders/2026/08/a.gz", "orders/2026/09/b.gz", "orders/2026/09/c.gz", "orders/2026/09/d.gz", "users/2026/09/e.gz"}
	for _, k := range keys {
		fake.put(k, k)
	}

	tests := []struct {
		prefix string
		want   []string
	}{
		{"", keys},
		{"orders/", keys[:4]},
		{"/orders/2026/09", keys[1:4]},
		{" users/ ", keys[4:]},
		{"nothing/", nil},
	}
	for _, tt := range tests {
		t.Run("prefix="+tt.prefix, func(t *testing.T) {
			objs, err := store.List(context.Background(), tt.prefix)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			var got []string
			for _, o := range objs {
				if o.SizeBytes != int64(len(o.Key)) || !o.ModTime.Equal(fakeModTime) ||
					o.ETag != `"etag-`+o.Key+`"` || o.StorageType != models.StorageS3 {
					t.Errorf("unexpected object metadata: %+v", o)
				}
				got = append(got, o.Key)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("List(%q) = %v, want %v", tt.prefix, got, tt.want)
			}
		})
	}
}

func TestIsS3NotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"NoSuchKey type", &types.NoSuchKey{}, true},
		{"NotFound type", &types.NotFound{}, true},
		{"wrapped NoSuchKey", fmt.Errorf("get: %w", &types.NoSuchKey{}), true},
		{"generic NoSuchKey code", &smithy.GenericAPIError{Code: "NoSuchKey"}, true},
		{"generic NotFound code", &smithy.GenericAPIError{Code: "NotFound"}, true},
		{"access denied", &smithy.GenericAPIError{Code: "AccessDenied"}, false},
		{"no such bucket", &types.NoSuchBucket{}, false},
		{"plain error", errors.New("connection refused"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isS3NotFound(tt.err); got != tt.want {
				t.Fatalf("isS3NotFound(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestS3PrefixIsAppliedAndStripped(t *testing.T) {
	f, store := newFakeS3(t)
	store.prefix = NormalizePrefix(" /team/mongorescue/ ")
	ctx := context.Background()
	obj, err := store.Save(ctx, "/shop/2026/09/b.archive", strings.NewReader("data"))
	if err != nil || obj.Key != "shop/2026/09/b.archive" {
		t.Fatalf("Save = %+v, %v; want the key without the prefix", obj, err)
	}
	f.mu.Lock()
	_, stored := f.objects["team/mongorescue/shop/2026/09/b.archive"]
	f.mu.Unlock()
	if !stored {
		t.Fatalf("object not stored under the prefix; requests: %v", f.log())
	}
	f.put("other/outside.archive", "x")
	list, err := store.List(ctx, "shop/")
	if err != nil || len(list) != 1 || list[0].Key != "shop/2026/09/b.archive" {
		t.Fatalf("List = %+v, %v", list, err)
	}
	all, err := store.List(ctx, "")
	if err != nil || len(all) != 1 {
		t.Fatalf("List(all) = %+v, %v; want only objects under the prefix", all, err)
	}
	if st, statErr := store.Stat(ctx, "shop/2026/09/b.archive"); statErr != nil || st.Key != "shop/2026/09/b.archive" || st.SizeBytes != 4 {
		t.Fatalf("Stat = %+v, %v", st, statErr)
	}
	rc, err := store.Retrieve(ctx, "shop/2026/09/b.archive")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "data" {
		t.Fatalf("Retrieve = %q", body)
	}
	if err := store.Delete(ctx, "shop/2026/09/b.archive"); err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{"", "/", "a", "/a/", "a/b"} {
		want := map[string]string{"": "", "/": "", "a": "a/", "/a/": "a/", "a/b": "a/b/"}[in]
		if got := NormalizePrefix(in); got != want {
			t.Errorf("NormalizePrefix(%q) = %q; want %q", in, got, want)
		}
	}
}
