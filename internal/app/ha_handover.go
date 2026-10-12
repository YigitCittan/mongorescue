package app

import (
	"context"

	"github.com/yigitcittan/mongorescue/internal/storage"
)

// prepareTakeover readies the data directory before the App opens as the active
// instance. The metadata handover is not implemented yet: the instance keeps its
// own database.
func (s *haSupervisor) prepareTakeover(context.Context) error { return nil }

// finalSnapshot writes the last metadata snapshot of a gracefully stopping active
// instance (not implemented yet).
func (s *haSupervisor) finalSnapshot(*App) {}

// checkSnapshotKey checks that this instance can read the cluster's metadata
// snapshots (not implemented yet).
func (s *haSupervisor) checkSnapshotKey(context.Context, storage.Storage, []byte) error { return nil }
