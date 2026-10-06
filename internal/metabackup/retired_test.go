package metabackup_test

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/metabackup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// listingTargets is fakeTargets that can also list its target.
type listingTargets struct{ fakeTargets }

func (l *listingTargets) List(context.Context) ([]*models.StorageTarget, error) {
	return []*models.StorageTarget{l.target}, nil
}

// TestRetiredSnapshotsArePrunedAfterTheGracePeriod rotates secret.key (recording
// the old install ID), then checks that the old prefix keeps its snapshots during
// the delete grace period, loses exactly them afterwards, and that the callback runs
// once everything is pruned.
func TestRetiredSnapshotsArePrunedAfterTheGracePeriod(t *testing.T) {
	ctx := context.Background()
	oldID, newID := newInstallID(t), newInstallID(t)
	st := storetest.OpenWithBox(t, filepath.Join(t.TempDir(), "mongorescue.db"), storetest.NewBox(t))
	if _, err := st.RotateSecretBox(ctx, store.SecretKeyRotation{Next: storetest.NewBox(t), RetiredInstallID: oldID}); err != nil {
		t.Fatal(err)
	}
	drv := storage.NewMockStorage()
	oldPrefix := metabackup.Prefix + oldID + "/"
	for _, k := range []string{oldPrefix + "mongorescue-20261001T100000000Z.db.age", oldPrefix + "notes.txt",
		metabackup.Prefix + newID + "/mongorescue-20261002T100000000Z.db.age"} {
		if _, err := drv.Save(ctx, k, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().Add(time.Hour)
	var called time.Time
	cfg := settings.Defaults()
	svc := metabackup.New(metabackup.Config{
		InstallID: newID, Store: st, DataDir: t.TempDir(),
		Targets:         &listingTargets{fakeTargets{target: &models.StorageTarget{ID: "stg_1", Name: "primary"}, driver: drv}},
		Settings:        func() settings.Settings { return cfg },
		Encryptor:       func() *encryption.Encryptor { return nil },
		Logger:          slog.New(slog.DiscardHandler),
		Now:             func() time.Time { return now },
		OnRetiredPruned: func(_ context.Context, last time.Time) { called = last },
	})

	if err := svc.PruneRetired(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := drv.Stat(ctx, oldPrefix+"mongorescue-20261001T100000000Z.db.age"); err != nil || !called.IsZero() {
		t.Fatalf("pruned within the grace period: %v, callback %v", err, called)
	}

	now = now.Add(cfg.Security.DeleteGrace())
	if err := svc.PruneRetired(ctx); err != nil {
		t.Fatal(err)
	}
	objs, _ := drv.List(ctx, "")
	var keys []string
	for _, o := range objs {
		keys = append(keys, o.Key)
	}
	if len(keys) != 2 || !strings.Contains(strings.Join(keys, ","), "notes.txt") || !strings.Contains(strings.Join(keys, ","), newID) {
		t.Fatalf("objects after pruning = %v; want notes.txt and the new install's snapshot", keys)
	}
	if called.IsZero() {
		t.Fatal("OnRetiredPruned did not run")
	}
	list, _ := st.RetiredInstalls(ctx)
	if len(list) != 1 || list[0].PrunedAt == nil {
		t.Fatalf("retired installs = %+v", list)
	}
}
