//go:build desktop

// Package main is the MongoRescue desktop app: the embedded web dashboard in a native
// window (Wails v2), with the REST API served in-process to the webview and no TCP
// listener. Build it with "make desktop" (the desktop build tag and CGO are required).
//
// Flags: -data-dir (default <user config dir>/MongoRescue/data) and -log-level. Logs go
// to <data dir>/desktop.log. --after-update=<pid> is set by the in-app update on
// Windows when it starts the new version: the app then waits for the old process to
// exit before it opens the data directory.
//
// On Windows the app keeps running in the background with a tray icon: closing the
// window hides it, so scheduled backups continue, and the tray menu quits the app.
// --hidden, which the "Start with Windows" entry passes, starts it hidden in the
// tray.
//
// At startup, every 30 minutes while it runs, when the window is shown again after
// 5 minutes and on request (the tray's "Check for updates", the dashboard header),
// the app checks GitHub for a newer release (see internal/update): a higher MAJOR
// version blocks the window until the user updates, MINOR and PATCH updates are
// offered. Builds without a release version (-ldflags "-X main.Version=X.Y.Z") skip
// the check.
package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/yigitcittan/mongorescue/internal/app"
	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/desktop"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// Version metadata populated at build time via -ldflags. A Version that is not a
// final release (X.Y.Z), such as the default, disables the update check.
var (
	Version = "dev"
	Commit  = "dev"
)

// singleInstanceID identifies the app for the Wails single instance lock: the data
// directory is locked, so a second instance could not start anyway.
const singleInstanceID = "io.github.yigitcittan.mongorescue.desktop"

// quitGrace is how long a SIGINT/SIGTERM waits for the window to close normally before
// the app stops its runs itself and exits.
const quitGrace = 10 * time.Second

// lockRetry is the interval at which a version started after an update tries again
// to lock the data directory while the old process still holds it.
const lockRetry = 250 * time.Millisecond

// appIcon is the MongoRescue logo. wails build turns it into the macOS and Windows
// icons; on Linux the window icon is set from it at run time.
//
//go:embed build/appicon.png
var appIcon []byte

// errStartFailed is returned by run when the background services did not start.
var errStartFailed = errors.New("background services did not start")

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stderr))
}

