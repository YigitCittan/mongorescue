// Spec 8: create a job with a recovery point objective in the job form and find its
// database in the Overview's "Recovery readiness" table.
import { expect, test } from "./fixtures.js";

test("shows a new job's database in the recovery readiness table", async ({ page, services }) => {
  await page.goto("/");
  await page.getByRole("tab", { name: /^Jobs/ }).click();
  // The section header's button (an empty list shows a second one).
  await page.getByRole("tabpanel", { name: "Jobs" }).getByRole("button", { name: "New job" }).first().click();

  const dialog = page.getByRole("dialog", { name: "New backup job" });
  await expect(dialog).toBeVisible();
  await dialog.getByRole("textbox", { name: "Name" }).fill("e2e-rpo");
  const database = dialog.getByRole("combobox", { name: "Database" });
  await expect(database.getByRole("option", { name: new RegExp(`^${services.database}\\b`) })).toBeAttached();
  await database.selectOption({ value: services.database });
  // The hint shows the default of the schedule (@daily: two days and an hour).
  const rpo = dialog.getByRole("spinbutton", { name: "Recovery point objective (hours)" });
  await expect(dialog.getByText(/default of this schedule: 2 d 1 h/)).toBeVisible();
  await rpo.fill("12");
  await dialog.getByRole("button", { name: "Create job" }).click();
  await expect(dialog).toBeHidden();

  await page.getByRole("tab", { name: "Overview" }).click();
  const table = page.getByRole("table", { name: "Recovery readiness of every database a job backs up" });
  const row = table.getByRole("row").filter({ hasText: "e2e-rpo" });
  await expect(row).toHaveCount(1);
  await expect(row.getByRole("rowheader")).toContainText(services.database);
  await expect(row).toContainText("no backup yet, target 12 h");
  await expect(row).toContainText("Warning");
  await expect(row).toContainText("No backup yet");
});
