// Spec 20: API key expiry. The administrator creates a key that expires in 30
// days and one that expires tomorrow, which raises the "expire soon" banner;
// with a maximum lifetime the dialog no longer offers keys that never expire.
import { type Locator } from "@playwright/test";
import { expect, test } from "./fixtures.js";

type Key = { id: string; name: string; expires_at?: string };

test("creates expiring API keys and enforces a maximum lifetime", async ({ page }) => {
  const keys = async () => page.evaluate(async () => (await (await fetch("/api/v1/api-keys")).json()).data) as Promise<Key[]>;
  const openKeys = async () => {
    await page.getByRole("tab", { name: "Settings" }).click();
    await page.locator(".settings-nav-btn[data-section=apikeys]").click();
    return page.locator("#settings-apikeys");
  };
  const create = async (name: string, choose: (dialog: Locator) => Promise<void>) => {
    const panel = await openKeys();
    await panel.getByRole("button", { name: "Create API key" }).first().click();
    const dialog = page.getByRole("dialog", { name: "Create API key" });
    await expect(dialog).toBeVisible();
    await dialog.getByRole("textbox", { name: "Name" }).fill(name);
    await choose(dialog);
    await dialog.getByRole("button", { name: "Create" }).click();
    await expect(page.getByRole("dialog", { name: "API key created" })).toBeVisible();
    await page.getByRole("dialog", { name: "API key created" }).getByRole("button", { name: "Done" }).click();
    return panel;
  };

  await page.goto("/");
  const panel = await create("e2e-expiring-30", async dialog => {
    await dialog.getByLabel("Expires").selectOption("30");
  });
  const row = panel.getByRole("row").filter({ hasText: "e2e-expiring-30" });
  await expect(row).toBeVisible();
  let key = (await keys()).find(k => k.name === "e2e-expiring-30");
  const days = (Date.parse(key?.expires_at || "") - Date.now()) / 86_400_000;
  expect(days).toBeGreaterThan(29.9);
  expect(days).toBeLessThan(30.1);

  // A key that expires tomorrow is within the 14-day warning: banner and chip.
  const tomorrow = new Date(Date.now() + 86_400_000);
  const day = `${tomorrow.getFullYear()}-${String(tomorrow.getMonth() + 1).padStart(2, "0")}-${String(tomorrow.getDate()).padStart(2, "0")}`;
  await create("e2e-expiring-soon", async dialog => {
    await dialog.getByLabel("Expires").selectOption("date");
    await dialog.getByLabel("Expiry date").fill(day);
  });
  await expect(panel.getByRole("row").filter({ hasText: "e2e-expiring-soon" }).getByText("Expires soon")).toBeVisible();
  await page.reload();
  const banner = page.locator('#runtime-warnings [data-warning="api_keys_expiring"]');
  await expect(banner.getByRole("heading", { name: "API keys expire soon" })).toBeVisible();
  await expect(banner.getByText(/e2e-expiring-soon/)).toBeVisible();

  // A maximum lifetime: no "Never", the longest preset within it preselected.
  await page.getByRole("tab", { name: "Settings" }).click();
  await page.locator(".settings-nav-btn[data-section=security]").click();
  const security = page.locator("#settings-security");
  await security.getByLabel("Maximum API key lifetime (days)").fill("100");
  await security.getByRole("button", { name: "Save" }).click();
  await expect(security.getByText("Saved")).toBeVisible();
  const keysPanel = await openKeys();
  await keysPanel.getByRole("button", { name: "Create API key" }).first().click();
  const dialog = page.getByRole("dialog", { name: "Create API key" });
  const expiry = dialog.getByLabel("Expires");
  await expect(expiry).toHaveValue("90");
  await expect(expiry.locator('option[value="never"]')).toBeDisabled();
  await expect(expiry.locator('option[value="180"]')).toBeDisabled();
  await dialog.getByRole("button", { name: "Cancel" }).click();

  // Clean up: no maximum, and the keys revoked, so later runs start clean.
  await page.locator(".settings-nav-btn[data-section=security]").click();
  await security.getByLabel("Maximum API key lifetime (days)").fill("0");
  await security.getByRole("button", { name: "Save" }).click();
  await expect(security.getByText("Saved")).toBeVisible();
  for (const name of ["e2e-expiring-30", "e2e-expiring-soon"]) {
    key = (await keys()).find(k => k.name === name);
    const status = await page.evaluate(async id => {
      const csrf = (await (await fetch("/api/v1/auth/me")).json()).data.csrf_token;
      return (await fetch(`/api/v1/api-keys/${id}`, { method: "DELETE", headers: { "X-CSRF-Token": csrf } })).status;
    }, key?.id);
    expect(status).toBe(200);
  }
});
