package mongotools

import "testing"

func TestParseToolVersion(t *testing.T) {
	for out, want := range map[string]string{
		"mongorestore version: 100.12.2\ngit version: abc\n": "100.12.2",
		"mongorestore version: v100.16.0":                    "100.16.0",
		"mongorestore version: 100.9":                        "100.9",
	} {
		if got, ok := ParseToolVersion(out); !ok || got != want {
			t.Errorf("ParseToolVersion(%q) = %q, %v; want %q", out, got, ok, want)
		}
	}
	if _, ok := ParseToolVersion("built-without-version-string"); ok {
		t.Error("a version was read from output without one")
	}
}

func TestVersionAtLeast(t *testing.T) {
	for _, c := range []struct {
		v    string
		want bool
	}{{"100.12.0", true}, {"100.12.2", true}, {"100.16.0", true}, {"100.11.9", false}, {"100.9.4", false}, {"101.0", true}} {
		if got := VersionAtLeast(c.v, MinPITRToolsVersion); got != c.want {
			t.Errorf("VersionAtLeast(%s) = %v, want %v", c.v, got, c.want)
		}
	}
}

func TestLimitedBuffer(t *testing.T) {
	b := &limitedBuffer{max: 4}
	if n, err := b.Write([]byte("abcdef")); n != 6 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	_, _ = b.Write([]byte("gh"))
	if b.String() != "abcd" {
		t.Fatalf("kept %q", b.String())
	}
}
