//go:build desktop && !windows

package main

import "github.com/wailsapp/wails/v2/pkg/runtime"

// platformState is empty: the app runs in the background on Windows only.
type platformState struct{}

// initBackground does nothing: closing the window quits the app.
func (d *desktopApp) initBackground(string) {}

// startBackground does nothing: there is no tray icon.
func (d *desktopApp) startBackground() {
	d.platform.start()
}

// stopBackground does nothing: there is no tray icon.
func (d *desktopApp) stopBackground() {
	d.platform.stop()
}

// start does nothing.
func (*platformState) start() {}

// stop does nothing.
func (*platformState) stop() {}

// showWindow brings the window to the front.
func (d *desktopApp) showWindow() {
	ctx := d.context()
	if ctx == nil {
		return
	}
	runtime.WindowUnminimise(ctx)
	runtime.Show(ctx)
}
