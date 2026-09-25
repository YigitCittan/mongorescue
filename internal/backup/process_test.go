package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// helperEnv selects the behaviour of TestHelperProcess when the test binary is
// re-executed as a fake mongodump (the standard os/exec helper-process pattern).
const helperEnv = "MONGORESCUE_BACKUP_HELPER_PROCESS"

// helperPIDFileEnv names the file where the "tree" helper records its PIDs.
const helperPIDFileEnv = "MONGORESCUE_BACKUP_HELPER_PIDFILE"

// TestHelperProcess is not a real test: it acts as a mongodump stand-in writing an
// endless archive to a real OS pipe. "flood" writes as fast as possible, "slow"
// paces its output so a cancellation lands mid-stream, "hang" never writes (like a
// dump stuck on an unresolvable host), and "tree" additionally spawns a grandchild
// that ignores SIGTERM ("ignore-term") and records both PIDs.
func TestHelperProcess(_ *testing.T) {
	mode := os.Getenv(helperEnv)
	switch mode {
	case "":
		return
	case "hang":
		fmt.Fprintln(os.Stderr, "Failed: server selection error: lookup db.invalid: no such host")
		time.Sleep(time.Hour)
		os.Exit(0)
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
		fmt.Println("ready")
		_ = os.Stdout.Close()
		time.Sleep(time.Hour)
		os.Exit(0)
	case "tree":
		spawnHelperGrandchild()
		mode = "slow"
	}
	fmt.Fprintln(os.Stderr, "writing archive")
	chunk := bytes.Repeat([]byte("bson"), 16*1024)
	for {
		if _, err := os.Stdout.Write(chunk); err != nil {
			os.Exit(3)
		}
		if mode == "slow" {
			time.Sleep(2 * time.Millisecond)
		}
	}
}

// spawnHelperGrandchild starts an "ignore-term" grandchild, waits until it is ready and
// writes "<self> <grandchild>" to the PID file.
func spawnHelperGrandchild() {
	child := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	child.Env = append(os.Environ(), helperEnv+"=ignore-term")
	ready, err := child.StdoutPipe()
	if err != nil {
		os.Exit(4)
	}
	if err := child.Start(); err != nil {
		os.Exit(4)
	}
	if _, err := io.ReadFull(ready, make([]byte, len("ready\n"))); err != nil {
		os.Exit(4)
	}
	pids := fmt.Sprintf("%d %d", os.Getpid(), child.Process.Pid)
	if err := os.WriteFile(os.Getenv(helperPIDFileEnv), []byte(pids), 0o600); err != nil {
		os.Exit(4)
	}
}

// realToolRunner runs the helper through the production process runner (process
// group, SIGTERM-then-SIGKILL cancellation).
func realToolRunner(ctx context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
	return defaultProcessRunner(ctx, os.Args[0], "-test.run=^TestHelperProcess$")
}

func TestBackupStallWatchdogAbortsSilentDump(t *testing.T) {
	t.Setenv(helperEnv, "hang")
	store := storage.NewMockStorage()
	engine := NewEngine(store, "mongodb://db.invalid:27017", WithRunner(realToolRunner),
		WithStallTimeout(300*time.Millisecond), WithTimeout(time.Minute))

	start := time.Now()
	record, err := runWithDeadline(t, func() (*models.BackupRecord, error) {
		return engine.Run(context.Background(), models.BackupOptions{Database: "db"})
	})
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("expected ErrStalled, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("stall detection took %v", elapsed)
	}
	if record.Status != models.StatusFailed || !strings.Contains(record.ErrorMessage, "stalled") ||
		!strings.Contains(record.ErrorMessage, "no such host") {
		t.Fatalf("unexpected record: status=%s msg=%q", record.Status, record.ErrorMessage)
	}
	if record.SHA256 != "" || record.SizeBytes != 0 {
		t.Fatalf("failed backup must not carry size/checksum: size=%d sha=%q", record.SizeBytes, record.SHA256)
	}
}

func TestBackupTimeoutAbortsLongDump(t *testing.T) {
	t.Setenv(helperEnv, "hang")
	engine := NewEngine(storage.NewMockStorage(), "mongodb://db.invalid:27017", WithRunner(realToolRunner),
		WithTimeout(300*time.Millisecond))

	record, err := runWithDeadline(t, func() (*models.BackupRecord, error) {
		return engine.Run(context.Background(), models.BackupOptions{Database: "db"})
	})
	if !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected ErrTimeout, got %v", err)
	}
	if record.Status != models.StatusFailed || !strings.Contains(record.ErrorMessage, "maximum run duration") {
		t.Fatalf("unexpected record: status=%s msg=%q", record.Status, record.ErrorMessage)
	}
}

func TestBackupStallTimeoutIgnoresSlowStorage(t *testing.T) {
	t.Setenv(helperEnv, "slow")
	// Storage that reads slowly: time blocked on the upload must not count as a stall.
	engine := NewEngine(&slowStorage{MockStorage: storage.NewMockStorage(), pause: 150 * time.Millisecond, n: 4},
		"mongodb://localhost:27017", WithRunner(realToolRunner), WithStallTimeout(100*time.Millisecond))
	_, err := runWithDeadline(t, func() (*models.BackupRecord, error) {
		return engine.Run(context.Background(), models.BackupOptions{Database: "db"})
	})
	if errors.Is(err, ErrStalled) {
		t.Fatalf("slow storage must not trigger the stall watchdog: %v", err)
	}
}

// slowStorage pauses before each of its first n reads, then aborts the upload.
type slowStorage struct {
	*storage.MockStorage
	pause time.Duration
	n     int
}

