package models

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
)

// Read preference modes a connection or job may set (the MongoDB names). An empty
// mode keeps whatever the connection string says, which is the primary unless the
// URI sets readPreference itself.
const (
	ReadPrimary            = "primary"
	ReadPrimaryPreferred   = "primaryPreferred"
	ReadSecondary          = "secondary"
	ReadSecondaryPreferred = "secondaryPreferred"
	ReadNearest            = "nearest"
)

// ReadPreferenceModes lists the valid read preference modes in display order.
var ReadPreferenceModes = []string{ReadPrimary, ReadPrimaryPreferred, ReadSecondary, ReadSecondaryPreferred, ReadNearest}

// Limits of read preference tag sets.
const (
	// MaxReadPreferenceTagSets bounds the tag sets of one read preference.
	MaxReadPreferenceTagSets = 10
	// MaxReadPreferenceTags bounds the tags of one tag set.
	MaxReadPreferenceTags = 10
	// MaxReadPreferenceTagLength bounds a tag name or value.
	MaxReadPreferenceTagLength = 100
)

// ErrInvalidReadPreference is returned for an unknown read preference mode or an
// invalid tag set.
var ErrInvalidReadPreference = errors.New("invalid read preference")

// ReadPreference selects the replica set member a backup reads from: Mode is one of
// ReadPreferenceModes ("" keeps the connection string's own), Tags an ordered list of
// tag sets (the first set that matches an eligible member wins; an empty set matches
// any member).
type ReadPreference struct {
	// Mode is the read preference mode, or "" for the connection string's.
	Mode string `json:"read_preference,omitempty"`
	// Tags are the read preference tag sets; they require a mode other than primary.
	Tags []map[string]string `json:"read_preference_tags,omitempty"`
}

// IsZero reports whether r keeps the connection string's read preference.
func (r ReadPreference) IsZero() bool { return r.Mode == "" && len(r.Tags) == 0 }

// Clone returns a deep copy of r.
func (r ReadPreference) Clone() ReadPreference {
	return ReadPreference{Mode: r.Mode, Tags: CloneTagSets(r.Tags)}
}

// Or returns r when it sets a mode, else fallback (a job's read preference
// overrides its connection's).
func (r ReadPreference) Or(fallback ReadPreference) ReadPreference {
	if r.Mode != "" {
		return r
	}
	return fallback
}

// String renders r for logs and messages: "secondary", "secondary (dc=east)" or
// "" for the zero value.
func (r ReadPreference) String() string {
	if r.Mode == "" {
		return ""
	}
	if len(r.Tags) == 0 {
		return r.Mode
	}
	sets := make([]string, 0, len(r.Tags))
	for _, set := range r.Tags {
		sets = append(sets, FormatTagSet(set))
	}
	return r.Mode + " (" + strings.Join(sets, "; ") + ")"
}

// FormatTagSet renders a tag set as "k1=v1,k2=v2" sorted by name, or "{}" for the
// empty set.
func FormatTagSet(set map[string]string) string {
	if len(set) == 0 {
		return "{}"
	}
	parts := make([]string, 0, len(set))
	for _, k := range slices.Sorted(maps.Keys(set)) {
		parts = append(parts, k+"="+set[k])
	}
	return strings.Join(parts, ",")
}

// Validate checks r: a known mode (or none), tags only with a mode other than
// primary, at most MaxReadPreferenceTagSets sets of MaxReadPreferenceTags tags, and
// names and values without ',', ':' or control characters (the connection string
// separates tags with them) of at most MaxReadPreferenceTagLength characters.
func (r ReadPreference) Validate() error {
	if r.Mode != "" && !slices.Contains(ReadPreferenceModes, r.Mode) {
		return fmt.Errorf("%w: read_preference must be one of %s", ErrInvalidReadPreference, strings.Join(ReadPreferenceModes, ", "))
	}
	if len(r.Tags) == 0 {
		return nil
	}
	switch {
	case r.Mode == "" || r.Mode == ReadPrimary:
		return fmt.Errorf("%w: read_preference_tags need a read_preference other than primary", ErrInvalidReadPreference)
	case len(r.Tags) > MaxReadPreferenceTagSets:
		return fmt.Errorf("%w: at most %d read_preference_tags sets", ErrInvalidReadPreference, MaxReadPreferenceTagSets)
	}
	for _, set := range r.Tags {
		if len(set) > MaxReadPreferenceTags {
			return fmt.Errorf("%w: at most %d tags per read_preference_tags set", ErrInvalidReadPreference, MaxReadPreferenceTags)
		}
		for k, v := range set {
			if !validTag(k, false) || !validTag(v, true) {
				return fmt.Errorf("%w: tag names and values must be at most %d characters without ',', ':' or control characters (names not empty)",
					ErrInvalidReadPreference, MaxReadPreferenceTagLength)
			}
		}
	}
	return nil
}

