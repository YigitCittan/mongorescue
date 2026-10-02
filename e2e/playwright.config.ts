// Playwright configuration of the MongoRescue browser suite. The specs form one
// journey against one server (global-setup.ts) and run in order in one worker:
// auth.setup.ts creates the administrator, the numbered specs build on each other
// (connection, storage target, backup, restore, bulk delete, update banner,
// recovery readiness, a viewer).
import { defineConfig, devices } from "@playwright/test";
import { ADMIN_STATE_FILE } from "./paths.js";

const CI = !!process.env.CI;

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
  ],
});
