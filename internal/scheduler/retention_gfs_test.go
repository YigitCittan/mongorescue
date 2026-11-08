package scheduler

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func gfsRec(id, database string, at time.Time) *models.BackupRecord {
	return &models.BackupRecord{ID: id, JobID: "job_gfs", Database: database, Status: models.StatusCompleted, StartedAt: at.UTC()}
}

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("time zone data for %s unavailable: %v", name, err)
	}
	return loc
}

// keptReasons maps every kept backup of plan to its reasons rendered as strings.
func keptReasons(plan RetentionPlan) map[string][]string {
	out := map[string][]string{}
	for _, k := range plan.Kept {
		for _, r := range k.Reasons {
			out[k.BackupID] = append(out[k.BackupID], r.String())
		}
	}
	return out
}

func deletedIDs(plan RetentionPlan) []string {
	ids := []string{}
	for _, d := range plan.Delete {
		ids = append(ids, d.Backup.ID)
	}
	slices.Sort(ids)
	return ids
}

// TestPlanRetentionPolicyBuckets proves the GFS buckets across month and year
// boundaries, ISO week 53, DST transitions in a non-UTC zone and a leap day: which
// backups stay, the bucket that keeps each, and which are deleted.
func TestPlanRetentionPolicyBuckets(t *testing.T) {
	ny := mustZone(t, "America/New_York")
	ist := mustZone(t, "Europe/Istanbul")
	utc := func(y int, m time.Month, d, h, mi int) time.Time { return time.Date(y, m, d, h, mi, 0, 0, time.UTC) }

	tests := []struct {
		name   string
		now    time.Time
		policy models.RetentionPolicy
		loc    *time.Location
		recs   []*models.BackupRecord
		// keep maps the IDs that stay to a reason each must include.
		keep map[string]string
		del  []string
	}{
		{
			name: "month boundary: the newest of January and of February",
			now:  utc(2026, 2, 4, 12, 0), policy: models.RetentionPolicy{KeepMonthly: 2},
			recs: []*models.BackupRecord{
				gfsRec("jan28", "db", utc(2026, 1, 28, 2, 0)), gfsRec("jan31", "db", utc(2026, 1, 31, 23, 59)),
				gfsRec("feb01", "db", utc(2026, 2, 1, 0, 0)), gfsRec("feb03", "db", utc(2026, 2, 3, 2, 0)),
			},
			keep: map[string]string{"jan31": "keep_monthly 2026-01", "feb03": "keep_monthly 2026-02"},
			del:  []string{"feb01", "jan28"},
		},
		{
			name: "year boundary: 23:59 on 31 December and 00:00 on 1 January are different years",
			now:  utc(2026, 1, 5, 12, 0), policy: models.RetentionPolicy{KeepYearly: 3},
			recs: []*models.BackupRecord{
				gfsRec("y24a", "db", utc(2024, 6, 15, 2, 0)), gfsRec("y24b", "db", utc(2024, 12, 31, 2, 0)),
				gfsRec("y25a", "db", utc(2025, 3, 1, 2, 0)), gfsRec("y25b", "db", utc(2025, 12, 31, 23, 59)),
				gfsRec("y26", "db", utc(2026, 1, 1, 0, 0)),
			},
			keep: map[string]string{"y26": "keep_yearly 2026", "y25b": "keep_yearly 2025", "y24b": "keep_yearly 2024"},
			del:  []string{"y24a", "y25a"},
		},
		{
			name: "the oldest bucket beyond N is dropped",
			now:  utc(2026, 1, 5, 12, 0), policy: models.RetentionPolicy{KeepYearly: 2},
			recs: []*models.BackupRecord{
				gfsRec("y24", "db", utc(2024, 12, 31, 2, 0)), gfsRec("y25", "db", utc(2025, 12, 31, 2, 0)), gfsRec("y26", "db", utc(2026, 1, 1, 2, 0)),
			},
			keep: map[string]string{"y26": "keep_yearly 2026", "y25": "keep_yearly 2025"},
			del:  []string{"y24"},
		},
		{
			name: "ISO week 53: 31 Dec 2020 and 1 Jan 2021 are week 53 of 2020, 4 Jan 2021 is week 1 of 2021",
			now:  utc(2021, 1, 6, 12, 0), policy: models.RetentionPolicy{KeepWeekly: 2},
			recs: []*models.BackupRecord{
				gfsRec("dec31", "db", utc(2020, 12, 31, 2, 0)), gfsRec("jan01", "db", utc(2021, 1, 1, 2, 0)),
				gfsRec("jan04", "db", utc(2021, 1, 4, 2, 0)), gfsRec("jan05", "db", utc(2021, 1, 5, 2, 0)),
			},
			keep: map[string]string{"jan05": "keep_weekly 2021-W01", "jan01": "keep_weekly 2020-W53"},
			del:  []string{"dec31", "jan04"},
		},
		{
			name: "ISO week 53 of 2026 holds 28 Dec to 3 Jan 2027",
			now:  utc(2027, 1, 4, 12, 0), policy: models.RetentionPolicy{KeepWeekly: 1},
			recs: []*models.BackupRecord{
				gfsRec("dec28", "db", utc(2026, 12, 28, 2, 0)), gfsRec("jan03", "db", utc(2027, 1, 3, 2, 0)),
			},
			keep: map[string]string{"jan03": "keep_weekly 2026-W53"},
			del:  []string{"dec28"},
		},
		{
			name: "a zone east of UTC moves a late UTC backup into the next day",
			now:  utc(2026, 5, 3, 12, 0), policy: models.RetentionPolicy{KeepDaily: 2}, loc: ist,
			recs: []*models.BackupRecord{
				// 22:30 UTC on 1 May is 01:30 on 2 May in Istanbul (UTC+3).
				gfsRec("may1_late", "db", utc(2026, 5, 1, 22, 30)), gfsRec("may2_noon", "db", utc(2026, 5, 2, 9, 0)),
				gfsRec("may2_night", "db", utc(2026, 5, 2, 20, 0)),
			},
			// Istanbul: 20:00 UTC on 2 May is 23:00 on 2 May; 09:00 UTC is 12:00 on 2 May;
			// 22:30 UTC on 1 May is 01:30 on 2 May: all one day, the newest wins, and
			// the second day bucket does not exist.
			keep: map[string]string{"may2_night": "keep_daily 2026-05-02"},
			del:  []string{"may1_late", "may2_noon"},
		},
		{
			name: "DST end in New York: the 25-hour day is one bucket, and 23:30 EST still belongs to it",
			now:  utc(2026, 11, 3, 12, 0), policy: models.RetentionPolicy{KeepDaily: 2}, loc: ny,
			recs: []*models.BackupRecord{
				gfsRec("edt0030", "db", utc(2026, 11, 1, 4, 30)), // 00:30 EDT, 1 Nov
				gfsRec("est0130", "db", utc(2026, 11, 1, 6, 30)), // 01:30 EST (second pass), 1 Nov
				gfsRec("est2330", "db", utc(2026, 11, 2, 4, 30)), // 23:30 EST, 1 Nov
				gfsRec("nov2", "db", utc(2026, 11, 2, 5, 30)),    // 00:30 EST, 2 Nov
			},
			keep: map[string]string{"nov2": "keep_daily 2026-11-02", "est2330": "keep_daily 2026-11-01"},
			del:  []string{"edt0030", "est0130"},
		},
		{
			name: "the same backups cut in UTC fall into different days",
			now:  utc(2026, 11, 3, 12, 0), policy: models.RetentionPolicy{KeepDaily: 2},
			recs: []*models.BackupRecord{
				gfsRec("edt0030", "db", utc(2026, 11, 1, 4, 30)), gfsRec("est0130", "db", utc(2026, 11, 1, 6, 30)),
				gfsRec("est2330", "db", utc(2026, 11, 2, 4, 30)), gfsRec("nov2", "db", utc(2026, 11, 2, 5, 30)),
			},
			keep: map[string]string{"nov2": "keep_daily 2026-11-02", "est0130": "keep_daily 2026-11-01"},
			del:  []string{"edt0030", "est2330"},
		},
		{
			name: "DST start in New York: the 23-hour day is one bucket",
			now:  utc(2026, 3, 10, 12, 0), policy: models.RetentionPolicy{KeepDaily: 3}, loc: ny,
			recs: []*models.BackupRecord{
				gfsRec("mar7", "db", utc(2026, 3, 8, 4, 59)),   // 23:59 EST, 7 Mar
				gfsRec("mar8_a", "db", utc(2026, 3, 8, 5, 1)),  // 00:01 EST, 8 Mar
				gfsRec("mar8_b", "db", utc(2026, 3, 9, 3, 59)), // 23:59 EDT, 8 Mar
				gfsRec("mar9", "db", utc(2026, 3, 9, 4, 1)),    // 00:01 EDT, 9 Mar
				gfsRec("mar6", "db", utc(2026, 3, 6, 15, 0)),   // 6 Mar
			},
			keep: map[string]string{"mar9": "keep_daily 2026-03-09", "mar8_b": "keep_daily 2026-03-08", "mar7": "keep_daily 2026-03-07"},
			del:  []string{"mar6", "mar8_a"},
		},
		{
			name: "leap day: 29 February is its own day and month bucket",
			now:  utc(2024, 3, 2, 12, 0), policy: models.RetentionPolicy{KeepDaily: 2, KeepMonthly: 2},
			recs: []*models.BackupRecord{
				gfsRec("feb28", "db", utc(2024, 2, 28, 2, 0)), gfsRec("feb29", "db", utc(2024, 2, 29, 2, 0)), gfsRec("mar01", "db", utc(2024, 3, 1, 2, 0)),
			},
			keep: map[string]string{"mar01": "keep_daily 2024-03-01", "feb29": "keep_daily 2024-02-29"},
			del:  []string{"feb28"},
		},
		{
			name: "leap day: yearly buckets of 29 February 2024 and 1 March 2025",
			now:  utc(2025, 3, 2, 12, 0), policy: models.RetentionPolicy{KeepYearly: 2},
			recs: []*models.BackupRecord{
				gfsRec("feb28", "db", utc(2024, 2, 28, 2, 0)), gfsRec("feb29", "db", utc(2024, 2, 29, 2, 0)), gfsRec("y25", "db", utc(2025, 3, 1, 2, 0)),
			},
			keep: map[string]string{"y25": "keep_yearly 2025", "feb29": "keep_yearly 2024"},
			del:  []string{"feb28"},
		},
		{
			name: "union with retention_days: a backup beyond the age rule stays when a bucket keeps it",
			now:  utc(2026, 11, 8, 12, 0), policy: models.RetentionPolicy{RetentionDays: 3, KeepMonthly: 2},
			recs: []*models.BackupRecord{
				gfsRec("nov07", "db", utc(2026, 11, 7, 2, 0)), gfsRec("nov06", "db", utc(2026, 11, 6, 2, 0)), gfsRec("nov05", "db", utc(2026, 11, 5, 2, 0)),
				gfsRec("oct31", "db", utc(2026, 10, 31, 2, 0)), gfsRec("oct15", "db", utc(2026, 10, 15, 2, 0)),
				gfsRec("sep30", "db", utc(2026, 9, 30, 2, 0)),
			},
			keep: map[string]string{"nov07": "keep_monthly 2026-11", "nov06": "retention_days", "oct31": "keep_monthly 2026-10"},
			del:  []string{"nov05", "oct15", "sep30"},
		},
		{
			name: "union with retention_count: backups within the count stay next to the buckets",
			now:  utc(2026, 11, 8, 12, 0), policy: models.RetentionPolicy{RetentionCount: 2, KeepYearly: 2},
			recs: []*models.BackupRecord{
				gfsRec("n7", "db", utc(2026, 11, 7, 2, 0)), gfsRec("n6", "db", utc(2026, 11, 6, 2, 0)),
				gfsRec("n5", "db", utc(2026, 11, 5, 2, 0)), gfsRec("y25", "db", utc(2025, 12, 31, 2, 0)),
				gfsRec("y25old", "db", utc(2025, 6, 1, 2, 0)),
			},
			keep: map[string]string{"n7": "retention_count", "n6": "retention_count", "y25": "keep_yearly 2025"},
			del:  []string{"n5", "y25old"},
		},
		{
			name: "gfs only: the newest backup always stays, however old",
			now:  utc(2030, 1, 1, 0, 0), policy: models.RetentionPolicy{KeepDaily: 1},
			recs: []*models.BackupRecord{gfsRec("old", "db", utc(2026, 1, 1, 2, 0))},
			keep: map[string]string{"old": "keep_daily 2026-01-01"},
		},
		{
			name: "databases are planned separately",
			now:  utc(2026, 11, 8, 12, 0), policy: models.RetentionPolicy{KeepMonthly: 1},
			recs: []*models.BackupRecord{
				gfsRec("a_new", "a", utc(2026, 11, 7, 2, 0)), gfsRec("a_old", "a", utc(2026, 11, 1, 2, 0)),
				gfsRec("b_new", "b", utc(2026, 10, 7, 2, 0)), gfsRec("b_old", "b", utc(2026, 10, 1, 2, 0)),
			},
			keep: map[string]string{"a_new": "keep_monthly 2026-11", "b_new": "keep_monthly 2026-10"},
			del:  []string{"a_old", "b_old"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := PlanRetentionPolicy(tc.now, tc.policy, tc.loc, tc.recs)
			got := keptReasons(plan)
			for id, reason := range tc.keep {
				if reason == "" {
					continue
				}
				if !slices.Contains(got[id], reason) {
					t.Errorf("%s: reasons %v, want %q", id, got[id], reason)
				}
			}
			for _, id := range tc.del {
				if _, kept := got[id]; kept {
					t.Errorf("%s is kept (%v), want deleted", id, got[id])
				}
			}
			if d := deletedIDs(plan); !slices.Equal(d, slices.Sorted(slices.Values(tc.del))) {
				t.Errorf("deleted %v, want %v", d, tc.del)
			}
			if len(plan.Kept)+len(plan.Delete) != len(tc.recs) {
				t.Errorf("kept %d + deleted %d != %d backups", len(plan.Kept), len(plan.Delete), len(tc.recs))
			}
		})
	}
}

