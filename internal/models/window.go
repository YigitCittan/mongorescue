package models

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// WindowDays are the day names of a backup window, Monday first.
var WindowDays = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// ErrInvalidBackupWindow is returned for a backup window that cannot be read.
var ErrInvalidBackupWindow = errors.New("invalid backup_window")

// SkipOutsideWindow is the JobRun.SkipReason of a scheduled run that fell outside
// its job's backup window.
const SkipOutsideWindow = "outside window"

// BackupWindow restricts when the scheduled runs of a job may start: on Days (every
// day when empty) from Start to End, in Timezone. A window whose End is not after
// its Start crosses midnight and ends on the next day; Days name the day it opens.
// Manual runs ignore the window.
type BackupWindow struct {
	// Timezone is an IANA time zone name ("Europe/Istanbul"); "" means UTC.
	Timezone string `json:"timezone,omitempty"`
	// Days are the days the window opens on ("mon" to "sun"); empty means every day.
	Days []string `json:"days,omitempty"`
	// Start is when the window opens, "HH:MM" (00:00 to 23:59).
	Start string `json:"start"`
	// End is when it closes, "HH:MM" (00:00 to 24:00); not after Start means the
	// next day.
	End string `json:"end"`
	// CancelAtWindowEnd cancels a scheduled run still running when the window
	// closes (recorded as cancelled, not failed).
	CancelAtWindowEnd bool `json:"cancel_at_window_end,omitempty"`
}

// IsZero reports whether w sets nothing (no window).
func (w *BackupWindow) IsZero() bool {
	return w == nil || (w.Timezone == "" && len(w.Days) == 0 && w.Start == "" && w.End == "" && !w.CancelAtWindowEnd)
}

// Clone returns a deep copy of w (nil for nil).
func (w *BackupWindow) Clone() *BackupWindow {
	if w == nil {
		return nil
	}
	c := *w
	c.Days = slices.Clone(w.Days)
	return &c
}

// Normalize trims w, lower-cases and orders its days (Monday first, without
// duplicates; all seven become none) and checks it: a loadable time zone, known
// days, a Start of HH:MM and an End of HH:MM or 24:00 that differs from Start.
func (w *BackupWindow) Normalize() error {
	w.Timezone = strings.TrimSpace(w.Timezone)
	w.Start, w.End = strings.TrimSpace(w.Start), strings.TrimSpace(w.End)
	if _, err := loadZone(w.Timezone); err != nil {
		return fmt.Errorf("%w: unknown timezone %q", ErrInvalidBackupWindow, w.Timezone)
	}
	seen := make(map[string]bool, len(w.Days))
	for _, d := range w.Days {
		d = strings.ToLower(strings.TrimSpace(d))
		if !slices.Contains(WindowDays, d) {
			return fmt.Errorf("%w: days must be mon, tue, wed, thu, fri, sat or sun", ErrInvalidBackupWindow)
		}
		seen[d] = true
	}
	days := make([]string, 0, len(seen))
	for _, d := range WindowDays {
		if seen[d] {
			days = append(days, d)
		}
	}
	if len(days) == len(WindowDays) {
		days = nil
	}
	w.Days = days
	start, ok := clockMinutes(w.Start, false)
	if !ok {
		return fmt.Errorf("%w: start must be HH:MM", ErrInvalidBackupWindow)
	}
	end, ok := clockMinutes(w.End, true)
	if !ok {
		return fmt.Errorf("%w: end must be HH:MM (24:00 for midnight)", ErrInvalidBackupWindow)
	}
	if start == end {
		return fmt.Errorf("%w: start and end must differ (00:00 to 24:00 is the whole day)", ErrInvalidBackupWindow)
	}
	return nil
}

// clockMinutes parses "HH:MM" into minutes after midnight; allow24 accepts "24:00".
func clockMinutes(s string, allow24 bool) (int, bool) {
	hh, mm, ok := strings.Cut(s, ":")
	if !ok || len(hh) != 2 || len(mm) != 2 {
		return 0, false
	}
	h, errH := strconv.Atoi(hh)
	m, errM := strconv.Atoi(mm)
	if errH != nil || errM != nil || h < 0 || m < 0 || m > 59 {
		return 0, false
	}
	switch {
	case h < 24:
		return h*60 + m, true
	case h == 24 && m == 0 && allow24:
		return 24 * 60, true
	}
	return 0, false
}

