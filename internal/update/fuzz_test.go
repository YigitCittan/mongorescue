package update

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// FuzzParseVersion checks that ParseVersion never panics and that an accepted version
// is canonical: its String form, with the optional "v", is the input.
func FuzzParseVersion(f *testing.F) {
	for _, s := range []string{
		"1.2.3", "v0.3.2", "v10.0.100", "", "dev", "1.2", "1.2.3.4", "v1.2.3-rc.1", "1.2.3+build",
		"01.2.3", "1.-2.3", "vv1.2.3", "1..3", "1.2.x", "1.2.3 ", "999999999.999999999.999999999",
		"1000000000.0.0", "+1.2.3", "1.2.٣", "V1.2.3",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, err := ParseVersion(s)
		if err != nil {
			if !errors.Is(err, ErrInvalidVersion) {
				t.Fatalf("ParseVersion(%q): unexpected error %v", s, err)
			}
			return
		}
		if v.Major < 0 || v.Minor < 0 || v.Patch < 0 {
			t.Fatalf("ParseVersion(%q) = %v has a negative part", s, v)
		}
		if strings.TrimPrefix(s, "v") != v.String() {
			t.Fatalf("ParseVersion(%q) = %v is not canonical", s, v)
		}
		again, err := ParseVersion(v.String())
		if err != nil || again != v {
			t.Fatalf("ParseVersion(%q) = %v, %v; want %v", v.String(), again, err, v)
		}
	})
}

// FuzzCompare checks that Compare is a total order matching the numeric
// (major, minor, patch) order.
func FuzzCompare(f *testing.F) {
	f.Add(1, 0, 0, 1, 0, 0, 0, 0, 0)
	f.Add(0, 3, 2, 0, 10, 0, 1, 0, 0)
	f.Add(2, 0, 0, 1, 9, 9, 2, 0, 0)
	f.Add(999999999, 0, 0, 0, 999999999, 999999999, 0, 0, 1)
	f.Fuzz(func(t *testing.T, a1, a2, a3, b1, b2, b3, c1, c2, c3 int) {
		// ParseVersion yields parts in [0, 999999999].
		clamp := func(n int) int {
			if n < 0 {
				n = -(n + 1)
			}
			return n % 1_000_000_000
		}
		a := Version{clamp(a1), clamp(a2), clamp(a3)}
		b := Version{clamp(b1), clamp(b2), clamp(b3)}
		c := Version{clamp(c1), clamp(c2), clamp(c3)}
		ab, ba := a.Compare(b), b.Compare(a)
		if ab != -ba || ab < -1 || ab > 1 {
			t.Fatalf("Compare(%v, %v) = %d but Compare(%v, %v) = %d", a, b, ab, b, a, ba)
		}
		if a.Compare(a) != 0 || (ab == 0) != (a == b) {
			t.Fatalf("Compare(%v, %v) = %d disagrees with equality", a, b, ab)
		}
		want := 0
		switch {
		case a.Major != b.Major:
			want = sign(a.Major - b.Major)
		case a.Minor != b.Minor:
			want = sign(a.Minor - b.Minor)
		case a.Patch != b.Patch:
			want = sign(a.Patch - b.Patch)
		}
		if ab != want {
			t.Fatalf("Compare(%v, %v) = %d; want %d", a, b, ab, want)
		}
		if ab <= 0 && b.Compare(c) <= 0 && a.Compare(c) > 0 {
			t.Fatalf("Compare is not transitive for %v <= %v <= %v", a, b, c)
		}
	})
}

// sign returns -1, 0 or +1 as n is negative, zero or positive.
func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// FuzzFindChecksum checks that findChecksum never panics and returns a digest only
// when a line lists exactly the digest and the name (or "*name").
func FuzzFindChecksum(f *testing.F) {
	sum := sha256.Sum256([]byte("x"))
	digest := hex.EncodeToString(sum[:])
	f.Add(digest+"  a.zip\n", "a.zip")
	f.Add("# comment\n"+digest+" *a.zip\n", "a.zip")
	f.Add(digest+"  b.zip\n"+digest+"  a.zip", "a.zip")
	f.Add(digest[:62]+"  a.zip\n", "a.zip")
	f.Add(digest+"zz  a.zip\n", "a.zip")
	f.Add(digest+"  a.zip extra\n", "a.zip")
	f.Add(strings.ToUpper(digest)+"\ta.zip\r\n", "a.zip")
	f.Add(digest+"  *\n", "")
	f.Add(digest+"  a zip\n", "a zip")
	f.Add(strings.Repeat("a", 70000)+"\n"+digest+"  a.zip\n", "a.zip")
	f.Fuzz(func(t *testing.T, file, name string) {
		got, err := findChecksum(strings.NewReader(file), name)
		if err != nil {
			return
		}
		if len(got) != sha256.Size {
			t.Fatalf("findChecksum returned %d bytes", len(got))
		}
		for _, line := range strings.Split(file, "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
				continue
			}
			if d, err := hex.DecodeString(fields[0]); err == nil && bytes.Equal(d, got) {
				return
			}
		}
		t.Fatalf("findChecksum(%q, %q) = %x, which no line lists", file, name, got)
	})
}
