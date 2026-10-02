package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// WarningRecoveryKit identifies the reminder to download a recovery kit (see
// internal/recoverykit). It is active while no kit was downloaded since the material
// a kit carries last changed (secret.key, the encryption keys, the storage targets),
// and until it is dismissed for that state.
const WarningRecoveryKit = "recovery_kit_missing"

// WarningMetadataBackupUnencrypted identifies the warning raised while the scheduled
// metadata backup is on but backup encryption is off: snapshots are then uploaded
// without age encryption (their stored credentials stay sealed with secret.key).
// It cannot be dismissed; turning encryption on, or the metadata backup off,
// resolves it.
const WarningMetadataBackupUnencrypted = "metadata_backup_unencrypted"

// recoveryKitMessage is the text of the WarningRecoveryKit warning.
const recoveryKitMessage = "No recovery kit has been downloaded since secret.key, the encryption keys or the storage targets " +
	"last changed. Without secret.key a copy of the metadata database cannot be opened. Download one in Settings → Recovery."

// metadataUnencryptedMessage is the text of the WarningMetadataBackupUnencrypted
// warning.
const metadataUnencryptedMessage = "Metadata backups are on but backup encryption is off, so snapshots are uploaded " +
	"unencrypted. Turn encryption on in Settings → Encryption."

// recoveryKitKey stores the recovery kit state next to the import markers, so it
// survives restarts.
const recoveryKitKey = markerPrefix + "warning." + WarningRecoveryKit

// recoveryKitState is the stored state of the recovery kit reminder.
type recoveryKitState struct {
	// Fingerprint identifies the material of the last downloaded kit.
	Fingerprint string `json:"fingerprint,omitempty"`
	// DownloadedAt is when the last kit was downloaded.
	DownloadedAt *time.Time `json:"downloaded_at,omitempty"`
	// Dismissed is the fingerprint the reminder was dismissed for.
	Dismissed string `json:"dismissed,omitempty"`
}

// RecoveryKitStatus describes the last recovery kit download.
type RecoveryKitStatus struct {
	// DownloadedAt is when the last kit was downloaded (nil when never).
	DownloadedAt *time.Time `json:"downloaded_at,omitempty"`
	// UpToDate reports whether the last kit still carries the current secret.key,
	// encryption keys and storage targets.
	UpToDate bool `json:"up_to_date"`
}

// NoteRecoveryKitFingerprint records the fingerprint of the material a recovery
// kit would carry now (computed by internal/recoverykit). WarningRecoveryKit is only
// reported once a fingerprint has been noted.
func (s *Service) NoteRecoveryKitFingerprint(fingerprint string) {
	s.mu.Lock()
	s.kitCurrent = fingerprint
	s.mu.Unlock()
}

// MarkRecoveryKitDownloaded records that a kit with fingerprint was downloaded at,
// which resolves WarningRecoveryKit until the fingerprint changes.
func (s *Service) MarkRecoveryKitDownloaded(ctx context.Context, fingerprint string, at time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	at = at.UTC()
	s.mu.RLock()
	next := s.kit
	s.mu.RUnlock()
	next.Fingerprint, next.DownloadedAt = fingerprint, &at
	if err := s.saveRecoveryKitState(ctx, next); err != nil {
		return err
	}
	s.mu.Lock()
	s.kitCurrent = fingerprint
	s.mu.Unlock()
	return nil
}

// RecoveryKitStatus returns when the last kit was downloaded and whether it is
// still current.
func (s *Service) RecoveryKitStatus() RecoveryKitStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := RecoveryKitStatus{UpToDate: s.kit.Fingerprint != "" && s.kit.Fingerprint == s.kitCurrent}
	if s.kit.DownloadedAt != nil {
		at := *s.kit.DownloadedAt
		out.DownloadedAt = &at
	}
	return out
}

// recoveryKitWarningActive reports whether WarningRecoveryKit is shown. Caller holds
// mu (read).
func (s *Service) recoveryKitWarningActive() bool {
	cur := s.kitCurrent
	return cur != "" && cur != s.kit.Fingerprint && cur != s.kit.Dismissed
}

// dismissRecoveryKit dismisses WarningRecoveryKit for the current fingerprint: it
// comes back when the material changes. Caller holds writeMu.
func (s *Service) dismissRecoveryKit(ctx context.Context) error {
	s.mu.RLock()
	next, cur := s.kit, s.kitCurrent
	s.mu.RUnlock()
	if cur == "" {
		return nil
	}
	next.Dismissed = cur
	return s.saveRecoveryKitState(ctx, next)
}

// saveRecoveryKitState persists st and makes it current. Caller holds writeMu.
func (s *Service) saveRecoveryKitState(ctx context.Context, st recoveryKitState) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := s.repo.SaveSettings(ctx, map[string]string{recoveryKitKey: string(raw)}); err != nil {
		return fmt.Errorf("settings: save recovery kit state: %w", err)
	}
	s.mu.Lock()
	s.kit = st
	s.stored[recoveryKitKey] = true
	s.mu.Unlock()
	return nil
}

// loadRecoveryKitState decodes the stored recovery kit state (zero when none).
func loadRecoveryKitState(values map[string]string) recoveryKitState {
	var st recoveryKitState
	if raw, ok := values[recoveryKitKey]; ok {
		_ = json.Unmarshal([]byte(raw), &st)
	}
	return st
}
