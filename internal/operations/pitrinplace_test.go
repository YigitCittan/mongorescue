package operations_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// inPlaceFakes stand for the restore engine (clone restore, clone drop, swap) and
// the collector (pause, resume) of an in-place point-in-time restore, and record
// the order of their calls.
type inPlaceFakes struct {
	mu      sync.Mutex
	calls   []string
	swapErr error
	// snapshotErr fails SnapshotPITRClones (a clone that lists nothing).
	snapshotErr error
	// resumeErr fails ResumeStream.
	resumeErr error
	// swapStarted, when set, is closed once the swap starts, which then waits
	// for swapGate and records its context's error in swapCtxErr.
	swapStarted, swapGate chan struct{}
	swapCtxErr            error
	// pauseWas is what PauseStream reports.
	pauseWas bool
}

func (f *inPlaceFakes) call(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
}

func (f *inPlaceFakes) order() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *inPlaceFakes) CanDecryptMode(string) bool { return true }

func (f *inPlaceFakes) PreparePITR(req models.RestoreRequest, run restore.PITRRun) (*models.RestoreRecord, error) {
	return restore.NewEngine(nil, "mongodb://x").PreparePITR(req, run)
}

func (f *inPlaceFakes) ExecutePITR(_ context.Context, req models.RestoreRequest, _ restore.PITRRun, rec *models.RestoreRecord) (*models.RestoreRecord, error) {
	f.call("restore")
	if req.PostRestoreCommands != nil {
		f.call("post_restore_on_clones")
	}
	now := time.Now().UTC()
	rec.Status, rec.CompletedAt = models.RestoreStatusCompleted, &now
	return rec, nil
}

func (f *inPlaceFakes) DropPITRClones(_ context.Context, _ string, info *models.PITRRestore) string {
	f.call("drop_clones")
	return "; the clones " + strings.Join(info.Clones, ", ") + " were dropped"
}

func (f *inPlaceFakes) PauseStream(context.Context, string) (bool, error) {
	f.call("pause")
	return f.pauseWas, nil
}

func (f *inPlaceFakes) ResumeStream(context.Context, string) error {
	f.call("resume")
	return f.resumeErr
}

func (f *inPlaceFakes) SnapshotPITRClones(_ context.Context, _ models.RestoreRequest, rec *models.RestoreRecord) error {
	f.call("snapshot")
	if f.snapshotErr != nil {
		return f.snapshotErr
	}
	for _, db := range rec.PITR.InPlace.Databases {
		rec.PITR.InPlace.Snapshots = append(rec.PITR.InPlace.Snapshots, models.PITRCloneSnapshot{Database: db,
			Clone: db + rec.PITR.CloneSuffix, Collections: []models.CollectionCount{{Name: "orders", Documents: 1}}})
	}
	return nil
}

func (f *inPlaceFakes) SwapPITRClones(ctx context.Context, _ models.RestoreRequest, rec *models.RestoreRecord, swapped func(string)) error {
	f.call("swap")
	if f.swapStarted != nil {
		close(f.swapStarted)
		<-f.swapGate
		f.mu.Lock()
		f.swapCtxErr = ctx.Err()
		f.mu.Unlock()
	}
	for i, db := range rec.PITR.InPlace.Databases {
		if f.swapErr != nil && i == len(rec.PITR.InPlace.Databases)-1 {
			return f.swapErr
		}
		swapped(db)
	}
	return nil
}

// inPlaceEnv is an operations service with stream str_a on conn_a, the fakes and
// a backup engine whose mongodump fails when failBackup is set.
type inPlaceEnv struct {
	svc     *operations.Service
	st      *store.SQLiteStore
	fakes   *inPlaceFakes
	pub     *recordingPublisher
	manager *runs.Manager
}

