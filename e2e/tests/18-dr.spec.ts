// Spec 18: cross-region disaster recovery. A job copying from a target in one
// region to a target in another is refused while it requires locked copies on a
// target without Object Lock, and its readiness row shows the DR status.
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { expect, test } from "./fixtures.js";

type Target = { id: string; name: string; region?: string };

test("refuses unlocked copies when required and shows the DR status in readiness", async ({ page, services }) => {
  await page.goto("/");
  const paths = [mkdtempSync(join(tmpdir(), "mongorescue-e2e-dr-a-")), mkdtempSync(join(tmpdir(), "mongorescue-e2e-dr-b-"))];
  const result = await page.evaluate(async ({ paths, database }) => {
    const me = await (await fetch("/api/v1/auth/me", { credentials: "same-origin" })).json();
    const headers = { "Content-Type": "application/json", "X-CSRF-Token": me.data.csrf_token };
    const target = async (name: string, path: string, region: string) => {
      const list = (await (await fetch("/api/v1/storage-targets")).json()).data as Target[];
      const found = list.find(t => t.name === name);
      if (found) return found;
      const res = await fetch("/api/v1/storage-targets", {
        method: "POST", credentials: "same-origin", headers,
        body: JSON.stringify({ name, type: "local", local: { path }, region }),
      });
      return (await res.json()).data as Target;
    };
    const a = await target("e2e-dr-region-a", paths[0], "region-a");
    const b = await target("e2e-dr-region-b", paths[1], "region-b");
    const conns = (await (await fetch("/api/v1/connections")).json()).data as Array<{ id: string }>;
    const job = (requireLocked: boolean) => fetch("/api/v1/jobs", {
      method: "POST", credentials: "same-origin", headers,
      body: JSON.stringify({
        name: "e2e-dr", database, connection_id: conns[0].id, cron_expression: "@daily",
        storage_target_id: a.id, copy_targets: [b.id], require_locked_copies: requireLocked,
      }),
    });
    const refused = await job(true);
    const created = await job(false);
    return { a, b, refused: refused.status, refusedBody: await refused.text(), created: created.status };
  }, { paths, database: services.database });

  expect(result.a.region).toBe("region-a");
  expect(result.b.region).toBe("region-b");
  expect(result.refused).toBe(400);
  expect(result.refusedBody).toContain("Object Lock");
  expect(result.created).toBeLessThan(300);

  await page.reload();
  await page.getByRole("tab", { name: "Overview" }).click();
  const table = page.getByRole("table", { name: "Recovery readiness of every database a job backs up" });
  const row = table.getByRole("row").filter({ hasText: "e2e-dr" });
  await expect(row).toHaveCount(1);
  // A copy in another region, but not locked and never drilled: not proven yet.
  await expect(row).toContainText("DR: cross-region, unproven");
  await expect(row).toContainText("No recent DR drill");
  await expect(row).not.toContainText("No copy in another region");
});
