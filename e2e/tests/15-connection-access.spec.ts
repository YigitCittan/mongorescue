// Spec 15: per-connection access. The administrator adds a second connection and an
// operator limited to the e2e connection (the Connections checklist of the Add user
// dialog). Signed in, the operator sees only that connection, the user menu names
// it, and the server answers 404 for the other one.
import { randomBytes } from "node:crypto";
import { CONNECTION_NAME, expect, test } from "./fixtures.js";

const OPERATOR = "e2e-team-a";
const OTHER = "e2e-team-b";

test("an operator limited to one connection sees only that connection", async ({ page, browser, server }) => {
  const password = `e2e-${randomBytes(12).toString("hex")}`;
  await page.goto("/");

  // A second connection, which the operator must never see. Creating it does not
  // contact the server.
  const other = await page.evaluate(async (name) => {
    const me = await (await fetch("/api/v1/auth/me", { credentials: "same-origin" })).json();
    const res = await fetch("/api/v1/connections", {
      method: "POST", credentials: "same-origin",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": me.data.csrf_token },
      body: JSON.stringify({ name, uri: "mongodb://team-b.invalid:27017" }),
    });
    const json = await res.json();
    return { status: res.status, id: json.data && json.data.id };
  }, OTHER);
  expect(other.status).toBe(201);

  try {
    // The administrator adds the operator, limited to the e2e connection.
    await page.getByRole("tab", { name: "Settings" }).click();
    await page.getByRole("button", { name: "Users", exact: true }).click();
    await page.getByRole("button", { name: "Add user" }).first().click();
    const dialog = page.getByRole("dialog", { name: "Add user" });
    await dialog.getByLabel("Username").fill(OPERATOR);
    await dialog.getByLabel("Password", { exact: true }).fill(password);
    await dialog.getByLabel("Confirm password").fill(password);
    await dialog.getByRole("combobox", { name: "Role" }).selectOption("operator");
    const checklist = dialog.getByRole("group", { name: "Connections" });
    await expect(checklist.getByRole("checkbox", { name: OTHER })).toBeVisible();
    await checklist.getByRole("checkbox", { name: CONNECTION_NAME }).check();
    await dialog.getByRole("button", { name: "Add user" }).click();
    await expect(dialog).toBeHidden();
    const row = page.getByRole("row").filter({ hasText: OPERATOR });
    await expect(row.getByRole("button", { name: `Connections of ${OPERATOR}: Connections: 1` })).toBeVisible();

    // The operator signs in in a fresh browser context.
    const context = await browser.newContext({ baseURL: server.baseURL, storageState: { cookies: [], origins: [] } });
    try {
      const operator = await context.newPage();
      await operator.goto("/");
      const login = operator.getByRole("region", { name: "Sign in" });
      await login.getByLabel("Username").fill(OPERATOR);
      await login.getByLabel("Password").fill(password);
      await login.getByRole("button", { name: "Sign in" }).click();
      await expect(operator.getByRole("button", { name: OPERATOR })).toBeVisible();

      await operator.getByRole("tab", { name: /^Connections/ }).click();
      const panel = operator.getByRole("tabpanel", { name: "Connections" });
      await expect(panel.getByRole("row").filter({ hasText: CONNECTION_NAME })).toBeVisible();
      await expect(panel.getByText(OTHER)).toHaveCount(0);

      await operator.getByRole("button", { name: OPERATOR }).click();
      await expect(operator.getByText(`Your connections: ${CONNECTION_NAME}`)).toBeVisible();

      // The server answers 404 for the other connection, as for one that does not exist.
      const statuses = await operator.evaluate(async (id) => {
        const get = await fetch(`/api/v1/connections/${id}`, { credentials: "same-origin" });
        const list = await (await fetch("/api/v1/connections", { credentials: "same-origin" })).json();
        return { get: get.status, names: list.data.map((c: { name: string }) => c.name) };
      }, other.id);
      expect(statuses).toEqual({ get: 404, names: [CONNECTION_NAME] });
    } finally {
      await context.close();
    }
  } finally {
    await page.evaluate(async (id) => {
      const me = await (await fetch("/api/v1/auth/me", { credentials: "same-origin" })).json();
      await fetch(`/api/v1/connections/${id}`, {
        method: "DELETE", credentials: "same-origin", headers: { "X-CSRF-Token": me.data.csrf_token },
      });
    }, other.id);
  }
});