func newInPlaceEnv(t *testing.T, failBackup bool) *inPlaceEnv {
	t.Helper()
	st := storetest.New(t)
	seedPITRStream(t, st, "conn_a")
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	bRunner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		wait := func() error { return nil }
		if failBackup {
			wait = func() error { return errors.New("exit status 1") }
		}
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), wait, nil
	}
	mock := storage.NewMockStorage()
	fakes := &inPlaceFakes{pauseWas: true}
	pub := &recordingPublisher{}
	conn := &models.Connection{ID: "conn_a", Name: "rs", URI: "mongodb://u:pw@db.internal/?replicaSet=rs0",
		PostRestoreCommands: []models.PostRestoreCommand{{Database: "shop", Command: []byte(`{"delete":"users","deletes":[{"q":{"erased":true},"limit":0}]}`)}}}
	svc := operations.New(operations.Config{
		Store: st, Backup: backup.NewEngine(mock, "", backup.WithRunner(bRunner)), Restore: restore.NewEngine(mock, ""),
		Runs: manager, Connections: fakeConnections{"conn_a": conn}, Publisher: pub,
		PITR: st, PITRRestore: fakes, PITRBases: st.ListBaseBackups, PITRStreams: fakes, PITRSwap: fakes,
	})
	return &inPlaceEnv{svc: svc, st: st, fakes: fakes, pub: pub, manager: manager}
}

// pitrInPlace is an in-place point-in-time request for dbs, confirmed.
func pitrInPlace(dbs ...string) models.RestoreRequest {
	req := pitrAt(125, dbs...)
	req.PITR.InPlace, req.PITR.ConfirmInPlace = true, strings.Join(dbs, ",")
	return req
}

