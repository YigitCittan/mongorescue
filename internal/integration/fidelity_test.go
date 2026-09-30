//go:build integration

package integration

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/restore"
)

// TestRestoreFidelity backs up a database exercising collection options (validator,
// capped, collation, time-series, views), every index type and every BSON type with
// the real mongodump, restores it into the default safe clone and compares the
// clone with the source: collections and options, normalised index specifications,
// document counts and a per-collection hash of canonical extended JSON in _id order.
// It runs with gzip and age encryption on and off, and with the collection filters.
func TestRestoreFidelity(t *testing.T) {
	env := requireMongo(t)
	f := seedFidelity(t, env, "fid")
	want := env.snapshotDB(t, f.Name)
	if len(want) < 9 {
		t.Fatalf("seeded snapshot has only %d collections: %v", len(want), want)
	}
	enc, dec := keyPair(t)

	type variant struct{ gzip, encrypted bool }
	for _, target := range storageTargets(t) {
		variants := []variant{{true, true}}
		if target.Name == "local" {
			variants = []variant{{false, false}, {true, false}, {false, true}, {true, true}}
		}
		for _, v := range variants {
			t.Run(fmt.Sprintf("%s/gzip=%v/encrypted=%v", target.Name, v.gzip, v.encrypted), func(t *testing.T) {
				var bOpts []backup.Option
				var rOpts []restore.Option
				if v.encrypted {
					bOpts = append(bOpts, backup.WithEncryptor(enc))
					rOpts = append(rOpts, restore.WithDecryptor(dec))
				}
				bkp := mustBackup(t, env, target.Storage, models.BackupOptions{Database: f.Name, Gzip: v.gzip}, bOpts...)
				if bkp.Encrypted != v.encrypted {
					t.Fatalf("record encrypted = %v; want %v", bkp.Encrypted, v.encrypted)
				}
				if bkp.SizeBytes < fidelityBlobs*fidelityBlobSize {
					t.Fatalf("archive of %d bytes cannot hold the %d blobs", bkp.SizeBytes, fidelityBlobs)
				}
				rst := mustRestore(t, env, target.Storage, models.RestoreRequest{}, bkp, rOpts...)
				defer env.dropDB(t, rst.TargetDatabase)
				if !strings.HasPrefix(rst.TargetDatabase, f.Name+"_rescue_") {
					t.Fatalf("restore target %q; want the safe clone %s_rescue_<timestamp>", rst.TargetDatabase, f.Name)
				}
				assertSnapshotsEqual(t, want, env.snapshotDB(t, rst.TargetDatabase))
			})
		}
	}

	st := storageTargets(t)[0].Storage // local
	included := []string{"orders", "types", "validated"}
	if f.TimeSeries {
		included = append(included, "metrics")
	}

	t.Run("backup includes several collections", func(t *testing.T) {
		bkp := mustBackup(t, env, st, models.BackupOptions{Database: f.Name, Gzip: true, Collections: included})
		if !slices.Equal(bkp.Collections, included) {
			t.Fatalf("record collections = %v; want %v", bkp.Collections, included)
		}
		rst := mustRestore(t, env, st, models.RestoreRequest{}, bkp)
		defer env.dropDB(t, rst.TargetDatabase)
		assertSnapshotsEqual(t, want.only(included...), env.snapshotDB(t, rst.TargetDatabase))
	})

	t.Run("backup includes one collection", func(t *testing.T) {
		bkp := mustBackup(t, env, st, models.BackupOptions{Database: f.Name, Collections: []string{"places"}})
		rst := mustRestore(t, env, st, models.RestoreRequest{}, bkp)
		defer env.dropDB(t, rst.TargetDatabase)
		assertSnapshotsEqual(t, want.only("places"), env.snapshotDB(t, rst.TargetDatabase))
	})

	t.Run("backup excludes collections", func(t *testing.T) {
		excluded := []string{"blobs", "capped", "big_orders"}
		bkp := mustBackup(t, env, st, models.BackupOptions{Database: f.Name, Gzip: true, ExcludeCollections: excluded})
		rst := mustRestore(t, env, st, models.RestoreRequest{}, bkp)
		defer env.dropDB(t, rst.TargetDatabase)
		assertSnapshotsEqual(t, want.without(excluded...), env.snapshotDB(t, rst.TargetDatabase))
	})

	t.Run("restore selects collections", func(t *testing.T) {
		bkp := mustBackup(t, env, st, models.BackupOptions{Database: f.Name, Gzip: true, ExcludeCollections: []string{"blobs"}})
		selected := []string{"types", "places", "collated"}
		rst := mustRestore(t, env, st, models.RestoreRequest{SelectedCollections: selected}, bkp)
		defer env.dropDB(t, rst.TargetDatabase)
		assertSnapshotsEqual(t, want.only(selected...), env.snapshotDB(t, rst.TargetDatabase))
	})

	// The safe-clone restores above never wrote to the source database.
	assertSnapshotsEqual(t, want, env.snapshotDB(t, f.Name))
}