// validTag reports whether s may be a tag name (or a value when value is set).
func validTag(s string, value bool) bool {
	if (s == "" && !value) || len(s) > MaxReadPreferenceTagLength {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool { return r == ',' || r == ':' || r < 0x20 || r == 0x7f })
}

// CloneTagSets returns a deep copy of tag sets (nil for none).
func CloneTagSets(sets []map[string]string) []map[string]string {
	if sets == nil {
		return nil
	}
	out := make([]map[string]string, len(sets))
	for i, set := range sets {
		out[i] = maps.Clone(set)
		if out[i] == nil {
			out[i] = map[string]string{}
		}
	}
	return out
}

// SourceMember describes the MongoDB member a backup read from, as reported by the
// hello command on the member the read preference selected.
type SourceMember struct {
	// Host is the member's host:port as the replica set knows it ("me"); empty
	// for a standalone server.
	Host string `json:"host,omitempty"`
	// State is "primary", "secondary", "standalone", "mongos" or "other".
	State string `json:"state"`
	// SetName is the replica set name, if any.
	SetName string `json:"set_name,omitempty"`
}

// Member states of a SourceMember.
const (
	MemberPrimary    = "primary"
	MemberSecondary  = "secondary"
	MemberStandalone = "standalone"
	MemberMongos     = "mongos"
	MemberOther      = "other"
)

// String renders m as "host (state)" or the state alone.
func (m SourceMember) String() string {
	if m.Host == "" {
		return m.State
	}
	return m.Host + " (" + m.State + ")"
}

// Limits of the throttling options.
const (
	// MaxUploadMbps bounds max_upload_mbps (megabits per second; 0 = unlimited).
	MaxUploadMbps = 100000
	// MaxNumParallelCollections bounds num_parallel_collections (mongodump's
	// --numParallelCollections; 0 = mongodump's default of 4).
	MaxNumParallelCollections = 16
	// MaxConcurrentBackupsLimit bounds a connection's max_concurrent_backups.
	MaxConcurrentBackupsLimit = 64
)

// Throttling errors.
var (
	// ErrInvalidUploadRate is returned for a max_upload_mbps outside 0..MaxUploadMbps.
	ErrInvalidUploadRate = fmt.Errorf("max_upload_mbps must be between 0 (unlimited) and %d", MaxUploadMbps)
	// ErrInvalidParallelCollections is returned for a num_parallel_collections
	// outside 0..MaxNumParallelCollections.
	ErrInvalidParallelCollections = fmt.Errorf("num_parallel_collections must be between 1 and %d (0 for mongodump's default)", MaxNumParallelCollections)
	// ErrInvalidConcurrentBackups is returned for a max_concurrent_backups outside
	// 0..MaxConcurrentBackupsLimit.
	ErrInvalidConcurrentBackups = fmt.Errorf("max_concurrent_backups must be between 0 (unlimited) and %d", MaxConcurrentBackupsLimit)
)

// ValidateUploadMbps checks a max_upload_mbps value.
func ValidateUploadMbps(mbps float64) error {
	if math.IsNaN(mbps) || mbps < 0 || mbps > MaxUploadMbps {
		return ErrInvalidUploadRate
	}
	return nil
}

// UploadBytesPerSecond converts megabits per second to bytes per second.
func UploadBytesPerSecond(mbps float64) float64 { return mbps * 1e6 / 8 }
