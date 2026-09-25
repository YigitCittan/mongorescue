package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/notify"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// openLogged opens a store at path whose logs are captured in the returned buffer.
func openLogged(t *testing.T, path string) (*store.SQLiteStore, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	s, err := store.OpenSQLite(context.Background(), path, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		store.WithSecretBox(testBox))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, logs
}

// rawDB opens path with the same driver, for inspecting the schema directly.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func migrationCount(t *testing.T, path string) int {
	t.Helper()
	var n int
	if err := rawDB(t, path).QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// embeddedMigrations counts the migration files shipped in the binary.
func embeddedMigrations(t *testing.T) int {
	t.Helper()
	m, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	if err != nil || len(m) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	return len(m)
}

func TestOpenFreshDatabaseAppliesMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", dbFile)
	s, logs := openLogged(t, path)
	if s.Path() != path {
		t.Fatalf("Path() = %q; want %q", s.Path(), path)
	}
	if !strings.Contains(logs.String(), "applied metadata schema migration") {
		t.Fatalf("fresh open must log the applied migration; logs:\n%s", logs)
	}

	db := rawDB(t, path)
	for _, table := range []string{"jobs", "backups", "restores", "notification_channels", "notification_rules", "schema_migrations",
		"users", "sessions", "api_keys", "connections", "settings"} {
		var name string
		if err := db.QueryRow("SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&name); err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v; want wal", mode, err)
	}
	if n, want := migrationCount(t, path), embeddedMigrations(t); n != want {
		t.Fatalf("schema_migrations rows = %d; want %d", n, want)
	}
}

func TestReopenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	ctx := context.Background()

	first, _ := openLogged(t, path)
	if err := first.SaveJob(ctx, &models.Job{ID: "j", Name: "J"}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("second Close = %v; want nil", err)
	}

	for i := range 2 {
		s, logs := openLogged(t, path)
		if strings.Contains(logs.String(), "applied metadata schema migration") {
			t.Fatalf("reopen %d re-applied a migration; logs:\n%s", i, logs)
		}
		if _, err := s.GetJob(ctx, "j"); err != nil {
			t.Fatalf("reopen %d lost data: %v", i, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if n, want := migrationCount(t, path), embeddedMigrations(t); n != want {
		t.Fatalf("schema_migrations rows = %d; want %d", n, want)
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.Open(t, path)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB(t, path).Exec("INSERT INTO schema_migrations (version, name, applied_at) VALUES (9999, 'future', 'x')"); err != nil {
		t.Fatal(err)
	}
	_, err := store.OpenSQLite(context.Background(), path, slog.New(slog.DiscardHandler))
	if !errors.Is(err, store.ErrSchemaTooNew) {
		t.Fatalf("OpenSQLite on newer schema = %v; want ErrSchemaTooNew", err)
	}
}

func TestOpenRejectsInvalidPath(t *testing.T) {
	for _, p := range []string{"", filepath.Join(t.TempDir(), "x.db?mode=memory")} {
		if _, err := store.OpenSQLite(context.Background(), p, nil); !errors.Is(err, store.ErrInvalidPath) {
			t.Errorf("OpenSQLite(%q) = %v; want ErrInvalidPath", p, err)
		}
	}
}

func TestContextCancellation(t *testing.T) {
	s := storetest.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, errGetJob := s.GetJob(ctx, "x")
	_, errListJobs := s.ListJobs(ctx)
	_, errListBackups := s.ListBackupRecords(ctx, "d")
	_, errListChannels := s.ListChannels(ctx)
	for name, err := range map[string]error{
		"SaveJob":            s.SaveJob(ctx, &models.Job{ID: "j"}),
		"GetJob":             errGetJob,
		"ListJobs":           errListJobs,
		"ListBackupRecords":  errListBackups,
		"SaveRestoreRecord":  s.SaveRestoreRecord(ctx, &models.RestoreRecord{ID: "r"}),
		"ListChannels":       errListChannels,
		"DeleteChannel":      s.DeleteChannel(ctx, "c"),
		"SaveDeliveryStatus": s.SaveDeliveryStatus(ctx, "c", notify.DeliveryStatus{}),
	} {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s with cancelled context = %v; want context.Canceled", name, err)
		}
	}

	path := filepath.Join(t.TempDir(), dbFile)
	if _, err := store.OpenSQLite(ctx, path, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("OpenSQLite with cancelled context = %v; want context.Canceled", err)
	}
}

// TestConcurrentAccess hammers two stores on the same file (as two processes would)
// with parallel writers and readers. Run with -race; any SQLITE_BUSY fails the test.
func TestConcurrentAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	stores := []*store.SQLiteStore{storetest.OpenWithBox(t, path, testBox), storetest.OpenWithBox(t, path, testBox)}
	ctx := context.Background()
	if err := stores[0].SaveChannel(ctx, &notify.Channel{ID: "c", Name: "c", Type: notify.ChannelWebhook}); err != nil {
		t.Fatal(err)
	}

	const workers, perWorker = 8, 25
	errs := make(chan error, workers*perWorker*3)
	var wg sync.WaitGroup
	for w := range workers {
		s := stores[w%len(stores)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWorker {
				rec := &models.BackupRecord{
					ID: fmt.Sprintf("bkp_%d_%d", w, i), Database: fmt.Sprintf("db%d", w%3),
					Status: models.StatusInProgress, StartedAt: time.Now(),
				}
				if err := s.SaveBackupRecord(ctx, rec); err != nil {
					errs <- fmt.Errorf("save %s: %w", rec.ID, err)
				}
				if _, err := s.ListBackupRecords(ctx, rec.Database); err != nil {
					errs <- fmt.Errorf("list: %w", err)
				}
				if err := s.SaveDeliveryStatus(ctx, "c", notify.DeliveryStatus{Attempts: i}); err != nil {
					errs <- fmt.Errorf("delivery status: %w", err)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	all, err := stores[1].ListBackupRecords(ctx, "")
	if err != nil || len(all) != workers*perWorker {
		t.Fatalf("records after concurrent writes = %d, %v; want %d", len(all), err, workers*perWorker)
	}
}
