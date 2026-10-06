package restore

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// TestSameSecondSafeClonesBothSucceed restores one database twice with a clock
// stopped in one second: the clones get different names, and the second restore is
// not refused because the first clone exists.
func TestSameSecondSafeClonesBothSucceed(t *testing.T) {
	store := storage.NewMockStorage()
	src := plainBackup(t, store, []byte("archive-bytes"))
	at := time.Date(2026, 10, 6, 9, 30, 5, 0, time.UTC)
	admin := &fakeAdmin{exists: map[string]bool{}}
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner((&capturingRunner{}).run), WithDatabaseAdmin(admin),
		WithClock(func() time.Time { return at }), WithCloneID(fixedCloneIDs("ab12", "cd34")))

	var names []string
	for range 2 {
		rec, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID}, src)
		if err != nil || rec.Status != models.RestoreStatusCompleted {
			t.Fatalf("restore %d: %v (%+v)", len(names)+1, err, rec)
		}
		admin.exists[rec.TargetDatabase] = true // the clone now exists
		names = append(names, rec.TargetDatabase)
	}
	want := []string{"db_rescue_20261006_093005_ab12", "db_rescue_20261006_093005_cd34"}
	if names[0] != want[0] || names[1] != want[1] {
		t.Fatalf("clones %q; want %q", names, want)
	}
}

// TestPrepareNamesSafeClonesRandomly checks the default clone IDs (crypto/rand) and
// that in-place restores and named clones draw none.
func TestPrepareNamesSafeClonesRandomly(t *testing.T) {
	at := time.Date(2026, 10, 6, 9, 30, 5, 0, time.UTC)
	engine := NewEngine(storage.NewMockStorage(), "mongodb://h", WithClock(func() time.Time { return at }))
	src := &models.BackupRecord{ID: "bkp_1", Database: "shop"}
	rec, err := engine.Prepare(models.RestoreRequest{BackupID: "bkp_1"}, src)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^shop_rescue_20261006_093005_[0-9a-f]{4}$`).MatchString(rec.TargetDatabase) || !rec.StartedAt.Equal(at) {
		t.Fatalf("clone %q started %s", rec.TargetDatabase, rec.StartedAt)
	}

	engine = NewEngine(storage.NewMockStorage(), "mongodb://h", WithCloneID(func() (string, error) { return "", errors.New("no entropy") }))
	if _, err = engine.Prepare(models.RestoreRequest{BackupID: "bkp_1"}, src); err == nil {
		t.Fatal("a failed clone ID must fail the restore")
	}
	no := false
	if rec, err = engine.Prepare(models.RestoreRequest{BackupID: "bkp_1", SafeClone: &no, ConfirmInPlace: true}, src); err != nil || rec.TargetDatabase != "shop" {
		t.Fatalf("in place = %+v, %v", rec, err)
	}
	if rec, err = engine.Prepare(models.RestoreRequest{BackupID: "bkp_1", CloneDatabase: "shop_rescue_verify_x"}, src); err != nil || rec.TargetDatabase != "shop_rescue_verify_x" {
		t.Fatalf("named clone = %+v, %v", rec, err)
	}
}

// fixedCloneIDs returns a clone ID source that hands out ids in order.
func fixedCloneIDs(ids ...string) func() (string, error) {
	return func() (string, error) {
		if len(ids) == 0 {
			return "", errors.New("no more clone ids")
		}
		id := ids[0]
		ids = ids[1:]
		return id, nil
	}
}
