//go:build desktop && desktop_e2e

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/desktop"
)

// The desktop update end-to-end test (internal/desktop/updatee2e) runs a build with
// the desktop_e2e tag as --e2e-update=<dir>. Without a window, the app then
// creates its Updater as run does, with the release source of
// desktop.E2EUpdateBaseURLEnv, and drives it through Updater.Handler as the
// dashboard's update script does: POST check, GET status, POST install, and GET
// status until the update is ready (macOS, Linux), failed, or asked the app to quit
// (Windows, after it started the new version). It writes the outcome to
// <dir>/result.json and exits. The installer launch, the file manager and the
// browser are replaced by files in <dir> that record the call (installer-launched,
// revealed, opened-url), so nothing else is started. A version started after the
// update (with --after-update=<pid> as well) waits for the old process to exit,
// removes the update's leftovers as the app does at startup, writes
// <dir>/restarted.json and exits.

// e2eFlag is the flag, with the result directory as its value, that runs the
// headless update.
const e2eFlag = "--e2e-update="

// Time limits of the headless update: the whole run, and how long the install
// may be refused while a check runs.
const (
	e2eTimeout     = 3 * time.Minute
	e2eInstallWait = 30 * time.Second
)

// errE2ENoInstaller is returned by the installer launch of the headless update.
var errE2ENoInstaller = errors.New("the end-to-end test never starts an installer")

// e2eResult is <dir>/result.json.
type e2eResult struct {
	Version string `json:"version"`
	PID     int    `json:"pid"`
	// Check is the HTTP status of POST UpdateCheckPath.
	Check int `json:"check"`
	// Install is the HTTP status of POST UpdateInstallPath, 0 when not sent.
	Install int `json:"install"`
	// Status is the last update status.
	Status desktop.UpdateStatus `json:"status"`
	// Quit reports whether the updater asked the app to quit.
	Quit bool `json:"quit"`
	// Error says why the run stopped early (timeout, an unexpected answer).
	Error string `json:"error,omitempty"`
}

// e2eRestarted is <dir>/restarted.json.
type e2eRestarted struct {
	Version   string `json:"version"`
	PID       int    `json:"pid"`
	OldPID    int    `json:"old_pid"`
	OldExited bool   `json:"old_exited"`
	Exe       string `json:"exe"`
}

// runE2E runs the headless update when args hold e2eFlag and returns its exit code
// and true; otherwise it returns false.
func runE2E(args []string) (int, bool) {
	oldPID, rest := desktop.ParseAfterUpdate(args)
	dir := ""
	for _, a := range rest {
		if v, ok := strings.CutPrefix(a, e2eFlag); ok {
			dir = v
		}
	}
	if dir == "" {
		return 0, false
	}
	logFile, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("harness-%d.log", os.Getpid())), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1, true
	}
	defer func() { _ = logFile.Close() }()
	logger := slog.New(slog.NewTextHandler(io.MultiWriter(logFile, os.Stderr), &slog.HandlerOptions{Level: slog.LevelDebug}))
	logger.Info("desktop update e2e", slog.String("version", Version), slog.Int("pid", os.Getpid()), slog.Int("old_pid", oldPID))
	if oldPID > 0 {
		return e2eRestart(dir, oldPID, logger), true
	}
	return e2eUpdate(dir, rest, logger), true
}

// e2eRestart is the version started by the update: it waits for the old process,
// removes the update's leftovers and writes restarted.json.
func e2eRestart(dir string, oldPID int, logger *slog.Logger) int {
	exited := desktop.WaitForProcessExit(oldPID, desktop.AfterUpdateWait)
	desktop.RemoveUpdateLeftovers(logger)
	exe, err := desktop.ExecutablePath()
	if err != nil {
		logger.Error("executable path", slog.Any("error", err))
	}
	return e2eWrite(dir, "restarted.json", e2eRestarted{Version: Version, PID: os.Getpid(), OldPID: oldPID, OldExited: exited, Exe: exe}, logger)
}

