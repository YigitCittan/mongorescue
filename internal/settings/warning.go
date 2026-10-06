package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// WarningEncryptionOff identifies the warning raised when the previous (deprecated)
// configuration had backup encryption enabled but it is off now: releases up to
// v0.7.1 imported the encryption settings without the switch, so backups taken since
// the upgrade are not encrypted.
const WarningEncryptionOff = "encryption_off_after_upgrade"

// Warning is a persistent notice for the operator, shown by the dashboard until it
// is resolved or dismissed.
type Warning struct {
	// ID identifies the warning (e.g. WarningEncryptionOff).
	ID string `json:"id"`
	// Message is the English text; the dashboard shows a translation keyed by ID.
	Message string `json:"message"`
	// Setting names the settings section that resolves the warning.
	Setting string `json:"setting"`
	// RestoreID and Databases name the restore and the databases of a
	// WarningPostRestoreClonesKept.
	RestoreID string   `json:"restore_id,omitempty"`
	Databases []string `json:"databases,omitempty"`
}

// WarningPostRestoreClonesKept identifies the warning about clones a restore kept
// after a failed post-restore command: they still hold data the commands should
// have erased. It is raised per restore by the server from the restore records
// (not stored here) until the clones are dropped.
const WarningPostRestoreClonesKept = "post_restore_clones_kept"

// PostRestoreClonesKept returns the WarningPostRestoreClonesKept warning of
// restore restoreID, which kept databases.
func PostRestoreClonesKept(restoreID string, databases []string) Warning {
	return Warning{ID: WarningPostRestoreClonesKept, RestoreID: restoreID, Databases: databases,
		Message: fmt.Sprintf("Restore %s kept the clone(s) %s after a failed post-restore command; they still hold data the commands should have erased (such as erasures). Drop them when done.",
			restoreID, strings.Join(databases, ", "))}
}

// encryptionOffMessage is the text of the WarningEncryptionOff warning.
const encryptionOffMessage = "Encryption was enabled in your previous configuration but is currently off; " +
	"backups since the upgrade are NOT encrypted. Turn it on in Settings → Encryption."

// warningKey stores the state of the WarningEncryptionOff warning next to the import
// markers, so it survives restarts.
const warningKey = markerPrefix + "warning." + WarningEncryptionOff

// warningNotifiedKey records when the WarningEncryptionOff alert reached the
// notification service.
const warningNotifiedKey = warningKey + ".notified"

// States of the WarningEncryptionOff warning.
const (
	warningActive    = "active"    // shown until encryption is on or it is dismissed
	warningDismissed = "dismissed" // the admin dismissed it or turned encryption off
	warningResolved  = "resolved"  // encryption was turned on
)

// Warnings returns the active warnings. WarningEncryptionOff is only reported while
// encryption is still off; WarningMetadataBackupUnencrypted while metadata backups
// are on and encryption is off; WarningRecoveryKit while the last recovery kit is
// missing or outdated (see recoverykit.go).
func (s *Service) Warnings() []Warning {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []Warning{}
	if s.warnState == warningActive && !s.cur.Encryption.Enabled {
		out = append(out, Warning{ID: WarningEncryptionOff, Message: encryptionOffMessage, Setting: "encryption"})
	}
	if s.cur.MetadataBackup.Enabled && !s.cur.Encryption.Enabled {
		out = append(out, Warning{ID: WarningMetadataBackupUnencrypted, Message: metadataUnencryptedMessage, Setting: "encryption"})
	}
	if s.recoveryKitWarningActive() {
		out = append(out, Warning{ID: WarningRecoveryKit, Message: recoveryKitMessage, Setting: "recovery"})
	}
	if s.oidcRoleKept == warningActive {
		out = append(out, Warning{ID: WarningOIDCRoleKept, Message: oidcRoleKeptMessage, Setting: "sso"})
	}
	if s.previousKey {
		out = append(out, Warning{ID: WarningPreviousKey, Message: previousKeyMessage, Setting: "security"})
	}
	return out
}

// WarningOIDCRoleKept identifies the warning raised when a single sign-on would have
// demoted the last administrator, or an administrator while the two-person rule is
// on: the stored role was kept and the sign-in went on.
const WarningOIDCRoleKept = "oidc_role_kept"

// oidcRoleKeptMessage is the text of the WarningOIDCRoleKept warning.
const oidcRoleKeptMessage = "A single sign-on would have demoted the last administrator, or an administrator while the two-person rule is on, " +
	"so the stored admin role was kept (a demotion under the two-person rule waits in Approvals). " +
	"Map an identity provider group to admin or keep a second administrator. See Settings → Single sign-on."

// oidcRoleKeptKey stores the state of the WarningOIDCRoleKept warning.
const oidcRoleKeptKey = markerPrefix + "warning." + WarningOIDCRoleKept

