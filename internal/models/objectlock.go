package models

import (
	"fmt"
	"strings"
)

// ObjectLockSettings is the S3 Object Lock of a storage target: its mode, retention
// and legal hold on pin (see S3Target).
type ObjectLockSettings struct {
	// Mode is "" (none), ObjectLockGovernance or ObjectLockCompliance.
	Mode ObjectLockMode `json:"mode"`
	// RetentionDays is the retention of every upload; 0 without a mode.
	RetentionDays int `json:"retention_days"`
	// LegalHoldOnPin sets a legal hold on pinned backups' archives.
	LegalHoldOnPin bool `json:"legal_hold_on_pin"`
}

// NormalizeObjectLockMode trims and lower-cases m and maps ObjectLockNone to "".
func NormalizeObjectLockMode(m ObjectLockMode) ObjectLockMode {
	m = ObjectLockMode(strings.ToLower(strings.TrimSpace(string(m))))
	if m == ObjectLockNone {
		return ""
	}
	return m
}

// LockSettings returns the object lock of t, normalized (the zero value for nil).
func (t *S3Target) LockSettings() ObjectLockSettings {
	if t == nil {
		return ObjectLockSettings{}
	}
	return ObjectLockSettings{Mode: NormalizeObjectLockMode(t.ObjectLock), RetentionDays: t.RetentionDays, LegalHoldOnPin: t.LegalHoldOnPin}.normalized()
}

// SetLockSettings replaces the object lock of t with l.
func (t *S3Target) SetLockSettings(l ObjectLockSettings) {
	l = l.normalized()
	t.ObjectLock, t.RetentionDays, t.LegalHoldOnPin = l.Mode, l.RetentionDays, l.LegalHoldOnPin
}

// Locked reports whether l locks uploads.
func (l ObjectLockSettings) Locked() bool {
	return l.Mode == ObjectLockGovernance || l.Mode == ObjectLockCompliance
}

// Valid reports whether l is a lock a target may have: no mode, or a mode with a
// retention in range.
func (l ObjectLockSettings) Valid() bool {
	switch NormalizeObjectLockMode(l.Mode) {
	case "":
		return !l.LegalHoldOnPin
	case ObjectLockGovernance, ObjectLockCompliance:
		return l.RetentionDays >= MinObjectLockRetentionDays && l.RetentionDays <= MaxObjectLockRetentionDays
	}
	return false
}

// String describes l in English ("compliance, 30 days, legal hold on pin").
func (l ObjectLockSettings) String() string {
	if !l.Locked() {
		return "no object lock"
	}
	s := fmt.Sprintf("%s, %d days", l.Mode, l.RetentionDays)
	if l.LegalHoldOnPin {
		s += ", legal hold on pin"
	}
	return s
}

// normalized clears the retention and legal hold of a lock without a mode.
func (l ObjectLockSettings) normalized() ObjectLockSettings {
	l.Mode = NormalizeObjectLockMode(l.Mode)
	if !l.Locked() {
		l.RetentionDays, l.LegalHoldOnPin = 0, false
	}
	return l
}

// lockRank orders the modes by strength: none, governance, compliance.
func lockRank(m ObjectLockMode) int {
	switch m {
	case ObjectLockCompliance:
		return 2
	case ObjectLockGovernance:
		return 1
	}
	return 0
}

// SplitLockChange splits a change of a target's object lock from current to
// requested into what applies at once and what lowers it. Raising a part (a
// stronger mode, a longer retention, a legal hold on pin turned on) applies at
// once; lowering a part (governance instead of compliance, no lock, a shorter
// retention, the legal hold turned off) is held: now keeps the current value of
// that part, and held is the requested lock, to apply after the delete grace
// period. held is nil when nothing is lowered.
func SplitLockChange(current, requested ObjectLockSettings) (now ObjectLockSettings, held *ObjectLockSettings) {
	current, requested = current.normalized(), requested.normalized()
	now.Mode = current.Mode
	if lockRank(requested.Mode) > lockRank(current.Mode) {
		now.Mode = requested.Mode
	}
	switch {
	case current.Locked() && requested.Locked():
		now.RetentionDays = max(current.RetentionDays, requested.RetentionDays)
	case current.Locked():
		now.RetentionDays = current.RetentionDays
	default:
		now.RetentionDays = requested.RetentionDays
	}
	now.LegalHoldOnPin = current.LegalHoldOnPin || requested.LegalHoldOnPin
	now = now.normalized()
	if requested == now {
		return now, nil
	}
	return now, &requested
}

// LowerLock applies a held lowering to the lock in force when it takes effect:
// every part becomes the weaker of the two, so a lowering never raises a part that
// was changed meanwhile.
func LowerLock(current, lowered ObjectLockSettings) ObjectLockSettings {
	current, lowered = current.normalized(), lowered.normalized()
	out := ObjectLockSettings{Mode: current.Mode}
	if lockRank(lowered.Mode) < lockRank(current.Mode) {
		out.Mode = lowered.Mode
	}
	if lowered.Locked() && current.Locked() {
		out.RetentionDays = min(current.RetentionDays, lowered.RetentionDays)
	} else {
		out.RetentionDays = current.RetentionDays
	}
	out.LegalHoldOnPin = current.LegalHoldOnPin && lowered.LegalHoldOnPin
	return out.normalized()
}
