//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// interruptAfter is how far into the stream the interruptions happen: past two S3
// parts (5 MiB each), so a multipart upload is in progress.
const interruptAfter = 12 << 20

// seedBulk inserts about mb MiB of incompressible data into db.bulk.
func (m *mongoEnv) seedBulk(t *testing.T, db string, mb int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	for i := 0; i < mb; i++ {
		payload := make([]byte, 1<<20)
		_, _ = rand.Read(payload)
		if _, err := m.Client.Database(db).Collection("bulk").InsertOne(ctx, bson.D{{Key: "_id", Value: i}, {Key: "payload", Value: bson.Binary{Data: payload}}}); err != nil {
			t.Fatalf("seed %s.bulk: %v", db, err)
		}
	}
}

// interruptTarget is a storage target whose leftovers the test can inspect.
type interruptTarget struct {
	Name     string
	Storage  storage.Storage
	LocalDir string            // local targets
	S3       *storage.S3Config // S3 targets
}

// interruptTargets returns a local target with a known directory and every
// configured S3 provider.
func interruptTargets(t *testing.T) []interruptTarget {
	t.Helper()
	dir := t.TempDir()
	local, err := storage.NewLocalStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := []interruptTarget{{Name: "local", Storage: local, LocalDir: dir}}
	for _, p := range configuredS3Providers() {
		cfg := p.Config
		out = append(out, interruptTarget{Name: p.Name, Storage: newS3Storage(t, p), S3: &cfg})
	}
	return out
}

// assertNoLeftovers checks that a failed backup left nothing behind: no object, no
// temporary file, no incomplete multipart upload.
func (tg interruptTarget) assertNoLeftovers(t *testing.T, key string) {
	t.Helper()
	assertObjectGone(t, tg.Storage, key)
	if tg.LocalDir != "" {
		var files []string
		_ = filepath.WalkDir(tg.LocalDir, func(path string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				files = append(files, path)
			}
			return nil
		})
		if len(files) > 0 {
			t.Fatalf("a failed backup left files behind: %v", files)
		}
	}
	if tg.S3 != nil {
		if n := incompleteUploads(t, *tg.S3, key); n != 0 {
			t.Fatalf("a failed backup left %d incomplete multipart upload(s) for %s", n, key)
		}
	}
}

// hookStorage calls hook once Save has read after bytes of the backup stream.
type hookStorage struct {
	storage.Storage
	after int64
	hook  func()
}

func (h *hookStorage) Save(ctx context.Context, key string, r io.Reader) (*models.StorageObject, error) {
	return h.Storage.Save(ctx, key, &hookReader{r: r, after: h.after, hook: h.hook})
}

type hookReader struct {
	r     io.Reader
	after int64
	read  int64
	hook  func()
	once  sync.Once
}

func (h *hookReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	h.read += int64(n)
	if h.read >= h.after {
		h.once.Do(h.hook)
	}
	return n, err
}

// killableRunner starts the real mongodump and exposes a function that kills it
// with SIGKILL, like an OOM kill or an operator's kill -9.
type killableRunner struct {
	mu  sync.Mutex
	cmd *exec.Cmd
}

func (k *killableRunner) run(ctx context.Context, name string, args ...string) (io.ReadCloser, io.Reader, func() error, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, nil, nil, err
	}
	cmd := exec.CommandContext(ctx, path, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	k.mu.Lock()
	k.cmd = cmd
	k.mu.Unlock()
	return stdout, stderr, cmd.Wait, nil
}

func (k *killableRunner) kill() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cmd != nil && k.cmd.Process != nil {
		_ = k.cmd.Process.Kill()
	}
}

