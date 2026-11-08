package scheduler

import (
	"fmt"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// Rules named in KeepReason.Rule (besides ProtectedPinned and ProtectedLastVerified).
const (
	// KeepNewest is one of the max(retention_count, 1) newest backups of a database,
	// which no rule deletes.
	KeepNewest = "newest"
	// KeepDays is a backup within retention_days.
	KeepDays = "retention_days"
	// KeepCount is a backup within the retention_count newest, or younger than
	// MinCountPruneAge.
	KeepCount = "retention_count"
	// KeepDaily is the newest backup of a day within the last keep_daily days that
	// have one.
	KeepDaily = "keep_daily"
	// KeepWeekly is the newest backup of an ISO week within the last keep_weekly
	// weeks that have one.
	KeepWeekly = "keep_weekly"
	// KeepMonthly is the newest backup of a month within the last keep_monthly months
	// that have one.
	KeepMonthly = "keep_monthly"
	// KeepYearly is the newest backup of a year within the last keep_yearly years
	// that have one.
	KeepYearly = "keep_yearly"
)

// KeepReason is one rule that keeps a backup. Bucket names the day ("2026-11-08"),
// ISO week ("2026-W45"), month ("2026-11") or year ("2026") of a bucket rule, in the
// zone the buckets are cut in.
type KeepReason struct {
	// Rule is one of the Keep* and Protected* constants.
	Rule string `json:"rule"`
	// Bucket names the bucket a keep_* rule keeps the backup for.
	Bucket string `json:"bucket,omitempty"`
}

// String returns the rule with its bucket ("keep_monthly 2026-10").
func (k KeepReason) String() string {
	if k.Bucket == "" {
		return k.Rule
	}
	return k.Rule + " " + k.Bucket
}

// KeptBackup is a backup the retention policy keeps, and the rules that keep it.
type KeptBackup struct {
	// BackupID identifies the backup.
	BackupID string `json:"backup_id"`
	// Database is the backup's database.
	Database string `json:"database"`
	// Reasons lists every rule that keeps the backup (at least one).
	Reasons []KeepReason `json:"reasons"`
}

// JoinReasons renders reasons for a message ("keep_daily 2026-11-08, retention_days").
func JoinReasons(reasons []KeepReason) string {
	parts := make([]string, len(reasons))
	for i, r := range reasons {
		parts[i] = r.String()
	}
	return strings.Join(parts, ", ")
}

// bucketNames lists the bucket rules p turns on ("keep_daily/keep_monthly").
func bucketNames(p models.RetentionPolicy) string {
	var names []string
	for _, b := range []struct {
		name string
		n    int
	}{{KeepDaily, p.KeepDaily}, {KeepWeekly, p.KeepWeekly}, {KeepMonthly, p.KeepMonthly}, {KeepYearly, p.KeepYearly}} {
		if b.n > 0 {
			names = append(names, b.name)
		}
	}
	return strings.Join(names, "/")
}

// bucketKey names the bucket of rule that t (already in the bucket zone) falls in.
func bucketKey(rule string, t time.Time) string {
	switch rule {
	case KeepDaily:
		return t.Format("2006-01-02")
	case KeepWeekly:
		year, week := t.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", year, week)
	case KeepMonthly:
		return t.Format("2006-01")
	default:
		return t.Format("2006")
	}
}

// bucketKeeps returns, per backup ID, the bucket reasons that keep it. successful is
// sorted newest first (planDatabase sorts it): for every rule with N > 0 the first
// (newest) backup of each of the first N distinct buckets is kept. Buckets are cut
// in loc, so a backup at 23:30 UTC belongs to the next day east of Greenwich, and an
// ISO week belongs to the ISO year of its Thursday (week 53 and week 1 across New
// Year follow time.Time.ISOWeek).
func bucketKeeps(p models.RetentionPolicy, loc *time.Location, successful []*models.BackupRecord) map[string][]KeepReason {
	out := map[string][]KeepReason{}
	for _, rule := range []struct {
		name string
		n    int
	}{{KeepDaily, p.KeepDaily}, {KeepWeekly, p.KeepWeekly}, {KeepMonthly, p.KeepMonthly}, {KeepYearly, p.KeepYearly}} {
		if rule.n <= 0 {
			continue
		}
		seen := map[string]bool{}
		for _, rec := range successful {
			key := bucketKey(rule.name, rec.StartedAt.In(loc))
			if seen[key] {
				continue
			}
			if len(seen) >= rule.n {
				break
			}
			seen[key] = true
			out[rec.ID] = append(out[rec.ID], KeepReason{Rule: rule.name, Bucket: key})
		}
	}
	return out
}
