//go:build !unix

package mongotools

import "os/exec"

// configureProcessGroup is a no-op where POSIX process groups are unavailable.
func configureProcessGroup(*exec.Cmd) {}

// terminateGroup kills the tool process (no graceful signal is available).
func terminateGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// killGroup kills the tool process.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
