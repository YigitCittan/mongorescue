// Spec 12: the administrator configures the external heartbeat in Settings →
// Monitoring and sends a test ping. The heartbeat URL is MongoRescue's own health
// endpoint (any 2xx answer counts); its query stands in for the token of a real
// check and must never come back from the server.
import { expect, test } from "./fixtures.js";

test("configures the heartbeat and sends a test ping", async ({ page, server }) => {
  const heartbeatURL = `${server.baseURL}/api/v1/health?token=e2e-heartbeat-secret`;
  const origin = new URL(server.baseURL);

  await page.goto("/");
  await page.getByRole("tab", { name: "Settings" }).click();
  await page.getByRole("button", { name: "Monitoring", exact: true }).click();
  const panel = page.locator("#settings-monitoring");
  await expect(panel.getByRole("heading", { name: "Monitoring" })).toBeVisible();

  const url = panel.getByLabel("Heartbeat URL");
  await url.fill(heartbeatURL);
  await panel.getByLabel("Ping every (minutes)").fill("2");
  await panel.getByRole("button", { name: "Save" }).click();
  await expect(panel.getByText("Saved")).toBeVisible();
  // The stored URL comes back masked: only its origin is shown.
  await expect(url).toHaveValue(`${origin.protocol}//${origin.host}/******`);

  // The masked value pings the stored URL.
  await panel.getByRole("button", { name: "Send test ping" }).click();
  await expect(panel.getByText(`The test ping reached ${origin.hostname}.`)).toBeVisible();

  const settings = await page.evaluate(async () => (await (await fetch("/api/v1/settings")).json()).data);
  expect(settings.monitoring.heartbeat_interval).toBe("2m0s");
  expect(JSON.stringify(settings)).not.toContain("e2e-heartbeat-secret");

  // Turn the heartbeat off again, so later runs of the suite start clean.
  await url.fill("");
  await panel.getByRole("button", { name: "Save" }).click();
  await expect(panel.getByText("Saved")).toBeVisible();
});
