package desktop

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWindowStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := LoadWindowState(dir); ok || err != nil {
		t.Fatalf("no file: ok=%v err=%v; want neither", ok, err)
	}
	want := WindowState{X: 40, Y: 30, Width: 1400, Height: 900, Maximised: true}
	if err := SaveWindowState(dir, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LoadWindowState(dir)
	if err != nil || !ok || got != want {
		t.Fatalf("LoadWindowState = %+v, %v, %v; want %+v", got, ok, err, want)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, WindowStateFile))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("mode %v; want 0600", fi.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("%d files in the data dir; want only %s", len(entries), WindowStateFile)
	}
}

func TestWindowStateIgnoresBadFiles(t *testing.T) {
	for name, content := range map[string]string{
		"zero size": `{"x":1,"y":1,"width":0,"height":0}`,
		"garbage":   `not json`,
		"oversized": `{"x":1,"y":1,"width":10,"height":10,"pad":"` + strings.Repeat("a", 8<<10) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, WindowStateFile), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, ok, _ := LoadWindowState(dir); ok {
				t.Error("loaded an unusable window state")
			}
		})
	}
	if err := SaveWindowState(t.TempDir(), WindowState{}); err != nil {
		t.Errorf("saving an empty state: %v; want a no-op", err)
	}
}

func TestWindowStateCapsHugeValues(t *testing.T) {
	for name, content := range map[string]string{
		"normal":    `{"x":99999999,"y":-99999999,"width":2147483647,"height":99999999}`,
		"maximised": `{"x":1,"y":1,"width":9223372036854775807,"height":70000,"maximised":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, WindowStateFile), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, ok, err := LoadWindowState(dir)
			if err != nil || !ok {
				t.Fatalf("LoadWindowState = %+v, %v, %v", got, ok, err)
			}
			if got.Width != MaxWindowSize || got.Height != MaxWindowSize {
				t.Errorf("size %dx%d; want capped at %d", got.Width, got.Height, MaxWindowSize)
			}
			if got.X < -MaxWindowSize || got.X > MaxWindowSize || got.Y < -MaxWindowSize || got.Y > MaxWindowSize {
				t.Errorf("position %d,%d; want within ±%d", got.X, got.Y, MaxWindowSize)
			}
			// What the app passes to wails.Run before any screen is known.
			if c := got.Clamp(0, 0, 960, 640); c.Width > MaxWindowSize || c.Height > MaxWindowSize || c.Maximised != got.Maximised {
				t.Errorf("Clamp without a screen = %+v", c)
			}
		})
	}
	// Saving caps too, and the minimum still applies after the cap.
	dir := t.TempDir()
	if err := SaveWindowState(dir, WindowState{Width: 1 << 30, Height: 10, Maximised: true}); err != nil {
		t.Fatal(err)
	}
	got, _, err := LoadWindowState(dir)
	if err != nil || got.Width != MaxWindowSize || !got.Maximised {
		t.Fatalf("saved state = %+v, %v; want width %d, maximised", got, err, MaxWindowSize)
	}
	if c := got.Clamp(0, 0, 960, 640); c.Height != 640 {
		t.Errorf("height after Clamp = %d; want the minimum 640", c.Height)
	}
}

func TestWindowStateClamp(t *testing.T) {
	for _, c := range []struct {
		name string
		in   WindowState
		w, h int
		want WindowState
	}{
		{"fits", WindowState{X: 100, Y: 50, Width: 1280, Height: 860}, 1920, 1080, WindowState{X: 100, Y: 50, Width: 1280, Height: 860}},
		{"off the right and bottom", WindowState{X: 3000, Y: 2000, Width: 1280, Height: 860}, 1920, 1080, WindowState{X: 640, Y: 220, Width: 1280, Height: 860}},
		{"negative", WindowState{X: -500, Y: -20, Width: 1280, Height: 860}, 1920, 1080, WindowState{X: 0, Y: 0, Width: 1280, Height: 860}},
		{"larger than the screen", WindowState{X: 10, Y: 10, Width: 4000, Height: 3000}, 1440, 900, WindowState{X: 0, Y: 0, Width: 1440, Height: 900}},
		{"below the minimum", WindowState{X: 10, Y: 10, Width: 200, Height: 100}, 1920, 1080, WindowState{X: 10, Y: 10, Width: 960, Height: 640}},
		{"unknown screen", WindowState{X: -5, Y: 9999, Width: 200, Height: 2000}, 0, 0, WindowState{X: -5, Y: 9999, Width: 960, Height: 2000}},
		{"screen below the minimum", WindowState{X: 50, Y: 50, Width: 1280, Height: 860}, 800, 600, WindowState{X: 0, Y: 0, Width: 960, Height: 640}},
	} {
		if got := c.in.Clamp(c.w, c.h, 960, 640); got != c.want {
			t.Errorf("%s: Clamp = %+v; want %+v", c.name, got, c.want)
		}
	}
}
