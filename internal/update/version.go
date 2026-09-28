// Package update checks GitHub for a newer release of the MongoRescue desktop app and
// downloads its installer or archive, verified against the release's SHA-256
// checksums. It knows the release asset names and the update policy (a higher MAJOR
// version is mandatory, MINOR and PATCH updates are optional); installing is left to
// the caller.
package update

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrInvalidVersion is returned for a version that is not MAJOR.MINOR.PATCH with an
// optional leading "v". Pre-release and build suffixes are rejected too: the app only
// updates to, and from, final releases.
var ErrInvalidVersion = errors.New("invalid version")

// Version is a final semantic version (MAJOR.MINOR.PATCH).
type Version struct {
	Major, Minor, Patch int
}

// ParseVersion parses "X.Y.Z" or "vX.Y.Z". Each part is a decimal number without a
// sign or leading zeros; anything else, such as "dev", "1.2" or "1.2.3-rc.1", wraps
// ErrInvalidVersion.
func ParseVersion(s string) (Version, error) {
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("%w: %q", ErrInvalidVersion, s)
	}
	var nums [3]int
	for i, p := range parts {
		n, err := parseNumber(p)
		if err != nil {
			return Version{}, fmt.Errorf("%w: %q", ErrInvalidVersion, s)
		}
		nums[i] = n
	}
	return Version{Major: nums[0], Minor: nums[1], Patch: nums[2]}, nil
}

// parseNumber parses a semver numeric identifier: digits only, no leading zero.
func parseNumber(p string) (int, error) {
	if p == "" || len(p) > 9 || (len(p) > 1 && p[0] == '0') {
		return 0, ErrInvalidVersion
	}
	for _, c := range p {
		if c < '0' || c > '9' {
			return 0, ErrInvalidVersion
		}
	}
	return strconv.Atoi(p)
}

// String returns the version as "X.Y.Z", without a leading "v".
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Compare returns -1, 0 or +1 as v is lower than, equal to or higher than o.
func (v Version) Compare(o Version) int {
	for _, d := range [3]int{v.Major - o.Major, v.Minor - o.Minor, v.Patch - o.Patch} {
		switch {
		case d < 0:
			return -1
		case d > 0:
			return 1
		}
	}
	return 0
}
