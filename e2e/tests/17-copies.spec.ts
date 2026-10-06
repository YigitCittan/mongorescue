// Spec 17: a job copies its backups to a second storage target (3-2-1); the
// backup's details show "1 copy" once the copy queue made it.
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { expect, test } from "./fixtures.js";

type Target = { id: string; name: string; is_default: boolean };
type Backup = { id: string; status: string; copies?: Array<{ status: string }> };

test("copies a job's backup to a second target and shows the copy", async ({ page, services }) => {
  await page.goto("/");

  // A second (local) storage target besides the default MinIO one.
  const copyName = "e2e-copy-local";
  const copyPath = mkdtempSync(join(tmpdir(), "mongorescue-e2e-copy-"));
  const copyTarget = await page.evaluate(async ({ name, path }) => {
    const me = await (await fetch("/api/v1/auth/me", { credentials: "same-origin" })).json();
    const headers = { "Content-Type": "application/json", "X-CSRF-Token": me.data.csrf_token };
    const list = (await (await fetch("/api/v1/storage-targets")).json()).data as Target[];
    const found = list.find(t => t.name === name);
    if (found) return found;
    const res = await fetch("/api/v1/storage-targets", {
      method: "POST", credentials: "same-origin", headers,
      body: JSON.stringify({ name, type: "local", local: { path } }),
    });
    return (await res.json()).data as Target;
  }, { name: copyName, path: copyPath });
  expect(copyTarget && copyTarget.id).toBeTruthy();
  await page.reload();

  // A job with the second target as its copy target.
  await page.getByRole("tab", { name: /^Jobs/ }).click();
  await page.getByRole("tabpanel", { name: "Jobs" }).getByRole("button", { name: "New job" }).first().click();
  const dialog = page.getByRole("dialog", { name: "New backup job" });
  await expect(dialog).toBeVisible();
  await dialog.getByRole("textbox", { name: "Name" }).fill("e2e-copies");
  const database = dialog.getByRole("combobox", { name: "Database" });
  await expect(database.getByRole("option", { name: new RegExp(`^${services.database}\\b`) })).toBeAttached();
  await database.selectOption({ value: services.database });
  await dialog.getByRole("group", { name: "Copy targets" }).getByRole("checkbox", { name: copyName }).check();
  await expect(dialog.getByRole("combobox", { name: "Copy mode" })).toHaveValue("async");
  await dialog.getByRole("button", { name: "Create job" }).click();
  await expect(dialog).toBeHidden();

  // Run it and wait for the backup and its copy.
  const backupID = await page.evaluate(async ({ target }) => {
    const me = await (await fetch("/api/v1/auth/me", { credentials: "same-origin" })).json();
    const headers = { "Content-Type": "application/json", "X-CSRF-Token": me.data.csrf_token };
    const jobs = (await (await fetch("/api/v1/jobs")).json()).data as Array<{ id: string; name: string; copy_targets?: string[] }>;
    const job = jobs.find(j => j.name === "e2e-copies");
    if (!job || !(job.copy_targets || []).includes(target)) throw new Error("the job has no copy target");
    await fetch(`/api/v1/jobs/${job.id}/run`, { method: "POST", credentials: "same-origin", headers });
    for (let i = 0; i < 120; i++) {
      const list = (await (await fetch(`/api/v1/backups?job_id=${encodeURIComponent(job.id)}`)).json()).data as Backup[];
      const b = list && list[0];
      if (b && b.status === "completed" && (b.copies || []).every(c => c.status === "done")) return b.id;
      if (b && b.status === "failed") throw new Error("the backup failed");
      await new Promise(r => setTimeout(r, 1000));
    }
    throw new Error("the backup or its copy did not complete");
  }, { target: copyTarget.id });

  await page.getByRole("tab", { name: /^Backups/ }).click();
  const panel = page.getByRole("tabpanel", { name: "Backups" });
  await panel.locator(`button[data-action="backup-details"][data-id="${backupID}"]`).first().click();
  const details = page.getByRole("dialog", { name: "Backup details" });
  await expect(details).toBeVisible();
  await expect(details).toContainText("1 copy");
  await expect(details).toContainText(copyName);
});
