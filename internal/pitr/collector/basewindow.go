package collector

import (
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// baseDuration returns how long base backup b took: its recorded duration, else
// the time between its start and completion; 0 when unknown.
func baseDuration(b *models.BackupRecord) time.Duration {
	if b.DurationSeconds > 0 {
		return time.Duration(b.DurationSeconds * float64(time.Second))
	}
	if b.CompletedAt != nil && b.CompletedAt.After(b.StartedAt) {
		return b.CompletedAt.Sub(b.StartedAt)
	}
	return 0
}

// baseDurations returns the duration of the newest successful base of bases
// (newest first) and the longest of the newest pitr.BaseHistory of them; both are
// 0 without a successful base that has a known duration.
func baseDurations(bases []*models.BackupRecord) (last, expected time.Duration) {
	seen := 0
	for _, b := range bases {
		if b.Status != models.StatusCompleted {
			continue
		}
		d := baseDuration(b)
		if d <= 0 {
			continue
		}
		if seen == 0 {
			last = d
		}
		expected = max(expected, d)
		if seen++; seen == pitr.BaseHistory {
			break
		}
	}
	return last, expected
}
