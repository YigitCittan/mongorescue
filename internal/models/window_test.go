package models

import (
	"errors"
	"testing"
	"time"
	_ "time/tzdata" // hermetic zone data for the DST cases
)

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func mustWindow(t *testing.T, w BackupWindow) *BackupWindow {
	t.Helper()
	if err := w.Normalize(); err != nil {
		t.Fatalf("Normalize(%+v): %v", w, err)
	}
	return &w
}

func TestBackupWindowNormalize(t *testing.T) {
	w := mustWindow(t, BackupWindow{Timezone: " Europe/Istanbul ", Days: []string{"SUN", "mon", "mon"}, Start: "22:00", End: "02:00"})
	if w.Timezone != "Europe/Istanbul" || len(w.Days) != 2 || w.Days[0] != "mon" || w.Days[1] != "sun" {
		t.Fatalf("normalized %+v", w)
	}
	all := mustWindow(t, BackupWindow{Days: WindowDays, Start: "00:00", End: "24:00"})
	if all.Days != nil {
		t.Fatalf("all seven days should become none: %v", all.Days)
	}
	for _, bad := range []BackupWindow{
		{Start: "22:00", End: "22:00"},
		{Start: "00:00", End: "00:00"},
		{Start: "24:00", End: "01:00"},
		{Start: "1:00", End: "02:00"},
		{Start: "01:60", End: "02:00"},
		{Start: "01:00", End: "24:01"},
		{Start: "01:00", End: "02:00", Days: []string{"monday"}},
		{Start: "01:00", End: "02:00", Timezone: "Mars/Olympus"},
		{Start: "01:00", End: "02:00", Timezone: "Local"},
	} {
		if err := bad.Normalize(); !errors.Is(err, ErrInvalidBackupWindow) {
			t.Errorf("Normalize(%+v) = %v, want ErrInvalidBackupWindow", bad, err)
		}
	}
}

func TestBackupWindowContains(t *testing.T) {
	cases := []struct {
		name string
		w    BackupWindow
		at   string
		want bool
	}{
		{"inside a daytime window", BackupWindow{Start: "09:00", End: "17:00"}, "2026-10-05T12:00:00Z", true},
		{"start is inclusive", BackupWindow{Start: "09:00", End: "17:00"}, "2026-10-05T09:00:00Z", true},
		{"end is exclusive", BackupWindow{Start: "09:00", End: "17:00"}, "2026-10-05T17:00:00Z", false},
		{"before it opens", BackupWindow{Start: "09:00", End: "17:00"}, "2026-10-05T08:59:59Z", false},
		{"crossing midnight, before midnight", BackupWindow{Start: "22:00", End: "02:00"}, "2026-10-05T23:30:00Z", true},
		{"crossing midnight, after midnight", BackupWindow{Start: "22:00", End: "02:00"}, "2026-10-06T01:59:00Z", true},
		{"crossing midnight, after it closes", BackupWindow{Start: "22:00", End: "02:00"}, "2026-10-06T02:00:00Z", false},
		{"crossing midnight, midday", BackupWindow{Start: "22:00", End: "02:00"}, "2026-10-06T12:00:00Z", false},
		// 2026-10-05 is a Monday: a Monday night window still holds early Tuesday,
		// but not early Monday (it opened on Sunday, which is not listed).
		{"opening day counts after midnight", BackupWindow{Days: []string{"mon"}, Start: "22:00", End: "02:00"}, "2026-10-06T01:00:00Z", true},
		{"previous day not listed", BackupWindow{Days: []string{"mon"}, Start: "22:00", End: "02:00"}, "2026-10-05T01:00:00Z", false},
		{"weekend only on a weekday", BackupWindow{Days: []string{"sat", "sun"}, Start: "00:00", End: "24:00"}, "2026-10-05T12:00:00Z", false},
		{"weekend only on a sunday", BackupWindow{Days: []string{"sat", "sun"}, Start: "00:00", End: "24:00"}, "2026-10-04T23:59:00Z", true},
		{"read in its time zone", BackupWindow{Timezone: "Europe/Istanbul", Start: "01:00", End: "05:00"}, "2026-10-05T23:30:00Z", true},
		{"read in its time zone, outside", BackupWindow{Timezone: "Europe/Istanbul", Start: "01:00", End: "05:00"}, "2026-10-05T03:00:00Z", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := mustWindow(t, tc.w)
			if got := w.Contains(utc(tc.at)); got != tc.want {
				t.Fatalf("Contains(%s) = %v, want %v", tc.at, got, tc.want)
			}
		})
	}
	var none *BackupWindow
	if !none.Contains(time.Now()) {
		t.Fatal("a nil window allows every time")
	}
}

