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
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// lockS3 is a minimal S3 endpoint for Object Lock: it records the headers and
// query of every request, stores puts and answers the bucket's lock and
// versioning configuration.
type lockS3 struct {
	mu         sync.Mutex
	requests   []*http.Request
	bodies     []string
	lockConfig string // "" answers ObjectLockConfigurationNotFoundError
	versioning string
	objects    map[string][]byte
}

func (f *lockS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	q := r.URL.Query()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Clone(context.Background()))
	f.bodies = append(f.bodies, string(body))
	_, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodGet && q.Has("object-lock"):
		if f.lockConfig == "" {
			writeS3Error(w, http.StatusNotFound, "ObjectLockConfigurationNotFoundError")
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprintf(w, `<ObjectLockConfiguration><ObjectLockEnabled>%s</ObjectLockEnabled></ObjectLockConfiguration>`, f.lockConfig)
	case r.Method == http.MethodGet && q.Has("versioning"):
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprintf(w, `<VersioningConfiguration><Status>%s</Status></VersioningConfiguration>`, f.versioning)
	case r.Method == http.MethodPut && q.Has("legal-hold"):
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost && q.Has("uploads"):
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>u1</UploadId></InitiateMultipartUploadResult>`, fakeBucket, key)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		w.Header().Set("ETag", `"part-`+q.Get("partNumber")+`"`)
	case r.Method == http.MethodPost && q.Has("uploadId"):
		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("x-amz-version-id", "v-multi")
		_, _ = fmt.Fprintf(w, `<CompleteMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><ETag>"multi"</ETag></CompleteMultipartUploadResult>`, fakeBucket, key)
		f.objects[key] = nil
	case r.Method == http.MethodPut:
		f.objects[key] = body
		w.Header().Set("ETag", `"etag"`)
		w.Header().Set("x-amz-version-id", "v-"+key)
	case r.Method == http.MethodHead:
		w.Header().Set("Content-Length", fmt.Sprint(len(f.objects[key])))
		w.Header().Set("x-amz-version-id", q.Get("versionId"))
	case r.Method == http.MethodGet:
		_, _ = w.Write(f.objects[key])
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		writeS3Error(w, http.StatusNotImplemented, "NotImplemented")
	}
}

// find returns the first recorded request with method and query parameter query
// ("" for a request on the object itself).
func (f *lockS3) find(method, query string) (*http.Request, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, r := range f.requests {
		q := r.URL.Query()
		plain := !q.Has("partNumber") && !q.Has("legal-hold") && !q.Has("uploads") && !q.Has("uploadId")
		if r.Method == method && (query == "" && plain || query != "" && q.Has(query)) {
			return r, f.bodies[i]
		}
	}
	return nil, ""
}

func newLockS3(t *testing.T, f *lockS3, lock ObjectLock) *S3Storage {
	t.Helper()
	isolateAWSEnv(t)
	if f.objects == nil {
		f.objects = map[string][]byte{}
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	st, err := NewS3Storage(context.Background(), S3Config{
		Endpoint: srv.URL, Bucket: fakeBucket, AccessKey: "a", SecretKey: "s", UsePathStyle: true,
		PartSizeMB: models.MinS3PartSizeMB, ObjectLock: lock,
	})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

var lockNow = time.Date(2026, 10, 1, 12, 0, 0, 500, time.UTC)

// TestS3SaveSetsObjectLockPerMode checks the upload parameters of every mode: a
// locked target sends the mode, the retain-until date (now + retention) and a CRC32
// checksum, and returns the version and the date; an unlocked one sends none.
func TestS3SaveSetsObjectLockPerMode(t *testing.T) {
	for _, tc := range []struct {
		mode     models.ObjectLockMode
		wantMode string
	}{
		{"", ""},
		{models.ObjectLockNone, ""},
		{models.ObjectLockGovernance, "GOVERNANCE"},
		{models.ObjectLockCompliance, "COMPLIANCE"},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			f := &lockS3{}
			st := newLockS3(t, f, ObjectLock{Mode: tc.mode, RetentionDays: 30, Now: func() time.Time { return lockNow }})
			obj, err := st.Save(context.Background(), "a.archive", strings.NewReader("data"))
			if err != nil {
				t.Fatal(err)
			}
			put, _ := f.find(http.MethodPut, "")
			if put == nil {
				t.Fatal("no PutObject")
			}
			wantUntil := lockNow.Add(30 * 24 * time.Hour).Truncate(time.Second)
			if got := put.Header.Get("X-Amz-Object-Lock-Mode"); got != tc.wantMode {
				t.Errorf("lock mode header = %q; want %q", got, tc.wantMode)
			}
			if obj.VersionID != "v-a.archive" {
				t.Errorf("version = %q", obj.VersionID)
			}
			if tc.wantMode == "" {
				if put.Header.Get("X-Amz-Object-Lock-Retain-Until-Date") != "" || put.Header.Get("X-Amz-Checksum-Crc32") != "" || obj.RetainUntil != nil {
					t.Errorf("unlocked upload sent lock or checksum headers: %v", put.Header)
				}
				return
			}
			until, err := time.Parse(time.RFC3339, put.Header.Get("X-Amz-Object-Lock-Retain-Until-Date"))
			if err != nil || !until.Equal(wantUntil) {
				t.Errorf("retain-until header = %q; want %s", put.Header.Get("X-Amz-Object-Lock-Retain-Until-Date"), wantUntil)
			}
			if put.Header.Get("X-Amz-Checksum-Crc32") == "" && put.Header.Get("X-Amz-Trailer") == "" {
				t.Errorf("locked upload without a checksum: %v", put.Header)
			}
			if obj.RetainUntil == nil || !obj.RetainUntil.Equal(wantUntil) {
				t.Errorf("RetainUntil = %v; want %s", obj.RetainUntil, wantUntil)
			}
		})
	}
}

// TestS3MultipartSaveSetsObjectLock checks that a multipart upload (an archive
// larger than one part) carries the lock on CreateMultipartUpload and a checksum on
// every part, and still streams with the configured part size.
func TestS3MultipartSaveSetsObjectLock(t *testing.T) {
	f := &lockS3{}
	st := newLockS3(t, f, ObjectLock{Mode: models.ObjectLockCompliance, RetentionDays: 1, Now: func() time.Time { return lockNow }})
	data := bytes.Repeat([]byte("x"), models.MinS3PartSizeMB<<20+10)
	obj, err := st.Save(context.Background(), "big.archive", io.MultiReader(bytes.NewReader(data)))
	if err != nil {
		t.Fatal(err)
	}
	create, _ := f.find(http.MethodPost, "uploads")
	if create == nil || create.Header.Get("X-Amz-Object-Lock-Mode") != "COMPLIANCE" || create.Header.Get("X-Amz-Object-Lock-Retain-Until-Date") == "" {
		t.Fatalf("CreateMultipartUpload without the lock: %v", create)
	}
	if create.Header.Get("X-Amz-Checksum-Algorithm") != "CRC32" {
		t.Errorf("checksum algorithm = %q; want CRC32", create.Header.Get("X-Amz-Checksum-Algorithm"))
	}
	part, _ := f.find(http.MethodPut, "partNumber")
	if part == nil || part.Header.Get("X-Amz-Checksum-Crc32") == "" && part.Header.Get("X-Amz-Trailer") == "" {
		t.Errorf("part without a checksum: %v", part)
	}
	if obj.VersionID != "v-multi" || obj.RetainUntil == nil {
		t.Errorf("object = %+v; want version v-multi and a retain-until date", obj)
	}
}

// TestS3ObjectLockConfigValidated refuses unknown modes and retentions out of range.
func TestS3ObjectLockConfigValidated(t *testing.T) {
	isolateAWSEnv(t)
	for _, l := range []ObjectLock{
		{Mode: "legal"},
		{Mode: models.ObjectLockGovernance, RetentionDays: 0},
		{Mode: models.ObjectLockCompliance, RetentionDays: models.MaxObjectLockRetentionDays + 1},
	} {
		if _, err := NewS3Storage(context.Background(), S3Config{Bucket: "b", ObjectLock: l}); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%+v = %v; want ErrInvalidConfig", l, err)
		}
	}
}

// TestS3CheckObjectLock refuses buckets without Object Lock or versioning and
// accepts a lock-enabled one, without ever writing the bucket configuration.
func TestS3CheckObjectLock(t *testing.T) {
	for _, tc := range []struct {
		name, lock, versioning string
		ok                     bool
	}{
		{"no lock configuration", "", "Enabled", false},
		{"versioning suspended", "Enabled", "Suspended", false},
		{"lock enabled", "Enabled", "Enabled", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &lockS3{lockConfig: tc.lock, versioning: tc.versioning}
			st := newLockS3(t, f, ObjectLock{Mode: models.ObjectLockGovernance, RetentionDays: 7})
			err := st.CheckObjectLock(context.Background())
			if tc.ok != (err == nil) {
				t.Fatalf("CheckObjectLock = %v; want ok=%v", err, tc.ok)
			}
			if !tc.ok && !errors.Is(err, ErrObjectLockUnavailable) {
				t.Errorf("error %v does not wrap ErrObjectLockUnavailable", err)
			}
			for _, r := range f.requests {
				if r.Method != http.MethodGet {
					t.Errorf("the check sent %s %s", r.Method, r.URL)
				}
			}
		})
	}
}

// TestS3VersionOperations checks that versioned reads, deletes and legal holds name
// the version, and that the generic helpers fall back for other drivers.
func TestS3VersionOperations(t *testing.T) {
	f := &lockS3{lockConfig: "Enabled", versioning: "Enabled"}
	st := newLockS3(t, f, ObjectLock{})
	ctx := context.Background()
	f.objects["k"] = []byte("v")
	rc, err := RetrieveVersion(ctx, st, "k", "v1")
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if err = DeleteVersion(ctx, st, "k", "v1"); err != nil {
		t.Fatal(err)
	}
	if err = SetLegalHold(ctx, st, "k", "v1", true); err != nil {
		t.Fatal(err)
	}
	if err = SetLegalHold(ctx, st, "k", "v1", false); err != nil {
		t.Fatal(err)
	}
	var gets, deletes, holds []string
	for i, r := range f.requests {
		switch {
		case r.Method == http.MethodGet:
			gets = append(gets, r.URL.Query().Get("versionId"))
		case r.Method == http.MethodDelete:
			deletes = append(deletes, r.URL.Query().Get("versionId"))
		case r.URL.Query().Has("legal-hold"):
			holds = append(holds, r.URL.Query().Get("versionId")+":"+map[bool]string{true: "ON", false: "OFF"}[strings.Contains(f.bodies[i], "ON")])
		}
	}
	if fmt.Sprint(gets, deletes, holds) != "[v1] [v1] [v1:ON v1:OFF]" {
		t.Fatalf("gets=%v deletes=%v holds=%v", gets, deletes, holds)
	}

	mock := NewMockStorage()
	if _, err = mock.Save(ctx, "m", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if rc, err = RetrieveVersion(ctx, mock, "m", "v1"); err != nil {
		t.Fatalf("fallback retrieve: %v", err)
	}
	_ = rc.Close()
	if err = DeleteVersion(ctx, mock, "m", "v1"); err != nil {
		t.Fatalf("fallback delete: %v", err)
	}
	if err = SetLegalHold(ctx, mock, "m", "", true); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("legal hold on a mock = %v; want ErrUnsupported", err)
	}
}

// statLocked is a versioned mock whose objects carry a version and a retention.
type statLocked struct {
	*MockStorage
	until   time.Time
	deleted []string
}

func (s *statLocked) Stat(ctx context.Context, key string) (*models.StorageObject, error) {
	obj, err := s.MockStorage.Stat(ctx, key)
	if obj != nil {
		obj.VersionID, obj.RetainUntil = "v-"+key, &s.until
	}
	return obj, err
}

func (s *statLocked) RetrieveVersion(ctx context.Context, key, _ string) (io.ReadCloser, error) {
	return s.Retrieve(ctx, key)
}

func (s *statLocked) DeleteVersion(ctx context.Context, key, versionID string) error {
	s.deleted = append(s.deleted, versionID)
	return s.Delete(ctx, key)
}

// TestDeleteUnlockedWaitsForTheRetention checks that an object under retention is
// kept (with the end of its retention returned) and deleted by version afterwards.
func TestDeleteUnlockedWaitsForTheRetention(t *testing.T) {
	ctx := context.Background()
	s := &statLocked{MockStorage: NewMockStorage(), until: lockNow.Add(24 * time.Hour)}
	if _, err := s.Save(ctx, "snap.db", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	until, err := DeleteUnlocked(ctx, s, "snap.db", lockNow)
	if err != nil || until == nil || !until.Equal(s.until) || len(s.deleted) != 0 {
		t.Fatalf("DeleteUnlocked under retention = %v, %v (deleted %v); want kept until %s", until, err, s.deleted, s.until)
	}
	if until, err = DeleteUnlocked(ctx, s, "snap.db", s.until); err != nil || until != nil || fmt.Sprint(s.deleted) != "[v-snap.db]" {
		t.Fatalf("DeleteUnlocked after retention = %v, %v (deleted %v); want the version deleted", until, err, s.deleted)
	}
	if _, err = DeleteUnlocked(ctx, s, "snap.db", s.until); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteUnlocked of a missing object = %v; want ErrNotFound", err)
	}
}