// e2eUpdate runs the update and writes result.json.
func e2eUpdate(dir string, args []string, logger *slog.Logger) int {
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	record := func(name, value string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o600); err != nil {
			logger.Error("record a call", slog.String("file", name), slog.Any("error", err))
		}
	}
	quit := make(chan struct{})
	var quitOnce sync.Once
	updater := desktop.NewUpdater(desktop.UpdaterOptions{
		Version: Version,
		Logger:  logger,
		Args:    args,
		Quit:    func() { quitOnce.Do(func() { close(quit) }) },
		OpenURL: func(url string) { record("opened-url", url) },
		Launch: func(path string, a []string) error {
			record("installer-launched", path+" "+strings.Join(a, " "))
			return errE2ENoInstaller
		},
		Reveal: func(_ context.Context, path string) error {
			record("revealed", path)
			return nil
		},
	})
	updateCtx, stopUpdates := context.WithCancel(ctx)
	defer stopUpdates()
	updater.Start(updateCtx)
	h := updater.Handler(http.NotFoundHandler())

	res := e2eResult{Version: Version, PID: os.Getpid()}
	res.Check, _ = e2eCall(h, http.MethodPost, desktop.UpdateCheckPath)
	_, res.Status = e2eCall(h, http.MethodGet, desktop.UpdatePath)
	if res.Check != http.StatusOK || !res.Status.Available {
		res.Error = "no update found"
	} else {
		res.Error = e2eInstall(ctx, h, quit, &res)
	}
	select {
	case <-quit:
		res.Quit = true
	default:
	}
	// The app quits now, as on a normal close: the update's goroutines end first.
	stopUpdates()
	updater.Wait()
	_, res.Status = e2eCall(h, http.MethodGet, desktop.UpdatePath)
	code := e2eWrite(dir, "result.json", res, logger)
	if res.Error != "" {
		logger.Error("headless update stopped", slog.String("error", res.Error))
		return 1
	}
	return code
}

// e2eInstall posts the install and polls the status until the update is ready,
// failed or quit the app; it records the answers in res and returns why it stopped
// early, or "".
func e2eInstall(ctx context.Context, h http.Handler, quit <-chan struct{}, res *e2eResult) string {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	// The startup check may still run: the install answers 409 until it is done.
	accept := time.After(e2eInstallWait)
	for {
		res.Install, res.Status = e2eCall(h, http.MethodPost, desktop.UpdateInstallPath)
		if res.Install == http.StatusAccepted {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Sprintf("install answered %d until %v", res.Install, ctx.Err())
		case <-accept:
			return fmt.Sprintf("install answered %d for %v", res.Install, e2eInstallWait)
		case <-tick.C:
		}
	}
	for {
		select {
		case <-quit:
			return ""
		case <-ctx.Done():
			return "update did not finish: " + ctx.Err().Error()
		case <-tick.C:
		}
		_, res.Status = e2eCall(h, http.MethodGet, desktop.UpdatePath)
		if res.Status.State == desktop.UpdateReady || res.Status.State == desktop.UpdateError {
			return ""
		}
	}
}

// e2eCall sends a same-origin request to the update endpoints, as the webview does,
// and returns the HTTP status and the decoded update status (zero for an error
// answer).
func e2eCall(h http.Handler, method, path string) (int, desktop.UpdateStatus) {
	const origin = "http://wails.localhost"
	req := httptest.NewRequest(method, origin+path, nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if method == http.MethodPost {
		req.Header.Set(desktop.UpdateHeader, "1")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var st desktop.UpdateStatus
	if rec.Code < 300 {
		_ = json.Unmarshal(rec.Body.Bytes(), &st)
	}
	return rec.Code, st
}

// e2eWrite writes v as JSON to dir/name and returns the exit code.
func e2eWrite(dir, name string, v any, logger *slog.Logger) int {
	b, err := json.MarshalIndent(v, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, name), b, 0o600)
	}
	if err != nil {
		logger.Error("write the result", slog.String("file", name), slog.Any("error", err))
		return 1
	}
	return 0
}
