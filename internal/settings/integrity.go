package settings

import (
	"fmt"
	"time"
)

// SweepSchedule says how often the integrity sweep re-verifies every stored archive.
type SweepSchedule string

// Sweep schedules.
const (
	// SweepOff disables the scheduled sweep (it can still be started by hand).
	SweepOff SweepSchedule = "off"
	// SweepDaily sweeps once a day.
	SweepDaily SweepSchedule = "daily"
	// SweepWeekly sweeps once a week.
	SweepWeekly SweepSchedule = "weekly"
	// SweepMonthly sweeps once every 30 days.
	SweepMonthly SweepSchedule = "monthly"
)

// Interval returns the time between scheduled sweeps, or 0 for SweepOff.
func (s SweepSchedule) Interval() time.Duration {
	switch s {
	case SweepDaily:
		return 24 * time.Hour
	case SweepWeekly:
		return 7 * 24 * time.Hour
	case SweepMonthly:
		return 30 * 24 * time.Hour
	default:
		return 0
	}
}

// Valid reports whether s is one of the defined schedules.
func (s SweepSchedule) Valid() bool {
	return s == SweepOff || s.Interval() > 0
}

// maxSweepBandwidth caps Integrity.SweepBandwidthLimit (MiB/s).
const maxSweepBandwidth = 100000

// Integrity configures archive verification, the integrity sweep and storage scans.
type Integrity struct {
	// VerifyAfterBackup re-reads every archive after its upload and fails a backup
	// whose stored bytes differ from what was written. Jobs may override it.
	VerifyAfterBackup bool `json:"verify_after_backup"`
	// VerifyDecrypt also decrypts encrypted archives to their end when verifying
	// (needs the identity or passphrase under Encryption), proving that the keys
	// still open them.
	VerifyDecrypt bool `json:"verify_decrypt"`
	// SweepSchedule re-verifies every completed backup, least recently verified
	// first, one at a time.
	SweepSchedule SweepSchedule `json:"sweep_schedule"`
	// SweepBandwidthLimit caps the sweep's read rate in MiB/s (0 = unlimited).
	SweepBandwidthLimit int `json:"sweep_bandwidth_limit"`
	// StorageScan compares every storage target with the backup records once a week
	// (orphan archives and missing ones). Nothing is ever deleted automatically.
	StorageScan bool `json:"storage_scan"`
}

// defaultIntegrity returns the integrity settings of a fresh installation:
// post-backup verification and weekly storage scans on, the sweep off (it reads
// every archive, which costs egress on cloud storage).
func defaultIntegrity() Integrity {
	return Integrity{VerifyAfterBackup: true, SweepSchedule: SweepOff, StorageScan: true}
}

// SweepBytesPerSecond returns the sweep's read limit in bytes per second (0 =
// unlimited).
func (i Integrity) SweepBytesPerSecond() int64 {
	return int64(i.SweepBandwidthLimit) << 20
}

// IntegrityPatch updates Integrity; see Integrity for the fields.
type IntegrityPatch struct {
	VerifyAfterBackup   *bool          `json:"verify_after_backup,omitempty"`
	VerifyDecrypt       *bool          `json:"verify_decrypt,omitempty"`
	SweepSchedule       *SweepSchedule `json:"sweep_schedule,omitempty"`
	SweepBandwidthLimit *int           `json:"sweep_bandwidth_limit,omitempty"`
	StorageScan         *bool          `json:"storage_scan,omitempty"`
}

// apply sets the fields of p that are present on i. A nil p changes nothing.
func (p *IntegrityPatch) apply(i *Integrity) {
	if p == nil {
		return
	}
	setIf(&i.VerifyAfterBackup, p.VerifyAfterBackup)
	setIf(&i.VerifyDecrypt, p.VerifyDecrypt)
	setIf(&i.SweepSchedule, p.SweepSchedule)
	setIf(&i.SweepBandwidthLimit, p.SweepBandwidthLimit)
	setIf(&i.StorageScan, p.StorageScan)
}

// validateIntegrity checks i.
func validateIntegrity(i *Integrity) error {
	switch {
	case !i.SweepSchedule.Valid():
		return fmt.Errorf("%w: integrity.sweep_schedule must be off, daily, weekly or monthly", ErrInvalid)
	case i.SweepBandwidthLimit < 0 || i.SweepBandwidthLimit > maxSweepBandwidth:
		return fmt.Errorf("%w: integrity.sweep_bandwidth_limit must be between 0 (unlimited) and %d MiB/s", ErrInvalid, maxSweepBandwidth)
	}
	return nil
}
