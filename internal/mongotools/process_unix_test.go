//go:build unix

package mongotools

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const processHelperEnv = "MONGOTOOLS_PROCESS_HELPER"

// TestProcessHelper is not a real test. In "parent" mode it spawns a grandchild that
// ignores SIGTERM, prints the grandchild's PID and waits; in "child" mode it ignores
// SIGTERM and sleeps.
func TestProcessHelper(_ *testing.T) {
	switch os.Getenv(processHelperEnv) {
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=^TestProcessHelper$")
		child.Env = append(os.Environ(), processHelperEnv+"=child")
		ready, err := child.StdoutPipe()
		if err != nil {
			os.Exit(2)
		}
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		// Wait until the grandchild ignores SIGTERM, then detach its stdout.
		if _, err := bufio.NewReader(ready).ReadString('\n'); err != nil {
			os.Exit(2)
		}
		fmt.Println(child.Process.Pid)
		time.Sleep(time.Hour)
	case "child":
		signal.Ignore(syscall.SIGTERM)
		fmt.Println("ready")
		_ = os.Stdout.Close()
		time.Sleep(time.Hour)
	}
}

// processGone polls until pid no longer exists (zombies are reaped by init).
func processGone(pid int) bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
	}
	return false
}

func TestCommandCancelKillsProcessGroup(t *testing.T) {
	t.Setenv(processHelperEnv, "parent")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := Command(ctx, os.Args[0], "-test.run=^TestProcessHelper$")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = Start(cmd); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("read grandchild pid: %v", err)
	}
	grandchild, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("parse grandchild pid %q: %v", line, err)
	}

	start := time.Now()
	cancel()
	_ = Wait(ctx, cmd)
	if elapsed := time.Since(start); elapsed > KillGracePeriod {
		t.Fatalf("Wait took %v after cancellation", elapsed)
	}
	if !processGone(cmd.Process.Pid) {
		t.Fatal("tool process survived cancellation")
	}
	if !processGone(grandchild) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatal("grandchild (ignoring SIGTERM) survived cancellation of its process group")
	}
}

func TestKillAllKillsRunningTools(t *testing.T) {
	t.Setenv(processHelperEnv, "child") // ignores SIGTERM
	cmd := Command(context.Background(), os.Args[0], "-test.run=^TestProcessHelper$")
	if err := Start(cmd); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Wait(context.Background(), cmd) }()

	if n := KillAll(); n < 1 {
		t.Fatalf("KillAll killed %d processes", n)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("KillAll did not terminate the tool")
	}
}
