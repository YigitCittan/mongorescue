package runlog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestTraversalIDsAreRejected(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	d := NewDir(dir)
	for _, id := range []string{
		"", ".", "..", "../x", `..\x`, "x/../../y", "/etc/passwd", `C:\x`, "c:x",
		"a\x00b", "a\nb", "a\x1b[31m", "bad\xffutf8", strings.Repeat("a", maxLegacyIDLength+1),
	} {
		if _, err := d.Create(id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Create(%q) = %v, want ErrInvalidID", id, err)
		}
		if _, err := d.Open(id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Open(%q) = %v, want ErrInvalidID", id, err)
		}
		if err := d.Remove(id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Remove(%q) = %v, want ErrInvalidID", id, err)
		}
	}
	// Nothing was written outside the log directory.
	if entries, err := os.ReadDir(filepath.Dir(dir)); err != nil || len(entries) != 0 {
		t.Fatalf("entries next to the log directory: %v, %v", entries, err)
	}
	for _, id := range []string{"bkp_shop_20260925", "v1..v2", "bkp_café_x", "rst-1"} {
		if models.ValidateID(id) != nil && !legacyID(id) {
			t.Errorf("%q must be a valid run ID", id)
		}
	}
}

func TestCheckedPath(t *testing.T) {
	d := NewDir(t.TempDir())
	sep := string(filepath.Separator)
	ok := filepath.Join(d.Path(), "bkp_1.log")
	if got, err := checkedPath(d.Path(), ok); err != nil || got != ok {
		t.Fatalf("checkedPath(%q) = %q, %v", ok, got, err)
	}
	if got, err := checkedPath(d.Path(), d.Path()+sep+"."+sep+"bkp_1.log"); err != nil || got != ok {
		t.Fatalf("checkedPath does not clean: %q, %v", got, err)
	}
	for _, p := range []string{
		d.Path() + sep + ".." + sep + "escape.log",
		d.Path() + sep + "a" + sep + ".." + sep + ".." + sep + "escape.log",
		d.Path() + "-sibling" + sep + "x.log",
		d.Path(),
		"relative.log",
		filepath.Join(d.Path(), "a\x00.log"),
		filepath.Join(d.Path(), "a\n.log"),
	} {
		if _, err := checkedPath(d.Path(), p); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("checkedPath(%q) = %v, want ErrInvalidPath", p, err)
		}
	}
}

func TestNewDirMakesThePathAbsolute(t *testing.T) {
	if p := NewDir("logs").Path(); !filepath.IsAbs(p) {
		t.Fatalf("NewDir(logs).Path() = %q, want an absolute path", p)
	}
}
