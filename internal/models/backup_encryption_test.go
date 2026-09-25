package models

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBackupRecordEncryptionFieldsBackwardCompatible(t *testing.T) {
	legacy := `{"id":"bkp_db_20260101_000000","database":"db","status":"completed","storage_type":"local","storage_key":"db/x.archive.gz","size_bytes":10,"started_at":"2026-01-01T00:00:00Z"}`
	var rec BackupRecord
	if err := json.Unmarshal([]byte(legacy), &rec); err != nil {
		t.Fatalf("legacy record must decode: %v", err)
	}
	if rec.Encrypted || rec.EncryptionMode != "" {
		t.Fatalf("legacy record must decode as unencrypted, got %+v", rec)
	}
	out, _ := json.Marshal(rec)
	if strings.Contains(string(out), "encrypt") {
		t.Fatalf("unencrypted record must omit encryption fields: %s", out)
	}

	rec.Encrypted, rec.EncryptionMode = true, "x25519"
	out, _ = json.Marshal(rec)
	if !strings.Contains(string(out), `"encrypted":true`) || !strings.Contains(string(out), `"encryption_mode":"x25519"`) {
		t.Fatalf("encrypted record must expose encryption fields: %s", out)
	}
}
