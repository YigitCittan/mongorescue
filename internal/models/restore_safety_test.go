package models

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRestoreRequestSafeCloneJSONCompatibility(t *testing.T) {
	cases := []struct {
		body      string
		safeClone bool
		inPlace   bool
		wantErr   bool
	}{
		{`{"backup_id":"b"}`, true, false, false},
		{`{"backup_id":"b","safe_clone":true}`, true, false, false},
		{`{"backup_id":"b","safe_clone":false}`, false, true, true},
		{`{"backup_id":"b","safe_clone":false,"confirm_in_place":true}`, false, true, false},
		{`{"backup_id":"b","target_database":"x"}`, true, true, true},
		{`{"backup_id":"b","safe_clone":false,"confirm_in_place":true,"target_database":"x"}`, false, true, false},
	}
	for _, tc := range cases {
		var req RestoreRequest
		if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		if req.IsSafeClone() != tc.safeClone || req.InPlace() != tc.inPlace {
			t.Fatalf("%s: IsSafeClone=%v InPlace=%v, want %v/%v", tc.body, req.IsSafeClone(), req.InPlace(), tc.safeClone, tc.inPlace)
		}
		err := req.ValidateTarget()
		if (err != nil) != tc.wantErr || (err != nil && !errors.Is(err, ErrInPlaceNotConfirmed)) {
			t.Fatalf("%s: ValidateTarget() = %v, wantErr %v", tc.body, err, tc.wantErr)
		}
	}

	// The zero value (and its JSON form) is a safe clone; omitempty keeps it compact.
	out, _ := json.Marshal(RestoreRequest{BackupID: "b"})
	if strings.Contains(string(out), "safe_clone") || strings.Contains(string(out), "confirm_in_place") {
		t.Fatalf("defaults must be omitted: %s", out)
	}
	off := false
	out, _ = json.Marshal(RestoreRequest{BackupID: "b", SafeClone: &off, ConfirmInPlace: true})
	if !strings.Contains(string(out), `"safe_clone":false`) || !strings.Contains(string(out), `"confirm_in_place":true`) {
		t.Fatalf("explicit in-place must round-trip: %s", out)
	}

	red := RestoreRequest{SafeClone: &off}.Redacted()
	*red.SafeClone = true
	if off {
		t.Fatal("Redacted must deep-copy SafeClone")
	}
}
