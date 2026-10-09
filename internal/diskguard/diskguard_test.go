package diskguard_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/diskguard"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runs"
)

// errSQLiteFull is the error of a SQLite write on a full disk.
var errSQLiteFull = errors.New("database or disk is full (13)")

// codedError carries a SQLite result code like the driver's errors.
type codedError struct{ code int }

func (e codedError) Error() string { return fmt.Sprintf("sqlite error %d", e.code) }
func (e codedError) Code() int     { return e.code }

// publisher records events.
type publisher struct {
	mu  sync.Mutex
	got []events.Event
}

func (p *publisher) Publish(_ context.Context, e events.Event) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, e)
	return true
}

func (p *publisher) types() []events.EventType {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []events.EventType
	for _, e := range p.got {
		out = append(out, e.Type)
	}
	return out
}

// space is a settable free-space probe.
type space struct{ free atomic.Uint64 }

func (s *space) probe(string) (uint64, error) { return s.free.Load(), nil }

func newGuard(t *testing.T, free uint64) (*diskguard.Guard, *space, *publisher, *atomic.Bool) {
	t.Helper()
	sp := &space{}
	sp.free.Store(free)
	pub := &publisher{}
	warn := &atomic.Bool{}
	g := diskguard.New(diskguard.Config{
		Dir: "/data", MinFree: 100 << 20, Free: sp.probe, Publisher: pub, Warn: warn.Store,
		SaveRetryDelays: []time.Duration{time.Millisecond},
	})
	return g, sp, pub, warn
}

