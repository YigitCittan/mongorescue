//go:build integration

package integration

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runlog"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// TestCancelRealBackupMidRun cancels a real mongodump through the run registry (as
// POST /api/v1/backups/{id}/cancel does) once the upload is past two S3 parts: the
// backup ends as cancelled with who cancelled it, leaves no object, temporary file or
// incomplete multipart upload, and its log holds mongodump's own output.
func TestCancelRealBackupMidRun(t *testing.T) {
	env := requireMongo(t)
	db := env.uniqueDB(t, "cnl")
	env.seedBulk(t, db, 40)

	for _, tg := range interruptTargets(t) {
		t.Run(tg.Name, func(t *testing.T) {
			logDir := t.TempDir()
			reg := runs.NewRegistry(runs.WithLogs(runlog.NewDir(logDir)))
			opts := models.BackupOptions{Database: db, MongoURI: env.URI}
			var id string
			var progress models.RunProgress
			st := &hookStorage{Storage: tg.Storage, after: interruptAfter, hook: func() {
				if run := reg.Get(id); run != nil {
					progress = run.Snapshot()
				}
				_ = reg.Cancel(id, runs.Cancellation{By: "alice"})
			}}
			engine := newBackupEngine(env, st)
			record, err := engine.Prepare(opts)
			if err != nil {
				t.Fatal(err)
			}
			id = record.ID
			run, err := reg.Register(runs.Meta{Kind: models.RunBackup, ID: id, Database: db})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
			defer cancel()
			rec, err := engine.Execute(run.Bind(ctx), opts, record)
			run.End()
			if !errors.Is(err, runs.ErrCancelled) || rec.Status != models.StatusCancelled || rec.CancelledBy != "alice" {
				t.Fatalf("want a backup cancelled by alice; got %v (%+v)", err, rec)
			}
			if rec.SHA256 != "" || rec.SizeBytes != 0 {
				t.Fatalf("a cancelled record must not carry a checksum or size: %+v", rec)
			}
			tg.assertNoLeftovers(t, rec.StorageKey)
			if progress.Bytes < interruptAfter || progress.Phase != models.PhaseDumping {
				t.Fatalf("progress at the cancellation = %+v", progress)
			}
			raw, err := os.ReadFile(filepath.Join(logDir, id+".log"))
			if err != nil {
				t.Fatal(err)
			}
			if progress.CollectionsTotal != 1 || progress.CurrentCollection != db+".bulk" {
				t.Errorf("mongodump's progress was not parsed: %+v", progress)
			}
			for _, want := range []string{"writing ", db + ".bulk", "cancellation requested (cancelled by alice)", "backup cancelled by alice"} {
				if !strings.Contains(string(raw), want) {
					t.Errorf("run log misses %q:\n%s", want, raw)
				}
			}
			assertNoSecret(t, env.Password, "run log", string(raw))
		})
	}
}

// TestCancelRealRestoreMidRun cancels a real mongorestore into a safe clone halfway
// through the archive: the restore ends as cancelled and the partial clone is dropped.
func TestCancelRealRestoreMidRun(t *testing.T) {
	env := requireMongo(t)
	db := env.uniqueDB(t, "cnr")
	env.seedBulk(t, db, 40)
	local, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := mustBackup(t, env, local, models.BackupOptions{Database: db})

	reg := runs.NewRegistry(runs.WithLogs(runlog.NewDir(t.TempDir())))
	var id string
	st := &hookRetrieveStorage{Storage: local, after: src.SizeBytes / 2, hook: func() {
		_ = reg.Cancel(id, runs.Cancellation{By: "alice"})
	}}
	engine := newRestoreEngine(env, st)
	req := models.RestoreRequest{BackupID: src.ID, MongoURI: env.URI}
	record, err := engine.Prepare(req, src)
	if err != nil {
		t.Fatal(err)
	}
	id = record.ID
	run, err := reg.Register(runs.Meta{Kind: models.RunRestore, ID: id, Database: record.TargetDatabase})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	rec, err := engine.Execute(run.Bind(ctx), req, src, record)
	run.End()
	if !errors.Is(err, runs.ErrCancelled) || rec.Status != models.RestoreStatusCancelled || rec.CancelledBy != "alice" {
		t.Fatalf("want a restore cancelled by alice; got %v (%+v)", err, rec)
	}
	if !strings.Contains(rec.ErrorMessage, "was dropped") {
		t.Fatalf("the record must say the partial clone was dropped: %q", rec.ErrorMessage)
	}
	if env.dbExists(t, rec.TargetDatabase) {
		t.Fatalf("the partial clone %s still exists", rec.TargetDatabase)
	}
}

// hookRetrieveStorage calls hook once a Retrieve stream has been read past after bytes.
type hookRetrieveStorage struct {
	storage.Storage
	after int64
	hook  func()
}

func (h *hookRetrieveStorage) Retrieve(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := h.Storage.Retrieve(ctx, key)
	if err != nil {
		return nil, err
	}
	return &hookReadCloser{hookReader: hookReader{r: rc, after: h.after, hook: h.hook}, c: rc}, nil
}

type hookReadCloser struct {
	hookReader
	c  io.Closer
	mu sync.Mutex
}

func (h *hookReadCloser) Read(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hookReader.Read(p)
}

func (h *hookReadCloser) Close() error { return h.c.Close() }
