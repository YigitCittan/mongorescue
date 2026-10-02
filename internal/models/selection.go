package models

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// SelectionMode says how a job chooses the databases it backs up.
type SelectionMode string

// Selection modes.
const (
	// SelectionSingle backs up one database (Job.Database), as jobs always did.
	SelectionSingle SelectionMode = "single"
	// SelectionList backs up the databases named in DatabaseSelection.Databases.
	SelectionList SelectionMode = "list"
	// SelectionAll backs up every database of the connection except the system ones
	// and those matched by DatabaseSelection.Exclude.
	SelectionAll SelectionMode = "all"
	// SelectionPattern backs up the databases matched by DatabaseSelection.Include and
	// not matched by DatabaseSelection.Exclude.
	SelectionPattern SelectionMode = "pattern"
)

// Limits of a database selection.
const (
	// MaxSelectionDatabases is the most databases a selection may name.
	MaxSelectionDatabases = 500
	// MaxSelectionPatterns is the most include or exclude patterns a selection may have.
	MaxSelectionPatterns = 50
	// MaxJobParallelism is the most databases a job run backs up at the same time.
	MaxJobParallelism = 4
)

// ErrInvalidSelection is returned for a database selection that cannot be used.
var ErrInvalidSelection = errors.New("invalid database selection")

// SystemDatabases are never backed up by list, all and pattern selections.
var SystemDatabases = []string{"admin", "config", "local"}

// IsSystemDatabase reports whether name is admin, config or local.
func IsSystemDatabase(name string) bool {
	return slices.Contains(SystemDatabases, name)
}

// DatabaseSelection chooses the databases a job backs up. A job with several
// databases backs up each of them into its own backup record and archive, grouped by
// the run's ID (BackupRecord.RunID).
type DatabaseSelection struct {
	// Mode is single, list, all or pattern.
	Mode SelectionMode `json:"mode"`
	// Databases names the database of a single selection, the databases of a list
	// selection, and databases always backed up in addition to what an all or
	// pattern selection matches.
	Databases []string `json:"databases,omitempty"`
	// Include lists the glob patterns of a pattern selection ("*" matches any run of
	// characters, "?" one character; case-sensitive).
	Include []string `json:"include,omitempty"`
	// Exclude lists glob patterns of databases an all or pattern selection skips.
	Exclude []string `json:"exclude,omitempty"`
	// AutoIncludeNew, for all and pattern selections, backs up databases that appear
	// on the server after the job was saved. Off (the default), the job keeps backing
	// up the databases it knew (Job.KnownDatabases) and reports new ones instead.
	AutoIncludeNew bool `json:"auto_include_new"`
}

// Multi reports whether the selection can cover more than one database.
func (s DatabaseSelection) Multi() bool {
	return s.Mode != SelectionSingle && s.Mode != ""
}

// Discovers reports whether the selection lists the server's databases to find what
// it matches (all and pattern).
func (s DatabaseSelection) Discovers() bool {
	return s.Mode == SelectionAll || s.Mode == SelectionPattern
}

// Clone returns a deep copy of s.
func (s DatabaseSelection) Clone() DatabaseSelection {
	s.Databases = slices.Clone(s.Databases)
	s.Include = slices.Clone(s.Include)
	s.Exclude = slices.Clone(s.Exclude)
	return s
}

// SameMatch reports whether s and o match the same databases on any server: the
// same mode, explicit databases and patterns (AutoIncludeNew aside).
func (s DatabaseSelection) SameMatch(o DatabaseSelection) bool {
	return s.Mode == o.Mode && slices.Equal(s.Databases, o.Databases) &&
		slices.Equal(s.Include, o.Include) && slices.Equal(s.Exclude, o.Exclude)
}

