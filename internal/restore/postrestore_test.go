package restore

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// fakeCommands records the post-restore commands the engine runs; fail fails the
// call with that (0-based) index.
type fakeCommands struct {
	mu    sync.Mutex
	dbs   []string
	docs  []string
	fail  int
	calls int
}

func (f *fakeCommands) RunPostRestoreCommand(_ context.Context, _, database string, command json.RawMessage) (models.PostRestoreCounts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dbs = append(f.dbs, database)
	f.docs = append(f.docs, string(command))
	f.calls++
	if f.calls-1 == f.fail {
		return models.PostRestoreCounts{}, errors.New("mongoconn: post-restore command failed: Unauthorized (code 13): not authorized")
	}
	return models.PostRestoreCounts{N: 2, Modified: 1}, nil
}

// auditRecorder collects the audited post-restore results.
type auditRecorder struct {
	mu      sync.Mutex
	entries []models.PostRestoreResult
	ids     []string
}

func (a *auditRecorder) record(_ context.Context, rec *models.RestoreRecord, res models.PostRestoreResult) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, res)
	a.ids = append(a.ids, rec.ID)
}

var erasures = []models.PostRestoreCommand{
	{Database: "*", Command: json.RawMessage(`{"delete": "users", "deletes": [{"q": {"_id": {"$in": [1, 2]}}, "limit": 0}]}`)},
	{Database: "db", Command: json.RawMessage(`{"update": "orders", "updates": [{"q": {"user": 1}, "u": {"$unset": {"address": ""}}, "multi": true}]}`)},
	{Database: "other", Command: json.RawMessage(`{"drop": "leads"}`)},
}

func postRestoreEngine(t *testing.T, cmds *fakeCommands, audit *auditRecorder, admin *fakeAdmin) (*Engine, *models.BackupRecord) {
	t.Helper()
	store := storage.NewMockStorage()
	src := plainBackup(t, store, []byte("archive-bytes"))
	opts := []Option{WithRunner((&capturingRunner{}).run), WithDatabaseAdmin(admin)}
	if cmds != nil {
		opts = append(opts, WithCommandRunner(cmds))
	}
	if audit != nil {
		opts = append(opts, WithCommandAudit(audit.record))
	}
	return NewEngine(store, "mongodb://localhost:27017", opts...), src
}

func TestPostRestoreCommandsRunAgainstTheCloneOnly(t *testing.T) {
	cmds, audit := &fakeCommands{fail: -1}, &auditRecorder{}
	engine, src := postRestoreEngine(t, cmds, audit, &fakeAdmin{})
	rec, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID, PostRestoreCommands: erasures}, src)
	if err != nil || rec.Status != models.RestoreStatusCompleted {
		t.Fatalf("restore: %v (%s)", err, rec.ErrorMessage)
	}
	clone := rec.TargetDatabase
	if clone == src.Database || !strings.HasPrefix(clone, "db_rescue_") {
		t.Fatalf("target %s is not a clone", clone)
	}
	// "*" and "db" run in the clone; "other" was not restored.
	if !slices.Equal(cmds.dbs, []string{clone, clone}) {
		t.Fatalf("commands ran in %q; want the clone %s twice", cmds.dbs, clone)
	}
	if slices.Contains(cmds.dbs, src.Database) {
		t.Fatalf("a command ran against the source database %s", src.Database)
	}
	if cmds.docs[0] != string(erasures[0].Command) || cmds.docs[1] != string(erasures[1].Command) {
		t.Fatalf("documents sent: %q", cmds.docs)
	}
	pr := rec.PostRestore
	if pr == nil || pr.Status != models.PostRestoreCompleted || len(pr.Commands) != 2 || len(pr.ClonesKept) != 0 {
		t.Fatalf("post_restore = %+v", pr)
	}
	first := pr.Commands[0]
	if first.Command != "delete" || first.Collection != "users" || first.Database != clone || first.SourceDatabase != "db" ||
		first.Status != models.PostRestoreCommandOK || first.N != 2 || first.Modified != 1 || first.Index != 0 {
		t.Fatalf("first result = %+v", first)
	}
	if pr.Commands[1].Index != 1 || pr.Commands[1].Command != "update" {
		t.Fatalf("second result = %+v", pr.Commands[1])
	}
	// Every executed command is audited, with its counts and never its document.
	if len(audit.entries) != 2 || audit.ids[0] != rec.ID || audit.entries[0].N != 2 {
		t.Fatalf("audited %+v for %v", audit.entries, audit.ids)
	}
	raw, _ := json.Marshal(rec)
	if strings.Contains(string(raw), "$unset") || strings.Contains(string(raw), `"$in"`) {
		t.Fatalf("the record must not hold command documents: %s", raw)
	}
}

