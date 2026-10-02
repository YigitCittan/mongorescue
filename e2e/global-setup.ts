// Starts the real MongoRescue binary for the browser suite: a fresh temporary data
// directory (so the server boots in setup mode), the embedded dashboard on a random
// loopback port, and the one-time setup code read from its log. The returned
// function is the global teardown: it stops the server and removes the directory.
import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import { createWriteStream, existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import path from "node:path";
import type { FullConfig } from "@playwright/test";
import { E2E_ROOT, REPO_ROOT, RUN_DIR, STATE_FILE, UPDATE_SCRIPT_FILE, type ServerState } from "./paths.js";

const START_TIMEOUT_MS = 60_000;

// freePort asks the kernel for an unused loopback port.
function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = createServer();
    srv.unref();
    srv.once("error", reject);
    srv.listen(0, "127.0.0.1", () => {
      const addr = srv.address();
      srv.close(() => (addr && typeof addr === "object" ? resolve(addr.port) : reject(new Error("no port"))));
    });
  });
}

// serverEnv is the runner's environment without MongoRescue's own variables: the
// server must start from an empty data directory, not import legacy settings.
function serverEnv(): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = {};
  for (const [key, value] of Object.entries(process.env)) {
    if (!key.startsWith("MONGORESCUE_") || key === "MONGORESCUE_TOOLS_DIR") env[key] = value;
  }
  return env;
}

async function waitForHealth(baseURL: string, child: ChildProcess, deadline: number): Promise<void> {
  while (Date.now() < deadline) {
    if (child.exitCode !== null) throw new Error(`mongorescue exited with code ${child.exitCode}; see ${RUN_DIR}/server.log`);
    try {
      const res = await fetch(`${baseURL}/api/v1/health`);
      if (res.ok) return;
    } catch {
      // not listening yet
    }
    await new Promise(r => setTimeout(r, 250));
  }
  throw new Error(`mongorescue did not become healthy within ${START_TIMEOUT_MS} ms; see ${RUN_DIR}/server.log`);
}

export default async function globalSetup(_config: FullConfig): Promise<() => Promise<void>> {
  for (const name of ["MONGORESCUE_E2E_MONGO_URI", "MONGORESCUE_E2E_S3_ENDPOINT"]) {
    if (!process.env[name]) {
      throw new Error(`${name} is not set: run the suite with "make test-e2e" (scripts/test-e2e-docker.sh)`);
    }
  }
  const binary =
    process.env.MONGORESCUE_E2E_BINARY ||
    path.join(REPO_ROOT, "bin", process.platform === "win32" ? "mongorescue.exe" : "mongorescue");
  if (!existsSync(binary)) throw new Error(`${binary} not found: run "make build" first`);

  rmSync(RUN_DIR, { recursive: true, force: true });
  mkdirSync(RUN_DIR, { recursive: true });

  // The desktop app's update script, injected by the update banner spec.
  const script = execFileSync("go", ["run", "./e2e/updatescript/main.go"], { cwd: REPO_ROOT, encoding: "utf8" });
  writeFileSync(UPDATE_SCRIPT_FILE, script);

  // <tmp>/data is the data directory; the default "Local disk" target is <tmp>/backups.
  const workDir = mkdtempSync(path.join(tmpdir(), "mongorescue-e2e-"));
  const dataDir = path.join(workDir, "data");
  const port = await freePort();
  const baseURL = `http://127.0.0.1:${port}`;

  const log = createWriteStream(path.join(RUN_DIR, "server.log"));
  const child = spawn(
    binary,
    ["--dashboard", "--host", "127.0.0.1", "--port", String(port), "--data-dir", dataDir],
    { cwd: E2E_ROOT, env: serverEnv(), stdio: ["ignore", "pipe", "pipe"] },
  );
  let output = "";
  const setupCode = new Promise<string>((resolve, reject) => {
    const onData = (chunk: Buffer) => {
      log.write(chunk);
      output += chunk.toString("utf8");
      const m = /setup_code=([A-Z0-9-]+)/.exec(output);
      if (m) resolve(m[1]);
    };
    child.stdout?.on("data", onData);
    child.stderr?.on("data", onData);
    child.once("exit", code => reject(new Error(`mongorescue exited with code ${code}; see ${RUN_DIR}/server.log`)));
  });

  // Awaited below; marked handled so an early exit is reported once, by waitForHealth.
  setupCode.catch(() => undefined);

  const deadline = Date.now() + START_TIMEOUT_MS;
  try {
    await waitForHealth(baseURL, child, deadline);
    const timeout = new Promise<never>((_, reject) =>
      setTimeout(() => reject(new Error("no setup code in the server log")), Math.max(0, deadline - Date.now())).unref(),
    );
    const state: ServerState = { baseURL, setupCode: await Promise.race([setupCode, timeout]), dataDir };
    writeFileSync(STATE_FILE, JSON.stringify(state, null, 2));
  } catch (err) {
    child.kill("SIGKILL");
    log.end();
    rmSync(workDir, { recursive: true, force: true });
    throw err;
  }

  return async () => {
    if (child.exitCode === null) {
      const exited = new Promise(r => child.once("exit", r));
      child.kill("SIGTERM");
      await Promise.race([exited, new Promise(r => setTimeout(r, 10_000))]);
      if (child.exitCode === null) child.kill("SIGKILL");
    }
    log.end();
    rmSync(workDir, { recursive: true, force: true });
  };
}