// TestPlanRetentionPolicyWithoutBucketsMatchesPlanRetention proves that a policy of
// days and count only is planned exactly as before buckets existed.
func TestPlanRetentionPolicyWithoutBucketsMatchesPlanRetention(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	var recs []*models.BackupRecord
	for i := range 20 {
		recs = append(recs, gfsRec("b"+string(rune('a'+i)), "db", now.Add(-time.Duration(i)*30*time.Hour)))
	}
	for _, c := range [][2]int{{0, 3}, {5, 0}, {5, 3}, {0, 0}, {2, 10}} {
		old := PlanRetention(now, c[0], c[1], recs)
		cur := PlanRetentionPolicy(now, models.RetentionPolicy{RetentionDays: c[0], RetentionCount: c[1]}, time.UTC, recs)
		if !slices.Equal(deletedIDs(old), deletedIDs(cur)) || len(old.Protected) != len(cur.Protected) {
			t.Errorf("days %d count %d: %v vs %v", c[0], c[1], deletedIDs(old), deletedIDs(cur))
		}
		for _, d := range cur.Delete {
			if d.Reason == models.RetentionGFS || strings.Contains(d.Detail, "bucket") {
				t.Errorf("days %d count %d: a policy without buckets mentions buckets: %+v", c[0], c[1], d)
			}
		}
	}
}

