package models

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestBackupRecordSerialization(t *testing.T) {
	now := time.Now().UTC()
	rec := BackupRecord{
		ID:         "bkp_test_123",
		Database:   "analytics",
		Status:     StatusCompleted,
		SizeBytes:  1024 * 1024,
		StorageKey: "analytics/test.gz",
		SHA256:     "abcdef123456",
		StartedAt:  now,
	}

	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("failed to marshal backup record: %v", err)
	}

	var decoded BackupRecord
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal backup record: %v", err)
	}

	if decoded.ID != rec.ID || decoded.Status != StatusCompleted {
		t.Errorf("decoded record mismatch: %+v", decoded)
	}
}

func TestJobModelDefaults(t *testing.T) {
	job := Job{
		ID:             "job_1",
		Name:           "Daily Backup",
		Database:       "users",
		CronExpression: "@daily",
		Enabled:        true,
		RetentionDays:  30,
		RetentionCount: 10,
	}

	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("failed to marshal job: %v", err)
	}

	var decoded Job
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal job: %v", err)
	}

	if decoded.RetentionDays != 30 || decoded.RetentionCount != 10 {
		t.Errorf("decoded retention mismatch: %+v", decoded)
	}
}

func TestValidateID(t *testing.T) {
	valid := []string{"a", "job_1", "Job-2_x", strings.Repeat("a", MaxIDLength)}
	for _, id := range valid {
		if err := ValidateID(id); err != nil {
			t.Errorf("ValidateID(%q) = %v; want nil", id, err)
		}
	}

	invalid := []string{"", "x');alert(1);//", "a b", "../etc", "<script>", "ü", strings.Repeat("a", MaxIDLength+1)}
	for _, id := range invalid {
		if err := ValidateID(id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("ValidateID(%q) = %v; want ErrInvalidID", id, err)
		}
	}
}

func TestSanitizeIDComponent(t *testing.T) {
	got := SanitizeIDComponent("My DB');<x>", 0)
	if err := ValidateID(got); err != nil {
		t.Fatalf("sanitized component %q is not a valid id: %v", got, err)
	}
	if got != "My_DB____x_" {
		t.Errorf("unexpected sanitized value %q", got)
	}
	if got := SanitizeIDComponent("abcdef", 3); got != "abc" {
		t.Errorf("expected truncation to 3, got %q", got)
	}
}

func TestRedacted(t *testing.T) {
	const uri = "mongodb://u:secret@h/db"

	job := &Job{ID: "j", ConnectionID: "c", Collections: []string{"a"}}
	cj := job.Clone()
	cj.Collections[0] = "b"
	if job.Collections[0] != "a" {
		t.Error("Clone must copy slice fields")
	}
	if (*Job)(nil).Clone() != nil {
		t.Error("nil job should clone to nil")
	}

	conn := &Connection{ID: "c", URI: uri}
	if rc := conn.Redacted(); strings.Contains(rc.URI, "secret") || conn.URI != uri {
		t.Errorf("connection not redacted or receiver mutated: %q", rc.URI)
	}
	if (*Connection)(nil).Redacted() != nil {
		t.Error("nil connection should redact to nil")
	}

	if got := (BackupOptions{MongoURI: uri}).Redacted().MongoURI; strings.Contains(got, "secret") {
		t.Errorf("backup options not redacted: %q", got)
	}
	if got := (RestoreRequest{MongoURI: uri}).Redacted().MongoURI; strings.Contains(got, "secret") {
		t.Errorf("restore request not redacted: %q", got)
	}
}
