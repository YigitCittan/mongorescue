// Spec 21: the "Credentials from Vault" sections of the connection and storage
// target forms. The suite's server runs without MONGORESCUE_VAULT_ADDR, so both
// sections say Vault is not configured and offer nothing to turn on; the API
// refuses a connection that names a Vault reference.
import { expect, test } from "./fixtures.js";

test("offers credentials from Vault only when Vault is configured", async ({ page }) => {
  await page.goto("/");
  await page.getByRole("tab", { name: /^Connections/ }).click();
  const panel = page.getByRole("tabpanel", { name: "Connections" });
  await panel.getByRole("button", { name: "Add connection" }).click();
  const dialog = page.getByRole("dialog", { name: "New connection" });
  await expect(dialog).toBeVisible();
  await dialog.getByText("Credentials from Vault").click();
  await expect(dialog.getByText(/Vault is not configured on this server/)).toBeVisible();
  await expect(dialog.getByRole("checkbox", { name: "Read the user name and password from Vault" })).toBeDisabled();
  await dialog.getByRole("button", { name: "Cancel" }).click();

  await page.getByRole("tab", { name: "Settings" }).click();
  const settings = page.getByRole("tabpanel", { name: "Settings" });
  await settings.getByRole("navigation", { name: "Settings" }).getByRole("button", { name: "Storage" }).click();
  await settings.getByRole("region", { name: "Storage targets" }).getByRole("button", { name: "Add target" }).click();
  const target = page.getByRole("dialog", { name: "New storage target" });
  await target.getByRole("combobox", { name: "Provider" }).selectOption({ label: "MinIO" });
  await target.getByText("Credentials from Vault").click();
  await expect(target.getByRole("checkbox", { name: "Read the access keys from Vault" })).toBeDisabled();
  await target.getByRole("button", { name: "Cancel" }).click();

  const res = await page.evaluate(async () => {
    const status = await (await fetch("/api/v1/vault", { credentials: "same-origin" })).json();
    const me = await (await fetch("/api/v1/auth/me", { credentials: "same-origin" })).json();
    const created = await fetch("/api/v1/connections", {
      method: "POST", credentials: "same-origin",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": me.data.csrf_token },
      body: JSON.stringify({ name: "e2e-vault", uri: "mongodb://vault-e2e.invalid:27017", credential_source: "vault",
        vault: { engine: "kv-v2", path: "mongo" } }),
    });
    return { configured: status.data?.configured, code: created.status, body: await created.text() };
  });
  expect(res.configured).toBe(false);
  expect(res.code).toBe(400);
  expect(res.body).toContain("MONGORESCUE_VAULT_ADDR");
});
