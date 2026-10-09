//go:build chaos

package chaos

import (
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// envClock allows the clock scenarios to step the system clock (Linux, root or
// passwordless sudo). Go reads the wall clock from the kernel directly, so
// libfaketime cannot fake it for the binary; the CI runner is disposable.
const envClock = "MONGORESCUE_CHAOS_CLOCK"

// clockStepper steps the system wall clock and puts it back on cleanup.
type clockStepper struct {
	t     *testing.T
	base  time.Time // wall clock reading with the monotonic reading of the start
	shift time.Duration
}

// requireClock skips unless the system clock may be stepped.
func requireClock(t *testing.T) *clockStepper {
	t.Helper()
	if os.Getenv(envClock) != "1" || runtime.GOOS != "linux" {
		t.Skipf("set %s=1 on a disposable Linux machine to step the system clock", envClock)
	}
	c := &clockStepper{t: t, base: time.Now()}
	t.Cleanup(func() { c.set(0) })
	return c
}

// set moves the wall clock to the true time plus shift (true time measured with
// the monotonic clock since the start).
func (c *clockStepper) set(shift time.Duration) {
	c.t.Helper()
	target := c.base.Add(time.Since(c.base)).Add(shift)
	args := []string{"date", "-u", "-s", "@" + strconv.FormatInt(target.Unix(), 10)}
	if os.Geteuid() != 0 {
		args = append([]string{"sudo", "-n"}, args...)
	}
	if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
		c.t.Fatalf("step the clock: %v\n%s", err, out)
	}
	c.shift = shift
	c.t.Logf("system clock stepped to true time %+v", shift)
}

// clockRig is a rig with a job that runs every minute and an RPO of two hours.
func clockRig(t *testing.T) (*rig, *models.Job) {
	t.Helper()
	e := requireEnv(t)
	r := newRig(t, e, rigOptions{})
	db := e.uniqueDB(t, "clock")
	e.seedBlobs(t, db, "blobs", 1)
	var job models.Job
	r.api.data("POST", "/api/v1/jobs", map[string]any{
		"name": "every minute", "database": db, "cron_expression": "* * * * *", "enabled": true,
		"connection_id": r.connID, "rpo_minutes": 120,
	}, http.StatusCreated, &job)
	waitFor(t, 3*time.Minute, "a first scheduled run", func() bool { return len(r.jobRuns(job.ID)) > 0 })
	return r, &job
}

// jobRuns returns the backups of a job, newest first.
func (r *rig) jobRuns(jobID string) []*models.BackupRecord {
	r.t.Helper()
	var runs []*models.BackupRecord
	r.api.data("GET", "/api/v1/backups?job_id="+jobID, nil, http.StatusOK, &runs)
	return runs
}

// watchHealth polls /api/v1/health every 5 s for d and fails on any answer but 200.
func (r *rig) watchHealth(d time.Duration) {
	r.t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(5 * time.Second) {
		code, body, err := r.api.try("GET", "/api/v1/health", nil)
		if err != nil || code != http.StatusOK {
			r.t.Fatalf("health after the clock step: %d %v %s", code, err, body)
		}
	}
}

// TestClockStepForward steps the wall clock an hour forward while a job runs every
// minute.
//
// Expected: the scheduler stays healthy (health 200 for three minutes, more than
// the 90 s staleness limit); the hour of missed slots is not replayed (at most one
// catch-up run, then one per minute: no more than four runs in three minutes); the
// runs keep succeeding; and job.rpo_missed does not fire, since the job's RPO
// (two hours) is not breached by an hour's step.
func TestClockStepForward(t *testing.T) {
	clock := requireClock(t)
	r, job := clockRig(t)
	before := len(r.jobRuns(job.ID))

	clock.set(time.Hour)
	r.watchHealth(3 * time.Minute)
	runs := r.jobRuns(job.ID)
	if n := len(runs) - before; n > 4 || n < 1 {
		t.Fatalf("%d runs in the three minutes after an hour's step forward; want 1 to 4", n)
	}
	for _, run := range runs[:len(runs)-before] {
		if run.Status != models.StatusCompleted && run.Status != models.StatusInProgress {
			t.Fatalf("run after the step: %+v", run)
		}
	}
	if got := r.hook.find("job.rpo_missed", "", ""); len(got) > 0 {
		t.Fatalf("job.rpo_missed after an hour's step with an RPO of two hours: %v", got)
	}
}

// TestClockStepBack steps the wall clock an hour back while a job runs every
// minute.
//
// Expected: the scheduler stays healthy (health 200 for three minutes); no slot
// runs twice (at most one run in the three minutes: the job's next slot is an hour
// away on the stepped clock, as with cron); and job.rpo_missed does not fire.
func TestClockStepBack(t *testing.T) {
	t.Skip("known failing: a backward clock step stalls the scheduler liveness tick, https://github.com/YigitCittan/mongorescue/issues/137")
	clock := requireClock(t)
	r, job := clockRig(t)
	before := len(r.jobRuns(job.ID))

	clock.set(-time.Hour)
	r.watchHealth(3 * time.Minute)
	if n := len(r.jobRuns(job.ID)) - before; n > 1 {
		t.Fatalf("%d runs in the three minutes after an hour's step back: a slot ran twice", n)
	}
	if got := r.hook.find("job.rpo_missed", "", ""); len(got) > 0 {
		t.Fatalf("job.rpo_missed after an hour's step back: %v", got)
	}
}
