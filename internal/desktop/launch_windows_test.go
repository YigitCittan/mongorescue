package desktop

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenLockedDeniesWriteAndDelete(t *testing.T) {
	p := filepath.Join(t.TempDir(), "setup.exe")
	if err := os.WriteFile(p, []byte("installer"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openLocked(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if w, werr := os.OpenFile(p, os.O_WRONLY, 0); werr == nil {
		_ = w.Close()
		t.Error("opened for writing while locked")
	}
	if os.Remove(p) == nil {
		t.Error("deleted while locked")
	}
	if os.Rename(p, p+".old") == nil {
		t.Error("renamed while locked")
	}
	r, err := os.Open(p)
	if err != nil {
		t.Fatalf("read while locked: %v", err)
	}
	_ = r.Close()
}

func TestInstallerParams(t *testing.T) {
	// NSIS GetOptions reads /RELAUNCH=<user> up to the next "/": the options are
	// passed unquoted, also with a space in the user name.
	if got := installerParams(silentInstallerArgs(`CORP\John Smith`, 42)); got != `/S /RELAUNCH=CORP\John Smith /WAITPID=42` {
		t.Errorf("params = %q", got)
	}
	if got := installerParams([]string{"/S"}); got != "/S" {
		t.Errorf("params = %q", got)
	}
	if got := installerParams([]string{"/S", `C:\Program Files\x`}); got != `/S "C:\Program Files\x"` {
		t.Errorf("quoted params = %q", got)
	}
}

func TestWaitForProcessExit(t *testing.T) {
	if !WaitForProcessExit(0, 0) || !WaitForProcessExit(os.Getpid(), 0) {
		t.Error("no process or the calling one must count as exited")
	}
}
