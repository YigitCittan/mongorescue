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
	if w, err := os.OpenFile(p, os.O_WRONLY, 0); err == nil {
		_ = w.Close()
		t.Error("opened for writing while locked")
	}
	if err := os.Remove(p); err == nil {
		t.Error("deleted while locked")
	}
	if err := os.Rename(p, p+".old"); err == nil {
		t.Error("renamed while locked")
	}
	r, err := os.Open(p)
	if err != nil {
		t.Fatalf("read while locked: %v", err)
	}
	_ = r.Close()
}

func TestInstallerParams(t *testing.T) {
	if got := installerParams(silentInstallerArgs("")); got != "/S /RELAUNCH" {
		t.Errorf("params = %q", got)
	}
	dir := `C:\Program Files\MongoRescue\MongoRescue`
	if got := installerParams(silentInstallerArgs(dir)); got != `/S /RELAUNCH /D=`+dir {
		t.Errorf("params with install dir = %q; /D= must be last and unquoted", got)
	}
	if got := installerParams([]string{"/S", `C:\Program Files\x`}); got != `/S "C:\Program Files\x"` {
		t.Errorf("quoted params = %q", got)
	}
}