// TestInterruptedBackupsLeaveNothingBehind cancels a backup mid-dump, kills
// mongodump mid-stream and makes S3 fail mid-upload. Each run must end with a
// failed record carrying the reason and leave no object, temporary file or
// incomplete multipart upload; retention must still keep the last good backup.
func TestInterruptedBackupsLeaveNothingBehind(t *testing.T) {
	env := requireMongo(t)
	db := env.uniqueDB(t, "int")
	env.seedBulk(t, db, 40)
	enc, _ := keyPair(t)

	var failed []*models.BackupRecord
	for _, tg := range interruptTargets(t) {
		t.Run(tg.Name, func(t *testing.T) {
			t.Run("context cancelled mid-dump", func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
				defer cancel()
				st := &hookStorage{Storage: tg.Storage, after: interruptAfter, hook: cancel}
				rec, err := newBackupEngine(env, st).Run(ctx, models.BackupOptions{Database: db, MongoURI: env.URI})
				if !errors.Is(err, context.Canceled) || rec.Status != models.StatusFailed || !strings.Contains(rec.ErrorMessage, "cancelled") {
					t.Fatalf("want a failed, cancelled record; got %v (%+v)", err, rec)
				}
				if rec.SHA256 != "" || rec.SizeBytes != 0 {
					t.Fatalf("a failed record must not carry a checksum or size: %+v", rec)
				}
				tg.assertNoLeftovers(t, rec.StorageKey)
				failed = append(failed, rec)
			})

			for _, encrypted := range []bool{false, true} {
				t.Run(fmt.Sprintf("mongodump killed mid-stream/encrypted=%v", encrypted), func(t *testing.T) {
					runner := &killableRunner{}
					opts := []backup.Option{backup.WithRunner(runner.run)}
					if encrypted {
						opts = append(opts, backup.WithEncryptor(enc))
					}
					st := &hookStorage{Storage: tg.Storage, after: interruptAfter, hook: runner.kill}
					ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
					defer cancel()
					rec, err := newBackupEngine(env, st, opts...).Run(ctx, models.BackupOptions{Database: db, MongoURI: env.URI})
					if !errors.Is(err, backup.ErrDumpFailed) || rec.Status != models.StatusFailed || !strings.Contains(rec.ErrorMessage, "mongodump failed") {
						t.Fatalf("want a failed record naming mongodump; got %v (%+v)", err, rec)
					}
					tg.assertNoLeftovers(t, rec.StorageKey)
					failed = append(failed, rec)
				})
			}

			if tg.S3 != nil && tg.S3.Endpoint != "" {
				t.Run("S3 fails mid-upload", func(t *testing.T) {
					proxied := *tg.S3
					var rejected atomic.Int32
					proxied.Endpoint = failingS3Proxy(t, tg.S3.Endpoint, &rejected)
					st, err := storage.NewS3Storage(context.Background(), proxied)
					if err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
					defer cancel()
					rec, err := newBackupEngine(env, st).Run(ctx, models.BackupOptions{Database: db, MongoURI: env.URI})
					if err == nil || rec.Status != models.StatusFailed || !strings.Contains(rec.ErrorMessage, "stream to storage") {
						t.Fatalf("want a failed record naming the upload; got %v (%+v)", err, rec)
					}
					if rejected.Load() == 0 {
						t.Fatal("the proxy never rejected a part")
					}
					tg.assertNoLeftovers(t, rec.StorageKey)
					failed = append(failed, rec)
				})
			}
		})
	}

	t.Run("retention keeps the last good backup", func(t *testing.T) {
		runRetentionAfterFailures(t, env, db, failed)
	})
}

// failingS3Proxy forwards S3 requests to endpoint but answers every UploadPart
// after the first with 403 AccessDenied (not retried by the SDK), counting them.
func failingS3Proxy(t *testing.T, endpoint string, rejected *atomic.Int32) string {
	t.Helper()
	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	proxy := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(target)
		// The request keeps the Host header it was signed with.
		pr.Out.Host = pr.In.Host
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Query().Get("uploadId") != "" && r.URL.Query().Get("partNumber") != "1" {
			rejected.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>injected failure</Message></Error>`)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// runRetentionAfterFailures applies retention to two good scheduled backups and the
// failed runs above (all newer): failed runs must not count, so only the older good
// backup is pruned and the newest good one survives with its artifact.
func runRetentionAfterFailures(t *testing.T, env *mongoEnv, db string, failed []*models.BackupRecord) {
	if len(failed) == 0 {
		t.Fatal("no failed runs recorded")
	}
	st, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	meta := storetest.New(t)

	older := mustBackup(t, env, st, models.BackupOptions{Database: db, Gzip: true, Collections: []string{"bulk"}})
	good := mustBackup(t, env, st, models.BackupOptions{Database: db, Gzip: true, Collections: []string{"bulk"}})
	older.StartedAt = time.Now().UTC().AddDate(0, 0, -40)
	good.StartedAt = time.Now().UTC().AddDate(0, 0, -30)
	records := []*models.BackupRecord{older, good}
	for _, f := range failed {
		f := *f
		f.StartedAt = time.Now().UTC()
		records = append(records, &f)
	}
	for _, r := range records {
		r.Trigger = models.TriggerScheduled
		r.JobID = "job_retention"
		if err = meta.SaveBackupRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	pruned, err := scheduler.PruneBackups(ctx, 7, 1, records, meta, st, discardLogger)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(pruned) != 1 || pruned[0] != older.ID {
		t.Fatalf("pruned %v; want only the older good backup %s", pruned, older.ID)
	}
	if _, err := st.Stat(ctx, good.StorageKey); err != nil {
		t.Fatalf("the last good backup's artifact was removed: %v", err)
	}
	if _, err := st.Stat(ctx, older.StorageKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("the pruned backup's artifact still exists: %v", err)
	}
	for _, f := range failed {
		got, err := meta.GetBackupRecord(ctx, f.ID)
		if err != nil || got.Status != models.StatusFailed {
			t.Fatalf("failed record %s changed by retention: %v %+v", f.ID, err, got)
		}
	}
}
