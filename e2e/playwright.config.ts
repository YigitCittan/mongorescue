// Playwright configuration of the MongoRescue browser suite. The specs form one
// journey against one server (global-setup.ts) and run in order in one worker:
// auth.setup.ts creates the administrator, the numbered specs build on each other
// (connection, storage target, backup, restore, bulk delete, undoing a delete, update banner,
// recovery readiness, a viewer, single sign-on, a backup of several databases,
// monitoring, collection filters per database, notification channels, users and roles,
// the audit log and HA views, job create and edit, and the axe accessibility scan).
import { defineConfig, devices } from "@playwright/test";
import { ADMIN_STATE_FILE } from "./paths.js";

const CI = !!process.env.CI;
// The Nightly workflow adds Firefox and WebKit (run one with --project=firefox); the
// pull request run covers Chromium only.
const CROSS_BROWSER = process.env.E2E_CROSS_BROWSER === "1";

export default defineConfig({
  testDir: "./tests",
  globalSetup: "./global-setup.ts",
  fullyParallel: false,
  workers: 1,
  // The journey is stateful: a retry would repeat steps against changed data.
  retries: 0,
  forbidOnly: CI,
  timeout: 60_000,
  expect: { timeout: 10_000 },
  outputDir: "./test-results",
  reporter: CI
    ? [["github"], ["list"], ["html", { open: "never", outputFolder: "playwright-report" }]]
    : [["list"], ["html", { open: "never", outputFolder: "playwright-report" }]],
  use: {
    // baseURL comes from the fixture in tests/fixtures.ts (the port is random).
    locale: "en-US",
    timezoneId: "UTC",
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    video: "off",
  },
  projects: [
    {
      name: "setup",
      testMatch: /auth\.setup\.ts/,
      use: { ...devices["Desktop Chrome"] },
    },
    {
      name: "chromium",
      testMatch: /\.spec\.ts/,
      dependencies: ["setup"],
      use: { ...devices["Desktop Chrome"], storageState: ADMIN_STATE_FILE },
    },
    ...(CROSS_BROWSER
      ? [
          {
            name: "firefox",
            testMatch: /\.spec\.ts/,
            dependencies: ["setup"],
            use: { ...devices["Desktop Firefox"], storageState: ADMIN_STATE_FILE },
          },
          {
            name: "webkit",
            testMatch: /\.spec\.ts/,
            dependencies: ["setup"],
            use: { ...devices["Desktop Safari"], storageState: ADMIN_STATE_FILE },
          },
        ]
      : []),
  ],
});
