// Package updatee2e holds the desktop update end-to-end test, behind the
// desktop_e2e build tag. scripts/test-desktop-update-e2e.sh builds the desktop app
// in two versions and packages them as the release does; the test installs the old
// one as users do, serves the new one from a fake GitHub release on a loopback port
// (desktop.E2EUpdateBaseURLEnv), runs the installed app's headless update and
// checks the outcome: on Windows the app swaps itself in place and restarts on the
// new version; on macOS and Linux it downloads and verifies the archive and shows
// it, leaving the installed app untouched. A tampered archive must fail the update
// and leave the old version intact on every platform. See docs/desktop.md.
package updatee2e
