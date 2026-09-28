# Desktop app

The desktop app runs MongoRescue on a workstation: the same dashboard as the container image, in a native window, with backups, restores, the scheduler and notifications running in the app process. It opens **no network port**. The dashboard and the REST API are served in-process to the window's webview ([Wails v2](https://wails.io)), so nothing else on the machine or the network can reach them.

| Distribution | Dashboard | Listener |
| :--- | :--- | :--- |
| Container image | served at `/` (`MONGORESCUE_DASHBOARD=true`) | HTTP on port 8080 |
| Desktop app | in the app window | none |
| Server binary (`mongorescue`) | off unless `-dashboard` / `MONGORESCUE_DASHBOARD=true` | HTTP (REST API, `/mcp`, `/metrics`) |

Use the server binary or the image when other programs need the REST API or MCP over HTTP; the desktop app serves neither.

## Install

Download the file for your system from [Releases](https://github.com/YigitCittan/mongorescue/releases):

- **Windows:** `MongoRescue-desktop_<version>_windows_amd64_installer.exe` (NSIS installer; a portable `.zip` is attached too). It needs the Microsoft Edge WebView2 runtime, which ships with Windows 10 and 11 but not with Windows Server 2016–2022; the installer fetches it if it is missing ([details](#windows-installer-and-webview2)).
- **macOS:** `MongoRescue-desktop_<version>_macos_universal.zip`, containing `MongoRescue.app` (Apple Silicon and Intel). The app is not notarized yet: open it with right-click → **Open** the first time.
- **Linux:** `MongoRescue-desktop_<version>_linux_amd64.tar.gz`, a single binary. It needs GTK 3 and WebKitGTK 4.1 (`libgtk-3-0` and `libwebkit2gtk-4.1-0` on Debian and Ubuntu 24.04+).

Install the [MongoDB Database Tools](https://www.mongodb.com/docs/database-tools/) (`mongodump`, `mongorestore`, 100.3.0 or newer) and make sure they are on the `PATH` the app sees. On macOS, apps started from Finder do not read your shell profile; Homebrew's `/opt/homebrew/bin` may not be on their `PATH`.

## First start

On first start the app fills the one-time setup code into the setup form itself and hides the field, since its window is the only client: choose a username and a password to create the administrator account. The server still checks the code as it does in the browser. The code is also written to the log file.

Only one instance runs at a time: starting the app again brings the open window to the front.

Closing the window stops the app: in-flight backups and restores are cancelled (the same shutdown as the server, up to about 35 seconds), and scheduled jobs run only while the app is open. `SIGINT`/`SIGTERM` close the window the same way; if it has not closed within 10 seconds, or on a second signal, the app stops its runs and releases the database itself and exits with status 1.

If the background services cannot start, the app logs the error to `desktop.log` and quits.

## Data and logs

| | Default location |
| :--- | :--- |
| Data directory (`mongorescue.db`, `secret.key`, lock) | `<user config dir>/MongoRescue/data` |
| Default "Local disk" backup target | `<user config dir>/MongoRescue/backups` |
| Log file (truncated on every start) | `<data dir>/desktop.log` |

`<user config dir>` is `%AppData%` on Windows, `~/Library/Application Support` on macOS and `$XDG_CONFIG_HOME` (usually `~/.config`) on Linux.

The app accepts `-data-dir` (or `MONGORESCUE_DATA_DIR`), `-log-level` and `MONGORESCUE_SECRET_KEY`; the listen address options and `MONGORESCUE_DASHBOARD` are not read, so leftover server variables cannot keep it from starting. As with the server, back up the data directory and keep a copy of `secret.key` apart from your database backups ([production.md](production.md#data-directory)).

A data directory can be used by one process at a time: stop a server that uses the same directory before opening it in the app.

## How it works

`cmd/mongorescue-desktop` builds the application with `internal/app` like the server does (`Dashboard` on) and calls `App.Start` when the window opens and `App.Stop` and `App.Close` when it closes. Instead of `App.Run`, which starts the HTTP server, it hands `App.Handler()` to the Wails asset server, which answers the webview's requests in-process.

`internal/desktop.Handler` wraps that handler for the webview:

- **Session cookie jar.** WKWebView (macOS) does not reliably store cookies for the custom `wails://` scheme. For same-origin requests (`Sec-Fetch-Site` absent, `same-origin` or `none`, and `Origin` absent or naming the request host) the wrapper records the `mr_session` cookie from `Set-Cookie` responses, forgets it on logout or expiry, and adds it to requests that do not carry it. Cross-site requests never receive the cookie and cannot set or clear it.
- **Same-origin requests.** The webview's origin is `wails://wails` (macOS, Linux) or `http://wails.localhost` (Windows). The server refuses unauthenticated `POST`s (setup, login) with a non-HTTP `Origin`, so the wrapper drops an `Origin` header that names the request's own host. Requests from any other origin keep it and are judged by the server.

Authentication, CSRF tokens, scopes and settings work exactly as in the browser.

## Building

The desktop build needs CGO and the platform webview headers, unlike the server (`CGO_ENABLED=0`). Its Go files carry the `desktop` build tag, so `go build ./...`, `go test ./...` and `go vet ./...` never compile Wails.

1. Install the Wails CLI matching `go.mod`: `go install github.com/wailsapp/wails/v2/cmd/wails@v2.16.0`, and check the platform prerequisites with `wails doctor`:
   - macOS: Xcode Command Line Tools.
   - Windows: the WebView2 runtime; [NSIS](https://nsis.sourceforge.io) (`makensis`) for the installer.
   - Linux: `gcc`, `pkg-config`, `libgtk-3-dev` and `libwebkit2gtk-4.0-dev`, or `libwebkit2gtk-4.1-dev` with `DESKTOP_TAGS=desktop,webkit2_41` (Ubuntu 24.04+).
2. Build:

```bash
make desktop            # app for this OS in cmd/mongorescue-desktop/build/bin/
make desktop-windows    # on Windows: MongoRescue.exe and the NSIS installer
```

The frontend is `web/static`, served by the Go handler, so the build skips the Wails frontend (npm) step and binding generation.

`cmd/mongorescue-desktop/build/` holds the committed build assets; Wails generates the other platform files there (`Info.plist`, `info.json`, `wails.exe.manifest`, `wails_tools.nsh`) and they stay untracked, as does the output in `build/bin` (the only directory `wails build -clean` empties):

| File | Use |
| :--- | :--- |
| `appicon.png` | The logo (from `web/static/favicon.svg`, 1024×1024): the macOS `.app` icon, and the Linux window icon (embedded in the binary) |
| `windows/icon.ico` | The `.exe`, window, taskbar, installer and uninstaller icon (16 to 256 px) |
| `windows/installer/project.nsi` | The NSIS installer script, customized from the Wails template (see below) |

After changing the logo, regenerate both icons; delete a file to get the Wails default back.

For a quick compile check without the Wails CLI: `go vet -tags desktop ./cmd/mongorescue-desktop/...`. A binary built with plain `go build` needs the `desktop,production` tags; `wails build` adds them.

Releases build the desktop app natively on Windows, macOS and Linux runners (`desktop` job in `.github/workflows/release.yml`) and attach the archives, with provenance attestations, to the GitHub release.

## Windows installer and WebView2

The app window needs the Microsoft Edge WebView2 runtime. Windows 10 and 11 include it; Windows Server 2016, 2019 and 2022 do not unless something installed it. The installer checks the per-machine registration (`HKLM\SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`, and the key without `WOW6432Node`) and the per-user one (`HKCU\Software\Microsoft\EdgeUpdate\Clients\{…}`), skips the step when either names a version other than `0.0.0.0`, and otherwise runs Microsoft's online bootstrapper, which downloads the runtime (well over 100 MB). The status line reads *Installing Microsoft Edge WebView2 Runtime…* and the bootstrapper shows its own progress window; with `/S` it runs silently. If the runtime is still missing afterwards the installer says so, with the exit code and the [download link](https://go.microsoft.com/fwlink/p/?LinkId=2124703), and finishes the installation; the app offers the download again when it starts without the runtime. On machines without internet access, install the WebView2 runtime first (the offline "Evergreen Standalone Installer").

v0.3.1 used the unmodified Wails template: it looked only at the per-machine key (the per-user key only for per-user installers) and ran the bootstrapper with `/silent`, ignoring its result. On a server without WebView2 the installer therefore sat on *Installing: WebView2 Runtime* with no progress for the whole download and installation, and looked hung. This is the likely cause of the long pause reported on Windows Server; it follows from the template and from Windows Server not shipping WebView2, but was not reproduced on a Windows machine. The installer's compression (NSIS default, zlib) is not a factor: extracting the files takes seconds. Antivirus scanning of the unsigned executable can add a short delay.