// RaiseOIDCRoleKeptWarning raises WarningOIDCRoleKept (again, also after it was
// dismissed: every kept role is a new occurrence).
func (s *Service) RaiseOIDCRoleKeptWarning(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.setOIDCRoleKept(ctx, warningActive)
}

// setOIDCRoleKept persists the state of WarningOIDCRoleKept. Caller holds writeMu.
func (s *Service) setOIDCRoleKept(ctx context.Context, state string) error {
	s.mu.RLock()
	same := s.oidcRoleKept == state
	s.mu.RUnlock()
	if same {
		return nil
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := s.repo.SaveSettings(ctx, map[string]string{oidcRoleKeptKey: string(raw)}); err != nil {
		return fmt.Errorf("settings: save warning state: %w", err)
	}
	s.mu.Lock()
	s.oidcRoleKept = state
	s.stored[oidcRoleKeptKey] = true
	s.mu.Unlock()
	return nil
}

// loadState decodes a stored warning state ("" when never raised).
func loadState(values map[string]string, key string) string {
	var state string
	if raw, ok := values[key]; ok {
		_ = json.Unmarshal([]byte(raw), &state)
	}
	return state
}

// RaiseEncryptionOffWarning raises WarningEncryptionOff when encryption is off, once:
// it reports true only the first time, and never after the warning was resolved,
// dismissed or encryption was explicitly turned off. It never enables encryption,
// since the imported recipients may no longer be right.
func (s *Service) RaiseEncryptionOffWarning(ctx context.Context) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.RLock()
	state, enabled := s.warnState, s.cur.Encryption.Enabled
	s.mu.RUnlock()
	if state != "" || enabled {
		return false, nil
	}
	if err := s.setWarningState(ctx, warningActive); err != nil {
		return false, err
	}
	return true, nil
}

// EncryptionOffAlertPending reports whether WarningEncryptionOff is active but its
// alert has not been handed to the notification service yet (see
// MarkEncryptionOffAlerted), for example because the process stopped before the
// event bus ran.
func (s *Service) EncryptionOffAlertPending() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.warnState == warningActive && !s.warnNotified && !s.cur.Encryption.Enabled
}

// MarkEncryptionOffAlerted records that the WarningEncryptionOff alert was delivered
// to the notification service, so later starts do not send it again. It is
// idempotent.
func (s *Service) MarkEncryptionOffAlerted(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.RLock()
	done := s.warnNotified
	s.mu.RUnlock()
	if done {
		return nil
	}
	stamp, err := json.Marshal(s.now().UTC())
	if err != nil {
		return err
	}
	if err := s.repo.SaveSettings(ctx, map[string]string{warningNotifiedKey: string(stamp)}); err != nil {
		return fmt.Errorf("settings: save warning alert: %w", err)
	}
	s.mu.Lock()
	s.warnNotified = true
	s.stored[warningNotifiedKey] = true
	s.mu.Unlock()
	return nil
}

// DismissWarning dismisses the warning id: WarningEncryptionOff for good,
// WarningRecoveryKit until the material a kit carries changes. Unknown IDs, and
// warnings that cannot be dismissed, return ErrInvalid.
func (s *Service) DismissWarning(ctx context.Context, id string) error {
	switch id {
	case WarningEncryptionOff:
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		return s.setWarningState(ctx, warningDismissed)
	case WarningRecoveryKit:
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		return s.dismissRecoveryKit(ctx)
	case WarningOIDCRoleKept:
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		return s.setOIDCRoleKept(ctx, warningDismissed)
	}
	return fmt.Errorf("%w: unknown warning %q", ErrInvalid, id)
}

// noteEncryptionChange records how an update affects WarningEncryptionOff: turning
// encryption on resolves it, and explicitly turning it off counts as the admin's
// decision, so the warning is never raised (again). Caller holds writeMu.
func (s *Service) noteEncryptionChange(ctx context.Context, p Patch, next Settings) error {
	s.mu.RLock()
	state := s.warnState
	s.mu.RUnlock()
	switch {
	case next.Encryption.Enabled && state != warningResolved:
		return s.setWarningState(ctx, warningResolved)
	case p.Encryption != nil && p.Encryption.Enabled != nil && !*p.Encryption.Enabled && state != warningDismissed && state != warningResolved:
		return s.setWarningState(ctx, warningDismissed)
	}
	return nil
}

// setWarningState persists state. Caller holds writeMu.
func (s *Service) setWarningState(ctx context.Context, state string) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := s.repo.SaveSettings(ctx, map[string]string{warningKey: string(raw)}); err != nil {
		return fmt.Errorf("settings: save warning state: %w", err)
	}
	s.mu.Lock()
	s.warnState = state
	s.stored[warningKey] = true
	s.mu.Unlock()
	return nil
}

// loadWarningState decodes the stored warning state (empty when never raised).
func loadWarningState(values map[string]string) string {
	var state string
	if raw, ok := values[warningKey]; ok {
		_ = json.Unmarshal([]byte(raw), &state)
	}
	return state
}
