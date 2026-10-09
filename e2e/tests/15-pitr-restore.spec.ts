// Spec 15: the point-in-time restore wizard of the Point-in-time recovery panel. The
// e2e MongoDB is a standalone server, which has no oplog, so the stream list, the
// preflight and the restore are stubbed with page.route(): the spec checks that the
// wizard bounds the time to the window, shows the plan of the preflight and starts
// the restore with the chosen time after a confirmation.
import { expect, test } from "./fixtures.js";

const windowStart = "2026-10-05T10:00:00Z";
const windowEnd = "2026-10-05T12:00:00Z";

const stream = {
  stream: { id: "str_e2e", connection_id: "conn_e2e", replica_set: "rs0", target_id: "tgt", enabled: true,
    base_cron: "0 2 * * *", base_keep_count: 7, base_keep_days: 14, oplog_max_days: 0, chunk_seconds: 60, base_on_gap: true,
    created_at: windowStart, updated_at: windowStart },
  state: { stream_id: "str_e2e", chain_id: "ch1", last: { ts: { t: 1791208800, i: 1 }, t: 1 }, status: "running", updated_at: windowEnd },
  running: true,
  lag_seconds: 2,
  headroom_seconds: 86400,
  durable_rpo_seconds: 30,
  windows: [{ chain_id: "ch1", open: true, start: { t: 1791201600, i: 1 }, end: { t: 1791208800, i: 1 },
    start_time: windowStart, end_time: windowEnd, bases: 1 }],
  chains: [],
  bases: [],
  chain_breaks: 0,
};

const preflight = {
  ok: true,
  checks: [
    { id: "pitr_chain", status: "pass", message: "base backup bkp_base_e2e and 120 oplog chunk(s) reach the target without a break" },
    { id: "connection", status: "pass", message: "connected" },
    { id: "tools_version", status: "pass", message: "mongorestore 100.16.0 (100.12.0 or newer is needed)" },
  ],
  pitr: { base_id: "bkp_base_e2e", base_started_at: windowStart, base_consistent_at: windowStart, base_bytes: 1048576,
    oplog_bytes: 524288, chunks: 120, unverified_chunks: 0, target_time: "2026-10-05T11:30:00Z", limit: "1791207001:0",
    clone_suffix: "_rescue_20261005_120500", estimated_seconds: 95, estimate_from: "default" },
};

test("restores to a point in time through the wizard", async ({ page }) => {
  const sent: { preflight?: unknown; restore?: unknown } = {};
  await page.route("**/api/v1/pitr/streams", route => route.fulfill({ json: { success: true, data: [stream] } }));
  await page.route("**/api/v1/restores/preflight", async route => {
    sent.preflight = route.request().postDataJSON();
    await route.fulfill({ json: { success: true, data: preflight } });
  });
  await page.route("**/api/v1/restore", async route => {
    sent.restore = route.request().postDataJSON();
    await route.fulfill({ status: 202, json: { success: true, data: { id: "rst_pitr_e2e", status: "in_progress" } } });
  });

  await page.goto("/");
  await page.getByRole("tab", { name: /^Connections/ }).click();
  const panel = page.locator("#pitr-panel");
  // A supported feature since #141: no "Experimental" badge.
  await expect(panel.getByText("Point-in-time recovery").first()).toBeVisible();
  await expect(page.getByText("Experimental", { exact: true })).toHaveCount(0);
  await panel.getByRole("button", { name: "Restore to a time" }).click();

  const wizard = page.locator("#form-pitr-restore");
  await expect(wizard.getByRole("heading", { name: /Restore to a point in time/ })).toBeVisible();
  await expect(wizard.getByText("Between 2026-10-05T10:00:00Z and 2026-10-05T11:59:59Z (UTC).")).toBeVisible();
  const at = wizard.getByLabel("Restore to (UTC)");
  await expect(at).toHaveAttribute("min", "2026-10-05T10:00:00");
  await expect(at).toHaveAttribute("max", "2026-10-05T11:59:59");
  const submit = wizard.getByRole("button", { name: "Restore", exact: true });
  await expect(submit).toBeDisabled();

  // A time outside the window is refused before anything is sent.
  await at.fill("2026-10-05T12:30");
  await wizard.getByRole("button", { name: "Check" }).click();
  await expect(wizard.getByText("Choose a time inside the window.")).toBeVisible();
  expect(sent.preflight).toBeUndefined();

  await at.fill("2026-10-05T11:30");
  await wizard.getByLabel(/Databases/).fill("shop");
  await wizard.getByRole("button", { name: "Check" }).click();
  await expect(wizard.getByText("bkp_base_e2e (2026-10-05T10:00:00Z)")).toBeVisible();
  await expect(wizard.getByText("120 chunk(s), 512 KiB")).toBeVisible();
  await expect(wizard.getByText("Estimated duration")).toBeVisible();
  await expect(wizard.getByText("pitr_chain")).toBeVisible();
  expect(sent.preflight).toEqual({ pitr: { stream_id: "str_e2e", at: "2026-10-05T11:30:00Z" }, databases: ["shop"] });

  await expect(submit).toBeEnabled();
  await submit.click();
  const confirm = page.getByRole("alertdialog", { name: "Start the point-in-time restore?" });
  await expect(confirm).toContainText("_rescue_<YYYYMMDD_HHMMSS>_<id>");
  await expect(confirm).not.toContainText("_rescue_20261005_120500");
  await confirm.getByRole("button", { name: "Restore" }).click();
  await expect(page.getByText("Point-in-time restore rst_pitr_e2e started.")).toBeVisible();
  expect(sent.restore).toEqual(sent.preflight);
  await expect(wizard).toBeHidden();
});
