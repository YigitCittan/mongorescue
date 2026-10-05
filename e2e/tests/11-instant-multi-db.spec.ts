// Spec 11: back up two databases at once from the Backup now dialog (Selected
// mode) and follow the run from its toast to the Backups list filtered by the run;
// then the selector's state logic and a backup of All without touching the list.
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
  await expect(page.getByRole("region", { name: "Messages" }).getByText("Backup of 2 databases started.")).toBeVisible();
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

// The selector's state logic (jobdbs.js), shared by Back up now and the job form:
// Selected includes the checked names, All excludes them.
test("database selector: what each mode backs up and when it is blocked", async ({ page }) => {
  await page.goto("/");
  const out = await page.evaluate(() => {
    const w = window as unknown as {
      dbSelIncluded: (mode: string, listed: string[], selected: Set<string>, excluded: Set<string>) => string[];
      dbSelProblem: (mode: string, listed: string[], selected: Set<string>, excluded: Set<string>) => string;
    };
    const listed = ["b", "a", "c"];
    const none = new Set<string>();
    const ab = new Set(["a", "b"]);
    const all = new Set(listed);
    return {
      listNone: [w.dbSelIncluded("list", listed, none, all), w.dbSelProblem("list", listed, none, all)],
      listAB: [w.dbSelIncluded("list", listed, ab, all), w.dbSelProblem("list", listed, ab, all)],
      allNone: [w.dbSelIncluded("all", listed, ab, none), w.dbSelProblem("all", listed, ab, none)],
      allAB: [w.dbSelIncluded("all", listed, none, ab), w.dbSelProblem("all", listed, none, ab)],
      allAll: [w.dbSelIncluded("all", listed, none, all), w.dbSelProblem("all", listed, none, all)],
      allEmpty: [w.dbSelIncluded("all", [], none, none), w.dbSelProblem("all", [], none, none)],
    };
  });
  expect(out.listNone).toEqual([[], "jobdb.need_selection"]);
  expect(out.listAB).toEqual([["a", "b"], ""]);
  expect(out.allNone).toEqual([["a", "b", "c"], ""]);
  expect(out.allAB).toEqual([["c"], ""]);
  expect(out.allAll).toEqual([[], "jobdb.all_excluded"]);
  expect(out.allEmpty).toEqual([[], ""]);
});

// Regression (v0.19.1): in All mode the checkboxes exclude, so they start
// unchecked and do not carry over what Selected checked; the dialog submits as is.
test("backs up all databases from Back up now without touching the list", async ({ page, services }) => {
  const databases = [services.database, services.database2];
  await page.goto("/");
  await page.getByRole("tab", { name: /^Backups/ }).click();
  const backups = page.getByRole("tabpanel", { name: "Backups" });
  await backups.getByRole("button", { name: "Back up now" }).first().click();

  const dialog = page.getByRole("dialog", { name: "Back up now" });
  await expect(dialog).toBeVisible();
  // Checked in Selected (included) must not turn into excluded in All.
  await dialog.getByRole("button", { name: "Selected", exact: true }).click();
  for (const db of databases) await dialog.getByRole("checkbox", { name: new RegExp(`^${db}\\b`) }).check();
  await dialog.getByRole("button", { name: "All", exact: true }).click();

  const excludes = dialog.getByRole("tree", { name: "Exclude" }).getByRole("checkbox");
  await expect(excludes.first()).toBeVisible();
  const count = await excludes.count();
  expect(count).toBeGreaterThanOrEqual(databases.length);
  for (let i = 0; i < count; i++) await expect(excludes.nth(i)).not.toBeChecked();
  await expect(dialog.getByRole("status").filter({ hasText: `${count} databases selected` })).toBeVisible();
  // Only excluding every database blocks the dialog, with its own message.
  for (let i = 0; i < count; i++) await excludes.nth(i).check();
  await expect(dialog.getByRole("status").filter({ hasText: "All databases are excluded." })).toBeVisible();
  await expect(dialog.getByText("Check at least one database.")).toHaveCount(0);
  for (let i = 0; i < count; i++) await excludes.nth(i).uncheck();
  await expect(dialog.getByRole("status").filter({ hasText: `${count} databases selected` })).toBeVisible();

  const request = page.waitForRequest(req => req.method() === "POST" && new URL(req.url()).pathname === "/api/v1/backups");
  await dialog.getByRole("button", { name: "Start backup" }).click();
  await expect(dialog).toBeHidden();
  const body = (await request).postDataJSON();
  expect(body.databases).toHaveLength(count);
  expect(body.databases).toEqual(expect.arrayContaining(databases));

  await expect(page.getByRole("region", { name: "Messages" }).getByText(`Backup of ${count} databases started.`)).toBeVisible();
  await page.getByRole("button", { name: "View run" }).last().click();
  await expect(backups.getByText(/^Run run_/)).toBeVisible();
  for (const db of databases) {
    const row = backups.getByRole("row").filter({ hasText: db }).filter({ hasNotText: `${db}_rescue_` });
    await expect(row).toHaveCount(1);
    await expect(row).toContainText("Succeeded", { timeout: 60_000 });
  }
});
