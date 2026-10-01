package integrity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
)

func (f *fixture) putObject(t *testing.T, key, data string) {
	t.Helper()
	if _, err := f.mem.Save(context.Background(), key, strings.NewReader(data)); err != nil {
		t.Fatal(err)
	}
}

func TestScanFindsOrphansAndMissing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var observed []int
	f.svc.cfg.ObserveScan = func(_ string, orphans, missing int, _ time.Time) { observed = []int{orphans, missing} }

	f.putBackup(t, "bkp_ok", "", f.now.Add(-48*time.Hour), []byte("ok"), nil)
	gone := f.putBackup(t, "bkp_gone", "", f.now.Add(-24*time.Hour), []byte("gone"), nil)
	if err := f.mem.Delete(ctx, gone.StorageKey); err != nil {
		t.Fatal(err)
	}
	// A backup completed after the listing started is not reported missing.
	fresh := f.putBackup(t, "bkp_fresh", "", f.now, []byte("fresh"), func(r *models.BackupRecord) {
		later := f.now.Add(time.Hour)
		r.CompletedAt = &later
	})
	_ = f.mem.Delete(ctx, fresh.StorageKey)
	// A running backup owns its (partial) archive.
	f.putBackup(t, "bkp_running", "", f.now, []byte("partial"), func(r *models.BackupRecord) { r.Status = models.StatusInProgress })
	// A pruned record whose archive could not be deleted: a leftover orphan.
	pruned := f.putBackup(t, "bkp_pruned", "", f.now.Add(-96*time.Hour), []byte("old"), func(r *models.BackupRecord) { r.Status = models.StatusPruned })
	f.putObject(t, "shop/2026/09/bkp_shop_20260901_030000_abcd.archive.gz", "orphan")
	f.putObject(t, ".mongorescue-probe-123", "probe")
	f.putObject(t, "notes.txt", "unrelated")

	report, err := f.svc.ScanTarget(ctx, "tgt_local", TriggerManual)
	if err != nil || report.Error != "" {
		t.Fatalf("scan = %+v, %v", report, err)
	}
	if report.Objects != 4 || report.Ignored != 2 || report.OrphanCount != 2 || report.MissingCount != 1 {
		t.Fatalf("report = %+v", report)
	}
	var orphanKeys []string
	for _, o := range report.Orphans {
		orphanKeys = append(orphanKeys, o.Key)
		if o.Key == pruned.StorageKey && (o.RecordID != pruned.ID || o.RecordStatus != "pruned") {
			t.Errorf("leftover orphan = %+v", o)
		}
	}
	slices.Sort(orphanKeys)
	if !slices.Equal(orphanKeys, []string{pruned.StorageKey, "shop/2026/09/bkp_shop_20260901_030000_abcd.archive.gz"}) {
		t.Fatalf("orphans = %v", orphanKeys)
	}
	if report.Missing[0].BackupID != gone.ID {
		t.Fatalf("missing = %+v", report.Missing)
	}
	if got := f.get(t, gone.ID); got.Status != models.StatusMissing || got.MissingSince == nil {
		t.Fatalf("the gone backup must be marked missing: %+v", got)
	}
	if got := f.get(t, fresh.ID); got.Status != models.StatusCompleted {
		t.Fatalf("a backup newer than the listing must stay completed: %s", got.Status)
	}
	if !slices.Equal(observed, []int{2, 1}) {
		t.Fatalf("observed = %v", observed)
	}
	if types := f.pub.types(); !slices.Equal(types, []events.EventType{events.DriftDetected}) {
		t.Fatalf("events = %v", types)
	}
	// Nothing was deleted.
	if _, err = f.mem.Stat(ctx, pruned.StorageKey); err != nil {
		t.Fatalf("a scan must never delete: %v", err)
	}
	if last, ok := f.svc.LastScan(ctx, "tgt_local"); !ok || last.OrphanCount != 2 {
		t.Fatalf("stored scan = %+v, %v", last, ok)
	}

	// The archive comes back: the next scan marks the backup completed again.
	f.putObject(t, gone.StorageKey, "gone")
	report, _ = f.svc.ScanTarget(ctx, "tgt_local", TriggerScheduled)
	if !slices.Equal(report.Recovered, []string{gone.ID}) || report.MissingCount != 0 {
		t.Fatalf("recovered = %+v", report)
	}
	if got := f.get(t, gone.ID); got.Status != models.StatusCompleted || got.MissingSince != nil {
		t.Fatalf("recovered backup = %+v", got)
	}

	if _, err = f.svc.ScanTarget(ctx, "nope", TriggerManual); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown target = %v", err)
	}
	ov, err := f.svc.Status(ctx)
	if err != nil || len(ov.Scans) != 1 || ov.NextScanAt == nil {
		t.Fatalf("overview = %+v, %v", ov, err)
	}
}

