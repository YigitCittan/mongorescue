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

// swapAdmin fakes the collections of the target server.
type swapAdmin struct {
	mu      sync.Mutex
	colls   map[string][]string
	dropped []string
}

func (a *swapAdmin) CollectionNames(_ context.Context, _, db string) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.colls[db]), nil
}

func (a *swapAdmin) DropCollection(_ context.Context, _, db, coll string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dropped = append(a.dropped, db+"."+coll)
	return nil
}

// swapTools fakes mongodump (an archive per clone) and mongorestore (it reads
// the archive and fails for failDB).
type swapTools struct {
	mu       sync.Mutex
	dumps    [][]string
	restores [][]string
	read     []string
	failDB   string
	dumpFail bool
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
	if s.failDB != "" && slices.Contains(args, "--nsTo="+s.failDB+".*") {
		return strings.NewReader("Failed: connection lost"), func() error { return errors.New("exit status 1") }, nil
	}
	return strings.NewReader("done"), func() error { return nil }, nil
}

// swapRecord is a completed in-place point-in-time restore of shop and crm.
func swapRecord() *models.RestoreRecord {
	const suffix = "_rescue_20261005_120000_ab12"
	return &models.RestoreRecord{ID: "rst_pitr_x", Status: models.RestoreStatusCompleted, PITR: &models.PITRRestore{
		Databases: []string{"shop", "crm"}, CloneSuffix: suffix, Clones: []string{"shop" + suffix, "crm" + suffix},
		InPlace: &models.PITRInPlace{Databases: []string{"shop", "crm"}, Stage: models.PITRInPlaceSwapping},
	}}
}

func TestSwapPITRClones(t *testing.T) {
	const suffix = "_rescue_20261005_120000_ab12"
	admin := &swapAdmin{colls: map[string][]string{
		"shop" + suffix: {"orders", "users"},
		"shop":          {"orders", "users", "later", "system.views"},
		// crm was dropped before the target time: its clone does not exist.
		"crm": {"contacts"},
	}}
	tools := &swapTools{}
	e := NewEngine(nil, "mongodb://u:pw@db/?replicaSet=rs0", WithRunner(tools.restore), WithDumpRunner(tools.dump),
		WithCollectionAdmin(admin), WithConfigDir(t.TempDir()))
	var swapped []string
	if err := e.SwapPITRClones(context.Background(), models.RestoreRequest{}, swapRecord(), func(db string) { swapped = append(swapped, db) }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(swapped, []string{"shop", "crm"}) {
		t.Fatalf("swapped = %v", swapped)
	}
	if len(tools.dumps) != 1 || len(tools.restores) != 1 {
		t.Fatalf("dumps %v, restores %v; want one of each (crm has no clone)", tools.dumps, tools.restores)
	}
	dump, restore := tools.dumps[0], tools.restores[0]
	if !strings.HasPrefix(dump[0], "--config=") || !slices.Contains(dump, "--archive") || !slices.Contains(dump, "--db=shop"+suffix) {
		t.Fatalf("mongodump args = %v", dump)
	}
	for _, want := range []string{"--archive", "--nsInclude=shop" + suffix + ".*", "--nsFrom=shop" + suffix + ".*", "--nsTo=shop.*", "--drop"} {
		if !slices.Contains(restore, want) {
			t.Fatalf("mongorestore args %v lack %s", restore, want)
		}
	}
	for _, a := range append(slices.Clone(dump), restore...) {
		if strings.Contains(a, "pw") || strings.Contains(a, "mongodb://") {
			t.Fatalf("a tool argument carries the URI: %q", a)
		}
	}
	if tools.read[0] != "archive of shop"+suffix {
		t.Fatalf("mongorestore read %q, want mongodump's archive", tools.read[0])
	}
	// The collections the restored state does not have are dropped; system
	// collections are left alone.
	if want := []string{"shop.later", "crm.contacts"}; !slices.Equal(admin.dropped, want) {
		t.Fatalf("dropped %v, want %v", admin.dropped, want)
	}
}

func TestSwapPITRClonesFailure(t *testing.T) {
	const suffix = "_rescue_20261005_120000_ab12"
	admin := &swapAdmin{colls: map[string][]string{"shop" + suffix: {"orders"}, "crm" + suffix: {"contacts"}, "shop": {"orders"}, "crm": {"contacts", "x"}}}
	tools := &swapTools{failDB: "crm"}
	e := NewEngine(nil, "mongodb://db/?replicaSet=rs0", WithRunner(tools.restore), WithDumpRunner(tools.dump),
		WithCollectionAdmin(admin), WithConfigDir(t.TempDir()))
	var swapped []string
	err := e.SwapPITRClones(context.Background(), models.RestoreRequest{}, swapRecord(), func(db string) { swapped = append(swapped, db) })
	if !errors.Is(err, ErrSwapFailed) || !strings.Contains(err.Error(), "crm") {
		t.Fatalf("err = %v, want ErrSwapFailed naming crm", err)
	}
	if !slices.Equal(swapped, []string{"shop"}) || len(admin.dropped) != 0 {
		t.Fatalf("swapped %v, dropped %v; want shop swapped and nothing of crm dropped", swapped, admin.dropped)
	}

	// A failing mongodump fails the swap too.
	tools = &swapTools{dumpFail: true}
	e = NewEngine(nil, "mongodb://db/?replicaSet=rs0", WithRunner(tools.restore), WithDumpRunner(tools.dump),
		WithCollectionAdmin(admin), WithConfigDir(t.TempDir()))
	if err := e.SwapPITRClones(context.Background(), models.RestoreRequest{}, swapRecord(), nil); !errors.Is(err, ErrSwapFailed) ||
		!strings.Contains(err.Error(), "mongodump") {
		t.Fatalf("err = %v, want a failed mongodump", err)
	}

	// Without a collection admin nothing runs.
	if err := NewEngine(nil, "mongodb://db").SwapPITRClones(context.Background(), models.RestoreRequest{}, swapRecord(), nil); err == nil {
		t.Fatal("a swap ran without a collection admin")
	}
}
