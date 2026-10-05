package models

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxDatabaseNameLength is the longest database name MongoDB accepts, in bytes.
const MaxDatabaseNameLength = 63

// ErrInvalidNamespace is returned when a database or collection name in a request is
// one MongoDB would refuse. Names reach mongodump and mongorestore only inside single
// "--db=<name>" style arguments, never through a shell, so validation is not what
// prevents argument injection; it keeps names that MongoDB cannot have (and that
// would change the meaning of storage keys) out of new jobs, backups and restores.
// Only client input is validated: scheduled runs of existing jobs and restores of
// existing backups are never refused for their names.
var ErrInvalidNamespace = errors.New("invalid namespace")

// databaseNameForbidden are the characters MongoDB forbids in database names on
// every platform (Windows forbids more, but a Linux server accepts those).
const databaseNameForbidden = "/\\. \"$"

// hasControl reports whether s contains a control character (including NUL, CR, LF).
func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// ValidateDatabaseName checks name against MongoDB's cross-platform rules for database
// names: 1 to MaxDatabaseNameLength bytes, none of / \ . space " $ and no control
// characters (NUL included). A leading '-' is refused as well, so a name can never
// read as a flag. Errors wrap ErrInvalidNamespace.
func ValidateDatabaseName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: database name is required", ErrInvalidNamespace)
	case len(name) > MaxDatabaseNameLength:
		return fmt.Errorf("%w: database name must be at most %d bytes", ErrInvalidNamespace, MaxDatabaseNameLength)
	case hasControl(name):
		return fmt.Errorf("%w: database name must not contain control characters", ErrInvalidNamespace)
	case strings.ContainsAny(name, databaseNameForbidden):
		return fmt.Errorf("%w: database name must not contain any of / \\ . space \" $", ErrInvalidNamespace)
	case strings.HasPrefix(name, "-"):
		return fmt.Errorf("%w: database name must not start with -", ErrInvalidNamespace)
	}
	return nil
}

// ValidateCollectionName checks name against MongoDB's rules for collection names:
// not empty, no '$' and no control characters (NUL included). Errors wrap
// ErrInvalidNamespace.
func ValidateCollectionName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: collection name is required", ErrInvalidNamespace)
	case hasControl(name):
		return fmt.Errorf("%w: collection name must not contain control characters", ErrInvalidNamespace)
	case strings.Contains(name, "$"):
		return fmt.Errorf("%w: collection name must not contain $", ErrInvalidNamespace)
	}
	return nil
}

// IsCollectionPattern reports whether a collection filter entry is a wildcard pattern:
// it contains "*" (any run of characters) or "?" (one character). Backup filters
// never take such an entry as a literal name; it is matched against the database's
// collections (see MatchCollectionPattern).
func IsCollectionPattern(name string) bool {
	return strings.ContainsAny(name, "*?")
}

// MatchCollectionPattern reports whether collection name matches the wildcard
// pattern ("*" any run of characters, "?" one character, case-sensitive). Patterns
// never match system collections (names starting with "system.").
func MatchCollectionPattern(pattern, name string) bool {
	return !strings.HasPrefix(name, "system.") && MatchDatabasePattern(pattern, name)
}

// ValidateCollectionNames validates every non-blank entry of names (blank entries are
// ignored by the tools' argument builders).
func ValidateCollectionNames(names []string) error {
	for _, n := range names {
		if trimmed := strings.TrimSpace(n); trimmed != "" {
			if err := ValidateCollectionName(trimmed); err != nil {
				return err
			}
		}
	}
	return nil
}

// Safe-clone restore targets are named "<source>_rescue_<YYYYMMDD_HHMMSS>".
const (
	rescueInfix      = "_rescue_"
	rescueTimeLayout = "20060102_150405"
)