func TestImportOrphan(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	key := "shop/2026/09/bkp_shop_20260901_030000_abcd.archive.gz.age"
	f.putObject(t, key, "orphan-ciphertext")

	rec, err := f.svc.StartImport(ctx, "tgt_local", key)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID != "bkp_shop_20260901_030000_abcd" || rec.Status != models.StatusPending || !rec.Imported || !rec.Encrypted || rec.Database != "shop" {
		t.Fatalf("pending import = %+v", rec)
	}
	if !rec.StartedAt.Equal(time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("started at = %v", rec.StartedAt)
	}
	f.waitIdle(t)
	got := f.get(t, rec.ID)
	sum := sha256.Sum256([]byte("orphan-ciphertext"))
	if got.Status != models.StatusCompleted || got.SHA256 != hex.EncodeToString(sum[:]) || got.SizeBytes != 17 || got.Verification != "" {
		t.Fatalf("imported = %+v", got)
	}

	// The archive now belongs to a record: it is no orphan any more.
	if _, err = f.svc.StartImport(ctx, "tgt_local", key); !errors.Is(err, ErrNotOrphan) {
		t.Fatalf("second import = %v", err)
	}
	report, _ := f.svc.ScanTarget(ctx, "tgt_local", TriggerManual)
	if report.OrphanCount != 0 {
		t.Fatalf("after import = %+v", report)
	}

	// The same ID as an existing record gets a fresh one; a custom layout is accepted.
	f.putObject(t, "elsewhere/custom.archive", "custom")
	custom, err := f.svc.StartImport(ctx, "tgt_local", "elsewhere/custom.archive")
	if err != nil || custom.Database != "elsewhere" || !strings.HasPrefix(custom.ID, "bkp_elsewhere_") {
		t.Fatalf("custom import = %+v, %v", custom, err)
	}
	f.waitIdle(t)

	for _, bad := range []string{"notes.txt", "missing/2026/09/bkp_x.archive", "../escape.archive"} {
		if _, err := f.svc.StartImport(ctx, "tgt_local", bad); err == nil {
			t.Errorf("import of %q must fail", bad)
		}
	}
	if _, err := f.svc.StartImport(ctx, "tgt_local", "missing/2026/09/bkp_x.archive"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing object = %v", err)
	}
}

func TestImportFailureDetachesTheRecord(t *testing.T) {
	f := newFixture(t)
	key := "shop/2026/09/bkp_shop_20260901_030000_beef.archive"
	f.putObject(t, key, "data")
	rec, err := f.svc.importRecord(context.Background(), f.targets.targets["tgt_local"], key, &models.StorageObject{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.SaveBackupRecord(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	f.svc.failImport(context.Background(), rec, errors.New("read timeout"))
	got := f.get(t, rec.ID)
	if got.Status != models.StatusFailed || got.StorageKey != "" || !strings.Contains(got.ErrorMessage, "left in place") {
		t.Fatalf("failed import = %+v", got)
	}
	if _, err := f.mem.Stat(context.Background(), key); err != nil {
		t.Fatalf("the archive must stay: %v", err)
	}
}

func TestParseKey(t *testing.T) {
	info := parseKey("my_db/2026/10/bkp_my_db_20261001_080910_a1b2.archive.gz")
	if info.database != "my_db" || info.id != "bkp_my_db_20261001_080910_a1b2" || !info.gzip || info.encrypted ||
		!info.startedAt.Equal(time.Date(2026, 10, 1, 8, 9, 10, 0, time.UTC)) {
		t.Fatalf("info = %+v", info)
	}
	if info = parseKey("flat.archive"); info.database != "" || info.id != "" {
		t.Fatalf("flat = %+v", info)
	}
	for key, want := range map[string]bool{"a.archive": true, "a.archive.gz": true, "a.archive.gz.age": true, "a.archive.age": true, "a.gz": false, "a.archive.tmp": false} {
		if IsArchiveKey(key) != want {
			t.Errorf("IsArchiveKey(%q) != %v", key, want)
		}
	}
}
