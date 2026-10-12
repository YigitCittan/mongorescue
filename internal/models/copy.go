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

// ErrUnlockedCopyTarget is returned when a job that requires locked copies names a
// copy target without S3 Object Lock. It wraps ErrInvalidCopyTargets.
var ErrUnlockedCopyTarget = fmt.Errorf("%w: the job requires locked copies", ErrInvalidCopyTargets)

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
	// SHA256 is the checksum the copy was checked against when it was made (the
	// backup's SHA-256 at that time). A copy whose SHA256 differs from its
	// backup's current one (the archive was re-encrypted since) is never read.
	SHA256 string `json:"sha256,omitempty"`
	// VersionID is the S3 version of the copy on a versioned target.
	VersionID string `json:"version_id,omitempty"`
	// ObjectLockMode and RetainUntil are the copy target's own Object Lock.
	ObjectLockMode ObjectLockMode `json:"object_lock_mode,omitempty"`
	RetainUntil    *time.Time     `json:"retain_until,omitempty"`
	// Error is the redacted reason of the last failed attempt.
	Error string `json:"error,omitempty"`
	// Written reports that a failed attempt may have left an object on the copy
	// target (it stored one without reading the whole archive): the copy keeps
	// its target in use and the purge deletes it.
	Written bool `json:"written,omitempty"`
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

// MayExist reports whether the object of c exists on its target or is about to be
// written: a done or pending copy, or a copy that is not purged and is still under
// its Object Lock or left an object behind (Written). Any other failed copy wrote
// no object (a mismatch fails the upload before it completes). The store's in-use
// check of storage targets uses the same rule.
func (c *BackupCopy) MayExist() bool {
	switch c.Status {
	case CopyDone, CopyPending:
		return true
	case CopyPurged:
		return false
	default:
		return c.RetainUntil != nil || c.Written
	}
}

// RecordFailure records a failed attempt with the redacted reason msg; obj is
// what the copy target stored anyway (nil when it stored nothing).
func (c *BackupCopy) RecordFailure(msg string, obj *StorageObject) {
	c.Status, c.SHA256OK, c.Error = CopyFailed, false, msg
	if obj != nil {
		c.Written = true
		c.SetStorageObject(obj)
	}
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

// AtCopy returns a shallow copy of r that reads its archive from copy c: the storage
// target, key, version, Object Lock and verification are c's. Restores and
// verifications of a copy use it.
func (r *BackupRecord) AtCopy(c *BackupCopy) *BackupRecord {
	v := *r
	v.StorageTargetID, v.StorageTargetName, v.StorageKey, v.StorageVersionID = c.TargetID, c.TargetName, c.StorageKey, c.VersionID
	v.RetainUntil, v.ObjectLockMode = c.RetainUntil, c.ObjectLockMode
	v.VerifiedAt, v.Verification, v.VerificationError = c.VerifiedAt, c.Verification, c.VerificationError
	if v.Status == StatusMissing {
		// The primary is missing, the copy is not.
		v.Status, v.MissingSince = StatusCompleted, nil
	}
	return &v
}

// CopyUsable reports whether copy c of r can be read in place of r's archive: a
// healthy copy (see BackupCopy.Healthy) made from r's current archive, so its
// checksum is r's.
func (r *BackupRecord) CopyUsable(c *BackupCopy) bool {
	return c != nil && c.Healthy() && r.SHA256 != "" && strings.EqualFold(c.SHA256, r.SHA256)
}

// RequeueCopies points every copy of r that is not purged at key (r's new
// archive after a re-encryption) and queues it again: the copy queue copies the
// new archive and checks it against r's new checksum. Until then no copy is
// usable.
func (r *BackupRecord) RequeueCopies(key string) {
	for i := range r.Copies {
		c := &r.Copies[i]
		if c.Status == CopyPurged {
			continue
		}
		*c = BackupCopy{TargetID: c.TargetID, TargetName: c.TargetName, StorageKey: key, Status: CopyPending}
	}
}

// ErrNotCopied is the error recorded on the pending copies of a backup that did not
// complete.
const ErrNotCopied = "not copied: the backup did not complete"

// AbandonCopies settles the copies of r, a backup that failed, was cancelled or
// was interrupted: a pending copy was never made and is marked failed (it then
// keeps no target in use), and a copy that was made (a synchronous copy finished
// before the backup failed) marks r ArchiveCleanupPending, so the purge deletes it.
func (r *BackupRecord) AbandonCopies() {
	for i := range r.Copies {
		c := &r.Copies[i]
		switch {
		case c.Status == CopyPending:
			c.Status, c.Error, c.NextAttemptAt = CopyFailed, ErrNotCopied, nil
		case c.MayExist():
			r.ArchiveCleanupPending = true
		}
	}
}

// RetryCopies queues every failed copy of r again with its attempts reset, and
// reports whether there was one.
func (r *BackupRecord) RetryCopies() bool {
	queued := false
	for i := range r.Copies {
		c := &r.Copies[i]
		if c.Status != CopyFailed {
			continue
		}
		c.Status, c.Attempts, c.NextAttemptAt = CopyPending, 0, nil
		queued = true
	}
	return queued
}

// Exhausted reports whether c failed and the copy queue will not try it again.
func (c *BackupCopy) Exhausted() bool {
	return c.Status == CopyFailed && c.NextAttemptAt == nil
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
