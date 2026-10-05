package models

import (
	"errors"
	"strings"
	"testing"
)

func TestReadPreferenceValidate(t *testing.T) {
	ok := []ReadPreference{
		{},
		{Mode: ReadPrimary},
		{Mode: ReadSecondary, Tags: []map[string]string{{"dc": "east"}, {}}},
		{Mode: ReadNearest, Tags: []map[string]string{{"use": ""}}},
	}
	for _, r := range ok {
		if err := r.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v", r, err)
		}
	}
	tooMany := make([]map[string]string, MaxReadPreferenceTagSets+1)
	bad := []ReadPreference{
		{Mode: "secondary_preferred"},
		{Mode: "Secondary"},
		{Tags: []map[string]string{{"dc": "east"}}},
		{Mode: ReadPrimary, Tags: []map[string]string{{"dc": "east"}}},
		{Mode: ReadSecondary, Tags: tooMany},
		{Mode: ReadSecondary, Tags: []map[string]string{{"": "east"}}},
		{Mode: ReadSecondary, Tags: []map[string]string{{"dc": "a,b"}}},
		{Mode: ReadSecondary, Tags: []map[string]string{{"d:c": "a"}}},
		{Mode: ReadSecondary, Tags: []map[string]string{{"dc": "a\nb"}}},
		{Mode: ReadSecondary, Tags: []map[string]string{{"dc": strings.Repeat("x", MaxReadPreferenceTagLength+1)}}},
	}
	for _, r := range bad {
		if err := r.Validate(); !errors.Is(err, ErrInvalidReadPreference) {
			t.Errorf("Validate(%+v) = %v, want ErrInvalidReadPreference", r, err)
		}
	}
}

func TestReadPreferenceOrAndString(t *testing.T) {
	conn := ReadPreference{Mode: ReadSecondaryPreferred}
	if got := (ReadPreference{}).Or(conn); got.Mode != ReadSecondaryPreferred {
		t.Fatalf("a job without a mode uses the connection's: %+v", got)
	}
	job := ReadPreference{Mode: ReadSecondary, Tags: []map[string]string{{"use": "backup", "dc": "east"}, {}}}
	if got := job.Or(conn); got.Mode != ReadSecondary {
		t.Fatalf("a job's mode wins: %+v", got)
	}
	if s := job.String(); s != "secondary (dc=east,use=backup; {})" {
		t.Fatalf("String = %q", s)
	}
	c := job.Clone()
	c.Tags[0]["dc"] = "west"
	if job.Tags[0]["dc"] != "east" {
		t.Fatal("Clone shares the tag maps")
	}
}

func TestValidateUploadMbps(t *testing.T) {
	for _, v := range []float64{0, 0.5, 100, MaxUploadMbps} {
		if err := ValidateUploadMbps(v); err != nil {
			t.Errorf("%v: %v", v, err)
		}
	}
	for _, v := range []float64{-1, MaxUploadMbps + 1} {
		if err := ValidateUploadMbps(v); err == nil {
			t.Errorf("%v accepted", v)
		}
	}
	if got := UploadBytesPerSecond(8); got != 1e6 {
		t.Fatalf("8 Mbit/s = %v B/s, want 1e6", got)
	}
}
