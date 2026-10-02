package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestVacuumIntoWritesAnOpenableSnapshot checks that the snapshot of a live database
// opens with the same secret key and carries its records, and that an existing
// destination is refused.
func TestVacuumIntoWritesAnOpenableSnapshot(t *testing.T) {
	ctx := context.Background()
	box := storetest.NewBox(t)
	s := storetest.OpenWithBox(t, filepath.Join(t.TempDir(), "mongorescue.db"), box)
	if err := s.SaveBackupRecord(ctx, &models.BackupRecord{ID: "b1", Database: "shop", Status: models.StatusCompleted}); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "snapshot.db")
	if err := s.VacuumInto(ctx, dest); err != nil {
		t.Fatal(err)
	}
	if err := s.VacuumInto(ctx, dest); !errors.Is(err, store.ErrSnapshotExists) {
		t.Fatalf("second VacuumInto = %v, want ErrSnapshotExists", err)
	}
	copyStore := storetest.OpenWithBox(t, dest, box)
	rec, err := copyStore.GetBackupRecord(ctx, "b1")
	if err != nil || rec.Database != "shop" {
		t.Fatalf("snapshot record = %+v, %v", rec, err)
	}
}
