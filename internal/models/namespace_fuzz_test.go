package models

import (
	"strings"
	"testing"
	"time"
)

// FuzzRescueDatabaseName checks that the safe-clone target of a valid source database
// is itself a valid database name of at most 63 bytes, ends with "_rescue_<timestamp>"
// and starts with (a prefix of) the source name, and that a name that fits is kept.
func FuzzRescueDatabaseName(f *testing.F) {
	for _, s := range []string{
		"shop", "ecommerce_prod", strings.Repeat("a", 40), strings.Repeat("a", 41), strings.Repeat("a", 63),
		strings.Repeat("é", 30), "日本語データベース名前がとても長い場合のテストケース", "a*b", "a-b_c", "", "legacy db",
	} {
		f.Add(s, int64(1790000000))
	}
	f.Add("shop", int64(-62135596800)) // year 1
	f.Add("shop", int64(253402300799)) // year 9999
	f.Fuzz(func(t *testing.T, source string, unix int64) {
		// Four-digit years only: that is what the timestamp layout produces.
		if unix < -62135596800 || unix > 253402300799 {
			return
		}
		at := time.Unix(unix, 0)
		name := RescueDatabaseName(source, at)
		suffix := "_rescue_" + at.UTC().Format("20060102_150405")
		if !strings.HasSuffix(name, suffix) || !strings.HasPrefix(source, strings.TrimSuffix(name, suffix)) {
			t.Fatalf("RescueDatabaseName(%q) = %q; want a prefix of the source and %q", source, name, suffix)
		}
		if len(name) > MaxDatabaseNameLength {
			t.Fatalf("RescueDatabaseName(%q) = %q is %d bytes", source, name, len(name))
		}
		if len(source)+len(suffix) <= MaxDatabaseNameLength && name != source+suffix {
			t.Fatalf("RescueDatabaseName(%q) = %q shortened a name that fits", source, name)
		}
		if ValidateDatabaseName(source) == nil {
			if err := ValidateDatabaseName(name); err != nil {
				t.Fatalf("RescueDatabaseName(%q) = %q: %v", source, name, err)
			}
		}
	})
}

// FuzzValidateNamespace checks that the database and collection name validators
// never panic and never accept a name MongoDB's cross-platform rules refuse.
func FuzzValidateNamespace(f *testing.F) {
	for _, s := range []string{
		"shop", "users", "a.b", "a b", "a/b", `a\b`, "a$b", "a\"b", "a*b", "", strings.Repeat("x", 64),
		"a\x00b", "a\nb", "a\x7fb", "-x", "--drop", "system.users", "..", "日本",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		control := strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f })
		if ValidateDatabaseName(name) == nil {
			if name == "" || len(name) > MaxDatabaseNameLength || control ||
				strings.ContainsAny(name, "/\\. \"$") || strings.HasPrefix(name, "-") {
				t.Fatalf("ValidateDatabaseName accepted %q", name)
			}
		}
		if ValidateCollectionName(name) == nil {
			if name == "" || control || strings.Contains(name, "$") {
				t.Fatalf("ValidateCollectionName accepted %q", name)
			}
		}
		if err := ValidateCollectionNames([]string{" ", name}); (err == nil) != (strings.TrimSpace(name) == "" || ValidateCollectionName(strings.TrimSpace(name)) == nil) {
			t.Fatalf("ValidateCollectionNames([%q]) = %v disagrees with ValidateCollectionName", name, err)
		}
	})
}
