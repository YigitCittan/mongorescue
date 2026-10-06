package models

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestRescueDatabaseNameSameSecond checks that two safe clones of one database
// started within the same second get different names that keep the _rescue_ infix.
func TestRescueDatabaseNameSameSecond(t *testing.T) {
	at := time.Date(2026, 10, 6, 9, 30, 5, 0, time.UTC)
	first, err := RescueDatabaseName("shop", at, "ab12")
	if err != nil {
		t.Fatal(err)
	}
	second, err := RescueDatabaseName("shop", at.Add(900*time.Millisecond), "cd34")
	if err != nil {
		t.Fatal(err)
	}
	if first != "shop_rescue_20261006_093005_ab12" || second != "shop_rescue_20261006_093005_cd34" {
		t.Fatalf("names %q and %q", first, second)
	}
	for _, name := range []string{first, second} {
		if !IsRescueClone(name) || IsRescueVerifyDatabaseName(name) {
			t.Fatalf("%q is not recognised as a safe clone", name)
		}
	}
	if got, err := RescueDatabaseName("shop", at.In(time.FixedZone("x", 3*3600)), "ab12"); err != nil || got != first {
		t.Fatalf("RescueDatabaseName in another zone = %q, %v; want %q (UTC)", got, err, first)
	}
}

// TestNewCloneID checks the shape of random clone IDs.
func TestNewCloneID(t *testing.T) {
	hexID := regexp.MustCompile(`^[0-9a-f]{4}$`)
	for range 20 {
		id, err := NewCloneID()
		if err != nil {
			t.Fatal(err)
		}
		if !hexID.MatchString(id) {
			t.Fatalf("NewCloneID = %q; want 4 lowercase hex characters", id)
		}
	}
}

// TestRescueDatabaseNameLength checks the 63-byte limit: sources up to 35 bytes are
// kept, longer ones are shortened at a character boundary.
func TestRescueDatabaseNameLength(t *testing.T) {
	at := time.Date(2026, 10, 6, 9, 30, 5, 0, time.UTC)
	const suffix = "_rescue_20261006_093005_ab12"
	if len(suffix) != MaxDatabaseNameLength-35 {
		t.Fatalf("suffix is %d bytes", len(suffix))
	}
	cases := []struct {
		source, wantSource string
	}{
		{"", ""},
		{strings.Repeat("a", 35), strings.Repeat("a", 35)},
		{strings.Repeat("a", 36), strings.Repeat("a", 35)},
		{strings.Repeat("a", MaxDatabaseNameLength), strings.Repeat("a", 35)},
		// "é" is 2 bytes: 17 of them (34 bytes) fit, an 18th would end at byte 36.
		{strings.Repeat("é", 18), strings.Repeat("é", 17)},
		{strings.Repeat("a", 34) + "é", strings.Repeat("a", 34)},
	}
	for _, tc := range cases {
		name, err := RescueDatabaseName(tc.source, at, "ab12")
		if err != nil {
			t.Fatal(err)
		}
		if name != tc.wantSource+suffix || len(name) > MaxDatabaseNameLength {
			t.Errorf("RescueDatabaseName(%d bytes) = %q (%d bytes); want %q", len(tc.source), name, len(name), tc.wantSource+suffix)
		}
	}
	if name, _ := RescueDatabaseName(strings.Repeat("a", 35), at, "ab12"); len(name) != MaxDatabaseNameLength {
		t.Errorf("a 35-byte source gives %d bytes; want exactly %d", len(name), MaxDatabaseNameLength)
	}
}

// TestRescueDatabaseNameRefusesBadIDs checks the clone ID validation and the
// pattern shown before a restore starts.
func TestRescueDatabaseNameRefusesBadIDs(t *testing.T) {
	at := time.Date(2026, 10, 6, 9, 30, 5, 0, time.UTC)
	for _, id := range []string{"", "abc", "abcde", "AB12", "zz12", "ab-1"} {
		if _, err := RescueDatabaseName("shop", at, id); !errors.Is(err, ErrInvalidCloneID) {
			t.Errorf("RescueDatabaseName with id %q: %v; want ErrInvalidCloneID", id, err)
		}
	}
	if got := RescueDatabasePattern("shop"); got != "shop_rescue_<YYYYMMDD_HHMMSS>_<id>" || !IsRescueClone(got) {
		t.Errorf("RescueDatabasePattern = %q", got)
	}
}
