package mongotools

import (
	"context"
	"os/exec"
	"sync"
	"time"
)

// KillGracePeriod is how long a cancelled tool process may take to exit after the
// polite termination signal (SIGTERM to its process group on Unix) before it is
// killed forcibly and its pipes are closed.
const KillGracePeriod = 10 * time.Second

// Command returns an exec.Cmd for a mongo tool that can be torn down reliably:
//
//   - the tool runs in its own process group (Unix), so signals reach any children;
//   - when ctx is done, the whole group receives SIGTERM (Windows: the process is
//     killed), and after KillGracePeriod the process is killed and its I/O pipes are
//     closed so Wait can never hang on a grandchild holding them open.
//
// Start the command with Start and reap it with Wait so that stragglers left in the
// process group are killed and KillAll can reach the process during shutdown.
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	configureProcessGroup(cmd)
	cmd.Cancel = func() error { return terminateGroup(cmd) }
	cmd.WaitDelay = KillGracePeriod
	return cmd
}

// running tracks started tool processes for KillAll.
var running = struct {
	sync.Mutex
	cmds map[*exec.Cmd]struct{}
}{cmds: make(map[*exec.Cmd]struct{})}

// Start starts cmd (built by Command) and registers it for KillAll.
func Start(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	running.Lock()
	running.cmds[cmd] = struct{}{}
	running.Unlock()
	return nil
}

// Wait waits for cmd to exit. If its context was cancelled, any process still left in
// the tool's process group (e.g. a grandchild that ignored SIGTERM) is killed.
func Wait(ctx context.Context, cmd *exec.Cmd) error {
	err := cmd.Wait()
	if ctx.Err() != nil {
		killGroup(cmd)
	}
	running.Lock()
	delete(running.cmds, cmd)
	running.Unlock()
	return err
}

// KillAll forcibly kills every running tool process (and its process group). It is
// the last resort of a bounded shutdown once the graceful deadline has passed.
func KillAll() int {
	running.Lock()
	defer running.Unlock()
	for cmd := range running.cmds {
		killGroup(cmd)
	}
	return len(running.cmds)
}
