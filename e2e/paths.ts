// Paths and the server state shared by global-setup.ts, the config and the specs.
import path from "node:path";
import { fileURLToPath } from "node:url";

/** E2E_ROOT is the e2e/ directory. */
export const E2E_ROOT = path.dirname(fileURLToPath(import.meta.url));
/** REPO_ROOT is the repository root (bin/mongorescue, go.mod). */
export const REPO_ROOT = path.resolve(E2E_ROOT, "..");
/** RUN_DIR holds the state of one run: server.log, state.json, the session. */
export const RUN_DIR = path.join(E2E_ROOT, ".run");
/** STATE_FILE is the ServerState written by global-setup.ts. */
export const STATE_FILE = path.join(RUN_DIR, "state.json");
/** ADMIN_STATE_FILE is the signed-in admin's storage state (auth.setup.ts). */
export const ADMIN_STATE_FILE = path.join(RUN_DIR, "admin.json");
/** UPDATE_SCRIPT_FILE is the desktop app's update script (e2e/updatescript). */
export const UPDATE_SCRIPT_FILE = path.join(RUN_DIR, "update-script.js");

/** ServerState describes the server started by global-setup.ts. */
export interface ServerState {
  baseURL: string;
  setupCode: string;
  dataDir: string;
}
