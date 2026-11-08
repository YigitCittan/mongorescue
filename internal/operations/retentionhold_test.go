package operations_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

// holdEnv is a protection environment with a job that copies to two targets.
type holdEnv struct {
	*protEnv
	t2, t3 string
}

func newHoldEnv(t *testing.T, twoPerson bool, job models.Job) *holdEnv {
	t.Helper()
	env := &holdEnv{protEnv: newProtEnv(t)}
	for i, dst := range []*string{&env.t2, &env.t3} {
		tg, err := env.targets.Create(context.Background(), targets.Input{Name: "copy" + string(rune('a'+i)), Type: models.StorageLocal, Local: &models.LocalTarget{Path: t.TempDir()}})
		if err != nil {
			t.Fatal(err)
		}
		*dst = tg.ID
	}
	job.ID, job.Name, job.Database, job.ConnectionID, job.CronExpression = "job_h", "h", "shop", "conn_ok", "@daily"
	job.CopyTargets = []string{env.t2, env.t3}
	if job.CopyRetention != nil {
		job.CopyRetention = map[string]models.RetentionPolicy{env.t2: job.CopyRetention["t2"]}
	}
	if err := env.st.SaveJob(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	if twoPerson {
		env.enableTwoPerson(t)
	}
	return env
}

// actor is the context a job edit is made with: a session under the two-person rule.
func actor(twoPerson bool) context.Context {
	if twoPerson {
		return asUser("alice", auth.ScopeAdmin)
	}
	return keyOf("alice")
}

func (env *holdEnv) update(t *testing.T, twoPerson bool, edit func(*operations.JobUpdate)) *operations.JobSaveResult {
	t.Helper()
	u := operations.JobUpdate{Name: "h", Database: "shop", ConnectionID: "conn_ok", CronExpression: "@daily"}
	edit(&u)
	res, err := env.svc.UpdateJob(actor(twoPerson), "job_h", u)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (env *holdEnv) stored(t *testing.T) *models.Job {
	t.Helper()
	job, err := env.st.GetJob(context.Background(), "job_h")
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// expectHeld checks that an edit was held back: the stored job is unchanged, and
// the change waits for the grace period (no two-person rule) or for a second
// administrator, whose approval still leaves the grace period. It returns the
// pending change once scheduled, and applies it after the grace period.
func (env *holdEnv) expectHeld(t *testing.T, twoPerson bool, res *operations.JobSaveResult, before *models.Job, check func(*models.PendingChange), after func(*models.Job)) {
	t.Helper()
	stored := env.stored(t)
	if stored.RetentionPolicy() != before.RetentionPolicy() || len(stored.CopyRetention) != len(before.CopyRetention) {
		t.Fatalf("stored retention = %+v / %+v; want it unchanged while held", stored.RetentionPolicy(), stored.CopyRetention)
	}
	for id, p := range before.CopyRetention {
		if stored.CopyRetention[id] != p {
			t.Fatalf("stored copy retention of %s = %+v; want %+v", id, stored.CopyRetention[id], p)
		}
	}
	var change *models.PendingChange
	if twoPerson {
		if res.Approval == nil || res.Approval.Action != models.ApprovalShortenRetention || res.PendingRetention != nil {
			t.Fatalf("result = %+v; want an approval request and no pending change", res)
		}
		if _, err := env.svc.Approve(asUser("alice", auth.ScopeAdmin), res.Approval.ID); !errors.Is(err, operations.ErrSelfApproval) {
			t.Fatalf("the requester approving = %v; want ErrSelfApproval", err)
		}
		if _, err := env.svc.Approve(asUser("bob", auth.ScopeAdmin), res.Approval.ID); err != nil {
			t.Fatal(err)
		}
		list, err := env.svc.PendingChanges(context.Background())
		if err != nil || len(list) != 1 {
			t.Fatalf("pending after approval = %+v, %v; want the grace period to follow", list, err)
		}
		change = list[0]
	} else {
		if res.Approval != nil || res.PendingRetention == nil {
			t.Fatalf("result = %+v; want a pending change and no approval", res)
		}
		change = res.PendingRetention
	}
	check(change)
	env.clock.Advance(models.GraceDuration(models.DefaultDeleteGraceDays) - time.Minute)
	env.svc.ApplyDueChanges(context.Background())
	if got := env.stored(t); got.RetentionPolicy() != before.RetentionPolicy() {
		t.Fatalf("retention %+v applied before the grace period ended", got.RetentionPolicy())
	}
	env.clock.Advance(time.Minute)
	env.svc.ApplyDueChanges(context.Background())
	after(env.stored(t))
}

// expectImmediate checks that an edit applied at once with nothing held.
func expectImmediate(t *testing.T, res *operations.JobSaveResult, got *models.Job, ok func(*models.Job) bool) {
	t.Helper()
	if res.Approval != nil || res.PendingRetention != nil || !ok(got) {
		t.Fatalf("result = %+v, stored %+v / %+v; want an immediate change", res, got.RetentionPolicy(), got.CopyRetention)
	}
}

func TestLoweringAGFSBucketIsHeld(t *testing.T) {
	buckets := []struct {
		name string
		set  func(*operations.JobUpdate, int)
		get  func(*models.Job) int
		pend func(*models.PendingChange) *int
	}{
		{"keep_daily", func(u *operations.JobUpdate, v int) { u.KeepDaily = &v }, func(j *models.Job) int { return j.KeepDaily }, func(c *models.PendingChange) *int { return c.KeepDaily }},
		{"keep_weekly", func(u *operations.JobUpdate, v int) { u.KeepWeekly = &v }, func(j *models.Job) int { return j.KeepWeekly }, func(c *models.PendingChange) *int { return c.KeepWeekly }},
		{"keep_monthly", func(u *operations.JobUpdate, v int) { u.KeepMonthly = &v }, func(j *models.Job) int { return j.KeepMonthly }, func(c *models.PendingChange) *int { return c.KeepMonthly }},
		{"keep_yearly", func(u *operations.JobUpdate, v int) { u.KeepYearly = &v }, func(j *models.Job) int { return j.KeepYearly }, func(c *models.PendingChange) *int { return c.KeepYearly }},
	}
	for _, twoPerson := range []bool{false, true} {
		for _, b := range buckets {
			for _, lowered := range []int{5, 0} {
				name := b.name + "/lower"
				if lowered == 0 {
					name = b.name + "/turn off"
				}
				if twoPerson {
					name += "/two-person"
				}
				t.Run(name, func(t *testing.T) {
					env := newHoldEnv(t, twoPerson, models.Job{KeepDaily: 10, KeepWeekly: 10, KeepMonthly: 10, KeepYearly: 10})
					before := env.stored(t)
					res := env.update(t, twoPerson, func(u *operations.JobUpdate) { b.set(u, lowered) })
					env.expectHeld(t, twoPerson, res, before, func(c *models.PendingChange) {
						if got := b.pend(c); got == nil || *got != lowered {
							t.Fatalf("pending %s = %v; want %d", b.name, got, lowered)
						}
					}, func(job *models.Job) {
						if b.get(job) != lowered {
							t.Fatalf("%s = %d after the grace period; want %d", b.name, b.get(job), lowered)
						}
					})
				})
			}
			t.Run(b.name+"/raise", func(t *testing.T) {
				env := newHoldEnv(t, twoPerson, models.Job{KeepDaily: 10, KeepWeekly: 10, KeepMonthly: 10, KeepYearly: 10})
				res := env.update(t, twoPerson, func(u *operations.JobUpdate) { b.set(u, 20) })
				expectImmediate(t, res, env.stored(t), func(j *models.Job) bool { return b.get(j) == 20 })
			})
		}
	}
}

func TestTurningAgeAndCountOffLeavingBucketsIsHeld(t *testing.T) {
	for _, twoPerson := range []bool{false, true} {
		env := newHoldEnv(t, twoPerson, models.Job{RetentionDays: 30, RetentionCount: 5, KeepDaily: 7})
		before := env.stored(t)
		zero := 0
		res := env.update(t, twoPerson, func(u *operations.JobUpdate) { u.RetentionDays, u.RetentionCount = &zero, &zero })
		env.expectHeld(t, twoPerson, res, before, func(c *models.PendingChange) {
			if c.RetentionDays == nil || *c.RetentionDays != 0 || c.RetentionCount == nil || *c.RetentionCount != 0 {
				t.Fatalf("pending = %+v; want both limits turned off", c)
			}
		}, func(job *models.Job) {
			if job.RetentionDays != 0 || job.RetentionCount != 0 || job.KeepDaily != 7 {
				t.Fatalf("stored = %+v", job.RetentionPolicy())
			}
		})
	}
}

func TestAddingABucketToAnUnlimitedJobIsHeld(t *testing.T) {
	for _, twoPerson := range []bool{false, true} {
		env := newHoldEnv(t, twoPerson, models.Job{})
		before := env.stored(t)
		seven := 7
		res := env.update(t, twoPerson, func(u *operations.JobUpdate) { u.KeepDaily = &seven })
		env.expectHeld(t, twoPerson, res, before, func(c *models.PendingChange) {
			if c.KeepDaily == nil || *c.KeepDaily != 7 {
				t.Fatalf("pending = %+v", c)
			}
		}, func(job *models.Job) {
			if job.KeepDaily != 7 {
				t.Fatalf("keep_daily = %d", job.KeepDaily)
			}
		})
	}
}

func TestLoweringCopyRetentionIsHeld(t *testing.T) {
	full := models.RetentionPolicy{RetentionDays: 30, RetentionCount: 5, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6, KeepYearly: 2}
	lowerOne := func(edit func(*models.RetentionPolicy)) func(env *holdEnv) (*map[string]models.RetentionPolicy, models.RetentionPolicy) {
		return func(env *holdEnv) (*map[string]models.RetentionPolicy, models.RetentionPolicy) {
			p := full
			edit(&p)
			return &map[string]models.RetentionPolicy{env.t2: p}, p
		}
	}
	cases := []struct {
		name string
		next func(env *holdEnv) (*map[string]models.RetentionPolicy, models.RetentionPolicy)
	}{
		{"retention_days", lowerOne(func(p *models.RetentionPolicy) { p.RetentionDays = 10 })},
		{"retention_count", lowerOne(func(p *models.RetentionPolicy) { p.RetentionCount = 2 })},
		{"keep_daily", lowerOne(func(p *models.RetentionPolicy) { p.KeepDaily = 3 })},
		{"keep_weekly", lowerOne(func(p *models.RetentionPolicy) { p.KeepWeekly = 1 })},
		{"keep_monthly", lowerOne(func(p *models.RetentionPolicy) { p.KeepMonthly = 3 })},
		{"keep_yearly", lowerOne(func(p *models.RetentionPolicy) { p.KeepYearly = 1 })},
		{"keep_yearly off", lowerOne(func(p *models.RetentionPolicy) { p.KeepYearly = 0 })},
		{"remove the override", func(*holdEnv) (*map[string]models.RetentionPolicy, models.RetentionPolicy) {
			return &map[string]models.RetentionPolicy{}, models.RetentionPolicy{}
		}},
	}
	for _, twoPerson := range []bool{false, true} {
		for _, c := range cases {
			name := c.name
			if twoPerson {
				name += "/two-person"
			}
			t.Run(name, func(t *testing.T) {
				env := newHoldEnv(t, twoPerson, models.Job{CopyRetention: map[string]models.RetentionPolicy{"t2": full}})
				before := env.stored(t)
				next, want := c.next(env)
				removal := len(*next) == 0
				res := env.update(t, twoPerson, func(u *operations.JobUpdate) { u.CopyRetention = next })
				env.expectHeld(t, twoPerson, res, before, func(ch *models.PendingChange) {
					p, ok := ch.CopyRetention[env.t2]
					switch {
					case !ok:
						t.Fatalf("pending copy retention = %+v; want an entry for %s", ch.CopyRetention, env.t2)
					case removal && p != nil, !removal && (p == nil || *p != want):
						t.Fatalf("pending copy retention of %s = %+v; want %+v", env.t2, p, want)
					}
				}, func(job *models.Job) {
					got, ok := job.CopyRetention[env.t2]
					switch {
					case removal && ok, !removal && got != want:
						t.Fatalf("copy retention of %s = %+v (%v) after the grace period; want %+v", env.t2, got, ok, want)
					}
				})
			})
		}
		t.Run("raise or add", func(t *testing.T) {
			env := newHoldEnv(t, twoPerson, models.Job{CopyRetention: map[string]models.RetentionPolicy{"t2": full}})
			higher := full
			higher.RetentionDays, higher.KeepMonthly = 60, 12
			next := map[string]models.RetentionPolicy{env.t2: higher, env.t3: {KeepDaily: 5}}
			res := env.update(t, twoPerson, func(u *operations.JobUpdate) { u.CopyRetention = &next })
			expectImmediate(t, res, env.stored(t), func(j *models.Job) bool {
				return j.CopyRetention[env.t2] == higher && j.CopyRetention[env.t3] == (models.RetentionPolicy{KeepDaily: 5})
			})
		})
	}
}

// TestRaisingOneValueWhileLoweringAnotherAppliesTheRaiseAtOnce proves a mixed edit is
// split: the raise is stored, only the lowering waits.
func TestRaisingOneValueWhileLoweringAnotherAppliesTheRaiseAtOnce(t *testing.T) {
	env := newHoldEnv(t, false, models.Job{KeepDaily: 10, KeepMonthly: 10})
	lower, raise := 3, 20
	res := env.update(t, false, func(u *operations.JobUpdate) { u.KeepDaily, u.KeepMonthly = &lower, &raise })
	if got := env.stored(t); got.KeepDaily != 10 || got.KeepMonthly != 20 || res.PendingRetention == nil || res.PendingRetention.KeepMonthly != nil {
		t.Fatalf("stored = %+v, pending = %+v; want keep_monthly raised and keep_daily held", got.RetentionPolicy(), res.PendingRetention)
	}
}

// TestSecondLoweringKeepsThePendingOne proves a later lowering of another value adds
// to the pending change instead of replacing it, and an edit that raises a value
// drops the pending lowering of that value.
func TestSecondLoweringKeepsThePendingOne(t *testing.T) {
	env := newHoldEnv(t, false, models.Job{KeepDaily: 10, KeepYearly: 10})
	two, three, fifty := 2, 3, 50
	env.update(t, false, func(u *operations.JobUpdate) { u.KeepDaily = &two })
	res := env.update(t, false, func(u *operations.JobUpdate) { u.KeepYearly = &three })
	c := res.PendingRetention
	if c == nil || c.KeepDaily == nil || *c.KeepDaily != 2 || c.KeepYearly == nil || *c.KeepYearly != 3 {
		t.Fatalf("pending = %+v; want both lowerings", c)
	}
	// keep_daily is raised above the stored value while keep_yearly is lowered further.
	one := 1
	res = env.update(t, false, func(u *operations.JobUpdate) { u.KeepDaily, u.KeepYearly = &fifty, &one })
	if c = res.PendingRetention; c == nil || c.KeepDaily != nil || c.KeepYearly == nil || *c.KeepYearly != 1 || env.stored(t).KeepDaily != 50 {
		t.Fatalf("pending = %+v; want only keep_yearly 1 and keep_daily raised to 50", c)
	}
}

// TestRetentionPreviewListsWhatAPendingChangeReleases proves the preview names the
// backups and copies a held lowering would release, with a pending change and with an
// approval request.
func TestRetentionPreviewListsWhatAPendingChangeReleases(t *testing.T) {
	for _, twoPerson := range []bool{false, true} {
		env := newHoldEnv(t, twoPerson, models.Job{RetentionCount: 1, KeepMonthly: 4, CopyRetention: map[string]models.RetentionPolicy{"t2": {KeepMonthly: 6}}})
		primary, err := env.svc.ResolveTarget(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		for i, age := range []int{10, 40, 70, 100} {
			id := "b" + string(rune('1'+i))
			env.backup(t, id, "job_h", time.Duration(age)*24*time.Hour)
			rec, _ := env.st.GetBackupRecord(context.Background(), id)
			rec.StorageTargetID = primary.ID
			rec.Copies = []models.BackupCopy{{TargetID: env.t2, Status: models.CopyDone}}
			if err = env.st.SaveBackupRecord(context.Background(), rec); err != nil {
				t.Fatal(err)
			}
		}
		p, err := env.svc.RetentionPreview(context.Background(), "job_h", nil, nil)
		if err != nil || p.Pending != nil {
			t.Fatalf("preview = %+v, %v; want nothing pending", p.Pending, err)
		}
		one := 1
		env.update(t, twoPerson, func(u *operations.JobUpdate) {
			u.RetentionCount, u.KeepMonthly = &one, &one
			u.CopyRetention = &map[string]models.RetentionPolicy{env.t2: {KeepMonthly: 1}}
		})
		p, err = env.svc.RetentionPreview(context.Background(), "job_h", nil, nil)
		if err != nil || p.Pending == nil {
			t.Fatalf("preview = %+v, %v; want the pending change", p, err)
		}
		if twoPerson != (p.Pending.ApprovalID != "") || twoPerson == (p.Pending.ChangeID != "") {
			t.Fatalf("pending = %+v; two-person %v", p.Pending, twoPerson)
		}
		var released []string
		for _, r := range p.Pending.Release {
			released = append(released, r.BackupID)
		}
		if !slices.Equal(released, []string{"b4", "b3", "b2"}) || p.Pending.KeepMonthly != 1 {
			t.Fatalf("released backups = %v, policy %+v; want b4, b3, b2 (oldest first)", released, p.Pending)
		}
		if len(p.Pending.Copies) != 1 || p.Pending.Copies[0].TargetID != env.t2 || !slices.Equal(p.Pending.Copies[0].Backups, []string{"b2", "b3", "b4"}) {
			t.Fatalf("released copies = %+v; want b2, b3, b4 on %s", p.Pending.Copies, env.t2)
		}
	}
}
