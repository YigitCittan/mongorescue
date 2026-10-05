package app

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// sweepAdmin records the databases a restart drops.
type sweepAdmin struct {
	mu      sync.Mutex
	dropped []string
}

func (a *sweepAdmin) DatabaseExists(context.Context, string, string) (bool, error) { return false, nil }

func (a *sweepAdmin) DropDatabase(_ context.Context, _, db string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dropped = append(a.dropped, db)
	return nil
}

// sweepConnections serves the target connection of the interrupted restores.
type sweepConnections struct{}

func (sweepConnections) Resolve(context.Context, string) (*models.Connection, error) {
	return &models.Connection{ID: "conn_a", Name: "rs", URI: "mongodb://u:pw@db/?replicaSet=rs0"}, nil
}

func (c sweepConnections) Get(ctx context.Context, id string) (*models.Connection, error) {
	return c.Resolve(ctx, id)
}

func (sweepConnections) List(context.Context) ([]*models.Connection, error) { return nil, nil }

// TestRestartDropsTheRecordedClonesOfInterruptedPITRRuns simulates a restart after
// a point-in-time restore and a chain test were interrupted: the new process drops
// exactly the clones each record names and says so in its message.
func TestRestartDropsTheRecordedClonesOfInterruptedPITRRuns(t *testing.T) {
	ctx := context.Background()
	fs := storetest.New(t)
	user := &models.RestoreRecord{ID: "rst_pitr_user", TargetConnectionID: "conn_a", Status: models.RestoreStatusInProgress,
		PITR: &models.PITRRestore{StreamID: "str_a", CloneSuffix: "_rescue_20261005_120000_ab12",
			Clones: []string{"shop_rescue_20261005_120000_ab12", "crm_rescue_20261005_120000_ab12"}}}
	chain := &models.RestoreRecord{ID: "rst_pitr_chain", TargetConnectionID: "conn_a", Status: models.RestoreStatusInProgress,
		PITR: &models.PITRRestore{StreamID: "str_a", ChainTest: true, CloneSuffix: "_rescue_cvtm0001cd34",
			Clones: []string{"shop_rescue_cvtm0001cd34"}}}
	plain := &models.RestoreRecord{ID: "rst_plain", TargetDatabase: "shop_rescue_20261005_120000", Status: models.RestoreStatusInProgress}
	for _, r := range []*models.RestoreRecord{user, chain, plain} {
		if err := fs.SaveRestoreRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	// The new process.
	adminDB := &sweepAdmin{}
	mock := storage.NewMockStorage()
	engine := restore.NewEngine(mock, "", restore.WithDatabaseAdmin(adminDB))
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	ops := operations.New(operations.Config{Store: fs, Backup: backup.NewEngine(mock, ""), Restore: engine, Runs: manager,
		Connections: sweepConnections{}, PITRRestore: engine})
	a := &App{metaStore: fs, logger: slog.Default(), cleanupPITR: ops.CleanupInterruptedPITR}
	a.failInterruptedRuns(ctx)

	slices.Sort(adminDB.dropped)
	want := []string{"crm_rescue_20261005_120000_ab12", "shop_rescue_20261005_120000_ab12", "shop_rescue_cvtm0001cd34"}
	if !slices.Equal(adminDB.dropped, want) {
		t.Fatalf("dropped %v, want exactly the recorded clones %v", adminDB.dropped, want)
	}
	for _, id := range []string{user.ID, chain.ID} {
		got, err := fs.GetRestoreRecord(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != models.RestoreStatusFailed || !strings.Contains(got.ErrorMessage, "interrupted") || !strings.Contains(got.ErrorMessage, "were dropped") {
			t.Fatalf("%s: %s %q", id, got.Status, got.ErrorMessage)
		}
	}
	if got, _ := fs.GetRestoreRecord(ctx, plain.ID); got.Status != models.RestoreStatusFailed || strings.Contains(got.ErrorMessage, "dropped") {
		t.Fatalf("a database restore: %s %q", got.Status, got.ErrorMessage)
	}
}
