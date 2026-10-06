package notify

import (
	"fmt"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/redact"
)

// All human-readable notification text lives in this file so wording stays consistent
// across transports.

// maxErrorLength bounds the error excerpt embedded in rendered messages.
const maxErrorLength = 500

// subjectTemplates maps an event type to its icon and headline.
var subjectTemplates = map[events.EventType]struct{ icon, headline string }{
	events.BackupSucceeded:           {"✅", "Backup succeeded"},
	events.BackupFailed:              {"❌", "Backup failed"},
	events.BackupCancelled:           {"⏹️", "Backup cancelled"},
	events.BackupSkipped:             {"⏭️", "Backup skipped"},
	events.RestoreSucceeded:          {"✅", "Restore succeeded"},
	events.RestoreFailed:             {"❌", "Restore failed"},
	events.RestoreCancelled:          {"⏹️", "Restore cancelled"},
	events.NotificationTest:          {"🔔", "Test notification"},
	events.EncryptionOffAfterUpgrade: {"⚠️", "Backup encryption is off"},
	events.VerificationFailed:        {"❌", "Backup verification failed"},
	events.RestoreTestSucceeded:      {"✅", "Restore test passed"},
	events.RestoreTestFailed:         {"❌", "Restore test failed"},
	events.DriftDetected:             {"⚠️", "Storage drift detected"},
	events.RetentionDeleted:          {"🗑️", "Backup deleted by retention"},
	events.JobDatabasesAdded:         {"➕", "New databases added to a job"},
	events.MetadataBackupFailed:      {"❌", "Metadata backup failed"},
	events.RestoreVerificationFailed: {"⚠️", "Restore verification failed"},
	events.JobRPOMissed:              {"⏰", "Recovery point objective missed"},
	events.JobRPORecovered:           {"✅", "Recovery point objective met again"},
	events.SecurityDestructiveAction: {"🛡️", "Destructive action"},
	events.SecurityApprovalRequested: {"🛡️", "Approval requested"},
	events.SecurityKeyRotated:        {"🔑", "Key rotated"},
	events.PITRChainBroken:           {"🚨", "PITR oplog chain broken"},
	events.PITRDiverged:              {"🚨", "PITR oplog diverged"},
	events.PITRLagHigh:               {"⏰", "PITR collector lagging"},
	events.PITRLagRecovered:          {"✅", "PITR collector caught up"},
	events.PITRWindowLow:             {"⚠️", "PITR oplog headroom low"},
	events.PITRCollectorFailed:       {"❌", "PITR collector failing"},
	events.PITRCollectorRecovered:    {"✅", "PITR collector recovered"},
	events.BackupCopyFailed:          {"❌", "Backup copy failing"},
	events.BackupCopyRecovered:       {"✅", "Backup copy recovered"},
}

