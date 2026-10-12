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
	return nil
}

func (f *inPlaceFakes) SwapPITRClones(_ context.Context, _ models.RestoreRequest, rec *models.RestoreRecord, swapped func(string)) error {
	f.call("swap")
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
	want := []string{"restore", "post_restore_on_clones", "pause", "swap", "drop_clones", "resume"}
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
	if got, want := env.fakes.order(), []string{"restore", "post_restore_on_clones", "drop_clones"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if ip := final.PITR.InPlace; ip.SwapStarted || ip.StreamPaused || len(ip.Swapped) != 0 {
		t.Fatalf("in place = %+v", ip)
	}
	if ev := env.inPlaceEvent(); ev == nil || ev.Status != events.InPlacePITRFailed {
		t.Fatalf("restore.in_place_pitr = %+v", ev)
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
	if got, want := env.fakes.order(), []string{"restore", "post_restore_on_clones", "pause", "swap", "resume"}; !slices.Equal(got, want) {
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