// loadZone loads an IANA zone; "" and "UTC" are UTC.
func loadZone(name string) (*time.Location, error) {
	if name == "" || name == "UTC" {
		return time.UTC, nil
	}
	if strings.EqualFold(name, "local") {
		return nil, errors.New("the server's local zone is not allowed")
	}
	return time.LoadLocation(name)
}

// Location returns the window's time zone (UTC when unset or unknown).
func (w *BackupWindow) Location() *time.Location {
	loc, err := loadZone(w.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// opensOn reports whether the window opens on weekday d.
func (w *BackupWindow) opensOn(d time.Weekday) bool {
	if len(w.Days) == 0 {
		return true
	}
	// time.Weekday is Sunday first; WindowDays is Monday first.
	return slices.Contains(w.Days, WindowDays[(int(d)+6)%7])
}

// occurrence returns the window that opens on the calendar day of day (in loc).
// Times that a DST change skips or repeats resolve as time.Date does.
func (w *BackupWindow) occurrence(day time.Time, loc *time.Location) (start, end time.Time) {
	sm, _ := clockMinutes(w.Start, false)
	em, _ := clockMinutes(w.End, true)
	y, mo, d := day.Date()
	start = wallTime(y, mo, d, sm, loc)
	endDay := d
	if em <= sm {
		endDay++
	}
	end = wallTime(y, mo, endDay, em, loc)
	return start, end
}

// wallTime returns the instant the wall clock of loc reaches minutes after
// midnight on the given day. A wall time skipped by a DST change resolves to the
// change itself (02:30 on a day that jumps from 02:00 to 03:00 is 03:00), so a
// window never opens or closes early; a repeated wall time is its first instance
// as time.Date chooses it.
func wallTime(y int, mo time.Month, d, minutes int, loc *time.Location) time.Time {
	t := time.Date(y, mo, d, minutes/60, minutes%60, 0, 0, loc)
	want := time.Date(y, mo, d, minutes/60, minutes%60, 0, 0, time.UTC)
	ly, lmo, ld := t.Date()
	got := time.Date(ly, lmo, ld, t.Hour(), t.Minute(), 0, 0, time.UTC)
	switch zoneStart, zoneEnd := t.ZoneBounds(); {
	case got.Before(want) && !zoneEnd.IsZero():
		return zoneEnd
	case got.After(want) && !zoneStart.IsZero():
		return zoneStart
	}
	return t
}

// Open returns the occurrence of the window that contains t: when it opened and
// when it closes. ok is false when t is outside the window (or w is nil).
func (w *BackupWindow) Open(t time.Time) (start, end time.Time, ok bool) {
	if w == nil {
		return time.Time{}, time.Time{}, false
	}
	loc := w.Location()
	local := t.In(loc)
	// A window that crosses midnight may have opened the day before.
	for _, offset := range []int{-1, 0} {
		y, mo, d := local.Date()
		day := time.Date(y, mo, d+offset, 12, 0, 0, 0, loc)
		if !w.opensOn(day.Weekday()) {
			continue
		}
		s, e := w.occurrence(day, loc)
		if !t.Before(s) && t.Before(e) {
			return s, e, true
		}
	}
	return time.Time{}, time.Time{}, false
}

// Contains reports whether a scheduled run may start at t. A nil window always
// allows it.
func (w *BackupWindow) Contains(t time.Time) bool {
	if w == nil {
		return true
	}
	_, _, ok := w.Open(t)
	return ok
}

// String renders w for messages: "mon,tue 22:00-02:00 Europe/Istanbul".
func (w *BackupWindow) String() string {
	if w == nil {
		return ""
	}
	days := "daily"
	if len(w.Days) > 0 {
		days = strings.Join(w.Days, ",")
	}
	tz := w.Timezone
	if tz == "" {
		tz = "UTC"
	}
	return fmt.Sprintf("%s %s-%s %s", days, w.Start, w.End, tz)
}