// Normalize trims and de-duplicates the names and patterns of s (keeping their
// order) and checks them: database names follow ValidateDatabaseName, patterns
// ValidateDatabasePattern, and each mode gets the fields it needs. A single
// selection names exactly one database; a list selection at least one, none of them
// a system database; a pattern selection at least one include pattern. Include is
// only used by pattern selections and AutoIncludeNew only by all and pattern ones.
// Errors wrap ErrInvalidSelection (and ErrInvalidNamespace for bad names).
func (s *DatabaseSelection) Normalize() error {
	s.Databases = cleanList(s.Databases)
	s.Include = cleanList(s.Include)
	s.Exclude = cleanList(s.Exclude)
	if len(s.Databases) > MaxSelectionDatabases {
		return fmt.Errorf("%w: at most %d databases", ErrInvalidSelection, MaxSelectionDatabases)
	}
	if len(s.Include) > MaxSelectionPatterns || len(s.Exclude) > MaxSelectionPatterns {
		return fmt.Errorf("%w: at most %d include and %d exclude patterns", ErrInvalidSelection, MaxSelectionPatterns, MaxSelectionPatterns)
	}
	for _, name := range s.Databases {
		if err := ValidateDatabaseName(name); err != nil {
			return fmt.Errorf("%w: databases: %w", ErrInvalidSelection, err)
		}
	}
	for _, p := range s.Include {
		if err := ValidateDatabasePattern(p); err != nil {
			return fmt.Errorf("%w: include: %w", ErrInvalidSelection, err)
		}
	}
	for _, p := range s.Exclude {
		if err := ValidateDatabasePattern(p); err != nil {
			return fmt.Errorf("%w: exclude: %w", ErrInvalidSelection, err)
		}
	}
	switch s.Mode {
	case SelectionSingle:
		if len(s.Databases) != 1 {
			return fmt.Errorf("%w: a single-database job names exactly one database", ErrInvalidSelection)
		}
		if len(s.Include) > 0 || len(s.Exclude) > 0 || s.AutoIncludeNew {
			return fmt.Errorf("%w: include, exclude and auto_include_new need mode all or pattern", ErrInvalidSelection)
		}
	case SelectionList:
		if len(s.Databases) == 0 {
			return fmt.Errorf("%w: mode list needs at least one database", ErrInvalidSelection)
		}
		if len(s.Include) > 0 || len(s.Exclude) > 0 || s.AutoIncludeNew {
			return fmt.Errorf("%w: include, exclude and auto_include_new need mode all or pattern", ErrInvalidSelection)
		}
	case SelectionAll:
		if len(s.Include) > 0 {
			return fmt.Errorf("%w: include patterns need mode pattern", ErrInvalidSelection)
		}
	case SelectionPattern:
		if len(s.Include) == 0 {
			return fmt.Errorf("%w: mode pattern needs at least one include pattern", ErrInvalidSelection)
		}
	default:
		return fmt.Errorf("%w: mode must be single, list, all or pattern", ErrInvalidSelection)
	}
	if s.Multi() {
		for _, name := range s.Databases {
			if IsSystemDatabase(name) {
				return fmt.Errorf("%w: %s is a system database; admin, config and local are never backed up by a multi-database job", ErrInvalidSelection, name)
			}
		}
	}
	return nil
}

