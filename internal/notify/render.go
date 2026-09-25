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
	events.BackupSucceeded:  {"✅", "Backup succeeded"},
	events.BackupFailed:     {"❌", "Backup failed"},
	events.RestoreSucceeded: {"✅", "Restore succeeded"},
	events.RestoreFailed:    {"❌", "Restore failed"},
	events.NotificationTest: {"🔔", "Test notification"},
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

	var target string
	switch e.Type {
	case events.BackupSucceeded, events.BackupFailed:
		job := e.JobID
		if job == "" {
			job = "manual"
		}
		target = fmt.Sprintf("job %s (db %s)", job, e.Database)
	case events.RestoreSucceeded, events.RestoreFailed:
		target = fmt.Sprintf("backup %s → db %s", e.BackupID, e.Database)
	case events.NotificationTest:
		target = "MongoRescue notification channel check"
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
