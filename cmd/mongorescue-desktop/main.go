//go:build desktop

// Package main is the MongoRescue desktop app: the embedded web dashboard in a native
// window (Wails v2), with the REST API served in-process to the webview and no TCP
// listener. Build it with "make desktop" (the desktop build tag and CGO are required).
//
// Flags: -data-dir (default <user config dir>/MongoRescue/data) and -log-level. Logs go
// to <data dir>/desktop.log.
//
// At startup the app checks GitHub for a newer release (see internal/update): a higher
// MAJOR version blocks the window until the user updates, MINOR and PATCH updates are
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
	"syscall"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/yigitcittan/mongorescue/internal/app"
	"github.com/yigitcittan/mongorescue/internal/desktop"
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

	application, err := app.New(cfg, logger, app.WithBuildInfo(Version, Commit), app.WithGetenv(getenv))
	if err != nil {
		logger.Error("application bootstrap failed", slog.Any("error", err))
		return 1
	}
	d := &desktopApp{app: application, logger: logger}
	// The update check and downloads live until shutdown.
	d.updateCtx, d.stopUpdates = context.WithCancel(context.Background())
	defer d.stopUpdates()
	d.updater = desktop.NewUpdater(desktop.UpdaterOptions{
		Version: Version,
		Logger:  logger,
		Quit:    d.quit,
		OpenURL: d.openURL,
	})

	// The signal watcher lives until run returns: done is closed after the shutdown.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
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
		AssetServer: &assetserver.Options{Handler: d.updater.Handler(desktop.Handler(application.Handler()))},
		OnStartup:   d.startup,
		OnDomReady:  d.domReady,
		OnShutdown:  d.shutdown,
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId:               singleInstanceID,
			OnSecondInstanceLaunch: d.secondInstance,
		},
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

// desktopApp binds the application lifecycle to the Wails window.
type desktopApp struct {
	app    *app.App
	logger *slog.Logger
	// wg tracks the goroutines started here (signal watcher, quit requests).
	wg sync.WaitGroup

	updater     *desktop.Updater
	updateCtx   context.Context // bounds the update check and downloads
	stopUpdates context.CancelFunc

	ctxMu    sync.Mutex
	wailsCtx context.Context // set by startup, used by the runtime calls
	startErr error

	setupLogOnce sync.Once

	mu   sync.Mutex // serializes shutdown
	done bool
}

// startup starts the scheduler and the background workers when the window opens. A
// failure is logged and quits the app; no dialog is opened, since the window's run
// loop is not up yet.
func (d *desktopApp) startup(ctx context.Context) {
	// The scheduler lives until Stop, not until the Wails context ends.
	err := d.app.Start(context.Background())
	d.ctxMu.Lock()
	d.wailsCtx, d.startErr = ctx, err
	d.ctxMu.Unlock()
	if err != nil {
		d.logger.Error("failed to start background services; quitting", slog.Any("error", err))
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			runtime.Quit(ctx)
		}()
		return
	}
	d.logger.Info("mongorescue desktop ready")
	d.updater.Start(d.updateCtx)
}

// quit asks the window to close without waiting for it: the updater calls it after
// starting the installer, and the shutdown waits for the updater.
func (d *desktopApp) quit() {
	ctx := d.context()
	if ctx == nil {
		return
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		runtime.Quit(ctx)
	}()
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

// secondInstance brings the window to the front when the app is launched again.
func (d *desktopApp) secondInstance(options.SecondInstanceData) {
	ctx := d.context()
	if ctx == nil {
		return
	}
	runtime.WindowUnminimise(ctx)
	runtime.Show(ctx)
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

// shutdown stops the updater, in-flight runs, the scheduler and the background
// workers, then releases the database and the data directory lock. It runs once;
// concurrent calls wait for the first to finish.
func (d *desktopApp) shutdown(context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.done {
		return
	}
	d.done = true
	d.stopUpdates()
	d.updater.Wait()
	d.app.Stop()
	if err := d.app.Close(); err != nil {
		d.logger.Error("failed to close metadata store", slog.Any("error", err))
	}
}