// cleanList trims the entries of list and drops empty and repeated ones.
func cleanList(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		v = strings.TrimSpace(v)
		if v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// ValidateDatabasePattern checks a database glob pattern: the rules of
// ValidateDatabaseName apply to it, with "*" and "?" as the only wildcards. A pattern
// of wildcards only (such as "*") is valid. Errors wrap ErrInvalidNamespace.
func ValidateDatabasePattern(pattern string) error {
	switch {
	case pattern == "":
		return fmt.Errorf("%w: pattern is required", ErrInvalidNamespace)
	case len(pattern) > MaxDatabaseNameLength:
		return fmt.Errorf("%w: pattern must be at most %d bytes", ErrInvalidNamespace, MaxDatabaseNameLength)
	case !utf8.ValidString(pattern):
		return fmt.Errorf("%w: pattern must be valid UTF-8", ErrInvalidNamespace)
	case hasControl(pattern):
		return fmt.Errorf("%w: pattern must not contain control characters", ErrInvalidNamespace)
	case strings.ContainsAny(pattern, databaseNameForbidden):
		return fmt.Errorf("%w: pattern must not contain any of / \\ . space \" $", ErrInvalidNamespace)
	case strings.HasPrefix(pattern, "-"):
		return fmt.Errorf("%w: pattern must not start with -", ErrInvalidNamespace)
	}
	return nil
}

// MatchDatabasePattern reports whether name matches the glob pattern: "*" matches any
// run of characters (also none), "?" exactly one character, and every other character
// itself. Matching is case-sensitive, like MongoDB database names.
func MatchDatabasePattern(pattern, name string) bool {
	p, n := []rune(pattern), []rune(name)
	pi, ni := 0, 0
	star, mark := -1, 0
	for ni < len(n) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == n[ni]):
			pi++
			ni++
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, ni
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			ni = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// matchesAny returns the first of patterns that name matches, or "".
func matchesAny(patterns []string, name string) string {
	for _, p := range patterns {
		if MatchDatabasePattern(p, name) {
			return p
		}
	}
	return ""
}

// Reasons a database is not backed up by a selection (ExcludedDatabase.Reason).
const (
	// ExcludedSystem is admin, config or local.
	ExcludedSystem = "system"
	// ExcludedByPattern is a database an exclude pattern matches.
	ExcludedByPattern = "excluded"
	// ExcludedNotMatched is a database no include pattern matches.
	ExcludedNotMatched = "not_matched"
	// ExcludedNotSelected is a database a list selection does not name.
	ExcludedNotSelected = "not_selected"
	// ExcludedNew is a database that appeared after the job was saved, which a job
	// without AutoIncludeNew does not back up until it is added.
	ExcludedNew = "new"
	// ExcludedNotFound is a named database the server does not have.
	ExcludedNotFound = "not_found"
)

// ExcludedDatabase is a database a selection does not back up, and why.
type ExcludedDatabase struct {
	// Name is the database name.
	Name string `json:"name"`
	// Reason is one of the Excluded constants.
	Reason string `json:"reason"`
	// Pattern is the exclude pattern that matched (Reason ExcludedByPattern).
	Pattern string `json:"pattern,omitempty"`
}

// DatabaseResolution is what a selection backs up on a server right now.
type DatabaseResolution struct {
	// Included lists the databases a run backs up, sorted.
	Included []string `json:"included"`
	// Excluded lists the other databases with the reason, sorted by name.
	Excluded []ExcludedDatabase `json:"excluded"`
	// Missing lists the named databases the server does not have (also in Excluded,
	// as ExcludedNotFound). A run records each of them as failed.
	Missing []string `json:"missing"`
	// New lists the databases matched since the job's known databases were recorded
	// (all and pattern). With AutoIncludeNew they are in Included, else in Excluded.
	New []string `json:"new_since_last_run"`
	// Known is the job's known databases after this resolution (all and pattern; nil
	// otherwise). Runs store it on the job.
	Known []string `json:"-"`
	// Warnings are notes such as patterns that match no database.
	Warnings []string `json:"warnings"`
}

// ResolveSelection resolves sel against the databases of a server. known is the
// job's KnownDatabases: nil means none were recorded yet, so everything an all or
// pattern selection matches counts as known. A single selection names its database
// whatever the server lists (as single-database jobs always did); a list selection
// backs up the named databases the server has and reports the others as missing;
// all and pattern selections match the server's databases (never admin, config or
// local) and add the explicitly named ones.
func ResolveSelection(sel DatabaseSelection, server, known []string) DatabaseResolution {
	res := DatabaseResolution{Included: []string{}, Excluded: []ExcludedDatabase{}, Missing: []string{}, New: []string{}, Warnings: []string{}}
	onServer := make(map[string]bool, len(server))
	for _, name := range server {
		onServer[name] = true
	}
	included := map[string]bool{}
	excluded := map[string]ExcludedDatabase{}
	include := func(name string) {
		included[name] = true
		delete(excluded, name)
	}
	exclude := func(name, reason, pattern string) {
		if !included[name] {
			excluded[name] = ExcludedDatabase{Name: name, Reason: reason, Pattern: pattern}
		}
	}
	missing := func(name string) {
		res.Missing = append(res.Missing, name)
		exclude(name, ExcludedNotFound, "")
	}

	switch sel.Mode {
	case SelectionSingle, "":
		for _, name := range sel.Databases {
			include(name)
		}
		for _, name := range server {
			if !included[name] {
				exclude(name, systemOr(name, ExcludedNotSelected), "")
			}
		}
	case SelectionList:
		for _, name := range sel.Databases {
			if onServer[name] {
				include(name)
			} else {
				missing(name)
			}
		}
		for _, name := range server {
			if !included[name] {
				exclude(name, systemOr(name, ExcludedNotSelected), "")
			}
		}
	case SelectionAll, SelectionPattern:
		var matched []string
		for _, name := range server {
			switch {
			case IsSystemDatabase(name):
				exclude(name, ExcludedSystem, "")
			case sel.Mode == SelectionPattern && matchesAny(sel.Include, name) == "":
				exclude(name, ExcludedNotMatched, "")
			case matchesAny(sel.Exclude, name) != "":
				exclude(name, ExcludedByPattern, matchesAny(sel.Exclude, name))
			default:
				matched = append(matched, name)
			}
		}
		slices.Sort(matched)
		knownSet := make(map[string]bool, len(known))
		for _, name := range known {
			knownSet[name] = true
		}
		res.Known = slices.Clone(known)
		for _, name := range matched {
			switch {
			case known == nil:
				res.Known = append(res.Known, name)
				include(name)
			case knownSet[name]:
				include(name)
			default:
				res.New = append(res.New, name)
				if sel.AutoIncludeNew {
					res.Known = append(res.Known, name)
					include(name)
				} else {
					exclude(name, ExcludedNew, "")
				}
			}
		}
		if res.Known == nil {
			res.Known = []string{}
		}
		slices.Sort(res.Known)
		res.Known = slices.Compact(res.Known)
		for _, name := range sel.Databases {
			if onServer[name] {
				include(name)
			} else {
				missing(name)
			}
		}
		for _, p := range sel.Include {
			if !slices.ContainsFunc(server, func(name string) bool { return MatchDatabasePattern(p, name) }) {
				res.Warnings = append(res.Warnings, fmt.Sprintf("include pattern %q matches no database", p))
			}
		}
		for _, p := range sel.Exclude {
			if !slices.ContainsFunc(server, func(name string) bool { return MatchDatabasePattern(p, name) }) {
				res.Warnings = append(res.Warnings, fmt.Sprintf("exclude pattern %q matches no database", p))
			}
		}
	}

	for name := range included {
		res.Included = append(res.Included, name)
	}
	slices.Sort(res.Included)
	for _, e := range excluded {
		res.Excluded = append(res.Excluded, e)
	}
	slices.SortFunc(res.Excluded, func(a, b ExcludedDatabase) int { return strings.Compare(a.Name, b.Name) })
	if len(res.Included) == 0 && sel.Multi() {
		res.Warnings = append(res.Warnings, "the selection matches no database")
	}
	return res
}

// systemOr returns ExcludedSystem for a system database and reason otherwise.
func systemOr(name, reason string) string {
	if IsSystemDatabase(name) {
		return ExcludedSystem
	}
	return reason
}

// Matches reports whether the selection backs up database name, its known databases
// aside: a single or list selection names it; an all selection does not exclude it;
// a pattern selection matches it with an include pattern and no exclude pattern. The
// databases an all or pattern selection names are always matched; system databases
// only when a single selection names them.
func (s DatabaseSelection) Matches(name string) bool {
	if slices.Contains(s.Databases, name) {
		return true
	}
	switch s.Mode {
	case SelectionAll:
		return !IsSystemDatabase(name) && matchesAny(s.Exclude, name) == ""
	case SelectionPattern:
		return !IsSystemDatabase(name) && matchesAny(s.Include, name) != "" && matchesAny(s.Exclude, name) == ""
	}
	return false
}

// SearchText is the text of the selection a search matches: its mode, names and
// patterns.
func (s DatabaseSelection) SearchText() string {
	parts := append([]string{string(s.Mode)}, s.Databases...)
	parts = append(parts, s.Include...)
	parts = append(parts, s.Exclude...)
	return strings.Join(parts, " ")
}