// run builds the application, runs the window until it is closed and returns the
// process exit code.
func run(args []string, getenv func(string) string, stderr io.Writer) int {
	// After an in-app update the old process is still shutting down: wait for it to
	// exit (it holds the data directory lock and the single instance lock) before
	// the log file is truncated and the data directory opened.
	oldPID, args := desktop.ParseAfterUpdate(args)
	// A relaunch after an update always shows the window.
	hidden, args := desktop.ParseHidden(args, oldPID > 0)
	oldExited := true
	if oldPID > 0 {
		oldExited = desktop.WaitForProcessExit(oldPID, desktop.AfterUpdateWait)
	}
	deadline := time.Now().Add(desktop.AfterUpdateLockWait)
	cfg, err := desktop.ParseConfig(args, getenv, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	logger, logFile, err := desktop.NewLogger(cfg, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer func() { _ = logFile.Close() }()
	slog.SetDefault(logger)
	logger.Info("starting mongorescue desktop", slog.String("version", Version), slog.String("data_dir", cfg.DataDir))
	if oldPID > 0 {
		logger.Info("started after an update", slog.Int("old_pid", oldPID), slog.Bool("old_exited", oldExited))
	}

	application, err := newApp(cfg, logger, getenv, oldPID > 0, deadline)
	if err != nil {
		logger.Error("application bootstrap failed", slog.Any("error", err))
		if oldPID > 0 && errors.Is(err, store.ErrDataDirLocked) {
			desktop.ShowError("MongoRescue", "MongoRescue was updated, but the new version could not start: the previous version still holds the data directory "+
				cfg.DataDir+".\r\n\r\nClose MongoRescue (or end MongoRescue.exe in Task Manager) and start it again.")
		}
		return 1
	}
	d := &desktopApp{app: application, logger: logger, closed: make(chan struct{})}
	// The update check and downloads live until shutdown.
	d.updateCtx, d.stopUpdates = context.WithCancel(context.Background())
	defer d.stopUpdates()
	d.updater = desktop.NewUpdater(desktop.UpdaterOptions{
		Version: Version,
		Logger:  logger,
		Args:    args,
		Quit:    d.quit,
		OpenURL: d.openURL,
		Busy:    application.Busy,
	})
	// Windows: the tray icon and hiding the window on close.
	d.initBackground(cfg.DataDir)
	var beforeClose func(context.Context) bool
	if d.background != nil {
		beforeClose = d.beforeClose
		d.startHidden = hidden
	} else if hidden {
		logger.Info("ignoring " + desktop.HiddenFlag + ": the app runs in the background on Windows only")
		hidden = false
	}
	// A new version whose old process has not exited in time must not hand its
	// start over to it as a second instance.
	singleInstance := &options.SingleInstanceLock{
		UniqueId:               singleInstanceID,
		OnSecondInstanceLaunch: d.secondInstance,
	}
	if !oldExited {
		singleInstance = nil
	}

	// The signal watcher lives until run returns: done is closed after the shutdown.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	done := d.closed
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.watchSignals(sigCh, done)
	}()

	err = wails.Run(&options.App{
		Title:     "MongoRescue",
		Width:     1280,
		Height:    860,
		MinWidth:  960,
		MinHeight: 640,
		// The handler serves the dashboard files as well as the API: no Assets FS.
		// The update endpoints are answered before the application handler.
		AssetServer:        &assetserver.Options{Handler: d.updater.Handler(desktop.Handler(application.Handler()))},
		OnStartup:          d.startup,
		OnDomReady:         d.domReady,
		OnShutdown:         d.shutdown,
		OnBeforeClose:      beforeClose,
		StartHidden:        hidden,
		SingleInstanceLock: singleInstance,
		Mac: &mac.Options{
			About: &mac.AboutInfo{Title: "MongoRescue", Message: "MongoDB backup and restore\nVersion " + Version, Icon: appIcon},
		},
		Linux: &linux.Options{
			Icon:        appIcon,
			ProgramName: "mongorescue",
			// Wails' default while Linux options are nil (wailsapp/wails#2977).
			WebviewGpuPolicy: linux.WebviewGpuPolicyNever,
		},
	})
	// OnShutdown normally stopped and closed the application; this covers wails.Run
	// failing before the window started.
	d.shutdown(context.Background())
	close(done)
	signal.Stop(sigCh)
	d.wg.Wait()

	if err == nil && d.startError() != nil {
		err = errStartFailed
	}
	if err != nil {
		logger.Error("desktop window terminated with error", slog.Any("error", err))
		return 1
	}
	logger.Info("mongorescue desktop shutdown complete")
	return 0
}

// newApp builds the application. After an update (afterUpdate) it tries again
// until deadline while the data directory is still locked by the old process.
func newApp(cfg *config.Config, logger *slog.Logger, getenv func(string) string, afterUpdate bool, deadline time.Time) (*app.App, error) {
	for {
		application, err := app.New(cfg, logger, app.WithBuildInfo(Version, Commit), app.WithGetenv(getenv), app.WithDesktop())
		if err == nil || !afterUpdate || !errors.Is(err, store.ErrDataDirLocked) || time.Now().After(deadline) {
			return application, err
		}
		time.Sleep(lockRetry)
	}
}

// desktopApp binds the application lifecycle to the Wails window.
type desktopApp struct {
	app    *app.App
	logger *slog.Logger
	// wg tracks the goroutines started here (signal watcher, quit requests).
	wg sync.WaitGroup
	// closed is closed once the window has closed and the shutdown has run.
	closed chan struct{}

	updater     *desktop.Updater
	updateCtx   context.Context // bounds the update check and downloads
	stopUpdates context.CancelFunc

	ctxMu    sync.Mutex
	wailsCtx context.Context // set by startup, used by the runtime calls
	startErr error

	setupLogOnce sync.Once

	// background is the background mode (Windows): nil elsewhere, where closing
	// the window quits the app.
	background *desktop.Background
	platform   platformState
	// quitting is set once the app quits for real, so closing the window no
	// longer only hides it.
	quitting atomic.Bool
	// forced is set by the force quit: the shutdown records the runs it cancels.
	forced atomic.Bool
	// startHidden is set when the window starts hidden in the tray.
	startHidden bool

	mu   sync.Mutex // serializes shutdown
	done bool
}

// startup starts the scheduler and the background workers when the window opens. A
// failure is logged and quits the app; no dialog is opened, since the window's run
// loop is not up yet.
func (d *desktopApp) startup(ctx context.Context) {
	// The single instance and data directory locks are held now: repair and remove
	// what an in-app update left (Windows).
	desktop.RemoveUpdateLeftovers(d.logger)
	// The scheduler lives until Stop, not until the Wails context ends.
	err := d.app.Start(context.Background())
	d.ctxMu.Lock()
	d.wailsCtx, d.startErr = ctx, err
	d.ctxMu.Unlock()
	if err != nil {
		d.logger.Error("failed to start background services; quitting", slog.Any("error", err))
		d.quitting.Store(true)
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			runtime.Quit(ctx)
		}()
		return
	}
	d.logger.Info("mongorescue desktop ready")
	d.updater.Start(d.updateCtx)
	d.startBackground()
}

