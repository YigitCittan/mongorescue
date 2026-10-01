package desktop

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// WindowStateFile is the file in the data directory that remembers the window's
// size, position and maximised state across restarts.
const WindowStateFile = "window.json"

// maxWindowStateBytes bounds the window state file read at startup.
const maxWindowStateBytes = 4 << 10

// WindowState is the window geometry saved when the window closes. X and Y are
// relative to the screen the window is on, as the Wails runtime reports them.
type WindowState struct {
	// X is the left edge of the window.
	X int `json:"x"`
	// Y is the top edge of the window.
	Y int `json:"y"`
	// Width is the window width in logical pixels.
	Width int `json:"width"`
	// Height is the window height in logical pixels.
	Height int `json:"height"`
	// Maximised restores the window maximised; X, Y, Width and Height then keep the
	// last normal geometry.
	Maximised bool `json:"maximised,omitempty"`
}

// Valid reports whether s has a usable size.
func (s WindowState) Valid() bool {
	return s.Width > 0 && s.Height > 0
}

// Clamp fits s onto a screen of screenW × screenH: the size is at least minW × minH
// and at most the screen, and the window is moved so that it lies entirely on the
// screen. A non-positive screen size leaves the position alone and only enforces the
// minimum size.
func (s WindowState) Clamp(screenW, screenH, minW, minH int) WindowState {
	s.Width = max(s.Width, minW)
	s.Height = max(s.Height, minH)
	if screenW <= 0 || screenH <= 0 {
		return s
	}
	s.Width = min(s.Width, max(screenW, minW))
	s.Height = min(s.Height, max(screenH, minH))
	s.X = min(max(s.X, 0), max(screenW-s.Width, 0))
	s.Y = min(max(s.Y, 0), max(screenH-s.Height, 0))
	return s
}

// LoadWindowState reads the window state saved in dataDir. ok is false when there
// is none or it is unreadable, in which case the app opens with its default size.
func LoadWindowState(dataDir string) (state WindowState, ok bool, err error) {
	f, err := os.Open(filepath.Join(dataDir, WindowStateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return WindowState{}, false, nil
	}
	if err != nil {
		return WindowState{}, false, fmt.Errorf("desktop: read window state: %w", err)
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(&limitedReader{f: f, n: maxWindowStateBytes})
	if err := dec.Decode(&state); err != nil {
		return WindowState{}, false, fmt.Errorf("desktop: read window state: %w", err)
	}
	if !state.Valid() {
		return WindowState{}, false, nil
	}
	return state, true, nil
}

// SaveWindowState writes s to dataDir atomically (a temporary file renamed over the
// old one), readable by the owner only.
func SaveWindowState(dataDir string, s WindowState) error {
	if !s.Valid() {
		return nil
	}
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("desktop: save window state: %w", err)
	}
	tmp, err := os.CreateTemp(dataDir, WindowStateFile+".*.tmp")
	if err != nil {
		return fmt.Errorf("desktop: save window state: %w", err)
	}
	name := tmp.Name()
	_, werr := tmp.Write(append(data, '\n'))
	cerr := tmp.Close()
	if err = errors.Join(werr, cerr); err == nil {
		err = os.Rename(name, filepath.Join(dataDir, WindowStateFile))
	}
	if err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("desktop: save window state: %w", err)
	}
	return nil
}

// limitedReader reads at most n bytes and then fails, so an oversized file is an
// error rather than silently truncated JSON.
type limitedReader struct {
	f *os.File
	n int64
}

// Read implements io.Reader.
func (r *limitedReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, errors.New("file too large")
	}
	if int64(len(p)) > r.n {
		p = p[:r.n]
	}
	n, err := r.f.Read(p)
	r.n -= int64(n)
	return n, err
}