func TestPostRestoreFailureFailsTheRestoreAndKeepsTheClone(t *testing.T) {
	cmds, audit, admin := &fakeCommands{fail: 0}, &auditRecorder{}, &fakeAdmin{}
	engine, src := postRestoreEngine(t, cmds, audit, admin)
	rec, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID, PostRestoreCommands: erasures}, src)
	if !errors.Is(err, ErrPostRestoreFailed) || rec.Status != models.RestoreStatusFailed {
		t.Fatalf("restore: %v, status %s; want ErrPostRestoreFailed and failed", err, rec.Status)
	}
	if len(admin.dropped) != 0 {
		t.Fatalf("the clone must be kept for inspection, dropped %v", admin.dropped)
	}
	if cmds.calls != 1 {
		t.Fatalf("%d commands ran after the first failed; want none", cmds.calls-1)
	}
	pr := rec.PostRestore
	if pr == nil || pr.Status != models.PostRestoreFailed || !slices.Equal(pr.ClonesKept, []string{rec.TargetDatabase}) {
		t.Fatalf("post_restore = %+v", pr)
	}
	if pr.Commands[0].Status != models.PostRestoreCommandFailed || !strings.Contains(pr.Commands[0].Error, "Unauthorized") ||
		pr.Commands[1].Status != models.PostRestoreCommandNotRun {
		t.Fatalf("results = %+v", pr.Commands)
	}
	for _, want := range []string{"post-restore command failed", "command 1 of 2 (delete on users in " + rec.TargetDatabase + ")", "kept for inspection", rec.TargetDatabase} {
		if !strings.Contains(rec.ErrorMessage, want) {
			t.Fatalf("message %q lacks %q", rec.ErrorMessage, want)
		}
	}
	if len(audit.entries) != 1 || audit.entries[0].Status != models.PostRestoreCommandFailed {
		t.Fatalf("audited %+v; want the failed command", audit.entries)
	}
}

func TestPostRestoreWithoutARunnerFails(t *testing.T) {
	admin := &fakeAdmin{}
	engine, src := postRestoreEngine(t, nil, nil, admin)
	rec, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID, PostRestoreCommands: erasures}, src)
	if !errors.Is(err, ErrPostRestoreFailed) || rec.Status != models.RestoreStatusFailed || len(admin.dropped) != 0 {
		t.Fatalf("restore without a command runner: %v, %s, dropped %v", err, rec.Status, admin.dropped)
	}
}

func TestPostRestoreSkippedInPlaceAndInDryRuns(t *testing.T) {
	cmds := &fakeCommands{fail: -1}
	engine, src := postRestoreEngine(t, cmds, nil, &fakeAdmin{})
	no := false

	rec, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID, SafeClone: &no, ConfirmInPlace: true,
		PostRestoreCommands: erasures}, src)
	if err != nil || rec.Status != models.RestoreStatusCompleted {
		t.Fatalf("in-place restore: %v", err)
	}
	if rec.PostRestore == nil || rec.PostRestore.Status != models.PostRestoreSkipped || !strings.Contains(rec.Warning, "re-apply them") {
		t.Fatalf("in place: post_restore %+v, warning %q", rec.PostRestore, rec.Warning)
	}

	rec, err = engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID, DryRun: true, PostRestoreCommands: erasures}, src)
	if err != nil || rec.Status != models.RestoreStatusCompleted {
		t.Fatalf("dry run: %v", err)
	}
	pr := rec.PostRestore
	if pr == nil || pr.Status != models.PostRestoreSkipped || len(pr.Commands) != 2 || pr.Commands[0].Status != models.PostRestoreCommandPlanned {
		t.Fatalf("dry run: post_restore %+v", pr)
	}
	if cmds.calls != 0 {
		t.Fatalf("%d commands ran in place or in a dry run", cmds.calls)
	}
}

