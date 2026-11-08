package store

import (
	"errors"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// sqliteCode returns the extended SQLite result code of err, or false when err is
// not an error of the SQLite driver.
func sqliteCode(err error) (int, bool) {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code(), true
	}
	return 0, false
}

// isPrimaryKeyViolation reports whether err is a PRIMARY KEY constraint failure
// (SQLITE_CONSTRAINT_PRIMARYKEY): an insert of a row whose key is taken.
func isPrimaryKeyViolation(err error) bool {
	code, ok := sqliteCode(err)
	return ok && code == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
}

// isUniqueIndexViolation reports whether err is a UNIQUE constraint failure
// (SQLITE_CONSTRAINT_UNIQUE) of a column, a table constraint or a unique index.
func isUniqueIndexViolation(err error) bool {
	code, ok := sqliteCode(err)
	return ok && code == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}

// isUniqueViolation reports whether err is a failed insert or update of a row whose
// primary key or unique value is taken. It reads the driver's result code, never the
// error text.
func isUniqueViolation(err error) bool {
	return isPrimaryKeyViolation(err) || isUniqueIndexViolation(err)
}
