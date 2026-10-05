// Spec 11: back up two databases at once from the Backup now dialog (Selected
// mode) and follow the run from its toast to the Backups list filtered by the run.
import { expect, test } from "./fixtures.js";

test("backs up two databases in one run from Back up now", async ({ page, services }) => {
  const databases = [services.database, services.database2];
  await page.goto("/");
  await page.getByRole("tab", { name: /^Backups/ }).click();
  const backups = page.getByRole("tabpanel", { name: "Backups" });
  await backups.getByRole("button", { name: "Back up now" }).first().click();

  const dialog = page.getByRole("dialog", { name: "Back up now" });
  await expect(dialog).toBeVisible();
  await dialog.getByRole("button", { name: "Selected", exact: true }).click();
  for (const db of databases) {
    const box = dialog.getByRole("checkbox", { name: new RegExp(`^${db}\\b`) });
    await expect(box).toBeVisible();
    await box.check();
  }
  // The count and the estimated size of the selection.
  await expect(dialog.getByRole("status").filter({ hasText: "2 databases selected" })).toContainText("in total");
  const request = page.waitForRequest(req => req.method() === "POST" && new URL(req.url()).pathname === "/api/v1/backups");
  await dialog.getByRole("button", { name: "Start backup" }).click();
  await expect(dialog).toBeHidden();
  const body = (await request).postDataJSON();
  expect(body.databases).toEqual([...databases].sort());
  expect(body.database).toBeUndefined();

  // One toast for the run; it opens the run's backups.
  await expect(page.getByText("Backup of 2 databases started.")).toBeVisible();
  await page.getByRole("button", { name: "View run" }).click();
  await expect(backups).toBeVisible();
  await expect(backups.getByText(/^Run run_/)).toBeVisible();
  await expect(page).toHaveURL(/[#&?]run=run_/);
  for (const db of databases) {
    const row = backups.getByRole("row").filter({ hasText: db }).filter({ hasNotText: `${db}_rescue_` });
    await expect(row).toHaveCount(1);
    await expect(row).toContainText("Succeeded", { timeout: 60_000 });
  }
});