// Render turns an event into a short human-readable Message. The subject is a single
// line (all control characters collapsed), e.g.
//
//	❌ Backup failed: job nightly-shop (db shop)
//
// and the body is one line of the form
//
//	❌ Backup failed: job nightly-shop (db shop) — <error> — 2026-09-24T03:00Z
//
// followed by optional detail lines.
func Render(e events.Event) Message {
	tpl, ok := subjectTemplates[e.Type]
	if !ok {
		tpl = struct{ icon, headline string }{"ℹ️", "MongoRescue event " + string(e.Type)}
	}
	if e.Type == events.BackupFailed && e.Run != nil && e.Run.Status == "partial" {
		tpl = struct{ icon, headline string }{"⚠️", "Backup partially failed"}
	}

	var target string
	switch e.Type {
	case events.BackupSucceeded, events.BackupFailed, events.BackupCancelled, events.BackupSkipped:
		job := e.JobID
		if job == "" {
			job = "manual"
		}
		target = fmt.Sprintf("job %s (db %s)", job, e.Database)
		if r := e.Run; r != nil && r.Multi {
			target = fmt.Sprintf("job %s (%d of %d databases backed up)", job, r.Succeeded, r.Databases)
		}
	case events.JobDatabasesAdded:
		target = fmt.Sprintf("job %s now also backs up %d database(s)", e.JobID, len(e.Databases))
	case events.JobRPOMissed, events.JobRPORecovered:
		target = fmt.Sprintf("job %s (db %s)", e.JobID, e.Database)
	case events.RestoreSucceeded, events.RestoreFailed, events.RestoreCancelled, events.RestoreVerificationFailed:
		target = fmt.Sprintf("backup %s → db %s", e.BackupID, e.Database)
	case events.NotificationTest:
		target = "MongoRescue notification channel check"
	case events.EncryptionOffAfterUpgrade:
		target = "it was enabled in your previous configuration; backups since the upgrade are NOT encrypted. Turn it on in Settings → Encryption"
	case events.VerificationFailed, events.VerificationSucceeded:
		target = fmt.Sprintf("backup %s (db %s, %s check: %s)", e.BackupID, e.Database, e.Source, e.Verification)
	case events.RestoreTestSucceeded, events.RestoreTestFailed:
		target = fmt.Sprintf("job %s (db %s), backup %s", e.JobID, e.Database, e.BackupID)
	case events.DriftDetected:
		name := e.TargetName
		if name == "" {
			name = e.TargetID
		}
		target = fmt.Sprintf("storage target %s: %d orphan archive(s), %d missing archive(s)", name, e.Orphans, e.Missing)
	case events.RetentionDeleted:
		target = fmt.Sprintf("job %s (db %s), backup %s", e.JobID, e.Database, e.BackupID)
	case events.SecurityDestructiveAction:
		target = fmt.Sprintf("%s by %s", e.Detail, e.Actor)
	case events.SecurityKeyRotated:
		target = fmt.Sprintf("%s key rotated by %s: %s", e.Action, e.Actor, e.Detail)
	case events.SecurityApprovalRequested:
		target = fmt.Sprintf("%s requested by %s; another administrator must approve it (request %s)", e.Detail, e.Actor, e.ApprovalID)
	case events.PITRChainBroken, events.PITRDiverged, events.PITRLagHigh, events.PITRLagRecovered,
		events.PITRWindowLow, events.PITRCollectorFailed, events.PITRCollectorRecovered:
		target = fmt.Sprintf("stream %s (connection %s)", e.Stream, e.ConnectionID)
	case events.BackupCopyFailed, events.BackupCopyRecovered:
		name := e.TargetName
		if name == "" {
			name = e.TargetID
		}
		target = fmt.Sprintf("backup %s (db %s) to storage target %s", e.BackupID, e.Database, name)
	case events.MetadataBackupFailed:
		name := e.TargetName
		if name == "" {
			name = e.TargetID
		}
		if name == "" {
			name = "(default)"
		}
		target = fmt.Sprintf("snapshot of the MongoRescue database to storage target %s", name)
	}

	subject := singleLine(fmt.Sprintf("%s %s: %s", tpl.icon, tpl.headline, target))

	ts := e.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	stamp := ts.UTC().Format("2006-01-02T15:04Z")

	parts := []string{subject}
	if e.Error != "" {
		parts = append(parts, truncate(singleLine(redact.Text(e.Error)), maxErrorLength))
	}
	parts = append(parts, stamp)

	var body strings.Builder
	body.WriteString(strings.Join(parts, " — "))
	if e.Duration > 0 {
		fmt.Fprintf(&body, "\nDuration: %s", e.Duration.Round(time.Millisecond))
	}
	if e.SizeBytes > 0 {
		fmt.Fprintf(&body, "\nSize: %s", humanBytes(e.SizeBytes))
	}
	if e.Detail != "" {
		fmt.Fprintf(&body, "\nDetail: %s", truncate(singleLine(redact.Text(e.Detail)), maxErrorLength))
	}
	if r := e.Run; r != nil && r.Multi {
		if len(r.FailedDatabases) > 0 {
			fmt.Fprintf(&body, "\nFailed databases: %s", truncate(singleLine(strings.Join(r.FailedDatabases, ", ")), maxErrorLength))
		}
		if len(r.SkippedDatabases) > 0 {
			fmt.Fprintf(&body, "\nSkipped (already running): %s", truncate(singleLine(strings.Join(r.SkippedDatabases, ", ")), maxErrorLength))
		}
		if len(r.NewDatabases) > 0 {
			fmt.Fprintf(&body, "\n%d new database(s) not included: %s", len(r.NewDatabases),
				truncate(singleLine(strings.Join(r.NewDatabases, ", ")), maxErrorLength))
		}
	}
	if e.RunID != "" && e.Run != nil && e.Run.Multi {
		fmt.Fprintf(&body, "\nRun ID: %s", e.RunID)
	}
	if e.RestoreID != "" {
		fmt.Fprintf(&body, "\nRestore ID: %s", e.RestoreID)
	} else if e.BackupID != "" {
		fmt.Fprintf(&body, "\nBackup ID: %s", e.BackupID)
	}

	return Message{Subject: subject, Body: body.String(), Event: e}
}

// singleLine collapses every control character (including CR and LF) into a space.
func singleLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// truncate shortens s to at most n runes, appending an ellipsis when cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// humanBytes formats a byte count using binary units.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// markdownV2Special lists the characters Telegram requires to be escaped in MarkdownV2.
const markdownV2Special = "_*[]()~`>#+-=|{}.!\\"

// EscapeMarkdownV2 escapes s for Telegram's MarkdownV2 parse mode.
func EscapeMarkdownV2(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		if strings.ContainsRune(markdownV2Special, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// telegramText renders msg for Telegram. In MarkdownV2 mode the subject is bold and
// every dynamic fragment is escaped.
func telegramText(msg Message, parseMode string) string {
	if parseMode != ParseModeMarkdownV2 {
		return msg.Body
	}
	body := strings.TrimPrefix(msg.Body, msg.Subject)
	return "*" + EscapeMarkdownV2(msg.Subject) + "*" + EscapeMarkdownV2(body)
}

// smsText renders msg for SMS, bounded to Twilio's 1600-character limit.
func smsText(msg Message) string {
	return truncate(msg.Body, 1500)
}
