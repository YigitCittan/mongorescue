package models

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// MaxFilterCollections is the most collections one DatabaseFilter may name in
// Collections or in ExcludeCollections.
const MaxFilterCollections = 1000

// ErrInvalidDatabaseFilter is returned for a database entry whose JSON is neither a
// database name nor an object with a name.
var ErrInvalidDatabaseFilter = errors.New("a database entry is a name or an object with name, collections and exclude_collections")

// ErrIncludeAndExclude is returned for a database filter that sets both collections
// and exclude_collections: a database's filter either names the collections backed
// up or the ones skipped.
var ErrIncludeAndExclude = errors.New("a database's collection filter takes collections or exclude_collections, not both")

// DatabaseFilter names one database of a backup of several databases, with an
// optional collection filter for it: Collections backs up only those collections,
// ExcludeCollections every collection but those (one of the two), exactly like the
// collection filters of a backup of one database ("*" and "?" are wildcards the
// backup expands). In JSON it is either the database name ("shop", no filter) or an
// object {"name": "shop", "collections": [...]} or {"name": "shop",
// "exclude_collections": [...]}; an
// entry without a filter is written as its name, so lists of names stay readable
// by clients that predate filters.
type DatabaseFilter struct {
	// Name is the database name.
	Name string `json:"name"`
	// Collections, when not empty, are the only collections backed up.
	Collections []string `json:"collections,omitempty"`
	// ExcludeCollections are collections the backup skips.
	ExcludeCollections []string `json:"exclude_collections,omitempty"`
}

// DatabaseNames returns an entry without a filter for each of names.
func DatabaseNames(names ...string) []DatabaseFilter {
	out := make([]DatabaseFilter, len(names))
	for i, n := range names {
		out[i] = DatabaseFilter{Name: n}
	}
	return out
}

// FilterNames returns the database names of filters, in order.
func FilterNames(filters []DatabaseFilter) []string {
	out := make([]string, len(filters))
	for i, f := range filters {
		out[i] = f.Name
	}
	return out
}

// Filtered reports whether f restricts its database to some collections.
func (f DatabaseFilter) Filtered() bool {
	return len(f.Collections) > 0 || len(f.ExcludeCollections) > 0
}

// Clone returns a deep copy of f.
func (f DatabaseFilter) Clone() DatabaseFilter {
	f.Collections = slices.Clone(f.Collections)
	f.ExcludeCollections = slices.Clone(f.ExcludeCollections)
	return f
}

// Normalize trims the name and drops blank and repeated collection names, then
// checks them: the database name follows ValidateDatabaseName, the collection names
// ValidateCollectionName, each list has at most MaxFilterCollections entries, and
// only one of the two lists is set. Errors wrap ErrInvalidNamespace or
// ErrIncludeAndExclude.
func (f *DatabaseFilter) Normalize() error {
	f.Name = strings.TrimSpace(f.Name)
	f.Collections = cleanList(f.Collections)
	f.ExcludeCollections = cleanList(f.ExcludeCollections)
	if err := ValidateDatabaseName(f.Name); err != nil {
		return err
	}
	if len(f.Collections) > MaxFilterCollections || len(f.ExcludeCollections) > MaxFilterCollections {
		return fmt.Errorf("%w: %s: at most %d collections and %d excluded collections", ErrInvalidNamespace, f.Name, MaxFilterCollections, MaxFilterCollections)
	}
	if err := ValidateCollectionNames(f.Collections); err != nil {
		return fmt.Errorf("%s: collections: %w", f.Name, err)
	}
	if err := ValidateCollectionNames(f.ExcludeCollections); err != nil {
		return fmt.Errorf("%s: exclude_collections: %w", f.Name, err)
	}
	if len(f.Collections) > 0 && len(f.ExcludeCollections) > 0 {
		return fmt.Errorf("%w (database %s)", ErrIncludeAndExclude, f.Name)
	}
	return nil
}

// UnmarshalJSON reads a database name or an object with name, collections and
// exclude_collections.
func (f *DatabaseFilter) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var name string
		if err := json.Unmarshal(b, &name); err != nil {
			return err
		}
		*f = DatabaseFilter{Name: name}
		return nil
	}
	if len(b) == 0 || b[0] != '{' {
		return ErrInvalidDatabaseFilter
	}
	type plain DatabaseFilter
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*f = DatabaseFilter(p)
	return nil
}