func (s *slowStorage) Save(_ context.Context, _ string, r io.Reader) (*models.StorageObject, error) {
	buf := make([]byte, 64*1024)
	for i := 0; i < s.n; i++ {
		time.Sleep(s.pause)
		if _, err := r.Read(buf); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("upload finished early")
}

// helperRunner starts the helper process through real OS pipes and keeps the
// *exec.Cmd so tests can assert that the child was reaped.
type helperRunner struct {
	mode string
	mu   sync.Mutex
	cmd  *exec.Cmd
}

func (h *helperRunner) run(ctx context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), helperEnv+"="+h.mode)
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
	h.mu.Lock()
	h.cmd = cmd
	h.mu.Unlock()
	return stdout, stderr, cmd.Wait, nil
}

func (h *helperRunner) assertReaped(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cmd == nil || h.cmd.ProcessState == nil {
		t.Fatal("helper process was not reaped (Wait never returned)")
	}
}

// failAfterStorage consumes n bytes of the upload and then fails, leaving the
// producer with a full OS pipe unless the engine closes it.
type failAfterStorage struct {
	storage.Storage
	n   int64
	err error
}

func (f *failAfterStorage) Save(_ context.Context, _ string, r io.Reader) (*models.StorageObject, error) {
	_, _ = io.CopyN(io.Discard, r, f.n)
	return nil, f.err
}

// cancelAfterStorage cancels the run context after n bytes but keeps reading to EOF,
// the way a slow uploader would observe a killed mongodump. Delete only records the
// call, so the test can tell a never-sealed artifact from a cleaned-up one.
type cancelAfterStorage struct {
	*storage.MockStorage
	n      int64
	cancel context.CancelFunc

	mu      sync.Mutex
	deleted []string
}

func (c *cancelAfterStorage) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err // cleanup must not run on the cancelled context
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleted = append(c.deleted, key)
	return nil
}

type cancelAfterReader struct {
	r      io.Reader
	left   int64
	cancel context.CancelFunc
}

func (c *cancelAfterReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left <= 0 {
		c.cancel()
	}
	return n, err
}

func (c *cancelAfterStorage) Save(_ context.Context, key string, r io.Reader) (*models.StorageObject, error) {
	// The run ctx is deliberately not forwarded: MockStorage would reject it up front.
	return c.MockStorage.Save(context.Background(), key, &cancelAfterReader{r: r, left: c.n, cancel: c.cancel})
}

func encryptorModes(t *testing.T) map[string]*encryption.Encryptor {
	t.Helper()
	_, r := newKeyPair(t)
	enc, err := encryption.NewX25519Encryptor([]string{r})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]*encryption.Encryptor{"plaintext": nil, "encrypted": enc}
}

func runWithDeadline(t *testing.T, fn func() (*models.BackupRecord, error)) (*models.BackupRecord, error) {
	t.Helper()
	type result struct {
		rec *models.BackupRecord
		err error
	}
	done := make(chan result, 1)
	go func() {
		rec, err := fn()
		done <- result{rec, err}
	}()
	select {
	case res := <-done:
		return res.rec, res.err
	case <-time.After(20 * time.Second):
		t.Fatal("backup hung: mongodump was never released/reaped")
		return nil, nil
	}
}

func TestBackupRealPipeStorageFailureDoesNotHang(t *testing.T) {
	for name, enc := range encryptorModes(t) {
		t.Run(name, func(t *testing.T) {
			runner := &helperRunner{mode: "flood"}
			uploadErr := errors.New("s3: multipart upload aborted")
			engine := NewEngine(&failAfterStorage{Storage: storage.NewMockStorage(), n: 1 << 20, err: uploadErr},
				"mongodb://localhost:27017", WithRunner(runner.run), WithEncryptor(enc))

			start := time.Now()
			record, err := runWithDeadline(t, func() (*models.BackupRecord, error) {
				return engine.Run(context.Background(), models.BackupOptions{Database: "db"})
			})
			if !errors.Is(err, uploadErr) {
				t.Fatalf("expected storage error, got %v", err)
			}
			if record.Status != models.StatusFailed {
				t.Fatalf("status = %s", record.Status)
			}
			runner.assertReaped(t)
			t.Logf("returned after %v", time.Since(start))
		})
	}
}

func TestBackupRealPipeCancelLeavesNoArtifact(t *testing.T) {
	for name, enc := range encryptorModes(t) {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runner := &helperRunner{mode: "slow"}
			store := &cancelAfterStorage{MockStorage: storage.NewMockStorage(), n: 512 << 10, cancel: cancel}
			engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner.run), WithEncryptor(enc))

			record, err := runWithDeadline(t, func() (*models.BackupRecord, error) {
				return engine.Run(ctx, models.BackupOptions{Database: "db"})
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("expected context.Canceled, got %v", err)
			}
			if record.Status != models.StatusFailed {
				t.Fatalf("status = %s", record.Status)
			}
			store.mu.Lock()
			deleted := slices.Contains(store.deleted, record.StorageKey)
			store.mu.Unlock()
			if !deleted {
				t.Fatal("cancelled backup artifact was not deleted with a live cleanup context")
			}
			if enc != nil {
				// Even if cleanup failed, an encrypted artifact must never have been sealed.
				if _, err := store.Retrieve(context.Background(), record.StorageKey); !errors.Is(err, storage.ErrNotFound) {
					t.Fatalf("truncated encrypted stream was sealed and stored (retrieve err = %v)", err)
				}
			}
			runner.assertReaped(t)
		})
	}
}
