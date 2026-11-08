// Spec 25: scheduled jobs from the dashboard: create a job in the New backup job
// dialog, see it in the Jobs list with the schedule's plain-language reading, edit it
// (name, schedule), refuse an invalid schedule, and delete it after confirming.
import type { Locator, Page } from "@playwright/test";
import { CONNECTION_NAME, expect, test } from "./fixtures.js";

const JOB = "e2e-nightly";
const RENAMED = "e2e-nightly-renamed";

// rowAction opens the row's "More actions" menu and picks an item. The list redraws while
// jobs refresh (which closes an open menu), so opening and picking retry together.
async function rowAction(page: Page, row: Locator, item: string): Promise<void> {
  await expect(async () => {
    await row.getByRole("button", { name: "More actions" }).click();
    await page.getByRole("menuitem", { name: item, exact: true }).click({ timeout: 2_000 });
  }).toPass({ timeout: 15_000 });
}

test("creates, edits and deletes a scheduled job", async ({ page, services }) => {
  await page.goto("/");
  await page.getByRole("tab", { name: /^Jobs/ }).click();
  const panel = page.getByRole("tabpanel", { name: "Jobs" });
  const messages = page.getByRole("region", { name: "Messages" });

  // Create.
  await panel.getByRole("button", { name: "New job" }).first().click();
  const dialog = page.getByRole("dialog", { name: "New backup job" });
  await expect(dialog).toBeVisible();
  await dialog.getByLabel("Name", { exact: true }).fill(JOB);
  await dialog.getByRole("combobox", { name: "Connection" }).selectOption({ label: CONNECTION_NAME });
  await dialog.locator("#job-database-select").selectOption(services.database);
  await dialog.getByLabel("Schedule", { exact: true }).fill("0 2 * * *");
  await dialog.getByRole("button", { name: "Create job" }).click();
  await expect(dialog).toBeHidden();
  await expect(messages.getByText("Job created", { exact: true })).toBeVisible();

  const row = panel.getByRole("row").filter({ hasText: JOB });
  await expect(row).toHaveCount(1);
  await expect(row).toContainText(services.database);
  await expect(row.getByText("0 2 * * *", { exact: true })).toBeVisible();
  await expect(row.getByText("Every day at 02:00", { exact: true })).toBeVisible();

  // Edit: the dialog starts from the saved values.
  await rowAction(page, row, "Edit");
  const edit = page.getByRole("dialog", { name: "Edit backup job" });
  await expect(edit.getByLabel("Name", { exact: true })).toHaveValue(JOB);
  await expect(edit.getByLabel("Schedule", { exact: true })).toHaveValue("0 2 * * *");
  await edit.getByLabel("Name", { exact: true }).fill(RENAMED);
  await edit.getByLabel("Schedule", { exact: true }).fill("30 4 * * *");
  await edit.getByRole("button", { name: "Save changes" }).click();
  await expect(edit).toBeHidden();
  await expect(messages.getByText("Job updated", { exact: true })).toBeVisible();

  const renamed = panel.getByRole("row").filter({ hasText: RENAMED });
  await expect(renamed).toHaveCount(1);
  await expect(renamed.getByText("30 4 * * *", { exact: true })).toBeVisible();
  await expect(renamed.getByText("Every day at 04:30", { exact: true })).toBeVisible();
  await expect(panel.getByRole("row").filter({ hasText: JOB, hasNotText: RENAMED })).toHaveCount(0);

  // An invalid schedule is refused and the dialog stays open.
  await rowAction(page, renamed, "Edit");
  await edit.getByLabel("Schedule", { exact: true }).fill("not a schedule");
  await edit.getByRole("button", { name: "Save changes" }).click();
  await expect(edit).toBeVisible();
  await edit.getByRole("button", { name: "Cancel" }).click();
  await expect(edit).toBeHidden();
  await expect(renamed.getByText("30 4 * * *", { exact: true })).toBeVisible();

  // Delete asks first.
  await rowAction(page, renamed, "Delete");
  const confirm = page.getByRole("alertdialog", { name: "Delete" });
  await expect(confirm.getByText(`${RENAMED} · ${services.database}`, { exact: true })).toBeVisible();
  await confirm.getByRole("button", { name: "Delete", exact: true }).click();
  await expect(messages.getByText("Job deleted", { exact: true })).toBeVisible();
  await expect(panel.getByRole("row").filter({ hasText: RENAMED })).toHaveCount(0);
});