// MarshalJSON writes the name alone for an entry without a filter, else the object.
func (f DatabaseFilter) MarshalJSON() ([]byte, error) {
	if !f.Filtered() {
		return json.Marshal(f.Name)
	}
	type plain DatabaseFilter
	return json.Marshal(plain(f))
}

// UnmarshalJSON reads a selection whose databases may be names or objects with a
// collection filter (see DatabaseFilter): the filter of an object entry is added to
// CollectionFilters. In a list (or single) selection every entry also names its
// database in Databases. In an all or pattern selection, where Databases are the
// databases always backed up in addition to what the selection matches, an entry
// with a filter only attaches the filter: it does not add the database to Databases
// (a name does, as before).
func (s *DatabaseSelection) UnmarshalJSON(b []byte) error {
	type plain DatabaseSelection
	var raw struct {
		plain
		Databases []DatabaseFilter `json:"databases,omitempty"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*s = DatabaseSelection(raw.plain)
	s.Databases = nil
	if raw.Databases != nil {
		s.Databases = make([]string, 0, len(raw.Databases))
	}
	attachOnly := s.Discovers()
	for _, entry := range raw.Databases {
		if entry.Filtered() {
			s.CollectionFilters = append(s.CollectionFilters, entry)
			if attachOnly {
				continue
			}
		}
		s.Databases = append(s.Databases, entry.Name)
	}
	return nil
}

// FilterFor returns the collection filter the selection sets for database name, if
// any (list and all selections only).
func (s DatabaseSelection) FilterFor(name string) (DatabaseFilter, bool) {
	for _, f := range s.CollectionFilters {
		if f.Name == name {
			return f.Clone(), true
		}
	}
	return DatabaseFilter{}, false
}

// normalizeFilters checks CollectionFilters (see Normalize): every filter is
// normalized and dropped when it filters nothing, names a database once, and only
// list and all selections have filters. A list selection's filters name databases it
// lists; an all selection's name databases it can back up (no system database and
// none its exclude patterns skip, unless it names them in Databases). Filters are
// kept sorted by database name. Errors wrap ErrInvalidSelection.
func (s *DatabaseSelection) normalizeFilters() error {
	if len(s.CollectionFilters) == 0 {
		s.CollectionFilters = nil
		return nil
	}
	if len(s.CollectionFilters) > MaxSelectionDatabases {
		return fmt.Errorf("%w: at most %d collection filters", ErrInvalidSelection, MaxSelectionDatabases)
	}
	out := make([]DatabaseFilter, 0, len(s.CollectionFilters))
	for _, f := range s.CollectionFilters {
		f = f.Clone()
		if err := f.Normalize(); err != nil {
			return fmt.Errorf("%w: collection_filters: %w", ErrInvalidSelection, err)
		}
		if !f.Filtered() {
			continue
		}
		if slices.ContainsFunc(out, func(o DatabaseFilter) bool { return o.Name == f.Name }) {
			return fmt.Errorf("%w: collection_filters: database %s has more than one filter", ErrInvalidSelection, f.Name)
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		s.CollectionFilters = nil
		return nil
	}
	switch s.Mode {
	case SelectionList:
		for _, f := range out {
			if !slices.Contains(s.Databases, f.Name) {
				return fmt.Errorf("%w: collection_filters: %s is not one of the databases", ErrInvalidSelection, f.Name)
			}
		}
	case SelectionAll:
		for _, f := range out {
			if slices.Contains(s.Databases, f.Name) {
				continue
			}
			if IsSystemDatabase(f.Name) {
				return fmt.Errorf("%w: collection_filters: %s is a system database", ErrInvalidSelection, f.Name)
			}
			if p := matchesAny(s.Exclude, f.Name); p != "" {
				return fmt.Errorf("%w: collection_filters: filter for an excluded database: %s is excluded by %q", ErrInvalidSelection, f.Name, p)
			}
		}
	default:
		return fmt.Errorf("%w: collection filters per database need mode list or all (a single-database job uses collections and exclude_collections; pattern selections have none)", ErrInvalidSelection)
	}
	slices.SortFunc(out, func(a, b DatabaseFilter) int { return strings.Compare(a.Name, b.Name) })
	s.CollectionFilters = out
	return nil
}
