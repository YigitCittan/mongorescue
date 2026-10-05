package desktop

// E2EUpdateBaseURLEnv names the environment variable that points the updater at a
// fake GitHub API and release download server (an http URL on a loopback address,
// such as http://127.0.0.1:54321), for the desktop update end-to-end test
// (internal/desktop/updatee2e). Only builds with the desktop_e2e build tag read it;
// release builds, which never have the tag, ignore it and always use GitHub.
const E2EUpdateBaseURLEnv = "MONGORESCUE_E2E_UPDATE_BASE_URL"