// RescueDatabaseName returns the safe-clone restore target for source at t:
// "<source>_rescue_<YYYYMMDD_HHMMSS>" in UTC. The source part is shortened (at a
// character boundary) when needed so that the name fits in MaxDatabaseNameLength;
// the suffix alone adds 23 bytes, so any source longer than 40 bytes used to give a
// name MongoDB refuses.
func RescueDatabaseName(source string, t time.Time) string {
	return withSuffix(source, rescueInfix+t.UTC().Format(rescueTimeLayout))
}

// RescueCloneSuffix returns the suffix a point-in-time restore started at t appends
// to the name of every database it restores: "_rescue_<YYYYMMDD_HHMMSS>" in UTC.
// Unlike RescueDatabaseName it never shortens the source name, since mongorestore
// renames a whole instance with one pattern.
func RescueCloneSuffix(t time.Time) string {
	return rescueInfix + t.UTC().Format(rescueTimeLayout)
}

// RescueVerifyCloneSuffix returns the suffix of the databases of a PITR chain test
// started at t: "_rescue_verify_<YYYYMMDD_HHMMSS>_<suffix>", where suffix is
// RescueVerifySuffixLength lowercase hex characters (ErrInvalidVerifySuffix).
func RescueVerifyCloneSuffix(t time.Time, suffix string) (string, error) {
	if !isLowerHex(suffix, RescueVerifySuffixLength) {
		return "", ErrInvalidVerifySuffix
	}
	return rescueVerifyInfix + t.UTC().Format(rescueTimeLayout) + "_" + suffix, nil
}

// rescueVerifyInfix marks the temporary databases of automated restore tests.
const rescueVerifyInfix = "_rescue_verify_"

// RescueVerifySuffixLength is the length of the random hex suffix of a restore
// test's temporary database name.
const RescueVerifySuffixLength = 6

// ErrInvalidVerifySuffix is returned for a restore test suffix that is not
// RescueVerifySuffixLength lowercase hex characters.
var ErrInvalidVerifySuffix = errors.New("restore test suffix must be 6 lowercase hex characters")

// RescueVerifyDatabaseName returns the temporary database of an automated restore
// test of source at t: "<source>_rescue_verify_<YYYYMMDD_HHMMSS>_<suffix>" in UTC,
// where suffix is RescueVerifySuffixLength random lowercase hex characters (see
// NewRescueVerifySuffix), so concurrent tests of one database never share a name.
// The source part is shortened like RescueDatabaseName so that the name fits in
// MaxDatabaseNameLength.
func RescueVerifyDatabaseName(source string, t time.Time, suffix string) (string, error) {
	if !isLowerHex(suffix, RescueVerifySuffixLength) {
		return "", ErrInvalidVerifySuffix
	}
	return withSuffix(source, rescueVerifyInfix+t.UTC().Format(rescueTimeLayout)+"_"+suffix), nil
}

// NewRescueVerifySuffix returns a random suffix for RescueVerifyDatabaseName.
func NewRescueVerifySuffix() (string, error) {
	b := make([]byte, RescueVerifySuffixLength/2)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random restore test suffix: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// IsRescueVerifyDatabaseName reports whether name has the shape of a restore test's
// temporary database (see RescueVerifyDatabaseName).
func IsRescueVerifyDatabaseName(name string) bool {
	i := strings.LastIndex(name, rescueVerifyInfix)
	if i <= 0 {
		return false
	}
	// "<YYYYMMDD_HHMMSS>_<suffix>"
	rest := name[i+len(rescueVerifyInfix):]
	n := len(rescueTimeLayout)
	if len(rest) != n+1+RescueVerifySuffixLength || rest[n] != '_' || !isLowerHex(rest[n+1:], RescueVerifySuffixLength) {
		return false
	}
	_, err := time.Parse(rescueTimeLayout, rest[:n])
	return err == nil
}

// isLowerHex reports whether s is n lowercase hex characters.
func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// withSuffix appends suffix to source, shortening source at a character boundary so
// that the result fits in MaxDatabaseNameLength.
func withSuffix(source, suffix string) string {
	if limit := MaxDatabaseNameLength - len(suffix); len(source) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(source[cut]) {
			cut--
		}
		source = source[:cut]
	}
	return source + suffix
}