// quit asks the window to close without waiting for it: the updater calls it after
// starting the new version (in-app update) or the installer, and the shutdown waits
// for the updater; the tray's Quit and Force quit call it too (Windows). The window's shutdown stops the runs and releases the database
// and the data directory lock; the new version, or the installer, waits for this
// process to exit. When the window has not closed within quitGrace, the app stops
// the runs and releases the database itself and exits, so the new version is not
// left waiting.
func (d *desktopApp) quit() {
	ctx := d.context()
	if ctx == nil {
		return
	}
	d.quitting.Store(true)
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		runtime.Quit(ctx)
		timer := time.NewTimer(quitGrace)
		defer timer.Stop()
		select {
		case <-d.closed:
			return
		case <-timer.C:
		}
		d.logger.Warn("the window did not close in time, stopping now", slog.Duration("grace", quitGrace))
		d.shutdown(context.Background())
		d.logger.Info("mongorescue desktop stopped")
		os.Exit(0)
	}()
}

// beforeClose hides the window instead of closing it while the app runs in the
// background with a working tray icon (Windows), and lets it close once the app
// quits or when the tray is not available.
func (d *desktopApp) beforeClose(context.Context) bool {
	if d.background == nil || d.quitting.Load() || d.startError() != nil {
		return false
	}
	return d.background.WindowClosing()
}

// openURL opens url in the system browser.
func (d *desktopApp) openURL(url string) {
	if ctx := d.context(); ctx != nil {
		runtime.BrowserOpenURL(ctx, url)
	}
}

// context returns the Wails context, or nil before startup.
func (d *desktopApp) context() context.Context {
	d.ctxMu.Lock()
	defer d.ctxMu.Unlock()
	return d.wailsCtx
}

// startError returns the error of App.Start, if any.
func (d *desktopApp) startError() error {
	d.ctxMu.Lock()
	defer d.ctxMu.Unlock()
	return d.startErr
}

// domReady runs on every page load. It injects the update prompt, so a reload cannot
// get around a mandatory update, and fills the one-time setup code into the
// dashboard's setup form until the first administrator exists: the desktop app has
// no console to print the code to, and its window is the only client. The server
// still verifies the code. It is also logged, once per process.
func (d *desktopApp) domReady(ctx context.Context) {
	if d.startError() != nil {
		return
	}
	runtime.WindowExecJS(ctx, desktop.UpdateScript())
	code := d.app.SetupCode()
	if code == "" {
		return
	}
	d.setupLogOnce.Do(func() {
		d.logger.Warn("Setup required: the setup code "+code+" is filled in the window", slog.String("setup_code", code))
	})
	runtime.WindowExecJS(ctx, desktop.SetupCodeScript(code))
}

// secondInstance brings the window to the front when the app is launched again,
// also when it is hidden in the tray.
func (d *desktopApp) secondInstance(options.SecondInstanceData) {
	d.showWindow()
}

// watchSignals quits the window on the first SIGINT/SIGTERM. When the window has not
// closed within quitGrace, or a second signal arrives, it stops the runs and releases
// the database itself and exits with status 1, so no mongodump or mongorestore
// outlives the app. It returns once done is closed.
func (d *desktopApp) watchSignals(sigCh <-chan os.Signal, done <-chan struct{}) {
	select {
	case <-done:
		return
	case sig := <-sigCh:
		d.logger.Info("signal received, closing the window", slog.String("signal", sig.String()))
	}
	if ctx := d.context(); ctx != nil {
		d.quitting.Store(true)
		runtime.Quit(ctx)
	}
	timer := time.NewTimer(quitGrace)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-sigCh:
		d.logger.Warn("second signal received, stopping now")
	case <-timer.C:
		d.logger.Warn("the window did not close in time, stopping now", slog.Duration("grace", quitGrace))
	}
	d.shutdown(context.Background())
	d.logger.Info("mongorescue desktop stopped after a signal")
	os.Exit(1)
}

// shutdown stops the tray, the updater, in-flight runs, the scheduler and the
// background workers, then releases the database and the data directory lock.
// After a force quit, the cancelled runs are recorded with
// desktop.ForceQuitReason. It runs once; concurrent calls wait for the first to
// finish.
func (d *desktopApp) shutdown(context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.done {
		return
	}
	d.done = true
	d.quitting.Store(true)
	d.stopBackground()
	d.stopUpdates()
	d.updater.Wait()
	if d.forced.Load() {
		d.app.ForceStop(desktop.ForceQuitReason)
	} else {
		d.app.Stop()
	}
	if err := d.app.Close(); err != nil {
		d.logger.Error("failed to close metadata store", slog.Any("error", err))
	}
}