// openLength returns how long the occurrence of w that contains at lasts.
func openLength(t *testing.T, w *BackupWindow, at string) time.Duration {
	t.Helper()
	start, end, ok := w.Open(utc(at))
	if !ok {
		t.Fatalf("%s is outside %s", at, w)
	}
	return end.Sub(start)
}

func TestBackupWindowDST(t *testing.T) {
	ny := func(start, end string, days ...string) *BackupWindow {
		return mustWindow(t, BackupWindow{Timezone: "America/New_York", Days: days, Start: start, End: end})
	}
	t.Run("New York spring forward shortens the window", func(t *testing.T) {
		// 2026-03-08 02:00 EST becomes 03:00 EDT: 01:00-04:00 lasts two hours.
		w := ny("01:00", "04:00")
		if d := openLength(t, w, "2026-03-08T06:30:00Z"); d != 2*time.Hour {
			t.Fatalf("length %v, want 2h", d)
		}
		if !w.Contains(utc("2026-03-08T07:59:00Z")) || w.Contains(utc("2026-03-08T08:00:00Z")) {
			t.Fatal("the window must close at 04:00 EDT (08:00Z)")
		}
	})
	t.Run("New York fall back lengthens a window crossing midnight", func(t *testing.T) {
		// Saturday 2026-10-31 22:00 EDT to Sunday 06:00 EST: nine hours.
		w := ny("22:00", "06:00", "sat")
		if d := openLength(t, w, "2026-11-01T10:30:00Z"); d != 9*time.Hour {
			t.Fatalf("length %v, want 9h", d)
		}
		if w.Contains(utc("2026-11-01T11:00:00Z")) {
			t.Fatal("the window must close at 06:00 EST (11:00Z)")
		}
		if w.Contains(utc("2026-11-02T04:00:00Z")) {
			t.Fatal("Sunday night is not in a Saturday window")
		}
	})
	t.Run("a start skipped by DST opens at the end of the gap", func(t *testing.T) {
		w := ny("02:30", "04:00")
		if w.Contains(utc("2026-03-08T06:59:00Z")) { // 01:59 EST
			t.Fatal("open before the gap")
		}
		if !w.Contains(utc("2026-03-08T07:45:00Z")) { // 03:45 EDT
			t.Fatal("closed after the gap")
		}
	})
	t.Run("Istanbul keeps its offset all year", func(t *testing.T) {
		// Europe switches on 2026-03-29; Istanbul has stayed at +03 since 2016.
		w := mustWindow(t, BackupWindow{Timezone: "Europe/Istanbul", Start: "23:00", End: "01:00"})
		if d := openLength(t, w, "2026-03-29T20:30:00Z"); d != 2*time.Hour {
			t.Fatalf("length %v, want 2h", d)
		}
		if !w.Contains(utc("2026-03-28T21:59:00Z")) || w.Contains(utc("2026-03-28T22:00:00Z")) {
			t.Fatal("the window must close at 01:00 +03 (22:00Z)")
		}
	})
	t.Run("Istanbul's last spring forward", func(t *testing.T) {
		// 2016-03-27 03:00 EET became 04:00 EEST: 02:00-05:00 lasted two hours.
		w := mustWindow(t, BackupWindow{Timezone: "Europe/Istanbul", Start: "02:00", End: "05:00"})
		if d := openLength(t, w, "2016-03-27T00:30:00Z"); d != 2*time.Hour {
			t.Fatalf("length %v, want 2h", d)
		}
	})
}
