package models

import (
	"errors"
	"fmt"
	"strings"
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
