package models

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestValidatePITRInPlace is the validation matrix of in-place point-in-time
// restores (issue #142): pitr.in_place needs a database selection that
// pitr.confirm_in_place names exactly, and the options a point-in-time restore
// never offers stay refused with it.
func TestValidatePITRInPlace(t *testing.T) {
	const at = `"pitr":{"stream_id":"str_a","at":"2026-10-05T12:00:00Z"`
	for name, c := range map[string]struct {
		body string
		want error
		// wantText is a part of the error when want is nil and an error is expected.
		wantText string
	}{
		"clone":                       {body: `{` + at + `},"databases":["shop"]}`},
		"in place":                    {body: `{` + at + `,"in_place":true,"confirm_in_place":"shop"},"databases":["shop"]}`},
		"in place, two in any order":  {body: `{` + at + `,"in_place":true,"confirm_in_place":" crm , shop"},"databases":["shop","crm"]}`},
		"in place, keep clone":        {body: `{` + at + `,"in_place":true,"confirm_in_place":"shop","keep_clone":true},"databases":["shop"]}`},
		"in place without databases":  {body: `{` + at + `,"in_place":true,"confirm_in_place":"shop"}}`, want: ErrPITRInPlaceDatabases},
		"in place unconfirmed":        {body: `{` + at + `,"in_place":true},"databases":["shop"]}`, want: ErrPITRInPlaceNotConfirmed},
		"in place, wrong name":        {body: `{` + at + `,"in_place":true,"confirm_in_place":"shoP"},"databases":["shop"]}`, want: ErrPITRInPlaceNotConfirmed},
		"in place, one missing":       {body: `{` + at + `,"in_place":true,"confirm_in_place":"shop"},"databases":["shop","crm"]}`, want: ErrPITRInPlaceNotConfirmed},
		"in place, one too many":      {body: `{` + at + `,"in_place":true,"confirm_in_place":"shop,crm"},"databases":["shop"]}`, want: ErrPITRInPlaceNotConfirmed},
		"in place, target_database":   {body: `{` + at + `,"in_place":true,"confirm_in_place":"shop"},"databases":["shop"],"target_database":"shop"}`, want: ErrPITRInPlace},
		"in place, safe_clone false":  {body: `{` + at + `,"in_place":true,"confirm_in_place":"shop"},"databases":["shop"],"safe_clone":false}`, want: ErrPITRInPlace},
		"in place, top-level confirm": {body: `{` + at + `,"in_place":true,"confirm_in_place":"shop"},"databases":["shop"],"confirm_in_place":true}`, want: ErrPITRInPlace},
		"in place, collections": {body: `{` + at + `,"in_place":true,"confirm_in_place":"shop"},"databases":["shop"],"selected_collections":["orders"]}`,
			wantText: "selected_collections"},
		"in place, users and roles": {body: `{` + at + `,"in_place":true,"confirm_in_place":"shop"},"databases":["shop"],"restore_users_and_roles":true}`,
			wantText: "restore_users_and_roles"},
		"in place, drop_target": {body: `{` + at + `,"in_place":true,"confirm_in_place":"shop"},"databases":["shop"],"drop_target":true}`,
			wantText: "drop_target"},
		"in place, admin":                   {body: `{` + at + `,"in_place":true,"confirm_in_place":"admin"},"databases":["admin"]}`, wantText: "never restored"},
		"confirm without in place":          {body: `{` + at + `,"confirm_in_place":"shop"},"databases":["shop"]}`, wantText: "only apply"},
		"keep_clone without in place":       {body: `{` + at + `,"keep_clone":true},"databases":["shop"]}`, wantText: "only apply"},
		"top-level in place without pitr's": {body: `{` + at + `},"databases":["shop"],"safe_clone":false,"confirm_in_place":true}`, want: ErrPITRInPlace},
	} {
		var req RestoreRequest
		if err := json.Unmarshal([]byte(c.body), &req); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		err := req.ValidatePITR()
		switch {
		case c.want != nil && !errors.Is(err, c.want):
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		case c.wantText != "" && (err == nil || !strings.Contains(err.Error(), c.wantText)):
			t.Errorf("%s: err = %v, want one mentioning %q", name, err, c.wantText)
		case c.want == nil && c.wantText == "" && err != nil:
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// TestPITRTargetIsInPlace checks the nil-safe accessor.
func TestPITRTargetIsInPlace(t *testing.T) {
	var none *PITRTarget
	if none.IsInPlace() || (&PITRTarget{}).IsInPlace() || !(&PITRTarget{InPlace: true}).IsInPlace() {
		t.Fatal("IsInPlace")
	}
}
