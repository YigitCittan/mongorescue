package store_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/store"
)

// lockHelperEnv makes the test binary act as a second MongoRescue process that holds
// the data directory lock (see TestLockHelperProcess).
const lockHelperEnv = "MONGORESCUE_TEST_LOCK_HELPER_DIR"

// TestLockHelperProcess is not a test: run as a subprocess, it takes the lock on the
// directory named by lockHelperEnv, reports it on stdout and holds it until stdin
// closes or the process is killed.
func TestLockHelperProcess(t *testing.T) {
	dir := os.Getenv(lockHelperEnv)
	if dir == "" {
		t.Skip("helper process for TestLockDataDirAcrossProcesses")
	}
	lock, err := store.LockDataDir(dir)
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(3)
	}
	fmt.Println("locked")
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = lock.Release()
	os.Exit(0)
}

// startLockHolder starts a process holding the lock on dir.
func startLockHolder(t *testing.T, dir string) (*exec.Cmd, io.WriteCloser) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$", "-test.count=1") //nolint:gosec // G204: re-executes the test binary itself.
	cmd.Env = append(os.Environ(), lockHelperEnv+"="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(stdout).ReadString('\n')
		line <- s
		_, _ = io.Copy(io.Discard, stdout)
	}()
	select {
	case s := <-line:
		if strings.TrimSpace(s) != "locked" {
			t.Fatalf("lock holder: %q", s)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("lock holder did not start")
	}
	return cmd, stdin
}

// TestLockDataDirAcrossProcesses checks that a second process cannot take the lock
// of a running instance, and that the lock of a crashed instance (killed without
// releasing it, leaving mongorescue.lock behind) is free again: the operating system
// releases it with the process, so no stale lock file ever needs to be removed.
func TestLockDataDirAcrossProcesses(t *testing.T) {
	if os.Getenv(lockHelperEnv) != "" {
		t.Skip("inside the helper process")
	}
	dir := t.TempDir()

	// Clean shutdown of the other instance.
	cmd, stdin := startLockHolder(t, dir)
	if lock, err := store.LockDataDir(dir); !errors.Is(err, store.ErrDataDirLocked) {
		if lock != nil {
			_ = lock.Release()
		}
		t.Fatalf("lock while another process holds it = %v; want ErrDataDirLocked", err)
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("lock holder exit: %v", err)
	}
	lock, err := store.LockDataDir(dir)
	if err != nil {
		t.Fatalf("lock after the other instance stopped: %v", err)
	}
	if err = lock.Release(); err != nil {
		t.Fatal(err)
	}

	// Crash: the process dies holding the lock.
	cmd, _ = startLockHolder(t, dir)
	if _, err = store.LockDataDir(dir); !errors.Is(err, store.ErrDataDirLocked) {
		t.Fatalf("lock while another process holds it = %v; want ErrDataDirLocked", err)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if _, err = os.Stat(filepath.Join(dir, store.LockFileName)); err != nil {
		t.Fatalf("the crashed instance should leave its lock file behind: %v", err)
	}
	lock, err = store.LockDataDir(dir)
	if err != nil {
		t.Fatalf("lock after the other instance crashed: %v", err)
	}
	if err = lock.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestLockDataDirIgnoresLeftoverLockFile checks that the content of a lock file left
// by an earlier run (or any other file of that name) does not matter.
func TestLockDataDirIgnoresLeftoverLockFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, store.LockFileName), []byte("pid 12345\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := store.LockDataDir(dir)
	if err != nil {
		t.Fatalf("lock with a leftover lock file: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
}
