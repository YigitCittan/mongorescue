// Spec 6: bulk delete of backups. The dialog first runs a dry run of the selection
// and shows its counts; nothing is deleted until the action is confirmed.
import { type Locator, type Page } from "@playwright/test";
import { expect, test } from "./fixtures.js";

// stat is the value (<dd>) next to the label (<dt>) in the review's statistics.
function stat(dialog: Locator, label: string): Locator {
  return dialog.getByRole("term").filter({ hasText: new RegExp(`^${label}$`) }).locator("xpath=following-sibling::dd[1]");
}

async function backUpNow(page: Page, database: string): Promise<void> {
  const backups = page.getByRole("tabpanel", { name: "Backups" });
  await backups.getByRole("button", { name: "Back up now" }).click();
  const dialog = page.getByRole("dialog", { name: "Back up now" });
  await expect(dialog.getByRole("combobox", { name: "Database" }).getByRole("option", { name: new RegExp(`^${database}\\b`) })).toBeAttached();
  await dialog.getByRole("combobox", { name: "Database" }).selectOption({ value: database });
  await dialog.getByRole("button", { name: "Start backup" }).click();
  await expect(dialog).toBeHidden();
}

test("bulk deletes backups after a dry-run preview", async ({ page, services }) => {
  await page.goto("/");
  await page.getByRole("tab", { name: /^Backups/ }).click();
  const backups = page.getByRole("tabpanel", { name: "Backups" });
  const rows = backups.getByRole("row").filter({ hasText: services.database });

  // A second backup, so the selection holds more than one item.
  await expect(rows).toHaveCount(1);
  await backUpNow(page, services.database);
  await expect(rows).toHaveCount(2);
  await expect(rows.filter({ hasText: "Succeeded" })).toHaveCount(2, { timeout: 60_000 });

  await backups.getByRole("checkbox", { name: "Select all rows on this page" }).check();
  const toolbar = backups.getByRole("region", { name: "Bulk actions" });
  await expect(toolbar).toContainText("2 selected");

  const dryRun = page.waitForRequest(
    req => req.method() === "POST" && new URL(req.url()).pathname === "/api/v1/backups/bulk" && req.postDataJSON()?.dry_run === true,
  );
  await toolbar.getByRole("button", { name: "Delete" }).click();
  const request = (await dryRun).postDataJSON();
  expect(request.action).toBe("delete");
  expect(request.ids).toHaveLength(2);

  // The title is "Delete · Backups" for the review, "Delete · Finished" afterwards.
  const dialog = page.getByRole("dialog", { name: /^Delete · / });
  await expect(dialog).toHaveAccessibleName("Delete · Backups");
  await expect(stat(dialog, "Selected")).toHaveText("2");
  await expect(stat(dialog, "Will be changed")).toHaveText("2");
  await expect(stat(dialog, "Skipped")).toHaveText("0");
  await expect(dialog).toContainText("This cannot be undone");
  // The preview deleted nothing.
  await expect(rows).toHaveCount(2);

  await dialog.getByRole("button", { name: "Delete (2)" }).click();
  await expect(dialog).toHaveAccessibleName("Delete · Finished", { timeout: 30_000 });
  await expect(dialog.getByRole("status")).toHaveText("2 succeeded · 0 skipped · 0 failed");
  await dialog.getByRole("button", { name: "Close" }).last().click();
  await expect(dialog).toBeHidden();
  await expect(rows).toHaveCount(0);
});
