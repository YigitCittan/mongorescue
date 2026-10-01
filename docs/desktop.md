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

- **Windows:** `MongoRescue-desktop_<version>_windows_amd64_installer.exe` (NSIS installer; a portable `.zip` is attached too). The installer installs for the current user, without an administrator prompt, into `%LOCALAPPDATA%\Programs\MongoRescue`, with its Start menu and desktop shortcuts and its entry in *Installed apps* under your account (HKCU); earlier versions installed per machine into `Program Files\MongoRescue\MongoRescue` ([moving from Program Files](#updates)). It needs the Microsoft Edge WebView2 runtime, which ships with Windows 10 and 11 but not with Windows Server 2016–2022; the installer fetches it if it is missing ([details](#windows-installer-and-webview2)).
- **macOS:** `MongoRescue-desktop_<version>_macos_universal.zip`, containing `MongoRescue.app` (Apple Silicon and Intel). The app is not notarized yet: open it with right-click → **Open** the first time.
- **Linux:** `MongoRescue-desktop_<version>_linux_amd64.tar.gz`, the `MongoRescue` binary and its `tools/` directory; keep them together. It needs GTK 3 and WebKitGTK 4.1 (`libgtk-3-0` and `libwebkit2gtk-4.1-0` on Debian and Ubuntu 24.04+); the bundled tools need glibc 2.34+ and `libgssapi_krb5.so.2` (`libgssapi-krb5-2`).

Every package includes `mongodump` and `mongorestore` from the [MongoDB Database Tools](https://www.mongodb.com/docs/database-tools/) (100.12.2, Apache License 2.0; `LICENSE.md` and `THIRD-PARTY-NOTICES` sit next to them): `<install dir>\tools\` on Windows (installer and portable zip), `MongoRescue.app/Contents/Resources/tools/` on macOS (universal binaries) and `tools/` next to the binary on Linux. The app finds them before `PATH`, so nothing else needs to be installed. To use other tools, set `MONGORESCUE_TOOLS_DIR` to their directory ([search order](configuration.md#mongodb-database-tools)).

## First start

On first start the app fills the one-time setup code into the setup form itself and hides the field, since its window is the only client: choose a username and a password to create the administrator account. The server still checks the code as it does in the browser. The code is also written to the log file.

Only one instance runs at a time: starting the app again brings the open window to the front.

Closing the window stops the app: in-flight backups and restores are cancelled (the same shutdown as the server, up to about 35 seconds), and scheduled jobs run only while the app is open. `SIGINT`/`SIGTERM` close the window the same way; if it has not closed within 10 seconds, or on a second signal, the app stops its runs and releases the database itself and exits with status 1.

If the background services cannot start, the app logs the error to `desktop.log` and quits.

## Updates

When the window opens, the app asks GitHub for the latest release (`GET https://api.github.com/repos/YigitCittan/mongorescue/releases/latest`, 10 second timeout), and again every 30 minutes while it runs, which on Windows can be for days in the tray. It also checks when the window is shown again from the tray or by starting the app a second time, if the last check is more than 5 minutes old, and on request:

- **Tray (Windows):** **Check for updates** (*Güncellemeleri denetle*). While the check runs the status line reads *Checking for updates…*, then for two minutes *MongoRescue is up to date (vX)* or *Could not check for updates*. When a newer version is available the item reads **Update to vX** and starts the in-app update (or opens the release page when the release has no file for your system); the status line shows *Updating MongoRescue…* while it downloads and installs.
- **Dashboard:** click the version label in the header (or focus it and press Enter) for a small popover with **Check for updates**. It shows *Up to date (vX)*, the error, or the new version, in which case the update bar comes back even after **Later**.

All checks run one at a time in the same loop: a request while a check runs shares its result. After a failed check (offline, GitHub unreachable) the next one waits 1 hour, then 2, 4 and at most 6 hours; the first successful check returns to 30 minutes. When GitHub answers that its rate limit is used up (`403`/`429` with `X-RateLimit-Remaining: 0` or `Retry-After`), the app does not ask again before `X-RateLimit-Reset` or `Retry-After` (at most 6 hours), and a check requested meanwhile reports the rate limit without a request. Without a token GitHub allows 60 requests an hour per address, far more than the app uses. The window picks up a new result within 10 minutes, and shortly after it is shown again. Releases are published only once their desktop files and checksums are attached ([Building](#building)). Drafts, pre-releases and tags with a pre-release suffix (`v1.2.0-rc.1`) are ignored.

| Latest release | What the app does |
| :--- | :--- |
| Higher **MAJOR** version (`1.x.y` → `2.0.0`) | **Mandatory.** A full-screen "Update required" screen covers the dashboard, with the release notes, **Update now** and a link to the release page. It cannot be closed (Escape does nothing, the dashboard behind it cannot be reached) and comes back on every reload until the new version is installed. |
| Higher MINOR or PATCH version | **Optional.** A bar at the bottom of the window: **Update**, **Release notes** (shown in the bar) and **Later**, which hides the bar until the app is started again. An **Update** button in the dashboard header (its tooltip names the new version) stays while the update is available, also after **Later**: it shows the bar again and starts the update, or opens the release page when there is no file for your system. |
| Same or lower version | Nothing. |

A major release is mandatory only when it has the file for your system and the checksums file. Without them (a platform the release has no build for, or files still missing) the app shows the optional bar with the release page instead of **Update**. **Try again** after a failed update looks the release up again before downloading.

**Update** downloads the file for your system from the release and verifies it before using it:

- **Windows:** the app updates itself in place, without an installer window or administrator prompt, when you can write to the folder `MongoRescue.exe` runs from (symlinks resolved; checked by creating and removing a temporary file): the per-user install in `%LOCALAPPDATA%\Programs\MongoRescue` and portable copies. It downloads the portable archive (`…_windows_amd64_portable.zip`) into a new directory under `%LocalAppData%\MongoRescue\updates` (earlier ones are removed); the bar and the header button show *Downloading x%*. The archive is opened so that nothing can change or delete it, checked against its SHA-256 again through that handle and unpacked, *Installing…*, into `<install dir>\.update-<version>`: only `MongoRescue.exe` at the root and plain files directly in `tools\` are accepted (names of 1 to 64 letters, digits, `.`, `_` or `-` starting with a letter or digit, no Windows device names such as `CON` or `NUL.txt`, no duplicates ignoring case; no other paths, `..`, absolute paths, streams or links; at most 512 MiB per file), each streamed to disk through a handle rooted in that folder. The folder is marked as the update's (`.mongorescue-staging`) when it is created and as complete (`.complete`) once everything is unpacked. While a backup or restore runs, the update waits, showing *Update will install after the running backup finishes*, and goes on by itself once no run is left (quitting would cancel the run). The app then renames the running `MongoRescue.exe` to `MongoRescue.exe.old` (Windows allows renaming a running program) and moves the new one in, and does the same with `tools\` (`tools.old`). If any step fails, every rename is undone, the bar shows the error with **Try again** and the running version stays. Otherwise the app starts the new version as the same user with `--after-update=<pid>` and the arguments it was started with, shows *Restarting…* and closes, cancelling in-flight runs and releasing the data directory as on a normal close (a backup that is running is cancelled, as on quit). The new version waits up to 60 seconds for the old process to exit before it opens the data directory (and up to 30 seconds more while the directory is still locked; then it shows an error dialog instead of exiting silently), so the window comes back after a second or two; once it holds the single instance and data directory locks it removes `MongoRescue.exe.old` and `tools.old` (only while `MongoRescue.exe` and `tools\` exist) and the `.update-<version>` folders that carry the update's marker; nothing else in the folder is touched. If the swap was cut short (a crash or power loss between the renames), the next start first repairs it: a missing `MongoRescue.exe` comes back from `MongoRescue.exe.old`, and a missing `tools\` from a complete staging folder (the new executable was already in place) or else from `tools.old`. An update fails, and is rolled back, while a `mongodump` or `mongorestore` from `tools\` still runs and keeps the folder in use; try again once the run is over.

  A copy you cannot write to, typically a per-machine install in `Program Files` by an earlier version, falls back to the installer (`…_windows_amd64_installer.exe`, verified the same way), started silently with `/S /RELAUNCH=<DOMAIN\user> /WAITPID=<pid>` through `ShellExecute`. The bar reads *Installing the update…* and the app closes. The installer waits up to 180 seconds for the app's process to exit (and up to 180 seconds more for a running `MongoRescue.exe` in its folder), which outlasts the app's shutdown, installs and starts the new version only when it runs as the user named in `/RELAUNCH` (compared case-insensitively): if another administrator's credentials answered a UAC prompt, the app is not started under their account. The current per-user installer needs no prompt and installs into `%LOCALAPPDATA%\Programs\MongoRescue`; if the installer cannot be started, the app stays open with **Try again** and the release page. Starting the installer yourself (without `/S`) shows the wizard.

  **Moving from Program Files.** When a per-machine copy is still registered (its uninstall entry under `HKLM\Software\Microsoft\Windows\CurrentVersion\Uninstall\MongoRescueMongoRescue`) and it is not the copy that runs, the app shows a bar: *An older copy of MongoRescue is installed in Program Files*, with **Remove** and **Dismiss**. **Remove** starts that copy's `uninstall.exe /S`; Windows asks for administrator rights. **Dismiss** hides the bar for good. Both copies use the same data in `%APPDATA%\MongoRescue`, so nothing is migrated and removing the old copy keeps your data. In effect, updating a per-machine copy moves you to a per-user copy: the new version lives in `%LOCALAPPDATA%\Programs\MongoRescue` with its own shortcuts, while the old copy's all-users Start menu and desktop shortcuts keep pointing to the old version in Program Files until you remove it.

  The mandatory update uses the same paths.
- **macOS:** `…_macos_universal.zip` is saved in `~/Downloads` and shown in Finder. Quit MongoRescue and replace `MongoRescue.app` with the one in the archive; its bundled tools come with it.
- **Linux:** `…_linux_amd64.tar.gz` is saved in `$XDG_DOWNLOAD_DIR` (when set to an absolute path) or `~/Downloads`, and the folder is opened. Quit MongoRescue and replace the binary and its `tools/` directory with the ones in the archive.

On Windows the in-app update, or the installer, replaces `<install dir>\tools` with the tools of the new version; on macOS and Linux the app never replaces files itself.

Integrity: the release lookup and the downloads use HTTPS, and only files under `https://github.com/YigitCittan/mongorescue/releases/download/` are accepted (GitHub redirects them to its file storage; at most 5 redirects are followed, each to `https` on `github.com` or a `*.githubusercontent.com` host). The file is streamed to a temporary file (mode `0600`) while its SHA-256 is computed and compared with the release's `MongoRescue-desktop_<version>_checksums.txt`; a missing checksum or a mismatch discards the file and shows the error, with a **Try again** button and the release page link. Nothing is installed from an unverified file.

Offline, rate-limited or when GitHub is unreachable, the check fails quietly: it is logged at `info` level in `desktop.log`, the previous result stays (at startup: no update is offered) and the app works as usual; the next periodic check (see the backoff above), a check on request or the next start tries again. A mandatory update is therefore only enforced once the app has seen the new release. Builds without a release version (a plain `go build`, or a `git describe` version such as `v0.3.2-4-gabc123`) skip the check.

The prompt talks to the app through `/desktop/update` endpoints answered in-process before the dashboard handler (`internal/desktop.Updater`): only same-origin requests from the app's own window are served (`GET /desktop/update` for the status with the download percentage and the legacy copy, `POST /desktop/update/install`, `/desktop/update/release-page`, `/desktop/update/remove-legacy` and `/desktop/update/check`, which checks now and answers with the status once the check is done, `502` when it failed and `409` for a build without a release version), and the `POST` endpoints also require the `X-MongoRescue-Desktop: 1` header. The check and the download are bound to the app's lifetime and stop when it closes.

## Running in the background

On Windows the app keeps running with a MongoRescue icon in the notification area (system tray), so scheduled backups continue while its window is closed. macOS and Linux are unchanged: closing the window quits the app.

- **Closing the window** (X or Alt+F4) hides it. The first time, a notification says *MongoRescue keeps running in the background*; `<data dir>/background-notice-shown` records that it was shown. If the tray icon could not be created, or has stopped working, closing the window quits the app as on the other platforms, so the app is never left running out of reach.
- **Clicking the tray icon**, **Open MongoRescue**, or starting the app again shows the window and brings it to the front.
- The menu shows the current state, refreshed every few seconds: *Idle*, *Running: 1 backup*, or *Quitting after 1 running backup finishes…*.
- **Check for updates** checks GitHub now; it becomes **Update to vX** once a newer version is available ([Updates](#updates)).
- **Start with Windows** (a checkbox) writes, or removes, the value `MongoRescue` = `"<path to MongoRescue.exe>" --hidden` under `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`. The checkbox is ticked only when the value starts this copy of `MongoRescue.exe` (compared case-insensitively). A value that points to another copy, such as an old install folder, shows unticked, and ticking the checkbox rewrites it for this copy. `--hidden` starts the app with its window hidden in the tray; if the tray icon has not come up within 10 seconds, the window is shown.
- **Quit** quits at once when no backup or restore runs. Otherwise no new backup or restore starts, and the app quits once the running ones have finished, however long they take. Scheduled runs are skipped. Runs started from the dashboard, the REST API or MCP are refused with `503 Service Unavailable` and *MongoRescue is shutting down*, so the wait cannot go on forever. The tray shows *MongoRescue is shutting down: new backups and restores are refused* under the status line. Until the app quits, the menu offers **Cancel quit**, which lets runs start again.
- **Force quit** asks for confirmation while runs are active (*1 backup is running and will be cancelled. Quit anyway?*). It then cancels them and quits right away. Cancelled runs are recorded with the status `cancelled`, cancelled by `system`, so they do not count as failures. Their message starts with `cancelled: application force quit`, followed by the engine's detail, as in `cancelled: application force quit (backup cancelled: application force quit)`. The partial archive of a cancelled backup and the partial clone of a cancelled safe-clone restore are removed. The message shows in the history, in the run's log and in `desktop.log`.

The menu is in Turkish when the Windows display language is Turkish and in English otherwise. An in-app update also waits for running backups and restores before it installs (the tray then shows *Updating after …*). The new version it starts always shows its window, even if the old one was started with `--hidden`.

## Data and logs

| | Default location |
| :--- | :--- |
| Data directory (`mongorescue.db`, `secret.key`, lock) | `<user config dir>/MongoRescue/data` |
| Default "Local disk" backup target | `<user config dir>/MongoRescue/backups` |
| Log file (truncated on every start) | `<data dir>/desktop.log` |
| Run logs (one per backup and restore, see [api.md](api.md#run-logs)) | `<data dir>/logs/<id>.log` |

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

Releases build the desktop app natively on Windows, macOS and Linux runners (`desktop` job in `.github/workflows/release.yml`) and attach the archives, with provenance attestations, to the GitHub release. When all of them are attached, the `desktop-checksums` job adds `MongoRescue-desktop_<version>_checksums.txt` with one `<sha256>  <file>` line (`sha256sum` format) per desktop asset. GoReleaser creates the release as a **draft**; the `desktop-checksums` job publishes it only after the checksums are attached (marking it as latest unless the tag has a pre-release suffix), so the updater never sees a release without installable files. If a desktop build fails, the release stays a draft until it is fixed and published by hand. The asset names and this file are a stable contract (the app's updater reads them):

| Asset | Contents |
| :--- | :--- |
| `MongoRescue-desktop_<version>_windows_amd64_installer.exe` | NSIS installer (installs `MongoRescue.exe` and `tools\`) |
| `MongoRescue-desktop_<version>_windows_amd64_portable.zip` | `MongoRescue.exe`, `tools\` |
| `MongoRescue-desktop_<version>_macos_universal.zip` | `MongoRescue.app` (Apple Silicon and Intel), tools in `Contents/Resources/tools/` |
| `MongoRescue-desktop_<version>_linux_amd64.tar.gz` | `MongoRescue` binary, `tools/` |
| `MongoRescue-desktop_<version>_checksums.txt` | SHA-256 of the four files above |

Verify a download with `sha256sum --check --ignore-missing MongoRescue-desktop_<version>_checksums.txt` (macOS: `shasum -a 256 --check --ignore-missing …`).

Before building, the `desktop` job downloads the MongoDB Database Tools archive for the runner's platform from `fastdl.mongodb.org` (version `MONGO_TOOLS_VERSION`, the same as in `ci.yml`), checks it against the SHA-256 pinned in the workflow and fails on a mismatch, and stages `mongodump`, `mongorestore`, `LICENSE.md` and `THIRD-PARTY-NOTICES` in `build/bin/tools` (on macOS the arm64 and x86_64 binaries are merged with `lipo`; Linux uses the `ubuntu2204-x86_64` build). `wails build -nsis` packs that directory into the installer (`project.nsi` installs it to `$INSTDIR\tools` and the uninstaller removes it), the portable zip and the Linux archive include it, and on macOS it is moved into `MongoRescue.app/Contents/Resources/tools/` before the app is zipped. The build runs without `-clean`, which would delete `build/bin`. To bump the tools, change `MONGO_TOOLS_VERSION` and the four `MONGO_TOOLS_SHA256_*` values together. A local `wails build -nsis` without `build/bin/tools` builds an installer without the tools (makensis warns).

## Code signing

The release workflow can sign the Windows desktop build through [SignPath](https://signpath.io) (free for open source through [SignPath Foundation](https://signpath.org); see the code signing policy in the [README](../README.md#code-signing-policy)). Signing is **off** until it is configured: without the secret and the organization variable below, every signing step of the `desktop` job is skipped and the release ships the unsigned `wails build -nsis` output, exactly as before. macOS and Linux builds are not signed.

When it is on, the Windows leg of the `desktop` job:

1. builds `MongoRescue.exe` and the installer with `wails build -nsis` (the same step as without signing);
2. uploads `MongoRescue.exe` as a workflow artifact, submits it to SignPath and waits for the signed file, which replaces the unsigned one in `build/bin`;
3. rebuilds the installer around the signed exe with the same `makensis` call Wails makes (`makensis -DARG_WAILS_AMD64_BINARY=..\..\bin\MongoRescue.exe project.nsi` in `build/windows/installer`, reusing the `wails_tools.nsh` and WebView2 bootstrapper the first step wrote);
4. uploads the installer, has SignPath sign it and replaces it;
5. checks both files with `Get-AuthenticodeSignature` and fails unless both are `Valid`.

The portable zip then contains the signed exe and the installer asset is the signed installer. The bundled `mongodump.exe` and `mongorestore.exe` are not submitted to SignPath: they ship as MongoDB signed them. Provenance attestations and `MongoRescue-desktop_<version>_checksums.txt` are made from the uploaded release assets after signing, so they cover the signed files.

The uninstaller (`uninstall.exe`, written by the installer) stays unsigned: NSIS builds it while compiling the installer and can only sign it then, through `!uninstfinalize` with a signing command available on the build machine, and SignPath signs only files submitted to it. Windows SmartScreen evaluates the downloaded installer, which is signed.

### Maintainer setup

In SignPath (once the SignPath Foundation application is approved):

1. Create the project (slug `mongorescue` by default) and link it to this repository as a trusted GitHub build system, so only artifacts from the release workflow on GitHub-hosted runners are accepted.
2. Create the signing policy (slug `release-signing` by default). If it requires manual approval, approve each release's two requests (the exe, then the installer) within an hour: each step waits up to 3600 seconds.
3. Use an artifact configuration that accepts a ZIP archive with a single PE file at its root and signs it with Authenticode: every request contains exactly one file, `MongoRescue.exe` or `MongoRescue-amd64-installer.exe` (GitHub wraps workflow artifacts in a ZIP archive). Make it the project's default, or name it with the variable below.
4. Create an API token for a CI user that may submit requests with this policy.

In the GitHub repository (**Settings → Secrets and variables → Actions**):

| Name | Kind | Value |
| :--- | :--- | :--- |
| `SIGNPATH_API_TOKEN` | Secret | The SignPath API token (required) |
| `SIGNPATH_ORGANIZATION_ID` | Variable | The SignPath organization ID (required) |
| `SIGNPATH_PROJECT_SLUG` | Variable | Project slug; defaults to `mongorescue` |
| `SIGNPATH_SIGNING_POLICY_SLUG` | Variable | Signing policy slug; defaults to `release-signing` |
| `SIGNPATH_ARTIFACT_CONFIGURATION_SLUG` | Variable | Artifact configuration slug; optional, the project default is used when unset |

Signing turns on with the next tag once the secret and the organization ID exist; delete either to turn it off. The `desktop` job's token gets `actions: read` so SignPath can read the job and download the artifact; the unsigned artifacts are kept for one day.

## Windows installer and WebView2

The app window needs the Microsoft Edge WebView2 runtime. Windows 10 and 11 include it; Windows Server 2016, 2019 and 2022 do not unless something installed it. The installer checks the per-machine registration (`HKLM\SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`, and the key without `WOW6432Node`) and the per-user one (`HKCU\Software\Microsoft\EdgeUpdate\Clients\{…}`), skips the step when either names a version other than `0.0.0.0`, and otherwise runs Microsoft's online bootstrapper, which downloads the runtime (well over 100 MB). The status line reads *Installing Microsoft Edge WebView2 Runtime…* and the bootstrapper shows its own progress window; with `/S` it runs silently. If the runtime is still missing afterwards the installer says so, with the exit code and the [download link](https://go.microsoft.com/fwlink/p/?LinkId=2124703), and finishes the installation; the app offers the download again when it starts without the runtime. On machines without internet access, install the WebView2 runtime first (the offline "Evergreen Standalone Installer").

The installer is per-user by default: `project.nsi` defines `WAILS_INSTALL_SCOPE=user` and `REQUEST_EXECUTION_LEVEL=user` before it includes the generated `wails_tools.nsh`, so it runs without a UAC prompt, installs to `$LOCALAPPDATA\Programs\MongoRescue` and writes its shortcuts and uninstall entry for the current user (HKCU). `makensis -DWAILS_INSTALL_SCOPE=machine` builds the former per-machine installer (Program Files, HKLM, administrator rights). The WebView2 bootstrapper may still ask for administrator rights when it installs the runtime.

After installing (and uninstalling), the installer calls `SHChangeNotify(SHCNE_ASSOCCHANGED)` so that Explorer refreshes its icon cache: shortcuts and taskbar pins left by an older install otherwise keep the cached default Wails "W" icon.

v0.3.1 used the unmodified Wails template: it looked only at the per-machine key (the per-user key only for per-user installers) and ran the bootstrapper with `/silent`, ignoring its result. On a server without WebView2 the installer therefore sat on *Installing: WebView2 Runtime* with no progress for the whole download and installation, and looked hung. This is the likely cause of the long pause reported on Windows Server; it follows from the template and from Windows Server not shipping WebView2, but was not reproduced on a Windows machine. The installer's compression (NSIS default, zlib) is not a factor: extracting the files takes seconds. Antivirus scanning of the unsigned executable can add a short delay.