// wait polls restore id until it ended.
func (e *inPlaceEnv) wait(t *testing.T, id string) *models.RestoreRecord {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec, err := e.st.GetRestoreRecord(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Status != models.RestoreStatusInProgress && rec.Status != models.RestoreStatusPending {
			// The in-place event follows the stored record.
			for time.Now().Before(deadline) && e.inPlaceEvent() == nil {
				time.Sleep(5 * time.Millisecond)
			}
			return rec
		}
		if time.Now().After(deadline) {
			t.Fatalf("restore %s did not end", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *inPlaceEnv) inPlaceEvent() *events.Event {
	e.pub.mu.Lock()
	defer e.pub.mu.Unlock()
	for i := range e.pub.events {
		if e.pub.events[i].Type == events.RestoreInPlacePITR {
			ev := e.pub.events[i]
			return &ev
		}
	}
	return nil
}

func (e *inPlaceEnv) eventsOf(typ events.EventType) []events.Event {
	e.pub.mu.Lock()
	defer e.pub.mu.Unlock()
	var out []events.Event
	for _, ev := range e.pub.events {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

// safetyBackups returns the backups labelled as safety backups of restore id.
func (e *inPlaceEnv) safetyBackups(t *testing.T, id string) []*models.BackupRecord {
	t.Helper()
	list, err := e.st.ListBackupRecords(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var out []*models.BackupRecord
	for _, b := range list {
		if b.Label == models.BackupLabelPrePITRInPlace && b.RestoreID == id {
			out = append(out, b)
		}
	}
	return out
}

func TestPITRInPlaceRestore(t *testing.T) {
	env := newInPlaceEnv(t, false)
	rec, err := env.svc.StartRestore(admin(), pitrInPlace("shop", "crm"))
	if err != nil {
		t.Fatalf("StartRestore: %v", err)
	}
	if !rec.InPlace || rec.PITR.InPlace == nil || !slices.Equal(rec.PITR.InPlace.Databases, []string{"shop", "crm"}) {
		t.Fatalf("record = %+v (pitr %+v)", rec, rec.PITR)
	}
	final := env.wait(t, rec.ID)
	if final.Status != models.RestoreStatusCompleted || final.TargetDatabase != "shop,crm" {
		t.Fatalf("status %s, target %s: %s", final.Status, final.TargetDatabase, final.ErrorMessage)
	}
	// The clones are restored and verified (with the post-restore commands)
	// first, then backed up, the stream paused, the clones swapped in and
	// dropped, and the stream resumed.
	want := []string{"restore", "post_restore_on_clones", "snapshot", "pause", "swap", "drop_clones", "resume"}
	if got := env.fakes.order(); !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	ip := final.PITR.InPlace
	if ip.Stage != models.PITRInPlaceDone || !slices.Equal(ip.Swapped, []string{"shop", "crm"}) || !ip.StreamWasEnabled ||
		!ip.StreamPaused || !ip.StreamResumed || len(ip.SafetyBackupIDs) != 2 {
		t.Fatalf("in place = %+v", ip)
	}
	backups := env.safetyBackups(t, rec.ID)
	if len(backups) != 2 {
		t.Fatalf("safety backups = %d, want 2", len(backups))
	}
	for _, b := range backups {
		if b.Status != models.StatusCompleted || !slices.Contains(ip.SafetyBackupIDs, b.ID) || b.ConnectionID != "conn_a" {
			t.Fatalf("safety backup %+v", b)
		}
	}
	// The base of the new chain is tried: here backup encryption is off, which
	// base backups need, so the record says to take one.
	if ip.NewBaseID == "" && !strings.Contains(final.Warning, "base backup of the stream's new chain") {
		t.Fatalf("no base backup of the new chain was started (warning %q)", final.Warning)
	}
	ev := env.inPlaceEvent()
	if ev == nil || ev.Status != events.InPlacePITRSucceeded || !slices.Equal(ev.SafetyBackupIDs, ip.SafetyBackupIDs) || ev.RestoreID != rec.ID {
		t.Fatalf("restore.in_place_pitr = %+v", ev)
	}
	if d := env.eventsOf(events.SecurityDestructiveAction); len(d) != 1 || d[0].Action != string(models.ApprovalPITRInPlace) || d[0].RestoreID != rec.ID {
		t.Fatalf("destructive action events = %+v", d)
	}
}

func TestPITRInPlaceSafetyBackupFailureLeavesTheOriginals(t *testing.T) {
	env := newInPlaceEnv(t, true)
	rec, err := env.svc.StartRestore(admin(), pitrInPlace("shop"))
	if err != nil {
		t.Fatal(err)
	}
	final := env.wait(t, rec.ID)
	if final.Status != models.RestoreStatusFailed || !strings.Contains(final.ErrorMessage, "safety backup of shop failed") ||
		!strings.Contains(final.ErrorMessage, "untouched") {
		t.Fatalf("record = %s: %s", final.Status, final.ErrorMessage)
	}
	// Nothing touched the originals or the stream; the clones were dropped.
	if got, want := env.fakes.order(), []string{"restore", "post_restore_on_clones", "snapshot", "drop_clones"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if ip := final.PITR.InPlace; ip.SwapStarted || ip.StreamPaused || len(ip.Swapped) != 0 {
		t.Fatalf("in place = %+v", ip)
	}
	if ev := env.inPlaceEvent(); ev == nil || ev.Status != events.InPlacePITRFailed {
		t.Fatalf("restore.in_place_pitr = %+v", ev)
	}
}

// TestPITRInPlaceEmptyCloneLeavesTheOriginals: a clone that lists nothing after
// the restore (missing privileges, or a database absent at the target time)
// ends the run before the safety backup, the stream or the originals are
// touched.
func TestPITRInPlaceEmptyCloneLeavesTheOriginals(t *testing.T) {
	env := newInPlaceEnv(t, false)
	env.fakes.snapshotErr = errors.New("restore: the clone of an in-place point-in-time restore cannot be read completely: shop_rescue_x lists no collection")
	rec, err := env.svc.StartRestore(admin(), pitrInPlace("shop"))
	if err != nil {
		t.Fatal(err)
	}
	final := env.wait(t, rec.ID)
	if final.Status != models.RestoreStatusFailed || !strings.Contains(final.ErrorMessage, "lists no collection") ||
		!strings.Contains(final.ErrorMessage, "untouched") {
		t.Fatalf("record = %s: %s", final.Status, final.ErrorMessage)
	}
	if got, want := env.fakes.order(), []string{"restore", "post_restore_on_clones", "snapshot", "drop_clones"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if b := env.safetyBackups(t, rec.ID); len(b) != 0 {
		t.Fatalf("safety backups %d, want none", len(b))
	}
}

func TestPITRInPlaceSwapFailureKeepsTheCloneAndTheBackup(t *testing.T) {
	env := newInPlaceEnv(t, false)
	env.fakes.swapErr = errors.New("restore: replacing the original database with its clone failed: crm: mongorestore failed")
	rec, err := env.svc.StartRestore(admin(), pitrInPlace("shop", "crm"))
	if err != nil {
		t.Fatal(err)
	}
	final := env.wait(t, rec.ID)
	ip := final.PITR.InPlace
	if final.Status != models.RestoreStatusFailed || len(ip.SafetyBackupIDs) != 2 {
		t.Fatalf("record = %s: %s (%+v)", final.Status, final.ErrorMessage, ip)
	}
	// The clones are kept, the stream is enabled again.
	if got, want := env.fakes.order(), []string{"restore", "post_restore_on_clones", "snapshot", "pause", "swap", "resume"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for _, name := range append(slices.Clone(final.PITR.Clones), ip.SafetyBackupIDs...) {
		if !strings.Contains(final.ErrorMessage, name) {
			t.Fatalf("the message does not name %s: %s", name, final.ErrorMessage)
		}
	}
	if !strings.Contains(final.ErrorMessage, "shop already hold") || !ip.StreamResumed {
		t.Fatalf("message %q, in place %+v", final.ErrorMessage, ip)
	}
	if ev := env.inPlaceEvent(); ev == nil || ev.Status != events.InPlacePITRFailed || len(ev.SafetyBackupIDs) != 2 {
		t.Fatalf("restore.in_place_pitr = %+v", ev)
	}
}

func TestPITRInPlaceRefusals(t *testing.T) {
	env := newInPlaceEnv(t, false)
	unconfirmed := pitrInPlace("shop")
	unconfirmed.PITR.ConfirmInPlace = "crm"
	elsewhere := pitrInPlace("shop")
	elsewhere.TargetConnectionID = "conn_b"
	for name, c := range map[string]struct {
		ctx  context.Context
		req  models.RestoreRequest
		want error
	}{
		"operator":           {operator(), pitrInPlace("shop"), auth.ErrForbidden},
		"reader":             {readerCtx(), pitrInPlace("shop"), auth.ErrForbidden},
		"unconfirmed":        {admin(), unconfirmed, models.ErrPITRInPlaceNotConfirmed},
		"whole instance":     {admin(), func() models.RestoreRequest { r := pitrInPlace(); r.PITR.ConfirmInPlace = "shop"; return r }(), models.ErrPITRInPlaceDatabases},
		"another connection": {admin(), elsewhere, operations.ErrInvalid},
		"not in the base":    {admin(), pitrInPlace("later"), operations.ErrInvalid},
		"before the base": {admin(), func() models.RestoreRequest {
			r := pitrInPlace("shop")
			at := time.Unix(107, 0)
			r.PITR.At = &at
			return r
		}(), operations.ErrPITRNotRestorable},
	} {
		if _, err := env.svc.StartRestore(c.ctx, c.req); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	if got := env.fakes.order(); len(got) != 0 {
		t.Fatalf("a refused request ran %v", got)
	}

	// A backup of a replaced database holds the restore back.
	release, err := env.manager.Acquire(runs.BackupKey("conn_a", "shop"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.svc.StartRestore(admin(), pitrInPlace("shop")); !errors.Is(err, runs.ErrBusy) {
		t.Fatalf("busy: err = %v, want ErrBusy", err)
	}
	pre, err := env.svc.PreflightRestore(admin(), pitrInPlace("shop"))
	if err != nil {
		t.Fatal(err)
	}
	if c := pre.Check(models.PreflightCheckInPlace); c == nil || c.Status != models.PreflightFail || !strings.Contains(c.Message, "backup of shop") {
		t.Fatalf("in_place check = %+v", c)
	}
	release()
	pre, err = env.svc.PreflightRestore(admin(), pitrInPlace("shop"))
	if err != nil {
		t.Fatal(err)
	}
	if c := pre.Check(models.PreflightCheckInPlace); c == nil || c.Status != models.PreflightPass {
		t.Fatalf("in_place check = %+v", c)
	}
	if _, err := env.svc.PreflightRestore(operator(), pitrInPlace("shop")); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("an operator's in-place preflight: %v", err)
	}
}

// TestPITRInPlaceWaitsForASecondAdministrator checks the two-person rule: an
// in-place point-in-time restore waits for approval (pitr_in_place), and the
// approval starts the same request.
func TestPITRInPlaceWaitsForASecondAdministrator(t *testing.T) {
	env := newAdminEnv(t)
	seedPITRStream(t, env.st, "conn_ok")
	// The safety backups go to the stream's target: the environment's default.
	ctx := context.Background()
	def, err := env.targets.Resolve(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := env.st.GetStream(ctx, "str_a")
	if err != nil {
		t.Fatal(err)
	}
	stream.TargetID = def.ID
	if err = env.st.UpdateStream(ctx, stream); err != nil {
		t.Fatal(err)
	}
	fakes := &inPlaceFakes{pauseWas: true}
	cfg := env.cfg
	cfg.Users, cfg.SecondApproverCheck = env.auth, env.auth.CheckSecondApproverPossible
	cfg.PITR, cfg.PITRRestore, cfg.PITRBases, cfg.PITRStreams, cfg.PITRSwap = env.st, fakes, env.st.ListBaseBackups, fakes, fakes
	env.svc = operations.New(cfg)
	operations.SetNow(env.svc, env.clock.Now)

	_, err = env.svc.StartRestore(env.ctxOf(t, "alice"), pitrInPlace("shop"))
	a := pendingApproval(t, err)
	if a.Action != models.ApprovalPITRInPlace || a.Subject != "str_a" || a.Restore == nil || !a.Restore.PITR.IsInPlace() ||
		!strings.Contains(a.Summary, "shop") {
		t.Fatalf("request = %+v", a)
	}
	if got := fakes.order(); len(got) != 0 {
		t.Fatalf("the held restore ran %v", got)
	}
	done, err := env.svc.Approve(env.ctxOf(t, "bob"), a.ID)
	if err != nil || done.Status != models.ApprovalApproved || !strings.Contains(done.Result, "in-place point-in-time restore") {
		t.Fatalf("approve = %+v, %v", done, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !slices.Contains(fakes.order(), "resume") {
		if time.Now().After(deadline) {
			t.Fatalf("the approved restore did not finish: %v", fakes.order())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPITRInPlaceSwapSurvivesAShutdown is the regression test of a shutdown
// (SIGTERM, a high-availability step-down) killing mongorestore in the middle
// of --drop: the swap runs under its own context, marked critical, and the run
// completes although the run manager shut down meanwhile.
func TestPITRInPlaceSwapSurvivesAShutdown(t *testing.T) {
	env := newInPlaceEnv(t, false)
	env.fakes.swapStarted, env.fakes.swapGate = make(chan struct{}), make(chan struct{})
	rec, err := env.svc.StartRestore(admin(), pitrInPlace("shop"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-env.fakes.swapStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the swap did not start")
	}
	if n := env.manager.CriticalCount(); n != 1 {
		t.Fatalf("critical steps = %d during the swap, want 1", n)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- env.manager.Shutdown(context.Background()) }()
	select {
	case <-stopped:
		t.Fatal("the shutdown did not wait for the swap")
	case <-time.After(200 * time.Millisecond):
	}
	close(env.fakes.swapGate)
	if err = <-stopped; err != nil {
		t.Fatal(err)
	}
	final := env.wait(t, rec.ID)
	env.fakes.mu.Lock()
	ctxErr := env.fakes.swapCtxErr
	env.fakes.mu.Unlock()
	if ctxErr != nil || final.Status != models.RestoreStatusCompleted || env.manager.CriticalCount() != 0 {
		t.Fatalf("swap context %v, record %s (%s), critical %d", ctxErr, final.Status, final.ErrorMessage, env.manager.CriticalCount())
	}
	if !final.PITR.InPlace.StreamResumed || !strings.Contains(final.Warning, "during a shutdown") {
		t.Fatalf("stream resumed %v, warning %q", final.PITR.InPlace.StreamResumed, final.Warning)
	}
}

// interruptedInPlace is the record of an in-place restore of shop that a stop
// interrupted in stage, after storing that the stream was enabled.
func interruptedInPlace(stage string, paused bool) *models.RestoreRecord {
	const suffix = "_rescue_20261005_120000_ab12"
	return &models.RestoreRecord{ID: "rst_pitr_crash", Status: models.RestoreStatusInProgress, TargetConnectionID: "conn_a", InPlace: true,
		PITR: &models.PITRRestore{StreamID: "str_a", Databases: []string{"shop"}, CloneSuffix: suffix, Clones: []string{"shop" + suffix},
			InPlace: &models.PITRInPlace{Databases: []string{"shop"}, Stage: stage, StreamWasEnabled: true, StreamPaused: paused,
				SwapStarted: stage == models.PITRInPlaceSwapping || stage == models.PITRInPlaceResuming, SafetyBackupIDs: []string{"bkp_safety"}}}}
}

// TestInterruptedInPlaceResumesTheStream is the regression test of a stream left
// disabled by a crash between pausing it and enabling it again: whatever the
// stage, the next start enables it and starts a base of its new chain, and an
// enable that fails raises pitr.collector_failed.
func TestInterruptedInPlaceResumesTheStream(t *testing.T) {
	for _, c := range []struct {
		stage  string
		paused bool
	}{{models.PITRInPlacePausing, false}, {models.PITRInPlacePausing, true}, {models.PITRInPlaceSwapping, true}, {models.PITRInPlaceResuming, true}} {
		env := newInPlaceEnv(t, false)
		note := env.svc.CleanupInterruptedPITR(context.Background(), interruptedInPlace(c.stage, c.paused))
		if !strings.Contains(note, "str_a was enabled again") {
			t.Fatalf("%s: note %q", c.stage, note)
		}
		deadline := time.Now().Add(5 * time.Second)
		resumes := func() int {
			return len(slices.DeleteFunc(env.fakes.order(), func(c string) bool { return c != "resume" }))
		}
		for resumes() < 2 {
			if time.Now().After(deadline) {
				t.Fatalf("%s: the new chain was not awaited: %v", c.stage, env.fakes.order())
			}
			time.Sleep(10 * time.Millisecond)
		}
		swapping := c.stage == models.PITRInPlaceSwapping || c.stage == models.PITRInPlaceResuming
		if swapping {
			// The clone, the safety backup and the exact commands to finish or to
			// go back are named.
			for _, want := range []string{
				"--archive --db=shop_rescue_20261005_120000_ab12 | mongorestore",
				"--nsFrom='shop_rescue_20261005_120000_ab12.*' --nsTo='shop.*' --drop",
				`{"backup_id":"bkp_safety","target_connection_id":"conn_a","safe_clone":false,"confirm_in_place":true,"drop_target":true}`,
			} {
				if !strings.Contains(note, want) {
					t.Fatalf("a stop during the swap: the note lacks %q: %s", want, note)
				}
			}
		}
		if slices.Contains(env.fakes.order(), "drop_clones") == swapping {
			t.Fatalf("%s: clone handling %v", c.stage, env.fakes.order())
		}
	}

	// A stream that cannot be enabled again raises an alert.
	env := newInPlaceEnv(t, false)
	env.fakes.resumeErr = errors.New("the stream was deleted")
	note := env.svc.CleanupInterruptedPITR(context.Background(), interruptedInPlace(models.PITRInPlacePausing, false))
	if !strings.Contains(note, "could not be enabled again") {
		t.Fatalf("note %q", note)
	}
	alerts := env.eventsOf(events.PITRCollectorFailed)
	if len(alerts) != 1 || alerts[0].Stream != "str_a" || alerts[0].RestoreID != "rst_pitr_crash" {
		t.Fatalf("alerts = %+v", alerts)
	}

	// Still restoring into the clones: the stream was never paused.
	env = newInPlaceEnv(t, false)
	env.svc.CleanupInterruptedPITR(context.Background(), interruptedInPlace(models.PITRInPlaceRestoring, false))
	if slices.Contains(env.fakes.order(), "resume") {
		t.Fatalf("a stream that was never paused was resumed: %v", env.fakes.order())
	}
}
