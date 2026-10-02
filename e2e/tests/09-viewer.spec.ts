// Spec 9: dashboard roles. The administrator adds a user, who is a viewer by
// default. Signed in, the viewer sees every list but no usable control that changes
// something, no users or audit log, read-only settings, and the server refuses a
// forged request anyway.
import { randomBytes } from "node:crypto";
import { expect, test } from "./fixtures.js";

const VIEWER = "e2e-viewer";

test("a viewer sees no destructive controls and is refused by the server", async ({ page, browser, server }) => {
  const password = `e2e-${randomBytes(12).toString("hex")}`;

  // The administrator adds the user without touching the role: viewer.
  await page.goto("/");
  await page.getByRole("tab", { name: "Settings" }).click();
  await page.getByRole("button", { name: "Users", exact: true }).click();
  await page.getByRole("button", { name: "Add user" }).first().click();
  const dialog = page.getByRole("dialog", { name: "Add user" });
  await expect(dialog.getByRole("combobox", { name: "Role" })).toHaveValue("viewer");
  await dialog.getByLabel("Username").fill(VIEWER);
  await dialog.getByLabel("Password", { exact: true }).fill(password);
  await dialog.getByLabel("Confirm password").fill(password);
  await dialog.getByRole("button", { name: "Add user" }).click();
  await expect(dialog).toBeHidden();
  const row = page.getByRole("row").filter({ hasText: VIEWER });
  await expect(row.getByRole("combobox", { name: `Role of ${VIEWER}` })).toHaveValue("viewer");

  // The viewer signs in in a fresh browser context. Test options apply to
  // browser.newContext too, so the admin's storage state is replaced by an empty one.
  const context = await browser.newContext({ baseURL: server.baseURL, storageState: { cookies: [], origins: [] } });
  try {
    const viewer = await context.newPage();
    await viewer.goto("/");
    const login = viewer.getByRole("region", { name: "Sign in" });
    await login.getByLabel("Username").fill(VIEWER);
    await login.getByLabel("Password").fill(password);
    await login.getByRole("button", { name: "Sign in" }).click();
    await expect(viewer.getByRole("button", { name: VIEWER })).toBeVisible();

    // Starting a backup is disabled, with the reason as its tooltip.
    const backUp = viewer.getByRole("tabpanel", { name: "Overview" }).getByRole("button", { name: "Back up now" });
    await expect(backUp).toBeDisabled();
    await expect(backUp).toHaveAttribute("title", "Your role (Viewer) can only view");

    // No list offers a usable control that deletes, runs, restores or reconfigures.
    for (const tab of [/^Backups/, /^Jobs/, /^Restores/, /^Connections/, /^Notifications/]) {
      await viewer.getByRole("tab", { name: tab }).click();
      await expect(viewer.locator(
        '.tab-pane.active [data-action^="delete-"]:not([disabled]), .tab-pane.active [data-action^="new-"]:not([disabled]), ' +
        '.tab-pane.active [data-action="trigger-job"]:not([disabled]), .tab-pane.active [data-action="restore-backup"]:not([disabled]), ' +
        '.tab-pane.active [data-action="backup-now"]:not([disabled])',
      )).toHaveCount(0);
    }

    // Users and the audit log are not offered; settings are read-only.
    await viewer.getByRole("tab", { name: "Settings" }).click();
    await expect(viewer.getByRole("button", { name: "Users", exact: true })).toBeHidden();
    await expect(viewer.getByRole("button", { name: "Audit log", exact: true })).toBeHidden();
    const general = viewer.locator("#form-general");
    await expect(general.getByText("Only administrators can change these settings.")).toBeVisible();
    await expect(general.getByRole("button", { name: "Save" })).toBeDisabled();

    // The server is authoritative: a forged request with the session's CSRF token is
    // refused with 403.
    const statuses = await viewer.evaluate(async () => {
      const me = await (await fetch("/api/v1/auth/me", { credentials: "same-origin" })).json();
      const headers = { "Content-Type": "application/json", "X-CSRF-Token": me.data.csrf_token };
      const backup = await fetch("/api/v1/backups", {
        method: "POST", credentials: "same-origin", headers, body: JSON.stringify({ connection_id: "conn_x", database: "shop" }),
      });
      const users = await fetch("/api/v1/users", {
        method: "POST", credentials: "same-origin", headers, body: JSON.stringify({ username: "mallory", password: "a long enough password", role: "admin" }),
      });
      return { role: me.data.role, scope: me.data.scope, backup: backup.status, users: users.status };
    });
    expect(statuses).toEqual({ role: "viewer", scope: "read", backup: 403, users: 403 });
  } finally {
    await context.close();
  }
});
