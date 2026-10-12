// Spec 19: the Jobs table pages and filters on the server (GET /api/v1/jobs pages
// by cursor): rows per page, the range of the total, Previous / Next, and a
// search that the server answers.
import { expect, test } from "./fixtures.js";

test("pages and filters the jobs on the server", async ({ page, services }) => {
  await page.goto("/");
  const ids = await page.evaluate(async (database) => {
    const me = await (await fetch("/api/v1/auth/me", { credentials: "same-origin" })).json();
    const headers = { "Content-Type": "application/json", "X-CSRF-Token": me.data.csrf_token };
    const conns = (await (await fetch("/api/v1/connections")).json()).data as Array<{ id: string }>;
    const out: string[] = [];
    for (let i = 0; i < 30; i++) {
      const res = await fetch("/api/v1/jobs", {
        method: "POST", credentials: "same-origin", headers,
        body: JSON.stringify({ name: `e2e-page-${String(i).padStart(2, "0")}`, database, connection_id: conns[0].id, cron_expression: "@daily", enabled: false }),
      });
      out.push(((await res.json()).data as { id: string }).id);
    }
    return out;
  }, services.database);
  expect(ids).toHaveLength(30);

  try {
    await page.reload();
    await page.getByRole("tab", { name: /^Jobs/ }).click();
    const panel = page.getByRole("tabpanel", { name: "Jobs" });
    await panel.getByRole("searchbox", { name: "Search" }).fill("e2e-page-");
    const pager = panel.getByRole("navigation", { name: "Job pages" });
    await pager.getByRole("combobox", { name: "Rows per page" }).selectOption("25");
    await expect(pager).toContainText("1–25 of 30");
    await expect(panel.getByRole("row").filter({ hasText: "e2e-page-" })).toHaveCount(25);
    await expect(pager.getByRole("button", { name: /Previous/ })).toBeDisabled();

    await pager.getByRole("button", { name: /Next/ }).click();
    await expect(pager).toContainText("26–30 of 30");
    await expect(panel.getByRole("row").filter({ hasText: "e2e-page-" })).toHaveCount(5);
    await expect(panel.getByRole("row").filter({ hasText: "e2e-page-29" })).toHaveCount(1);
    await expect(pager.getByRole("button", { name: /Next/ })).toBeDisabled();

    await pager.getByRole("button", { name: /Previous/ }).click();
    await expect(pager).toContainText("1–25 of 30");
    await expect(panel.getByRole("row").filter({ hasText: "e2e-page-00" })).toHaveCount(1);

    // A column sort orders every page on the server: by name, descending, the
    // first page starts with the last job.
    await panel.getByRole("columnheader", { name: "Name" }).getByRole("button").click();
    await expect(panel.getByRole("row").filter({ hasText: "e2e-page-" }).first()).toContainText("e2e-page-29");
    await expect(pager).toContainText("1–25 of 30");
    await pager.getByRole("button", { name: /Next/ }).click();
    await expect(pager).toContainText("26–30 of 30");
    await expect(panel.getByRole("row").filter({ hasText: "e2e-page-" }).first()).toContainText("e2e-page-04");

    // A narrower search starts again at the first page.
    await panel.getByRole("searchbox", { name: "Search" }).fill("e2e-page-1");
    await expect(pager).toContainText("1–10 of 10");
    await expect(panel.getByRole("row").filter({ hasText: "e2e-page-" })).toHaveCount(10);
  } finally {
    // Remove the jobs again, so later runs of the suite start clean.
    await page.evaluate(async (ids) => {
      const me = await (await fetch("/api/v1/auth/me", { credentials: "same-origin" })).json();
      for (const id of ids) {
        await fetch(`/api/v1/jobs/${id}`, { method: "DELETE", credentials: "same-origin", headers: { "X-CSRF-Token": me.data.csrf_token } });
      }
    }, ids);
  }
});
