package desktop

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// revealTimeout bounds the file manager command of RevealFile.
const revealTimeout = 10 * time.Second

// RevealFile shows path in the file manager: selected in Finder on macOS (open -R),
// its directory opened with xdg-open on Linux and selected in Explorer on Windows.
func RevealFile(ctx context.Context, path string) error {
	name, args := revealCommand(runtime.GOOS, path)
	ctx, cancel := context.WithTimeout(ctx, revealTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, name, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, out)
	}
	return nil
}

// revealCommand returns the program and arguments that reveal path on goos.
func revealCommand(goos, path string) (string, []string) {
	switch goos {
	case "darwin":
		return "open", []string{"-R", path}
	case "windows":
		return "explorer", []string{"/select," + path}
	default:
		return "xdg-open", []string{filepath.Dir(path)}
	}
}
