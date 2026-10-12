package restore

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

const swapSuffix = "_rescue_20261005_120000_ab12"

// swapAdmin fakes the collections of the target server.
type swapAdmin struct {
	mu      sync.Mutex
	colls   map[string][]models.CollectionCount
	dropped []string
	// listErr fails the listing of these databases.
	listErr map[string]error
}

func (a *swapAdmin) CollectionCounts(_ context.Context, _, db string) ([]models.CollectionCount, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.listErr[db]; err != nil {
		return nil, err
	}
	return slices.Clone(a.colls[db]), nil
}

func (a *swapAdmin) DropCollection(_ context.Context, _, db, coll string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dropped = append(a.dropped, db+"."+coll)
	a.colls[db] = slices.DeleteFunc(a.colls[db], func(c models.CollectionCount) bool { return c.Name == coll })
	return nil
}

// restored copies the collections of clone over db, as mongorestore --drop does,
// leaving out skip.
func (a *swapAdmin) restored(clone, db, skip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.colls[clone] {
		if c.Name == skip {
			continue
		}
		a.colls[db] = slices.DeleteFunc(a.colls[db], func(o models.CollectionCount) bool { return o.Name == c.Name })
		a.colls[db] = append(a.colls[db], c)
	}
}

// swapTools fakes mongodump (an archive per clone) and mongorestore (it reads
// the archive, copies the clone in admin and fails for failDB).
type swapTools struct {
	mu       sync.Mutex
	admin    *swapAdmin
	dumps    [][]string
	restores [][]string
	read     []string
	failDB   string
	dumpFail bool
	// lose is a collection the copy leaves out.
	lose string
}

func (s *swapTools) dump(_ context.Context, name string, args ...string) (io.ReadCloser, io.Reader, func() error, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name != "mongodump" {
		return nil, nil, nil, errors.New("unexpected tool " + name)
	}
	s.dumps = append(s.dumps, args)
	var db string
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--db="); ok {
			db = v
		}
	}
	wait := func() error { return nil }
	if s.dumpFail {
		wait = func() error { return errors.New("exit status 1") }
	}
	return io.NopCloser(strings.NewReader("archive of " + db)), strings.NewReader("dump output"), wait, nil
}

func (s *swapTools) restore(_ context.Context, _ string, stdin io.Reader, args ...string) (io.Reader, func() error, error) {
	b, _ := io.ReadAll(stdin)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restores = append(s.restores, args)
	s.read = append(s.read, string(b))
	var from, to string
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--nsFrom="); ok {
			from = strings.TrimSuffix(v, ".*")
		}
		if v, ok := strings.CutPrefix(a, "--nsTo="); ok {
			to = strings.TrimSuffix(v, ".*")
		}
	}
	if s.failDB != "" && to == s.failDB {
		return strings.NewReader("Failed: connection lost"), func() error { return errors.New("exit status 1") }, nil
	}
	if s.admin != nil {
		s.admin.restored(from, to, s.lose)
	}
	return strings.NewReader("done"), func() error { return nil }, nil
}

func coll(name string, docs int64) models.CollectionCount {
	return models.CollectionCount{Name: name, Documents: docs}
}

// swapRecord is a completed in-place point-in-time restore of shop and crm.
func swapRecord() *models.RestoreRecord {
	return &models.RestoreRecord{ID: "rst_pitr_x", Status: models.RestoreStatusCompleted, PITR: &models.PITRRestore{
		Databases: []string{"shop", "crm"}, CloneSuffix: swapSuffix, Clones: []string{"shop" + swapSuffix, "crm" + swapSuffix},
		InPlace: &models.PITRInPlace{Databases: []string{"shop", "crm"}, Stage: models.PITRInPlaceSwapping},
	}}
}

