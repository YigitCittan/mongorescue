// Spec 24: the audit log view (Settings > Audit log) lists the administrator's
// actions, filters them and verifies the hash chain; and the high-availability view:
// without HA configured the dashboard stays on the active instance, and the standby
// page (served instead of the dashboard by a standby) renders the cluster state.
import { ADMIN_USERNAME, expect, test } from "./fixtures.js";

test("lists, filters and verifies the audit log", async ({ page }) => {
  await page.goto("/");
  await page.getByRole("tab", { name: "Settings" }).click();
  await page.getByRole("button", { name: "Audit log", exact: true }).click();
  const panel = page.getByRole("region", { name: "Audit log" });
  const rows = panel.locator("#auditlog-tbody").getByRole("row");
  await expect(panel.getByRole("heading", { name: "Audit log", exact: true })).toBeVisible();

  // The earlier specs left entries by the administrator, newest first.
  await expect(rows.first()).toContainText(ADMIN_USERNAME);
  const all = await rows.count();
  expect(all).toBeGreaterThan(3);

  // Filter by actor: only matching rows, then nothing for an unknown actor.
  const actor = panel.getByRole("searchbox", { name: "Actor", exact: true });
  await actor.fill(ADMIN_USERNAME);
  await expect(rows.first()).toContainText(ADMIN_USERNAME);
  await actor.fill("nobody-has-this-name");
  await expect(panel.getByText("No entries match.", { exact: true })).toBeVisible();
  await actor.fill("");
  await expect(rows.first()).toContainText(ADMIN_USERNAME);

  // Filter by actor kind: no entry was made by an API key or by the system in this run's start.
  await panel.getByRole("combobox", { name: "Actor kind" }).selectOption("api_key");
  await expect(panel.getByText("No entries match.", { exact: true })).toBeVisible();
  await panel.getByRole("combobox", { name: "Actor kind" }).selectOption("user");
  await expect(rows.first()).toContainText(ADMIN_USERNAME);
  await expect(panel.getByText("No entries match.", { exact: true })).toBeHidden();

  // The chain of what the run recorded is intact.
  await panel.getByRole("button", { name: "Verify chain" }).click();
  await expect(page.getByRole("region", { name: "Messages" }).getByText("The audit log chain is intact.", { exact: true })).toBeVisible();
  await expect(panel.getByRole("status").filter({ hasText: "Chain intact" })).toContainText(/\d+ entries verified/);
});

test("without HA configured the instance is active and the dashboard stays", async ({ page, request }) => {
  const health = await request.get("/api/v1/health");
  expect(health.ok()).toBe(true);
  const body = (await health.json()) as { success: boolean; data: { role?: string; ha?: unknown } };
  expect(body.success).toBe(true);
  expect(body.data.ha).toBeFalsy();
  expect(body.data.role ?? "active").toBe("active");

  await page.goto("/");
  await expect(page.getByRole("tablist", { name: "Sections" })).toBeVisible();
  await expect(page.getByRole("img", { name: /^Connected/ })).toBeVisible();
  expect(new URL(page.url()).pathname).toBe("/");
});

test("the standby page shows the cluster and the active instance", async ({ page }) => {
  const health = (ha: object) => (route: { fulfill: (o: object) => Promise<void> }) =>
    route.fulfill({ status: 503, json: { success: false, data: { role: "standby", ha } } });

  await page.route("**/api/v1/health", health({ cluster: "prod-cluster", holder: "backup-node-b" }));
  await page.goto("/standby.html");
  await expect(page.getByRole("heading", { name: "Standby instance", exact: true })).toBeVisible();
  await expect(page.locator("#ha-standby-cluster")).toHaveText("prod-cluster");
  await expect(page.locator("#ha-standby-holder")).toHaveText("backup-node-b");
  // It offers no sign-in and shows no data.
  await expect(page.getByRole("tablist")).toHaveCount(0);
  await expect(page.getByLabel("Password")).toHaveCount(0);

  // A standby that does not know its cluster yet shows "unknown".
  await page.unroute("**/api/v1/health");
  await page.route("**/api/v1/health", health({}));
  await page.goto("/standby.html");
  await expect(page.locator("#ha-standby-cluster")).toHaveText("unknown");
  await expect(page.locator("#ha-standby-holder")).toHaveText("unknown");
});
