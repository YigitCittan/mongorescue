package models

import "testing"

func TestParseServerVersion(t *testing.T) {
	for in, want := range map[string]ServerVersion{
		"8.0.4":       {8, 0, 4},
		"7.0.12-rc0":  {7, 0, 12},
		" 6.0 ":       {6, 0, 0},
		"4.4.29+ent":  {4, 4, 29},
		"5":           {5, 0, 0},
		"7.x.1":       {7, 0, 0},
		"100.10.1000": {100, 10, 1000},
	} {
		got, ok := ParseServerVersion(in)
		if !ok || got != want {
			t.Errorf("ParseServerVersion(%q) = %+v, %v; want %+v", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "unknown", "v8.0", "-1.0"} {
		if _, ok := ParseServerVersion(bad); ok {
			t.Errorf("ParseServerVersion(%q) parsed", bad)
		}
	}
	v := func(s string) ServerVersion { out, _ := ParseServerVersion(s); return out }
	if v("7.0.1").Compare(v("7.0.14")) != 0 || v("7.2").Compare(v("7.0")) != 1 || v("6.0").Compare(v("7.0")) != -1 {
		t.Fatal("Compare must order major and minor and ignore patch releases")
	}
}

func TestPreflightResult(t *testing.T) {
	r := &PreflightResult{OK: true}
	r.Add("a", PreflightPass, "fine")
	r.Add("b", PreflightWarn, "careful")
	if !r.OK || len(r.Warnings()) != 1 || r.Failed() != nil {
		t.Fatalf("after a warning: %+v", r)
	}
	r.Add("c", PreflightFail, "no")
	if r.OK || len(r.Failed()) != 1 || r.Check("c").Message != "no" || r.Check("x") != nil {
		t.Fatalf("after a failure: %+v", r)
	}
	var none *PreflightResult
	if none.Failed() != nil || none.Warnings() != nil || none.Check("a") != nil {
		t.Fatal("a nil result has no checks")
	}
}
