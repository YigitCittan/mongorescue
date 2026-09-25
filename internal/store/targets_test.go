package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

func TestStorageTargetsRepository(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.OpenWithBox(t, path, testBox)
	ctx := context.Background()
	now := time.Now().UTC()
	s3 := &models.StorageTarget{ID: "stg_b", Name: "bucket", Type: models.StorageS3, IsDefault: true, CreatedAt: now, UpdatedAt: now,
		S3: &models.S3Target{Bucket: "bkt", AccessKeyID: "AK", SecretAccessKey: "s3-secret-value"}}
	local := &models.StorageTarget{ID: "stg_a", Name: "disk", Type: models.StorageLocal, Local: &models.LocalTarget{Path: "/b"}, CreatedAt: now, UpdatedAt: now}
	for _, tg := range []*models.StorageTarget{s3, local} {
		if err := s.CreateStorageTarget(ctx, tg); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateStorageTarget(ctx, local); err == nil {
		t.Fatal("creating an existing target must fail (create is not an upsert)")
	}
	if raw := rawData(t, path, "storage_targets"); strings.Contains(raw, "s3-secret-value") || !strings.Contains(raw, secretbox.Prefix) {
		t.Fatalf("secret key not sealed: %s", raw)
	}
	got, err := s.GetStorageTarget(ctx, "stg_b")
	if err != nil || got.S3.SecretAccessKey != "s3-secret-value" || got.IsDefault {
		t.Fatalf("GetStorageTarget = %+v, %v; the default flag is only set by SetDefaultStorageTarget", got, err)
	}
	if !got.UpdatedAt.Equal(now) {
		t.Fatalf("updated_at = %v; want %v", got.UpdatedAt, now)
	}
	if err = s.SetDefaultStorageTarget(ctx, "stg_a"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetDefaultStorageTarget(ctx, "stg_b"); err != nil {
		t.Fatal(err)
	}
	// Updating keeps the stored default flag.
	read := got.UpdatedAt
	got.Name = "renamed"
	got.IsDefault = false
	got.UpdatedAt = read.Add(time.Second)
	if err = s.UpdateStorageTarget(ctx, got, read, false); err != nil {
		t.Fatal(err)
	}
	// A second update based on the old revision is a conflict.
	got.Name = "stale"
	if err = s.UpdateStorageTarget(ctx, got, read, false); !errors.Is(err, targets.ErrConflict) {
		t.Fatalf("stale update = %v; want ErrConflict", err)
	}
	list, err := s.ListStorageTargets(ctx)
	if err != nil || len(list) != 2 || list[0].ID != "stg_a" || list[0].IsDefault || !list[1].IsDefault || list[1].Name != "renamed" {
		t.Fatalf("ListStorageTargets = %+v, %v", list, err)
	}
	if _, err := s.GetStorageTarget(ctx, "nope"); !errors.Is(err, targets.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	if err := s.SetDefaultStorageTarget(ctx, "nope"); !errors.Is(err, targets.ErrNotFound) {
		t.Fatalf("SetDefault(missing) = %v", err)
	}
	if err := s.DeleteStorageTarget(ctx, "nope"); !errors.Is(err, targets.ErrNotFound) {
		t.Fatalf("Delete(missing) = %v", err)
	}
	if err := s.DeleteStorageTarget(ctx, "stg_b"); !errors.Is(err, targets.ErrIsDefault) {
		t.Fatalf("Delete(default) = %v; want ErrIsDefault", err)
	}
	// A planted ciphertext of another target does not open.
	if _, err := rawDB(t, path).Exec(`UPDATE storage_targets SET data = json_set(data, '$.s3', json_object('bucket', 'x',
		'secret_access_key', (SELECT json_extract(data, '$.s3.secret_access_key') FROM storage_targets WHERE id = 'stg_b'))) WHERE id = 'stg_a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetStorageTarget(ctx, "stg_a"); !errors.Is(err, secretbox.ErrDecrypt) {
		t.Fatalf("swapped secret = %v; want ErrDecrypt", err)
	}
}

func TestAssignStorageTargetBackfillsOldRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.OpenWithBox(t, path, testBox)
	ctx := context.Background()
	if err := s.SaveJob(ctx, &models.Job{ID: "j_old", Name: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveJob(ctx, &models.Job{ID: "j_new", Name: "new", StorageTargetID: "stg_other"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveBackupRecord(ctx, &models.BackupRecord{ID: "b_old", Status: models.StatusCompleted}); err != nil {
		t.Fatal(err)
	}
	tg := &models.StorageTarget{ID: "stg_def", Name: "Local disk", Type: models.StorageLocal}
	jobs, backups, err := s.AssignStorageTarget(ctx, tg)
	if err != nil || jobs != 1 || backups != 1 {
		t.Fatalf("AssignStorageTarget = %d, %d, %v", jobs, backups, err)
	}
	j, _ := s.GetJob(ctx, "j_old")
	b, _ := s.GetBackupRecord(ctx, "b_old")
	other, _ := s.GetJob(ctx, "j_new")
	if j.StorageTargetID != "stg_def" || j.StorageType != models.StorageLocal || b.StorageTargetID != "stg_def" ||
		b.StorageTargetName != "Local disk" || other.StorageTargetID != "stg_other" {
		t.Fatalf("after backfill: %+v / %+v / %+v", j, b, other)
	}
	if jobs, backups, _ := s.AssignStorageTarget(ctx, tg); jobs != 0 || backups != 0 {
		t.Fatal("second backfill must be a no-op")
	}
	// The column is kept in sync, so the target cannot be deleted while referenced.
	if err := s.CreateStorageTarget(ctx, tg); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteStorageTarget(ctx, "stg_def"); !errors.Is(err, targets.ErrInUse) {
		t.Fatalf("delete backfilled target = %v", err)
	}
}

func TestSettingsRepositorySealsSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.OpenWithBox(t, path, testBox)
	ctx := context.Background()
	values := map[string]string{
		settings.KeyBackupTimeout:        `"2h0m0s"`,
		settings.KeyEncryptionPassphrase: `"very secret passphrase"`,
		"legacy_import.X":                `"2026-01-01T00:00:00Z"`,
	}
	if err := s.SaveSettings(ctx, values); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSettings(ctx, map[string]string{"unknown.key": "1"}); err == nil {
		t.Fatal("unknown keys must be refused")
	}
	var raw string
	if err := rawDB(t, path).QueryRow("SELECT group_concat(value) FROM settings").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "very secret passphrase") || !strings.Contains(raw, "2h0m0s") {
		t.Fatalf("settings at rest: %s", raw)
	}
	got, err := s.LoadSettings(ctx)
	if err != nil || len(got) != 3 || got[settings.KeyEncryptionPassphrase] != values[settings.KeyEncryptionPassphrase] {
		t.Fatalf("LoadSettings = %v, %v (the key check value must be skipped)", got, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Sealed settings without a key check value make a new key a mismatch.
	if _, err := rawDB(t, path).Exec("DELETE FROM settings WHERE key = 'secret_key_check'"); err != nil {
		t.Fatal(err)
	}
	if _, err := openWith(t, path, storetest.NewBox(t)); !errors.Is(err, secretbox.ErrSecretKeyMismatch) {
		t.Fatalf("sealed settings without key check = %v", err)
	}
}

func TestStorageTargetUpdatesNeverResurrect(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	v1 := time.Date(2026, 9, 25, 10, 0, 0, 123456789, time.UTC)
	tg := &models.StorageTarget{ID: "stg_x", Name: "x", Type: models.StorageLocal, Local: &models.LocalTarget{Path: "/b"}, CreatedAt: v1, UpdatedAt: v1}
	if err := s.CreateStorageTarget(ctx, tg); err != nil {
		t.Fatal(err)
	}
	// A test result is recorded on the tested revision only.
	if ok, err := s.RecordStorageTargetTest(ctx, "stg_x", v1, v1.Add(time.Minute), true, ""); err != nil || !ok {
		t.Fatalf("record test = %v, %v", ok, err)
	}
	got, _ := s.GetStorageTarget(ctx, "stg_x")
	if got.LastTestAt == nil || !got.LastTestAt.Equal(v1.Add(time.Minute)) || !got.LastTestOK || !got.UpdatedAt.Equal(v1) {
		t.Fatalf("after test = %+v", got)
	}
	if ok, _ := s.RecordStorageTargetTest(ctx, "stg_x", v1.Add(time.Second), v1, false, "x"); ok {
		t.Fatal("a result for another revision must be ignored")
	}
	// Deleted meanwhile: neither a test result nor an update recreates it.
	if err := s.DeleteStorageTarget(ctx, "stg_x"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.RecordStorageTargetTest(ctx, "stg_x", v1, v1, false, "boom"); err != nil || ok {
		t.Fatalf("record test on a deleted target = %v, %v", ok, err)
	}
	tg.UpdatedAt = v1.Add(time.Hour)
	if err := s.UpdateStorageTarget(ctx, tg, v1, false); !errors.Is(err, targets.ErrNotFound) {
		t.Fatalf("update of a deleted target = %v; want ErrNotFound", err)
	}
	if list, _ := s.ListStorageTargets(ctx); len(list) != 0 {
		t.Fatalf("deleted target resurrected: %+v", list)
	}
}

func TestStorageTargetLocationIsLockedWhileItHoldsBackups(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	v1 := time.Now().UTC()
	tg := &models.StorageTarget{ID: "stg_l", Name: "l", Type: models.StorageLocal, Local: &models.LocalTarget{Path: "/b"}, CreatedAt: v1, UpdatedAt: v1}
	if err := s.CreateStorageTarget(ctx, tg); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveBackupRecord(ctx, &models.BackupRecord{ID: "b1", Status: models.StatusCompleted, StorageTargetID: "stg_l"}); err != nil {
		t.Fatal(err)
	}
	moved := tg.Clone()
	moved.Local.Path, moved.UpdatedAt = "/elsewhere", v1.Add(time.Second)
	err := s.UpdateStorageTarget(ctx, moved, v1, true)
	if !errors.Is(err, targets.ErrLocationInUse) || !strings.Contains(err.Error(), "create a new target instead") {
		t.Fatalf("moving a target with backups = %v", err)
	}
	renamed := tg.Clone()
	renamed.Name, renamed.UpdatedAt = "renamed", v1.Add(time.Second)
	if err := s.UpdateStorageTarget(ctx, renamed, v1, false); err != nil {
		t.Fatalf("renaming a target with backups = %v", err)
	}
	// Once only pruned or failed backups remain, the location may change.
	if err := s.SaveBackupRecord(ctx, &models.BackupRecord{ID: "b1", Status: models.StatusPruned, StorageTargetID: "stg_l"}); err != nil {
		t.Fatal(err)
	}
	moved.UpdatedAt = v1.Add(2 * time.Second)
	if err := s.UpdateStorageTarget(ctx, moved, renamed.UpdatedAt, true); err != nil {
		t.Fatalf("moving a target without live backups = %v", err)
	}
}
