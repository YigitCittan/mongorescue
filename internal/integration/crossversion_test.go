//go:build integration

package integration

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// envCrossSourceURI names the MongoDB the cross-version test backs up: an older
// release than the one MONGORESCUE_TEST_MONGO_URI points at, which receives the
// restore.
const envCrossSourceURI = "MONGORESCUE_TEST_CROSS_SOURCE_URI"

// TestCrossVersionRestore backs up the fidelity database (collection options, every
// index type and BSON type, a time-series collection and views) from an older
// MongoDB release (MONGORESCUE_TEST_CROSS_SOURCE_URI, such as 5.0 or 6.0) with the
// real mongodump and restores the archive into the safe clone of a newer one
// (MONGORESCUE_TEST_MONGO_URI, such as 8.0), then compares the clone with the
// source exactly like TestRestoreFidelity. The nightly workflow runs it for 5.0 and
// 6.0 into 8.0; it skips without a source server.
func TestCrossVersionRestore(t *testing.T) {
	src := connectMongo(t, envCrossSourceURI)
	dst := requireMongo(t)

	sMajor, sMinor := src.serverVersion(t)
	dMajor, dMinor := dst.serverVersion(t)
	t.Logf("backup from MongoDB %d.%d, restore into MongoDB %d.%d", sMajor, sMinor, dMajor, dMinor)
	if sMajor*100+sMinor >= dMajor*100+dMinor {
		t.Fatalf("the source (%d.%d) must be older than the target (%d.%d)", sMajor, sMinor, dMajor, dMinor)
	}

	f := seedFidelity(t, src, "xver")
	want := src.snapshotDB(t, f.Name)
	if len(want) < 9 {
		t.Fatalf("seeded snapshot has only %d collections: %v", len(want), want)
	}
	st := storageTargets(t)[0].Storage

	for _, gzip := range []bool{false, true} {
		t.Run(fmt.Sprintf("gzip=%v", gzip), func(t *testing.T) {
			bkp := mustBackup(t, src, st, models.BackupOptions{Database: f.Name, Gzip: gzip})
			rst := mustRestore(t, dst, st, models.RestoreRequest{}, bkp)
			if !strings.HasPrefix(rst.TargetDatabase, f.Name+"_rescue_") {
				t.Fatalf("restore target %q; want the safe clone %s_rescue_<timestamp>_<id>", rst.TargetDatabase, f.Name)
			}
			if dst.dbExists(t, f.Name) {
				t.Fatalf("the restore created %s on the target; it must only create the clone", f.Name)
			}
			assertSnapshotsEqual(t, want, withoutAutoTimeSeriesIndex(want, dst.snapshotDB(t, rst.TargetDatabase)))
		})
	}
}

// autoTimeSeriesIndex is the name of the index (metaField, timeField) that MongoDB
// 6.3 and later create on every time-series collection, including a restored one:
// the seeded collection has the meta field "sensor" and the time field "ts".
const autoTimeSeriesIndex = `"name":"sensor_1_ts_1"`

// withoutAutoTimeSeriesIndex returns got without the index the newer server adds to
// a time-series collection by itself, unless want has it too. It is the one known
// difference between releases; every other difference still fails the test.
func withoutAutoTimeSeriesIndex(want, got dbSnapshot) dbSnapshot {
	out := maps.Clone(got)
	for name, c := range out {
		if c.Type != "timeseries" {
			continue
		}
		hasIndex := func(idx string) bool { return strings.Contains(idx, autoTimeSeriesIndex) }
		if slices.ContainsFunc(want[name].Indexes, hasIndex) {
			continue
		}
		c.Indexes = slices.DeleteFunc(slices.Clone(c.Indexes), hasIndex)
		out[name] = c
	}
	return out
}
