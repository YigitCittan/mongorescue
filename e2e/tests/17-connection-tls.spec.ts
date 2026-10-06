// Spec 17: a connection with TLS material. The CA, client certificate and key are
// loaded from files, the key is masked after saving and kept by an edit, and turning
// off certificate verification asks for confirmation and shows warnings.
import path from "node:path";
import { E2E_ROOT } from "../paths.js";
import { expect, test } from "./fixtures.js";

const NAME = "e2e-tls";
// Never resolved: the connection test fails and the connection is saved anyway.
const URI = "mongodb://tls-e2e.invalid:27017/?tls=true&authMechanism=MONGODB-X509&serverSelectionTimeoutMS=2000";
const fixture = (name: string) => path.join(E2E_ROOT, "fixtures", name);

type Conn = Record<string, unknown>;

test("stores TLS material masked and warns about loosened checks", async ({ page }) => {
  const stored = async () => page.evaluate(async (name) => {
    const list = (await (await fetch("/api/v1/connections")).json()).data as Array<Record<string, unknown>>;
    return list.find(c => c.name === name);
  }, NAME) as Promise<Conn | undefined>;

  await page.goto("/");
  await page.getByRole("tab", { name: /^Connections/ }).click();
  const panel = page.getByRole("tabpanel", { name: "Connections" });
  await panel.getByRole("button", { name: "Add connection" }).click();
  const dialog = page.getByRole("dialog", { name: "New connection" });
  await expect(dialog).toBeVisible();
  await dialog.getByRole("textbox", { name: "Name" }).fill(NAME);
  await dialog.getByRole("textbox", { name: "Connection string" }).fill(URI);

  await dialog.getByText("TLS / client certificate").click();
  const files = dialog.locator("input[data-tls-file]");
  await files.nth(0).setInputFiles(fixture("ca.pem"));
  await files.nth(1).setInputFiles(fixture("client.crt"));
  await files.nth(2).setInputFiles(fixture("client.key"));
  await expect(dialog.getByRole("textbox", { name: "CA certificate (PEM)" })).toHaveValue(/BEGIN CERTIFICATE/);
  await expect(dialog.getByRole("textbox", { name: "Client private key (PEM)" })).toHaveValue(/BEGIN PRIVATE KEY/);

  await dialog.getByRole("checkbox", { name: "Allow invalid hostnames" }).check();
  await expect(dialog.getByText(/Hostname checking is off/)).toBeVisible();

  // The test fails (unresolvable host), then Save anyway stores it.
  await dialog.getByRole("button", { name: "Save" }).click();
  await dialog.getByRole("button", { name: "Save anyway" }).click({ timeout: 30_000 });
  await expect(dialog).toBeHidden({ timeout: 30_000 });

  let conn = await stored();
  expect(conn?.tls_ca_pem).toContain("BEGIN CERTIFICATE");
  expect(conn?.tls_client_cert_pem).toContain("BEGIN CERTIFICATE");
  expect(conn?.tls_client_key_pem).toBe("******");
  expect(conn?.tls_allow_invalid_hostnames).toBe(true);

  const row = panel.getByRole("row").filter({ hasText: NAME });
  await expect(row.getByText("Hostname not checked")).toBeVisible();
  await expect(row.getByText("x509 certificate")).toBeVisible();

  // Editing shows the key as stored; turning off verification asks first.
  await row.getByRole("button", { name: "Edit" }).click();
  const edit = page.getByRole("dialog", { name: "Edit connection" });
  await expect(edit).toBeVisible();
  const key = edit.getByRole("textbox", { name: "Client private key (PEM)" });
  await expect(key).toHaveValue("");
  await expect(key).toHaveAttribute("placeholder", "Stored. Leave empty to keep it.");

  const insecure = edit.getByRole("checkbox", { name: "Do not verify the server certificate" });
  page.once("dialog", d => d.dismiss());
  await insecure.click();
  await expect(insecure).not.toBeChecked();
  page.once("dialog", d => d.accept());
  await insecure.click();
  await expect(insecure).toBeChecked();
  await expect(edit.getByText(/Certificate verification is off/)).toBeVisible();

  await edit.getByRole("button", { name: "Save" }).click();
  await edit.getByRole("button", { name: "Save anyway" }).click({ timeout: 30_000 });
  await expect(edit).toBeHidden({ timeout: 30_000 });

  conn = await stored();
  expect(conn?.tls_insecure).toBe(true);
  expect(conn?.tls_client_key_pem).toBe("******");
  await expect(row.getByText("TLS not verified")).toBeVisible();

  // Clean up so later specs see only their own connections.
  const id = conn?.id as string;
  const status = await page.evaluate(async (connId) => {
    const me = await (await fetch("/api/v1/auth/me", { credentials: "same-origin" })).json();
    return (await fetch(`/api/v1/connections/${connId}`, {
      method: "DELETE", credentials: "same-origin", headers: { "X-CSRF-Token": me.data.csrf_token },
    })).status;
  }, id);
  expect([200, 204]).toContain(status);
});
