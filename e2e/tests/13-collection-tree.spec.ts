// Spec 13: back up several databases from Back up now with a collection filter per
// database. Selected: expand a database, uncheck one of its collections and back up;
// the archive holds only the other collections. All: open a database with the
// keyboard and exclude one of its collections.
import type { Page } from "@playwright/test";
import { expect, test } from "./fixtures.js";

async function openBackupNow(page: Page) {
  await page.goto("/");
  await page.getByRole("tab", { name: /^Backups/ }).click();
  const backups = page.getByRole("tabpanel", { name: "Backups" });
  await backups.getByRole("button", { name: "Back up now" }).first().click();
  const dialog = page.getByRole("dialog", { name: "Back up now" });
  await expect(dialog).toBeVisible();
  return { backups, dialog };
}

test("backs up a database without one of its collections", async ({ page, services }) => {
  const shop = services.database;
  const crm = services.database2;
  const { backups, dialog } = await openBackupNow(page);
  await dialog.getByRole("button", { name: "Selected", exact: true }).click();
  // The note that collection filters need a single database is gone.
  await expect(dialog.getByText("Collection filters only apply to a single database", { exact: false })).toHaveCount(0);

  const tree = dialog.getByRole("tree", { name: "Databases" });
  const shopBox = tree.getByRole("checkbox", { name: new RegExp(`^${shop}\\b`) });
  await shopBox.check();
  await tree.getByRole("checkbox", { name: new RegExp(`^${crm}\\b`) }).check();

  const caret = tree.getByRole("button", { name: `Collections of ${shop}`, exact: true });
  await expect(caret).toHaveAttribute("aria-expanded", "false");
  await caret.click();
  await expect(caret).toHaveAttribute("aria-expanded", "true");
  const item = tree.locator(`[role="treeitem"][aria-level="1"]`).filter({ has: page.getByRole("checkbox", { name: new RegExp(`^${shop}\\b`) }) });
  await expect(item).toHaveAttribute("aria-expanded", "true");
  const customers = item.getByRole("checkbox", { name: "customers", exact: true });
  const orders = item.getByRole("checkbox", { name: "orders", exact: true });
  // Selected: the collections of a checked database start checked (backed up).
  await expect(customers).toBeChecked();
  await expect(orders).toBeChecked();
  await customers.uncheck();
  // The database box shows that only some collections are backed up.
  await expect(shopBox).toHaveJSProperty("indeterminate", true);
  await expect(item.locator(".db-tree-summary")).toHaveText("orders");

  const request = page.waitForRequest(req => req.method() === "POST" && new URL(req.url()).pathname === "/api/v1/backups");
  await dialog.getByRole("button", { name: "Start backup" }).click();
  await expect(dialog).toBeHidden();
  const body = (await request).postDataJSON();
  expect(body.databases).toEqual([crm, { name: shop, collections: ["orders"] }].sort((a, b) =>
    (typeof a === "string" ? a : a.name).localeCompare(typeof b === "string" ? b : b.name)));

  await expect(page.getByRole("region", { name: "Messages" }).getByText("Backup of 2 databases started.")).toBeVisible();
  await page.getByRole("button", { name: "View run" }).last().click();
  await expect(backups.getByText(/^Run run_/)).toBeVisible();
  const row = backups.getByRole("row").filter({ hasText: shop }).filter({ hasNotText: `${shop}_rescue_` });
  await expect(row).toContainText("Succeeded", { timeout: 60_000 });
  // The filtered backup is marked as such; the whole-database one is not.
  await expect(row.getByText("Filtered", { exact: true })).toBeVisible();
  const crmRow = backups.getByRole("row").filter({ hasText: crm }).filter({ hasNotText: `${crm}_rescue_` });
  await expect(crmRow).toContainText("Succeeded", { timeout: 60_000 });
  await expect(crmRow.getByText("Filtered", { exact: true })).toHaveCount(0);

  // The archive holds only the collections backed up.
  const runID = page.url().match(/[#&?]run=(run_[^&]+)/)?.[1] ?? "";
  const names = await page.evaluate(async ({ run, db }) => {
    const list = await (await fetch(`/api/v1/backups?run_id=${encodeURIComponent(run)}`)).json();
    const backup = (list.data || []).find((b: { database: string }) => b.database === db);
    const colls = await (await fetch(`/api/v1/backups/${encodeURIComponent(backup.id)}/collections`)).json();
    return (colls.data.collections || []).map((c: { name: string }) => c.name).sort();
  }, { run: runID, db: shop });
  expect(names).toEqual(["orders"]);
});

test("excludes a collection in All with the keyboard", async ({ page, services }) => {
  const shop = services.database;
  const { dialog } = await openBackupNow(page);
  await dialog.getByRole("button", { name: "All", exact: true }).click();
  const tree = dialog.getByRole("tree", { name: "Exclude" });
  const shopBox = tree.getByRole("checkbox", { name: new RegExp(`^${shop}\\b`) });
  await shopBox.focus();
  // Right opens the database, Right again moves to its first collection.
  await page.keyboard.press("ArrowRight");
  const item = tree.locator(`[role="treeitem"][aria-level="1"]`).filter({ has: page.getByRole("checkbox", { name: new RegExp(`^${shop}\\b`) }) });
  await expect(item).toHaveAttribute("aria-expanded", "true");
  const first = item.getByRole("treeitem").first().getByRole("checkbox");
  await expect(first).toBeVisible();
  // All: nothing is excluded until checked.
  await expect(first).not.toBeChecked();
  await expect(shopBox).toBeFocused();
  await page.keyboard.press("ArrowRight");
  await expect(first).toBeFocused();
  await page.keyboard.press("Space");
  await expect(first).toBeChecked();
  const excluded = await first.inputValue();
  await expect(item.locator(".db-tree-summary")).toHaveText(`all except ${excluded}`);
  await expect(shopBox).toHaveJSProperty("indeterminate", true);
  // Left goes back to the database, Left again closes it.
  await page.keyboard.press("ArrowLeft");
  await expect(shopBox).toBeFocused();
  await page.keyboard.press("ArrowLeft");
  await expect(item).toHaveAttribute("aria-expanded", "false");

  const request = page.waitForRequest(req => req.method() === "POST" && new URL(req.url()).pathname === "/api/v1/backups");
  await dialog.getByRole("button", { name: "Start backup" }).click();
  await expect(dialog).toBeHidden();
  const body = (await request).postDataJSON();
  expect(body.databases).toContainEqual({ name: shop, exclude_collections: [excluded] });
  expect(body.databases).toContain(services.database2);
  await expect(page.getByRole("region", { name: "Messages" }).getByText(/^Backup of \d+ databases started\.$/)).toBeVisible();
});