func TestIsFull(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("boom"), false},
		{fmt.Errorf("store: save: %w", errSQLiteFull), true},
		{fmt.Errorf("write: %w", syscall.ENOSPC), true},
		{codedError{13}, true},
		{codedError{13 | 3<<8}, true},
		{codedError{5}, false},
	} {
		if got := diskguard.IsFull(tt.err); got != tt.want {
			t.Errorf("IsFull(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

// A run does not start while the data directory has less free space than the
// minimum, with an error that says so.
func TestCheckRefusesBelowTheMinimum(t *testing.T) {
	g, sp, _, _ := newGuard(t, 50<<20)
	err := g.Check()
	if !errors.Is(err, diskguard.ErrLowSpace) {
		t.Fatalf("Check = %v, want ErrLowSpace", err)
	}
	for _, want := range []string{"50 MiB free", "/data", "minimum of 100 MiB"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	sp.free.Store(200 << 20)
	if err := g.Check(); err != nil {
		t.Fatalf("Check with enough space = %v", err)
	}
	// The check is off with a zero minimum.
	off := diskguard.New(diskguard.Config{Dir: "/data", Free: func(string) (uint64, error) { return 0, nil }})
	if err := off.Check(); err != nil {
		t.Fatalf("Check with the check off = %v", err)
	}
	// A nil guard admits everything.
	var none *diskguard.Guard
	if err := none.Check(); err != nil {
		t.Fatal(err)
	}
}

// A failed write on a full disk publishes one system.disk_full event per
// episode, raises the warning and refuses runs until space is available.
func TestDiskFullEpisode(t *testing.T) {
	g, sp, pub, warn := newGuard(t, 1<<30)
	ctx := context.Background()
	if g.Observe(ctx, errors.New("constraint failed")) {
		t.Fatal("an unrelated error started an episode")
	}
	first, again := g.Observe(ctx, errSQLiteFull), g.Observe(ctx, errSQLiteFull)
	if !first || !again {
		t.Fatal("a full disk was not recognised")
	}
	if got := pub.types(); len(got) != 1 || got[0] != events.SystemDiskFull {
		t.Fatalf("events %v, want one system.disk_full", got)
	}
	if !events.SystemDiskFull.Broadcast() || !events.SystemDiskFull.Failed() {
		t.Error("system.disk_full must be a broadcast failure")
	}
	if !warn.Load() || !g.Full() {
		t.Fatal("warning not raised")
	}
	// Plenty of free space now: the next check ends the episode.
	if err := g.Check(); err != nil {
		t.Fatalf("Check after space was freed = %v", err)
	}
	if warn.Load() || g.Full() {
		t.Fatal("episode not ended")
	}
	// Full again with little space: refused, naming the full disk.
	g.Observe(ctx, errSQLiteFull)
	sp.free.Store(10 << 20)
	if err := g.Check(); !errors.Is(err, diskguard.ErrLowSpace) || !strings.Contains(err.Error(), "disk is full") {
		t.Fatalf("Check while full = %v", err)
	}
	// Within the event interval (an hour) a new episode is not announced again.
	if got := pub.types(); len(got) != 1 {
		t.Fatalf("a second episode within the hour published %v", got)
	}
	if !warn.Load() {
		t.Fatal("the second episode raised no warning")
	}
}

// After the event interval a new episode is announced again.
func TestDiskFullEventInterval(t *testing.T) {
	pub := &publisher{}
	sp := &space{}
	g := diskguard.New(diskguard.Config{Dir: "/data", Free: sp.probe, Publisher: pub, EventInterval: 20 * time.Millisecond})
	ctx := context.Background()
	g.Observe(ctx, errSQLiteFull)
	sp.free.Store(1 << 30)
	_ = g.Check()
	g.Observe(ctx, errSQLiteFull) // too soon
	sp.free.Store(1 << 30)
	_ = g.Check()
	time.Sleep(30 * time.Millisecond)
	g.Observe(ctx, errSQLiteFull)
	if got := pub.types(); len(got) != 2 {
		t.Fatalf("events %v, want 2 (the second within the interval debounced)", got)
	}
}

// When the free space cannot be read, a full episode ends once a write succeeds:
// the write probe, or a deferred save.
func TestFullEpisodeEndsWhenAWriteSucceeds(t *testing.T) {
	unreadable := func(string) (uint64, error) { return 0, errors.New("statfs: permission denied") }
	ctx := context.Background()

	var probeOK atomic.Bool
	g := diskguard.New(diskguard.Config{Dir: "/data", MinFree: 100 << 20, Free: unreadable,
		Probe: func(string) error {
			if probeOK.Load() {
				return nil
			}
			return syscall.ENOSPC
		}})
	g.Observe(ctx, errSQLiteFull)
	if err := g.Check(); !errors.Is(err, diskguard.ErrLowSpace) {
		t.Fatalf("Check while the probe fails = %v", err)
	}
	probeOK.Store(true)
	time.Sleep(5100 * time.Millisecond) // the probe runs at most every 5 s
	if err := g.Check(); err != nil || g.Full() {
		t.Fatalf("Check after a successful probe = %v (full %v)", err, g.Full())
	}

	// A deferred save that succeeds ends the episode too.
	g2 := diskguard.New(diskguard.Config{Dir: "/data", Free: unreadable, Probe: func(string) error { return syscall.ENOSPC }})
	g2.Observe(ctx, errSQLiteFull)
	g2.Defer("backup bkp_1", func(context.Context) error { return nil })
	g2.RetryDeferred(ctx)
	if g2.Full() || g2.Pending() != 0 {
		t.Fatalf("full %v, pending %d after a successful save", g2.Full(), g2.Pending())
	}

	// The default probe writes and removes a file in the directory.
	dir := t.TempDir()
	g3 := diskguard.New(diskguard.Config{Dir: dir, Free: unreadable})
	g3.Observe(ctx, errSQLiteFull)
	if err := g3.Check(); err != nil || g3.Full() {
		t.Fatalf("Check with the default probe = %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("the probe left %v", entries)
	}
}

// The runs manager asks the guard before every backup and restore, and only them.
func TestRunsAdmission(t *testing.T) {
	g, sp, _, _ := newGuard(t, 1<<20)
	m := runs.NewManager(nil)
	m.SetAdmission(g.Admit)
	for _, key := range []string{runs.BackupKey("c", "shop"), runs.RestoreKey("c", "shop"), runs.PITRBaseKey("c")} {
		if _, err := m.Acquire(key); !errors.Is(err, diskguard.ErrLowSpace) {
			t.Errorf("Acquire(%s) = %v, want ErrLowSpace", key, err)
		}
	}
	if err := m.Go(runs.BackupKey("c", "shop"), func(context.Context) {}); !errors.Is(err, diskguard.ErrLowSpace) {
		t.Errorf("Go = %v, want ErrLowSpace", err)
	}
	release, err := m.Acquire("scan:target")
	if err != nil {
		t.Fatalf("other operations are refused: %v", err)
	}
	release()
	sp.free.Store(1 << 30)
	release, err = m.Acquire(runs.BackupKey("c", "shop"))
	if err != nil {
		t.Fatalf("Acquire with space = %v", err)
	}
	release()
}

// A backup whose final record cannot be saved is reported as failed, its archive
// goes to the purge, and the failed record is saved once writes work again.
func TestFinishBackupUnsaved(t *testing.T) {
	g, _, _, _ := newGuard(t, 1<<30)
	ctx := context.Background()
	var full atomic.Bool
	full.Store(true)
	var saved []models.BackupRecord
	save := func(_ context.Context, rec *models.BackupRecord) error {
		if full.Load() {
			return fmt.Errorf("store: save backup record: %w", errSQLiteFull)
		}
		saved = append(saved, *rec)
		return nil
	}
	rec := &models.BackupRecord{ID: "bkp_1", Database: "shop", Status: models.StatusCompleted, StorageKey: "shop/1.archive.gz", SizeBytes: 10}
	err := g.FinishBackup(ctx, rec, save, nil, nil)
	if !errors.Is(err, diskguard.ErrNotSaved) {
		t.Fatalf("FinishBackup = %v, want ErrNotSaved", err)
	}
	if rec.Status != models.StatusFailed || !rec.ArchiveCleanupPending || !strings.HasPrefix(rec.ErrorMessage, models.ErrRecordNotSaved) {
		t.Fatalf("record %+v", rec)
	}
	if e := events.BackupEvent(rec, nil, "", ""); e.Type != events.BackupFailed {
		t.Fatalf("event %s, want backup.failed", e.Type)
	}
	if g.Pending() != 1 {
		t.Fatalf("pending %d", g.Pending())
	}
	// Still full: the save waits.
	g.RetryDeferred(ctx)
	if g.Pending() != 1 || len(saved) != 0 {
		t.Fatal("the save did not wait for space")
	}
	full.Store(false)
	_ = g.Check() // space is back: ends the episode
	g.RetryDeferred(ctx)
	if g.Pending() != 0 || len(saved) != 1 || saved[0].Status != models.StatusFailed || !saved[0].ArchiveCleanupPending {
		t.Fatalf("saved %+v, pending %d", saved, g.Pending())
	}

	// A record saved at once is left alone.
	ok := &models.BackupRecord{ID: "bkp_2", Status: models.StatusCompleted, StorageKey: "k"}
	if err := g.FinishBackup(ctx, ok, save, nil, nil); err != nil || ok.Status != models.StatusCompleted || ok.ArchiveCleanupPending {
		t.Fatalf("saved record changed: %+v, %v", ok, err)
	}
}

// A transient failure (a database locked for 4 s) never fails the backup nor
// sends its archive to the purge: the completed record is kept as it is and
// saved in the background once the lock is gone.
func TestFinishBackupTransientLock(t *testing.T) {
	g := diskguard.New(diskguard.Config{Dir: "/data", Free: func(string) (uint64, error) { return 1 << 30, nil }, PollInterval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { g.Run(ctx) })
	defer func() {
		cancel()
		wg.Wait()
	}()
	lockedUntil := time.Now().Add(4 * time.Second)
	var mu sync.Mutex
	var saved []models.BackupRecord
	save := func(_ context.Context, rec *models.BackupRecord) error {
		if time.Now().Before(lockedUntil) {
			return errors.New("database is locked (5) (SQLITE_BUSY)")
		}
		mu.Lock()
		defer mu.Unlock()
		saved = append(saved, *rec)
		return nil
	}
	rec := &models.BackupRecord{ID: "bkp_1", Status: models.StatusCompleted, StorageKey: "shop/1.archive.gz", SizeBytes: 10}
	saveCtx, cancelSave := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelSave()
	if err := g.FinishBackup(saveCtx, rec, save, nil, nil); err != nil {
		t.Fatalf("FinishBackup = %v; a lock is not a failure", err)
	}
	if rec.Status != models.StatusCompleted || rec.ArchiveCleanupPending || rec.ErrorMessage != "" {
		t.Fatalf("record changed by a lock: %+v", rec)
	}
	if e := events.BackupEvent(rec, nil, "", ""); e.Type != events.BackupSucceeded {
		t.Fatalf("event %s, want backup.succeeded", e.Type)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		n := len(saved)
		mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the record was not saved after the lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if saved[0].Status != models.StatusCompleted || saved[0].ArchiveCleanupPending || g.Full() {
		t.Fatalf("saved %+v (full %v); want the completed record, no cleanup", saved[0], g.Full())
	}
}

// Run retries the deferred saves and ends an episode once space is available.
func TestRunSettlesWhenSpaceReturns(t *testing.T) {
	sp := &space{}
	pub := &publisher{}
	g := diskguard.New(diskguard.Config{Dir: "/data", MinFree: 100 << 20, Free: sp.probe, Publisher: pub, PollInterval: 5 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { g.Run(ctx) })
	defer func() {
		cancel()
		wg.Wait()
	}()
	g.Observe(ctx, errSQLiteFull)
	var done atomic.Bool
	g.Defer("backup bkp_1", func(context.Context) error {
		done.Store(true)
		return nil
	})
	time.Sleep(20 * time.Millisecond)
	if done.Load() {
		t.Fatal("a deferred save ran while the disk was full")
	}
	sp.free.Store(1 << 30)
	deadline := time.Now().Add(5 * time.Second)
	for !done.Load() || g.Full() {
		if time.Now().After(deadline) {
			t.Fatal("the deferred save did not run once space returned")
		}
		time.Sleep(time.Millisecond)
	}
}