// swapEnv is an engine over the fakes, with shop and crm, their clones, and
// shop holding a collection created after the target time.
func swapEnv(t *testing.T) (*Engine, *swapAdmin, *swapTools) {
	t.Helper()
	admin := &swapAdmin{colls: map[string][]models.CollectionCount{
		"shop" + swapSuffix: {coll("orders", 42), coll("users", 3), {Name: "recent", View: true}},
		"shop":              {coll("orders", 50), coll("users", 3), coll("later", 7)},
		"crm" + swapSuffix:  {coll("contacts", 9)},
		"crm":               {coll("contacts", 1)},
	}}
	tools := &swapTools{admin: admin}
	e := NewEngine(nil, "mongodb://u:pw@db/?replicaSet=rs0", WithRunner(tools.restore), WithDumpRunner(tools.dump),
		WithCollectionAdmin(admin), WithConfigDir(t.TempDir()))
	return e, admin, tools
}

func TestSwapPITRClones(t *testing.T) {
	e, admin, tools := swapEnv(t)
	rec := swapRecord()
	if err := e.SnapshotPITRClones(context.Background(), models.RestoreRequest{}, rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.PITR.InPlace.Snapshots) != 2 || len(rec.PITR.InPlace.Snapshots[0].Collections) != 3 {
		t.Fatalf("snapshots = %+v", rec.PITR.InPlace.Snapshots)
	}
	var swapped []string
	if err := e.SwapPITRClones(context.Background(), models.RestoreRequest{}, rec, func(db string) { swapped = append(swapped, db) }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(swapped, []string{"shop", "crm"}) {
		t.Fatalf("swapped = %v", swapped)
	}
	if len(tools.dumps) != 2 || len(tools.restores) != 2 {
		t.Fatalf("dumps %v, restores %v", tools.dumps, tools.restores)
	}
	dump, restore := tools.dumps[0], tools.restores[0]
	if !strings.HasPrefix(dump[0], "--config=") || !slices.Contains(dump, "--archive") || !slices.Contains(dump, "--db=shop"+swapSuffix) {
		t.Fatalf("mongodump args = %v", dump)
	}
	for _, want := range []string{"--archive", "--nsInclude=shop" + swapSuffix + ".*", "--nsFrom=shop" + swapSuffix + ".*", "--nsTo=shop.*", "--drop"} {
		if !slices.Contains(restore, want) {
			t.Fatalf("mongorestore args %v lack %s", restore, want)
		}
	}
	for _, a := range append(slices.Clone(dump), restore...) {
		if strings.Contains(a, "pw") || strings.Contains(a, "mongodb://") {
			t.Fatalf("a tool argument carries the URI: %q", a)
		}
	}
	if tools.read[0] != "archive of shop"+swapSuffix {
		t.Fatalf("mongorestore read %q, want mongodump's archive", tools.read[0])
	}
	// Only the collection the restored state does not have is dropped.
	if want := []string{"shop.later"}; !slices.Equal(admin.dropped, want) {
		t.Fatalf("dropped %v, want %v", admin.dropped, want)
	}
}

// TestSnapshotRefusesAnEmptyClone is the regression test of an empty clone
// listing (missing privileges, or a database absent at the target time): it must
// never lead to emptying the original.
func TestSnapshotRefusesAnEmptyClone(t *testing.T) {
	e, admin, tools := swapEnv(t)
	admin.colls["crm"+swapSuffix] = nil
	rec := swapRecord()
	err := e.SnapshotPITRClones(context.Background(), models.RestoreRequest{}, rec)
	if !errors.Is(err, ErrCloneUnreadable) || !strings.Contains(err.Error(), "crm"+swapSuffix) {
		t.Fatalf("err = %v, want ErrCloneUnreadable naming the clone", err)
	}
	admin.listErr = map[string]error{"shop" + swapSuffix: errors.New("not authorized")}
	if err = e.SnapshotPITRClones(context.Background(), models.RestoreRequest{}, swapRecord()); !errors.Is(err, ErrCloneUnreadable) {
		t.Fatalf("err = %v, want ErrCloneUnreadable", err)
	}
	// Without a snapshot the swap touches nothing either.
	if err = e.SwapPITRClones(context.Background(), models.RestoreRequest{}, swapRecord(), nil); !errors.Is(err, ErrCloneUnreadable) {
		t.Fatalf("swap without snapshot: err = %v", err)
	}
	if len(admin.dropped) != 0 || len(tools.dumps) != 0 || len(tools.restores) != 0 {
		t.Fatalf("dropped %v, dumps %v, restores %v; want nothing", admin.dropped, tools.dumps, tools.restores)
	}
}

// TestSwapLeavesTheOriginalWhenTheCloneListsDifferently: a clone that lists
// nothing (or anything else than its snapshot) at swap time leaves its original
// untouched.
func TestSwapLeavesTheOriginalWhenTheCloneListsDifferently(t *testing.T) {
	e, admin, tools := swapEnv(t)
	rec := swapRecord()
	if err := e.SnapshotPITRClones(context.Background(), models.RestoreRequest{}, rec); err != nil {
		t.Fatal(err)
	}
	admin.colls["shop"+swapSuffix] = nil
	err := e.SwapPITRClones(context.Background(), models.RestoreRequest{}, rec, nil)
	if !errors.Is(err, ErrSwapFailed) || !errors.Is(err, ErrCloneUnreadable) || !strings.Contains(err.Error(), "shop is untouched") {
		t.Fatalf("err = %v", err)
	}
	if len(admin.dropped) != 0 || len(tools.dumps) != 0 {
		t.Fatalf("dropped %v, dumps %v; want nothing", admin.dropped, tools.dumps)
	}
}

// TestSwapDropsNothingAfterAnIncompleteCopy: when the copy holds fewer
// collections than the clone, nothing of the original is dropped.
func TestSwapDropsNothingAfterAnIncompleteCopy(t *testing.T) {
	e, admin, tools := swapEnv(t)
	tools.lose = "users"
	admin.colls["shop"] = []models.CollectionCount{coll("orders", 50), coll("later", 7)}
	rec := swapRecord()
	if err := e.SnapshotPITRClones(context.Background(), models.RestoreRequest{}, rec); err != nil {
		t.Fatal(err)
	}
	err := e.SwapPITRClones(context.Background(), models.RestoreRequest{}, rec, nil)
	if !errors.Is(err, ErrSwapFailed) || !strings.Contains(err.Error(), "users is missing") || !strings.Contains(err.Error(), "nothing was dropped") {
		t.Fatalf("err = %v", err)
	}
	if len(admin.dropped) != 0 {
		t.Fatalf("dropped %v after an incomplete copy", admin.dropped)
	}
}

func TestSwapPITRClonesFailure(t *testing.T) {
	e, admin, tools := swapEnv(t)
	tools.failDB = "crm"
	rec := swapRecord()
	if err := e.SnapshotPITRClones(context.Background(), models.RestoreRequest{}, rec); err != nil {
		t.Fatal(err)
	}
	var swapped []string
	err := e.SwapPITRClones(context.Background(), models.RestoreRequest{}, rec, func(db string) { swapped = append(swapped, db) })
	if !errors.Is(err, ErrSwapFailed) || !strings.Contains(err.Error(), "crm") {
		t.Fatalf("err = %v, want ErrSwapFailed naming crm", err)
	}
	if !slices.Equal(swapped, []string{"shop"}) || slices.ContainsFunc(admin.dropped, func(d string) bool { return strings.HasPrefix(d, "crm.") }) {
		t.Fatalf("swapped %v, dropped %v; want shop swapped and nothing of crm dropped", swapped, admin.dropped)
	}

	// A failing mongodump fails the swap too.
	e, _, tools = swapEnv(t)
	tools.dumpFail = true
	rec = swapRecord()
	if err = e.SnapshotPITRClones(context.Background(), models.RestoreRequest{}, rec); err != nil {
		t.Fatal(err)
	}
	if err = e.SwapPITRClones(context.Background(), models.RestoreRequest{}, rec, nil); !errors.Is(err, ErrSwapFailed) ||
		!strings.Contains(err.Error(), "mongodump") {
		t.Fatalf("err = %v, want a failed mongodump", err)
	}

	// Without a collection admin nothing runs.
	if err = NewEngine(nil, "mongodb://db").SwapPITRClones(context.Background(), models.RestoreRequest{}, swapRecord(), nil); err == nil {
		t.Fatal("a swap ran without a collection admin")
	}
}
