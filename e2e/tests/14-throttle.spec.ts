// Spec 14: a job reads from a secondary-preferred member with an upload cap and a
// backup window; a manual run warns that it ignores the window.
import { expect, test } from "./fixtures.js";

test("saves read preference, throttling and a backup window, and warns on a manual run", async ({ page, services }) => {
  await page.goto("/");
  await page.getByRole("tab", { name: /^Jobs/ }).click();
  await page.getByRole("tabpanel", { name: "Jobs" }).getByRole("button", { name: "New job" }).first().click();

  const dialog = page.getByRole("dialog", { name: "New backup job" });
  await expect(dialog).toBeVisible();
  await dialog.getByRole("textbox", { name: "Name" }).fill("e2e-window");
  const database = dialog.getByRole("combobox", { name: "Database" });
  await expect(database.getByRole("option", { name: new RegExp(`^${services.database}\\b`) })).toBeAttached();
  await database.selectOption({ value: services.database });

  await dialog.getByText("Read preference, throttling and backup window").click();
  await dialog.getByRole("combobox", { name: "Read preference" }).selectOption("secondaryPreferred");
  await dialog.getByRole("spinbutton", { name: "Upload cap (Mbit/s)" }).fill("80");
  await dialog.getByRole("spinbutton", { name: "Collections dumped in parallel" }).fill("2");
  await dialog.getByLabel("Start scheduled runs only within a backup window").check();
  await dialog.getByLabel("Opens at").fill("22:00");
  await dialog.getByLabel("Closes at").fill("02:00");
  await dialog.getByRole("textbox", { name: "Time zone" }).fill("Europe/Istanbul");
  await dialog.getByLabel("Cancel a run still going when the window closes").check();
  await dialog.getByRole("button", { name: "Create job" }).click();
  await expect(dialog).toBeHidden();

  const job = await page.evaluate(async () => {
    const jobs = (await (await fetch("/api/v1/jobs")).json()).data as Array<Record<string, unknown>>;
    return jobs.find(j => j.name === "e2e-window");
  });
  expect(job).toBeTruthy();
  expect(job!.read_preference).toBe("secondaryPreferred");
  expect(job!.max_upload_mbps).toBe(80);
  expect(job!.num_parallel_collections).toBe(2);
  expect(job!.backup_window).toEqual({ timezone: "Europe/Istanbul", start: "22:00", end: "02:00", cancel_at_window_end: true });

  // A manual run says that it ignores the window; cancelling starts nothing.
  const row = page.getByRole("tabpanel", { name: "Jobs" }).getByRole("row").filter({ hasText: "e2e-window" });
  await row.getByRole("button", { name: "Run now" }).click();
  const confirm = page.getByRole("dialog", { name: "Run outside the backup window?" });
  await expect(confirm).toBeVisible();
  await expect(confirm).toContainText("22:00–02:00 (Europe/Istanbul)");
  await confirm.getByRole("button", { name: "Cancel" }).click();
  await expect(confirm).toBeHidden();

  // Remove the job again, so later runs of the suite start clean.
  const status = await page.evaluate(async (id) => {
    const me = await (await fetch("/api/v1/auth/me", { credentials: "same-origin" })).json();
    return (await fetch(`/api/v1/jobs/${id}`, {
      method: "DELETE", credentials: "same-origin", headers: { "X-CSRF-Token": me.data.csrf_token },
    })).status;
  }, job!.id as string);
  expect([200, 204]).toContain(status);
});
