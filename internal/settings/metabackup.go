package settings

import (
	"fmt"
	"strings"
	"time"
)

// Limits of the metadata backup settings.
const (
	// MinMetadataBackupInterval is the shortest interval between scheduled metadata
	// snapshots.
	MinMetadataBackupInterval = time.Hour
	// MaxMetadataBackupInterval is the longest interval between scheduled metadata
	// snapshots.
	MaxMetadataBackupInterval = 30 * 24 * time.Hour
	// MaxMetadataBackupRetention caps MetadataBackup.RetentionCount.
	MaxMetadataBackupRetention = 1000
	// maxTargetIDLength bounds MetadataBackup.TargetID.
	maxTargetIDLength = 128
)

// MetadataBackup configures the scheduled self-backup of MongoRescue's own metadata
// database (jobs, backup records, users, settings, storage targets): an online
// snapshot is written to a storage target under the _mongorescue/metadata/ prefix,
// encrypted with the backup encryption settings when encryption is on.
type MetadataBackup struct {
	// Enabled turns the scheduled snapshot on (off by default).
	Enabled bool `json:"enabled"`
	// Interval is the time between scheduled snapshots (1h to 720h).
	Interval Duration `json:"interval"`
	// TargetID is the storage target snapshots are written to ("" = the default
	// target).
	TargetID string `json:"target_id"`
	// RetentionCount is how many snapshots are kept on the target (the newest).
	RetentionCount int `json:"retention_count"`
}

// defaultMetadataBackup returns the metadata backup settings of a fresh
// installation: off, daily, on the default target, 14 snapshots kept.
func defaultMetadataBackup() MetadataBackup {
	return MetadataBackup{Interval: Duration(24 * time.Hour), RetentionCount: 14}
}

// MetadataBackupPatch updates MetadataBackup; see MetadataBackup for the fields.
type MetadataBackupPatch struct {
	Enabled        *bool     `json:"enabled,omitempty"`
	Interval       *Duration `json:"interval,omitempty"`
	TargetID       *string   `json:"target_id,omitempty"`
	RetentionCount *int      `json:"retention_count,omitempty"`
}

// apply sets the fields of p that are present on m. A nil p changes nothing.
func (p *MetadataBackupPatch) apply(m *MetadataBackup) {
	if p == nil {
		return
	}
	setIf(&m.Enabled, p.Enabled)
	setIf(&m.Interval, p.Interval)
	if p.TargetID != nil {
		m.TargetID = strings.TrimSpace(*p.TargetID)
	}
	setIf(&m.RetentionCount, p.RetentionCount)
}

// validateMetadataBackup checks m.
func validateMetadataBackup(m *MetadataBackup) error {
	switch {
	case m.Interval.Std() < MinMetadataBackupInterval || m.Interval.Std() > MaxMetadataBackupInterval:
		return fmt.Errorf("%w: metadata_backup.interval must be between 1h and 720h", ErrInvalid)
	case m.RetentionCount < 1 || m.RetentionCount > MaxMetadataBackupRetention:
		return fmt.Errorf("%w: metadata_backup.retention_count must be between 1 and %d", ErrInvalid, MaxMetadataBackupRetention)
	case len(m.TargetID) > maxTargetIDLength || strings.ContainsFunc(m.TargetID, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return fmt.Errorf("%w: metadata_backup.target_id is not a storage target ID", ErrInvalid)
	}
	return nil
}
