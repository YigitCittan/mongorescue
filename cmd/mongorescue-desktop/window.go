//go:build desktop

package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/yigitcittan/mongorescue/internal/desktop"
)

// Default and minimum window sizes.
const (
	defaultWidth  = 1280
	defaultHeight = 860
	minWidth      = 960
	minHeight     = 640
)

// windowMemory remembers the window's size, position and maximised state in
// desktop.WindowStateFile in the data directory: it is loaded before the window
// opens, applied (clamped to the visible screen) at startup and saved when the
// window closes or the app quits.
type windowMemory struct {
	dataDir string
	logger  *slog.Logger

	mu    sync.Mutex
	state desktop.WindowState
	// saved is set once a state was loaded or saved, so a position is restored.
	saved bool
}

// loadWindowMemory reads the saved state of dataDir, or starts with the default size.
func loadWindowMemory(dataDir string, logger *slog.Logger) *windowMemory {
	m := &windowMemory{dataDir: dataDir, logger: logger, state: desktop.WindowState{Width: defaultWidth, Height: defaultHeight}}
	state, ok, err := desktop.LoadWindowState(dataDir)
	if err != nil {
		logger.Warn("ignoring the saved window state", slog.Any("error", err))
	}
	if ok {
		m.state, m.saved = state.Clamp(0, 0, minWidth, minHeight), true
	}
	return m
}

// initial returns the size the window opens with and whether it opens maximised.
func (m *windowMemory) initial() (width, height int, maximised bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.Width, m.state.Height, m.state.Maximised
}

// restore moves the window to the saved position and size, clamped to the screen it
// is on. Without a saved state the window stays where the platform put it.
func (m *windowMemory) restore(ctx context.Context) {
	m.mu.Lock()
	state, saved := m.state, m.saved
	m.mu.Unlock()
	if !saved || state.Maximised {
		return
	}
	w, h := currentScreen(ctx)
	state = state.Clamp(w, h, minWidth, minHeight)
	runtime.WindowSetSize(ctx, state.Width, state.Height)
	runtime.WindowSetPosition(ctx, state.X, state.Y)
}

// save records the window's current geometry. A minimised window keeps the last
// state; a maximised one keeps the last normal geometry with the maximised flag.
func (m *windowMemory) save(ctx context.Context) {
	if ctx == nil || runtime.WindowIsMinimised(ctx) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.state
	if runtime.WindowIsMaximised(ctx) {
		state.Maximised = true
	} else {
		state.Maximised = false
		state.Width, state.Height = runtime.WindowGetSize(ctx)
		state.X, state.Y = runtime.WindowGetPosition(ctx)
	}
	if !state.Valid() {
		return
	}
	if err := desktop.SaveWindowState(m.dataDir, state); err != nil {
		m.logger.Warn("failed to save the window state", slog.Any("error", err))
		return
	}
	m.state, m.saved = state, true
}

// currentScreen returns the logical size of the screen the window is on (else the
// primary screen), or zeros when the runtime does not know.
func currentScreen(ctx context.Context) (width, height int) {
	screens, err := runtime.ScreenGetAll(ctx)
	if err != nil {
		return 0, 0
	}
	for _, s := range screens {
		if s.IsCurrent {
			return s.Size.Width, s.Size.Height
		}
	}
	for _, s := range screens {
		if s.IsPrimary {
			return s.Size.Width, s.Size.Height
		}
	}
	return 0, 0
}