// TestPlanRetentionPolicySafetyFloors proves the floors stay in force under buckets:
// pinned backups and the newest verified backup per database are never deleted, with
// their reason, and the newest backup always stays.
func TestPlanRetentionPolicySafetyFloors(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	newest := gfsRec("newest", "db", now.Add(-24*time.Hour))
	verified := gfsRec("verified", "db", now.Add(-48*time.Hour))
	verified.Verification = models.VerificationOK
	pinned := gfsRec("pinned", "db", now.Add(-72*time.Hour))
	pinned.Pinned = true
	older := gfsRec("older", "db", now.Add(-96*time.Hour))
	oldVerified := gfsRec("oldverified", "db", now.Add(-120*time.Hour))
	oldVerified.Verification = models.VerificationOK
	recs := []*models.BackupRecord{older, oldVerified, pinned, verified, newest}

	plan := PlanRetentionPolicy(now, models.RetentionPolicy{KeepDaily: 1}, time.UTC, recs)
	if d := deletedIDs(plan); !slices.Equal(d, []string{"older", "oldverified"}) {
		t.Fatalf("deleted %v, want older and oldverified", d)
	}
	got := keptReasons(plan)
	if !slices.Contains(got["newest"], "keep_daily 2026-11-07") {
		t.Errorf("newest: %v", got["newest"])
	}
	if !slices.Contains(got["verified"], ProtectedLastVerified) || !slices.Contains(got["pinned"], ProtectedPinned) {
		t.Errorf("protected reasons: verified %v, pinned %v", got["verified"], got["pinned"])
	}
	if len(plan.Protected) != 2 {
		t.Errorf("protected = %+v", plan.Protected)
	}

	// With a count rule next to the buckets the floor is the count.
	plan = PlanRetentionPolicy(now, models.RetentionPolicy{RetentionCount: 4, KeepMonthly: 1}, time.UTC, recs)
	if d := deletedIDs(plan); !slices.Equal(d, []string{"oldverified"}) {
		t.Errorf("count 4 + keep_monthly: deleted %v, want only oldverified", d)
	}
	// An inactive policy deletes nothing and keeps nothing to explain.
	plan = PlanRetentionPolicy(now, models.RetentionPolicy{}, time.UTC, recs)
	if len(plan.Delete) != 0 || len(plan.Kept) != 0 {
		t.Errorf("inactive policy: %+v", plan)
	}
}

// TestPlanRetentionPolicyIgnoresOnDemandAndInstanceBackups proves buckets only see
// completed scheduled database backups.
func TestPlanRetentionPolicyIgnoresOnDemandAndInstanceBackups(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	manual := gfsRec("manual", "db", now.Add(-2*time.Hour))
	manual.Trigger = models.TriggerManual
	failed := gfsRec("failed", "db", now.Add(-3*time.Hour))
	failed.Status = models.StatusFailed
	sched := gfsRec("sched", "db", now.Add(-26*time.Hour))
	plan := PlanRetentionPolicy(now, models.RetentionPolicy{KeepDaily: 1}, time.UTC, []*models.BackupRecord{manual, failed, sched})
	if plan.Considered != 1 || len(plan.Delete) != 0 || len(plan.Kept) != 1 || plan.Kept[0].BackupID != "sched" {
		t.Fatalf("plan = %+v", plan)
	}
}
