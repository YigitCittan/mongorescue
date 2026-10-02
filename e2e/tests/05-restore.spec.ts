// Spec 5: restore the backup into a safe clone (<db>_rescue_<timestamp>, the default)
// and wait for the restore to complete in the Restores list.
import { expect, test } from "./fixtures.js";

test("restores the backup as a safe clone", async ({ page, services }) => {
  await page.goto("/");
  await page.getByRole("tab", { name: /^Backups/ }).click();
  const backups = page.getByRole("tabpanel", { name: "Backups" });
  const backup = backups.getByRole("row").filter({ hasText: services.database }).filter({ hasText: "Succeeded" });
  await backup.first().getByRole("button", { name: "Restore" }).click();

  const dialog = page.getByRole("dialog", { name: "Restore backup" });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole("checkbox", { name: /Restore into a safe clone/ })).toBeChecked();
  await expect(dialog.getByRole("checkbox", { name: /Dry run/ })).not.toBeChecked();
  await expect(dialog.getByRole("checkbox", { name: /Drop target collections first/ })).not.toBeChecked();
  await dialog.getByRole("button", { name: "Start restore" }).click();
  await expect(dialog).toBeHidden();

  await page.getByRole("tab", { name: /^Restores/ }).click();
  const restores = page.getByRole("tabpanel", { name: "Restores" });
  const clone = new RegExp(`${services.database}_rescue_\\d`);
  const row = restores.getByRole("row").filter({ hasText: clone });
  await expect(row).toHaveCount(1);
  await expect(row).toContainText("Succeeded", { timeout: 60_000 });
});