func TestDeferredPostRestoreRunsThroughRunPostRestore(t *testing.T) {
	cmds := &fakeCommands{fail: -1}
	engine, src := postRestoreEngine(t, cmds, nil, &fakeAdmin{})
	req := models.RestoreRequest{BackupID: src.ID, PostRestoreCommands: erasures, DeferPostRestore: true}
	rec, err := engine.Run(context.Background(), req, src)
	if err != nil || rec.PostRestore != nil || cmds.calls != 0 {
		t.Fatalf("deferred: %v, %+v, %d calls", err, rec.PostRestore, cmds.calls)
	}
	rec.SourceDatabase = src.Database
	rec, err = engine.RunPostRestore(context.Background(), req, rec)
	if err != nil || rec.Status != models.RestoreStatusCompleted || rec.PostRestore.Status != models.PostRestoreCompleted {
		t.Fatalf("RunPostRestore: %v, %+v", err, rec.PostRestore)
	}
	if !slices.Equal(cmds.dbs, []string{rec.TargetDatabase, rec.TargetDatabase}) {
		t.Fatalf("ran in %q", cmds.dbs)
	}

	cmds.fail, cmds.calls = 0, 0
	rec.PostRestore = nil
	rec, err = engine.RunPostRestore(context.Background(), req, rec)
	if !errors.Is(err, ErrPostRestoreFailed) || rec.Status != models.RestoreStatusFailed || !strings.Contains(rec.ErrorMessage, "kept for inspection") {
		t.Fatalf("failing RunPostRestore: %v, %s, %q", err, rec.Status, rec.ErrorMessage)
	}
	// A failed record is left alone.
	cmds.calls = 0
	if _, err = engine.RunPostRestore(context.Background(), req, rec); err != nil || cmds.calls != 0 {
		t.Fatalf("RunPostRestore of a failed record: %v, %d calls", err, cmds.calls)
	}
}

func TestPITRCreatedMapsClonesToSources(t *testing.T) {
	info := &models.PITRRestore{CloneSuffix: "_rescue_20261006_101500_ab12", Clones: []string{"shop_rescue_20261006_101500_ab12", "crm_rescue_20261006_101500_ab12", "stray"}}
	got := pitrCreated(info)
	if len(got) != 2 || got["shop"] != "shop_rescue_20261006_101500_ab12" || got["crm"] != "crm_rescue_20261006_101500_ab12" {
		t.Fatalf("pitrCreated = %v", got)
	}
	if len(pitrCreated(&models.PITRRestore{Clones: []string{"x"}})) != 0 {
		t.Fatal("a restore without a suffix has no clones to map")
	}
}

func TestPITRPostRestoreRunsInEveryCloneAndKeepsThemOnFailure(t *testing.T) {
	f := newPITRFixture(t)
	for _, failing := range []bool{false, true} {
		r := &pitrRunner{applied: "applied 3 oplog entries"}
		admin := &pitrAdmin{names: []string{"shop", "other"}}
		cmds, audit := &fakeCommands{fail: -1}, &auditRecorder{}
		if failing {
			cmds.fail = 1
		}
		e := NewEngine(f.store, "", WithRunner(r.run), WithDecryptor(f.dec), WithDatabaseAdmin(admin), WithDatabaseLister(admin.list),
			WithCommandRunner(cmds), WithCommandAudit(audit.record))
		req := pitrRequest()
		req.PostRestoreCommands = []models.PostRestoreCommand{
			{Database: "*", Command: json.RawMessage(`{"delete": "users", "deletes": [{"q": {"_id": 1}, "limit": 0}]}`)},
		}
		rec, err := e.PreparePITR(req, f.run())
		if err != nil {
			t.Fatal(err)
		}
		suffix := rec.PITR.CloneSuffix
		rec, err = e.ExecutePITR(context.Background(), req, f.run(), rec)
		// "*" covers the clones the restore created, the one first seen in the
		// oplog included, in source name order; never a source database.
		want := []string{"other" + suffix, "shop" + suffix}
		if !slices.Equal(cmds.dbs, want) {
			t.Fatalf("failing=%v: commands ran in %q; want %q", failing, cmds.dbs, want)
		}
		if len(audit.entries) != 2 {
			t.Fatalf("failing=%v: audited %d commands", failing, len(audit.entries))
		}
		if !failing {
			if err != nil || rec.Status != models.RestoreStatusCompleted || rec.PostRestore.Status != models.PostRestoreCompleted {
				t.Fatalf("PITR with post-restore commands: %v, %+v", err, rec.PostRestore)
			}
			continue
		}
		if !errors.Is(err, ErrPostRestoreFailed) || rec.Status != models.RestoreStatusFailed {
			t.Fatalf("failing PITR post-restore: %v, %s", err, rec.Status)
		}
		if len(admin.dropped) != 0 || !slices.Equal(rec.PostRestore.ClonesKept, want) || !strings.Contains(rec.ErrorMessage, "kept for inspection") {
			t.Fatalf("clones must be kept: dropped %v, kept %v, message %q", admin.dropped, rec.PostRestore.ClonesKept, rec.ErrorMessage)
		}
	}
}
