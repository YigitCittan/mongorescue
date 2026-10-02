// Spec 2: add a MongoDB connection; its test reports the server version before it is
// saved, and the saved connection is listed without its password.
import { CONNECTION_NAME, expect, test } from "./fixtures.js";

test("adds a connection after a successful connection test", async ({ page, services }) => {
  await page.goto("/");
  await page.getByRole("button", { name: "Add connection" }).first().click();

  const dialog = page.getByRole("dialog", { name: "New connection" });
  await expect(dialog).toBeVisible();
  await dialog.getByRole("textbox", { name: "Name" }).fill(CONNECTION_NAME);
  await dialog.getByRole("textbox", { name: "Connection string" }).fill(services.mongoURI);
  await dialog.getByRole("button", { name: "Test connection" }).click();
  await expect(dialog.getByText(/Connected · MongoDB \d+\.\d+/)).toBeVisible({ timeout: 30_000 });
  await dialog.getByRole("button", { name: "Save" }).click();
  await expect(dialog).toBeHidden();

  await page.getByRole("tab", { name: /^Connections/ }).click();
  const panel = page.getByRole("tabpanel", { name: "Connections" });
  const row = panel.getByRole("row").filter({ hasText: CONNECTION_NAME });
  await expect(row).toBeVisible();
  const password = new URL(services.mongoURI).password;
  await expect(panel).not.toContainText(decodeURIComponent(password));
});
