package logsafe_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
)

func TestString(t *testing.T) {
	for in, want := range map[string]string{
		"bkp_shop_20260925":            "bkp_shop_20260925",
		"shop\nlevel=ERROR msg=forged": "shoplevel=ERROR msg=forged",
		"a\r\nb\x1b[31mred\x00\u0085":  "ab[31mred",
		"naïve – ünïcode ✓":            "naïve – ünïcode ✓",
		"bad\xffutf8":                  "badutf8",
	} {
		if got := logsafe.String(in); got != want {
			t.Errorf("String(%q) = %q; want %q", in, got, want)
		}
	}
	long := strings.Repeat("é", logsafe.MaxLength)
	if got := logsafe.String(long); len(got) > logsafe.MaxLength+len("…") || !strings.HasSuffix(got, "…") {
		t.Fatalf("String(long) has %d bytes", len(got))
	}
}

func TestAttrs(t *testing.T) {
	if a := logsafe.Error(errors.New("x\ny")); a.Key != "error" || a.Value.String() != "xy" {
		t.Fatalf("Error = %v", a)
	}
	if a := logsafe.Error(nil); a.Value.String() != "" {
		t.Fatalf("Error(nil) = %v", a)
	}
	if a := logsafe.Attr("backup_id", "b\r1"); a.Key != "backup_id" || a.Value.String() != "b1" {
		t.Fatalf("Attr = %v", a)
	}
}
