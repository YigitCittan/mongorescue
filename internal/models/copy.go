package models

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxCopyTargets is the largest number of copy targets a job or backup may list.
const MaxCopyTargets = 3

// ErrInvalidCopyTargets is returned for an invalid list of copy targets or copy mode.
var ErrInvalidCopyTargets = errors.New("invalid copy targets")

// CopyMode says when a backup with copy targets counts as completed.
type CopyMode string

// Copy modes.
const (
	// CopyAsync (the default, also the empty mode) completes the backup once its
	// primary archive is stored and verified; the copy queue copies it afterwards.
	CopyAsync CopyMode = "async"
	// CopySync completes the backup only when every copy succeeded: a copy that
	// fails fails the backup.
	CopySync CopyMode = "sync"
)

// Valid reports whether m is a known copy mode (the empty mode is CopyAsync).
func (m CopyMode) Valid() bool {
	return m == "" || m == CopyAsync || m == CopySync
}

// Sync reports whether m is CopySync.
func (m CopyMode) Sync() bool { return m == CopySync }

// CopyStatus is the state of one copy of a backup.
type CopyStatus string

// Copy states.
const (
	// CopyPending is a copy waiting in the copy queue: never tried, or retried
	// after a failed attempt (NextAttemptAt).
	CopyPending CopyStatus = "pending"
	// CopyDone is a copy stored on its target whose checksum equals the primary's.
	CopyDone CopyStatus = "done"
	// CopyFailed is a copy whose attempts failed; the queue retries it at
	// NextAttemptAt unless the attempts are exhausted.
	CopyFailed CopyStatus = "failed"
	// CopyPurged is a copy the purge removed (or the cleanup of a failed backup).
	CopyPurged CopyStatus = "purged"
)

// BackupCopy is one copy of a backup's archive on another storage target. The copy
// holds the same bytes (encrypted or not) under the same storage key.
type BackupCopy struct {
	// TargetID is the storage target holding the copy.
	TargetID string `json:"target_id"`
	// TargetName is a snapshot of the target's name when the copy was planned.
	TargetName string `json:"target_name,omitempty"`
	// StorageKey is the object key on the copy target (the primary's key).
	StorageKey string `json:"storage_key"`
	// Status is the copy's state.
	Status CopyStatus `json:"status"`
	// SHA256OK reports that the bytes copied hashed to the primary's SHA-256.
	SHA256OK bool `json:"sha256_ok"`
	// VersionID is the S3 version of the copy on a versioned target.
	VersionID string `json:"version_id,omitempty"`
	// ObjectLockMode and RetainUntil are the copy target's own Object Lock.
	ObjectLockMode ObjectLockMode `json:"object_lock_mode,omitempty"`
	RetainUntil    *time.Time     `json:"retain_until,omitempty"`
	// Error is the redacted reason of the last failed attempt.
	Error string `json:"error,omitempty"`
	// CopiedAt is when the copy was completed.
	CopiedAt *time.Time `json:"copied_at,omitempty"`
	// Attempts counts the attempts made so far.
	Attempts int `json:"attempts,omitempty"`
	// NextAttemptAt is when the copy queue tries a pending or failed copy again;
	// nil for a failed copy whose attempts are exhausted.
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	// VerifiedAt, Verification and VerificationError are the outcome of the last
	// integrity check of the copy (the sweep or an on-demand verification).
	VerifiedAt        *time.Time         `json:"verified_at,omitempty"`
	Verification      VerificationStatus `json:"verification,omitempty"`
	VerificationError string             `json:"verification_error,omitempty"`
	// PurgedAt is when the copy was removed (CopyPurged).
	PurgedAt *time.Time `json:"purged_at,omitempty"`
}

// Healthy reports whether c is a completed copy whose last check did not find it
// damaged or missing: a restore may read it.
func (c *BackupCopy) Healthy() bool {
	return c.Status == CopyDone && c.Verification != VerificationMismatch
}

// MayExist reports whether the object of c may exist on its target: anything but a
// purged copy or one that was never attempted.
func (c *BackupCopy) MayExist() bool {
	return c.Status != CopyPurged && (c.Status != CopyPending || c.Attempts > 0 || c.CopiedAt != nil)
}

// SetStorageObject records the S3 version and Object Lock retention of the stored
// copy obj (nothing for a nil obj).
func (c *BackupCopy) SetStorageObject(obj *StorageObject) {
	if obj == nil {
		return
	}
	c.VersionID = obj.VersionID
	if obj.RetainUntil != nil {
		until := obj.RetainUntil.UTC()
		c.RetainUntil, c.ObjectLockMode = &until, obj.ObjectLockMode
	}
}

// LockedAt reports whether the copy is still under its Object Lock retention.
func (c *BackupCopy) LockedAt(now time.Time) bool {
	return c.Status != CopyPurged && c.RetainUntil != nil && now.Before(*c.RetainUntil)
}

// CopyTarget is a resolved copy target of a backup: its ID and name.
type CopyTarget struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// NormalizeCopyTargets trims, de-duplicates and checks a list of copy target IDs
// against primary (the primary target ID, empty when it is the default and not yet
// resolved): at most MaxCopyTargets, none empty, none equal to primary.
func NormalizeCopyTargets(ids []string, primary string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		switch {
		case id == "":
			return nil, fmt.Errorf("%w: a copy target ID is empty", ErrInvalidCopyTargets)
		case primary != "" && id == primary:
			return nil, fmt.Errorf("%w: the primary storage target cannot also be a copy target", ErrInvalidCopyTargets)
		case seen[id]:
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) > MaxCopyTargets {
		return nil, fmt.Errorf("%w: at most %d copy targets are allowed", ErrInvalidCopyTargets, MaxCopyTargets)
	}
	return out, nil
}

// PlanCopies sets one pending copy per target of copies on r, under r's storage key.
func (r *BackupRecord) PlanCopies(copies []CopyTarget, mode CopyMode) {
	if len(copies) == 0 {
		return
	}
	if mode == "" {
		mode = CopyAsync
	}
	r.CopyMode = mode
	r.Copies = make([]BackupCopy, 0, len(copies))
	for _, t := range copies {
		r.Copies = append(r.Copies, BackupCopy{TargetID: t.ID, TargetName: t.Name, StorageKey: r.StorageKey, Status: CopyPending})
	}
}

// Copy returns the copy of r on target targetID, or nil.
func (r *BackupRecord) Copy(targetID string) *BackupCopy {
	for i := range r.Copies {
		if r.Copies[i].TargetID == targetID {
			return &r.Copies[i]
		}
	}
	return nil
}

// DoneCopies counts the completed copies of r.
func (r *BackupRecord) DoneCopies() int {
	n := 0
	for i := range r.Copies {
		if r.Copies[i].Status == CopyDone {
			n++
		}
	}
	return n
}

// CopiesComplete reports whether every planned copy of r is done.
func (r *BackupRecord) CopiesComplete() bool {
	return r.DoneCopies() == len(r.Copies)
}

// HoldsTarget reports whether r keeps storage target id in use: as its primary
// target or as the target of a copy whose object may exist.
func (r *BackupRecord) HoldsTarget(id string) bool {
	if r.StorageTargetID == id {
		return true
	}
	for i := range r.Copies {
		if r.Copies[i].TargetID == id && r.Copies[i].MayExist() {
			return true
		}
	}
	return false
}
