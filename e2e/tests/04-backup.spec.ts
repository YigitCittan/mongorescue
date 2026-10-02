// Spec 4: run a backup now of the seeded database to the MinIO target and wait for
// it to complete in the Backups list.
import { TARGET_NAME, expect, test } from "./fixtures.js";

test("runs a backup now and sees it complete", async ({ page, services }) => {
  await page.goto("/");
  await page.getByRole("tab", { name: "Overview" }).click();
  await page.getByRole("tabpanel", { name: "Overview" }).getByRole("button", { name: "Back up now" }).click();

  const dialog = page.getByRole("dialog", { name: "Back up now" });
  await expect(dialog).toBeVisible();
  const database = dialog.getByRole("combobox", { name: "Database" });
  await expect(database.getByRole("option", { name: new RegExp(`^${services.database}\\b`) })).toBeAttached();
  await database.selectOption({ value: services.database });
  // The MinIO target added by spec 3 is the default.
  await expect(dialog.getByRole("combobox", { name: "Storage target" }).locator("option:checked")).toHaveText(
    `${TARGET_NAME} (default)`,
  );
  await dialog.getByRole("button", { name: "Start backup" }).click();
  await expect(dialog).toBeHidden();

  await page.getByRole("tab", { name: /^Backups/ }).click();
  const panel = page.getByRole("tabpanel", { name: "Backups" });
  const row = panel.getByRole("row").filter({ hasText: services.database });
  await expect(row).toHaveCount(1);
  await expect(row).toContainText("Succeeded", { timeout: 60_000 });
  await expect(row).toContainText(TARGET_NAME);
});
