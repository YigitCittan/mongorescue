//go:build unix

package mongotools

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// configureProcessGroup places the tool in a new process group led by itself.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateGroup asks the tool's whole process group to exit (SIGTERM).
func terminateGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

// killGroup SIGKILLs every process left in the tool's process group.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
