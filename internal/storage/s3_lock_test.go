package storage

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
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
	// versions lists the versions of each key (ListObjectVersions), markers its
	// delete markers; until is the retain-until date of a version.
	versions map[string][]string
	markers  map[string][]string
	until    map[string]time.Time
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
	case r.Method == http.MethodPut && (q.Has("legal-hold") || q.Has("retention")):
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && q.Has("versions"):
		w.Header().Set("Content-Type", "application/xml")
		prefix := q.Get("prefix")
		_, _ = fmt.Fprintf(w, `<ListVersionsResult><Name>%s</Name>`, fakeBucket)
		for k, ids := range f.versions {
			for _, id := range ids {
				if strings.HasPrefix(k, prefix) {
					_, _ = fmt.Fprintf(w, `<Version><Key>%s</Key><VersionId>%s</VersionId></Version>`, k, id)
				}
			}
		}
		for k, ids := range f.markers {
			for _, id := range ids {
				if strings.HasPrefix(k, prefix) {
					_, _ = fmt.Fprintf(w, `<DeleteMarker><Key>%s</Key><VersionId>%s</VersionId></DeleteMarker>`, k, id)
				}
			}
		}
		_, _ = fmt.Fprint(w, `<IsTruncated>false</IsTruncated></ListVersionsResult>`)
	case r.Method == http.MethodPost && q.Has("uploads"):
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>k</Key><UploadId>u1</UploadId></InitiateMultipartUploadResult>`, fakeBucket)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		w.Header().Set("ETag", `"part-`+q.Get("partNumber")+`"`)
	case r.Method == http.MethodPost && q.Has("uploadId"):
		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("x-amz-version-id", "v-multi")
		_, _ = fmt.Fprintf(w, `<CompleteMultipartUploadResult><Bucket>%s</Bucket><Key>k</Key><ETag>"multi"</ETag></CompleteMultipartUploadResult>`, fakeBucket)
		f.objects[key] = nil
	case r.Method == http.MethodPut:
		f.objects[key] = body
		w.Header().Set("ETag", `"etag"`)
		w.Header().Set("x-amz-version-id", "v-"+key)
	case r.Method == http.MethodHead:
		w.Header().Set("Content-Length", fmt.Sprint(len(f.objects[key])))
		w.Header().Set("x-amz-version-id", cmp.Or(q.Get("versionId"), "v-current"))
		if u, ok := f.until[q.Get("versionId")]; ok {
			w.Header().Set("x-amz-object-lock-mode", "COMPLIANCE")
			w.Header().Set("x-amz-object-lock-retain-until-date", u.Format(time.RFC3339))
		}
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
			wantUntil := lockNow.Add(30 * 24 * time.Hour).Truncate(time.Second).Add(time.Second) // rounded up
			if got := put.Header.Get("X-Amz-Object-Lock-Mode"); got != tc.wantMode {
				t.Errorf("lock mode header = %q; want %q", got, tc.wantMode)
			}
			if want := map[bool]string{true: "v-a.archive"}[tc.wantMode != ""]; obj.VersionID != want {
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

// TestS3VersionOperations checks that versioned reads and legal holds name the
// version, and that the generic helpers fall back for other drivers.
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
	if err = SetLegalHold(ctx, st, "k", "v1", true); err != nil {
		t.Fatal(err)
	}
	if err = SetLegalHold(ctx, st, "k", "v1", false); err != nil {
		t.Fatal(err)
	}
	var gets, holds []string
	for i, r := range f.requests {
		switch {
		case r.Method == http.MethodGet:
			gets = append(gets, r.URL.Query().Get("versionId"))
		case r.URL.Query().Has("legal-hold"):
			holds = append(holds, r.URL.Query().Get("versionId")+":"+map[bool]string{true: "ON", false: "OFF"}[strings.Contains(f.bodies[i], "ON")])
		}
	}
	if fmt.Sprint(gets, holds) != "[v1] [v1:ON v1:OFF]" {
		t.Fatalf("gets=%v holds=%v", gets, holds)
	}

	mock := NewMockStorage()
	if _, err = mock.Save(ctx, "m", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if rc, err = RetrieveVersion(ctx, mock, "m", "v1"); err != nil {
		t.Fatalf("fallback retrieve: %v", err)
	}
	_ = rc.Close()
	if _, err = Purge(ctx, mock, "m", "v1", lockNow); err != nil {
		t.Fatalf("fallback purge: %v", err)
	}
	if err = SetLegalHold(ctx, mock, "m", "", true); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("legal hold on a mock = %v; want ErrUnsupported", err)
	}
}

// deletes returns the versionId of every DELETE request ("" for a plain delete).
func (f *lockS3) deletes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		if r.Method == http.MethodDelete {
			out = append(out, r.URL.Query().Get("versionId"))
		}
	}
	return out
}

// listed reports whether ListObjectVersions was called.
func (f *lockS3) listed() bool {
	r, _ := f.find(http.MethodGet, "versions")
	return r != nil
}

// TestPurgeOnALockedTargetWaitsThenDeletesEveryVersion checks that a locked
// target's purge lists the key's versions and delete markers (also for a record
// without a version, such as an import or an orphan), deletes nothing while one
// version is locked and returns when the lock ends, and afterwards deletes each
// version and marker by ID: never a plain delete that would leave the data.
func TestPurgeOnALockedTargetWaitsThenDeletesEveryVersion(t *testing.T) {
	f := &lockS3{
		versions: map[string][]string{"a.archive": {"v1", "v2"}, "a.archive.other": {"x"}},
		markers:  map[string][]string{"a.archive": {"m1"}},
		until:    map[string]time.Time{"v2": lockNow.Add(48 * time.Hour)},
	}
	st := newLockS3(t, f, ObjectLock{Mode: models.ObjectLockCompliance, RetentionDays: 1})
	ctx := context.Background()
	until, err := Purge(ctx, st, "a.archive", "", lockNow)
	if err != nil || until == nil || !until.Equal(lockNow.Add(48*time.Hour).Truncate(time.Second)) || len(f.deletes()) != 0 {
		t.Fatalf("purge while locked = %v, %v (deletes %v); want it kept until the lock ends", until, err, f.deletes())
	}
	until, err = Purge(ctx, st, "a.archive", "", lockNow.Add(48*time.Hour))
	if err != nil || until != nil {
		t.Fatalf("purge after the lock = %v, %v", until, err)
	}
	got := f.deletes()
	slices.Sort(got)
	if fmt.Sprint(got) != "[m1 v1 v2]" {
		t.Fatalf("deleted versions = %v; want every version and marker of the key, nothing else", got)
	}
	if _, err = Purge(ctx, st, "gone", "", lockNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("purge of a key without versions = %v; want ErrNotFound", err)
	}
}

// TestUnlockedVersionedBucketKeepsPlainDeletes checks that a target without a lock
// behaves as before on a bucket that returns versions (Backblaze B2 always does):
// uploads record no version, reads and Stat ignore it, and the purge sends a plain
// DeleteObject, which leaves a delete marker, without listing versions.
func TestUnlockedVersionedBucketKeepsPlainDeletes(t *testing.T) {
	f := &lockS3{versions: map[string][]string{"b.archive": {"v1"}}}
	st := newLockS3(t, f, ObjectLock{})
	ctx := context.Background()
	obj, err := st.Save(ctx, "b.archive", strings.NewReader("data"))
	if err != nil {
		t.Fatal(err)
	}
	if obj.VersionID != "" || obj.RetainUntil != nil {
		t.Fatalf("unlocked upload recorded %+v; want no version", obj)
	}
	if stat, statErr := st.Stat(ctx, "b.archive"); statErr != nil || stat.VersionID != "" {
		t.Fatalf("unlocked Stat = %+v, %v; want no version", stat, statErr)
	}
	if head, _ := f.find(http.MethodHead, ""); head == nil || head.URL.Query().Has("versionId") {
		t.Fatalf("unlocked HeadObject = %v; want one without a version", head)
	}
	if until, purgeErr := Purge(ctx, st, "b.archive", "", lockNow); purgeErr != nil || until != nil {
		t.Fatalf("purge = %v, %v", until, purgeErr)
	}
	if got := f.deletes(); len(got) != 1 || got[0] != "" || f.listed() {
		t.Fatalf("deletes %q, listed %v; want one plain DeleteObject and no version listing", got, f.listed())
	}
}

// TestLongUploadExtendsTheLock checks that an upload that ends after it began gets
// its lock extended to the end plus the retention, so no object is locked for less
// than the retention.
func TestLongUploadExtendsTheLock(t *testing.T) {
	f := &lockS3{}
	var calls int
	clock := func() time.Time {
		calls++
		if calls == 1 {
			return lockNow
		}
		return lockNow.Add(3 * time.Hour)
	}
	st := newLockS3(t, f, ObjectLock{Mode: models.ObjectLockGovernance, RetentionDays: 7, Now: clock})
	obj, err := st.Save(context.Background(), "long.archive", strings.NewReader("data"))
	if err != nil {
		t.Fatal(err)
	}
	want := lockNow.Add(3*time.Hour + 7*24*time.Hour).Truncate(time.Second).Add(time.Second)
	put, body := f.find(http.MethodPut, "retention")
	if put == nil || put.URL.Query().Get("versionId") != "v-long.archive" || !strings.Contains(body, want.Format("2006-01-02T15:04:05")) {
		t.Fatalf("PutObjectRetention = %v %q; want the version extended to %s", put, body, want)
	}
	if obj.RetainUntil == nil || !obj.RetainUntil.Equal(want) {
		t.Fatalf("RetainUntil = %v; want %s", obj.RetainUntil, want)
	}
}
