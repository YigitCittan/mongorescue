//go:build windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

// isTerminalFile reports whether f is a Windows console that processes ANSI escape
// sequences. A console is detected with GetConsoleMode (a pipe or a file is not
// one); virtual terminal processing is switched on for it, which Windows 10 and
// later (cmd.exe, PowerShell, Windows Terminal) support. A console that refuses it
// gets no colour rather than raw escape codes.
func isTerminalFile(f *os.File) bool {
	h := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return false
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return true
	}
	return windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}
