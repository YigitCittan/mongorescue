//go:build desktop

package main

import (
	"context"
	_ "embed"
	"path/filepath"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/yigitcittan/mongorescue/internal/desktop"
)

// trayIcon is the MongoRescue icon of the tray.
//
//go:embed build/windows/icon.ico
var trayIcon []byte

// platformState is the tray of the background mode.
type platformState struct {
	tray     *desktop.Tray
	trayCtx  context.Context
	stopTray context.CancelFunc
}

// initBackground sets up the background mode: closing the window hides it, and
// the tray icon opens it again and quits the app.
func (d *desktopApp) initBackground(dataDir string) {
	d.platform.trayCtx, d.platform.stopTray = context.WithCancel(context.Background())
	d.background = desktop.NewBackground(desktop.BackgroundOptions{
		Runs:      d.app,
		Language:  desktop.UILanguage(),
		Quit:      d.quit,
		ForceQuit: d.forceQuit,
		Confirm:   desktop.ConfirmBox,
		HideWindow: func() {
			if ctx := d.context(); ctx != nil {
				runtime.WindowHide(ctx)
			}
		},
		Notify:     func(title, message string) error { return d.platform.tray.Notify(title, message) },
		NoticeFile: filepath.Join(dataDir, desktop.BackgroundNoticeFile),
		UpdateWaiting: func() bool {
			return d.updater.Status().State == desktop.UpdateWaiting
		},
		Changed: func() { d.platform.tray.Refresh() },
		Logger:  d.logger,
	})
	d.platform.tray = desktop.NewTray(desktop.TrayOptions{
		Icon:       trayIcon,
		Background: d.background,
		Autostart:  &desktop.Autostart{Key: desktop.NewRunKey()},
		Open:       d.showWindow,
		Logger:     d.logger,
	})
}

// startBackground shows the tray icon once the app runs.
func (d *desktopApp) startBackground() {
	d.platform.start()
}

// stopBackground ends a soft quit or a confirmation in progress, then removes the
// tray icon and waits for its goroutines.
func (d *desktopApp) stopBackground() {
	if d.background != nil {
		d.background.Close()
	}
	d.platform.stop()
}

// start shows the tray icon.
func (p *platformState) start() {
	if p.tray != nil {
		p.tray.Start(p.trayCtx)
	}
}

// stop removes the tray icon and waits for its goroutines.
func (p *platformState) stop() {
	if p.tray != nil {
		p.stopTray()
		p.tray.Wait()
	}
}

// forceQuit quits like quit, and the shutdown records the backups and restores
// it cancels as failed with desktop.ForceQuitReason.
func (d *desktopApp) forceQuit() {
	d.forced.Store(true)
	d.quit()
}

// showWindow shows the window, also when it is hidden in the tray, and brings it
// to the front.
func (d *desktopApp) showWindow() {
	if ctx := d.context(); ctx != nil {
		runtime.WindowShow(ctx)
	}
}
