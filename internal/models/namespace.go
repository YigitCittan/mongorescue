package models

import (
	"errors"
	"fmt"
	"strings"
)

// MaxDatabaseNameLength is the longest database name MongoDB accepts, in bytes.
const MaxDatabaseNameLength = 63

// ErrInvalidNamespace is returned when a database or collection name is one MongoDB
// would refuse. Names reach mongodump and mongorestore only inside single
// "--db=<name>" style arguments, never through a shell, so validation is not what
// prevents argument injection; it keeps names that MongoDB cannot have (and that
// would change the meaning of storage keys and namespace patterns) out of jobs,
// backups and restores.
var ErrInvalidNamespace = errors.New("invalid namespace")

// databaseNameForbidden are the characters MongoDB forbids in database names on
// every platform, plus '*' which --nsInclude/--nsFrom would read as a wildcard.
const databaseNameForbidden = "/\\. \"$*"

// hasControl reports whether s contains a control character (including NUL, CR, LF).
func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// ValidateDatabaseName checks name against MongoDB's rules for database names: 1 to
// MaxDatabaseNameLength bytes, none of / \ . space " $ * and no control characters.
// Errors wrap ErrInvalidNamespace.
func ValidateDatabaseName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: database name is required", ErrInvalidNamespace)
	case len(name) > MaxDatabaseNameLength:
		return fmt.Errorf("%w: database name must be at most %d bytes", ErrInvalidNamespace, MaxDatabaseNameLength)
	case hasControl(name):
		return fmt.Errorf("%w: database name must not contain control characters", ErrInvalidNamespace)
	case strings.ContainsAny(name, databaseNameForbidden):
		return fmt.Errorf("%w: database name must not contain any of / \\ . space \" $ *", ErrInvalidNamespace)
	}
	return nil
}

// ValidateCollectionName checks name against MongoDB's rules for collection names:
// not empty, no '$', no control characters (including NUL), and no '*' (a wildcard in
// --nsInclude). Errors wrap ErrInvalidNamespace.
func ValidateCollectionName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: collection name is required", ErrInvalidNamespace)
	case hasControl(name):
		return fmt.Errorf("%w: collection name must not contain control characters", ErrInvalidNamespace)
	case strings.ContainsAny(name, "$*"):
		return fmt.Errorf("%w: collection name must not contain $ or *", ErrInvalidNamespace)
	}
	return nil
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
