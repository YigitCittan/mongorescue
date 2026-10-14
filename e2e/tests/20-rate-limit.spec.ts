// Spec 20: the REST API rate limit. With a small budget for the session, a burst
// of dashboard requests gets 429s; the dashboard waits for Retry-After, tells the
// user and sends the refused requests again, so every request still succeeds.
import { expect, test } from "./fixtures.js";

test("backs off after the REST API rate limit and recovers", async ({ page }) => {
  await page.goto("/");
  await page.getByRole("tab", { name: "Settings" }).click();
  await page.locator(".settings-nav-btn[data-section=security]").click();
  const security = page.locator("#settings-security");
  await expect(security.getByLabel("Requests per minute (per API key or session)")).toHaveValue("1200");
  await expect(security.getByLabel("Work requests per minute")).toHaveValue("60");

  const save = async (perMinute: number, burst: number) => {
    await security.getByLabel("Requests per minute (per API key or session)").fill(String(perMinute));
    await security.getByLabel("Burst", { exact: true }).fill(String(burst));
    await security.getByRole("button", { name: "Save" }).click();
    await expect(security.getByText("Saved")).toBeVisible();
  };
  await save(600, 10);

  // Sequential requests beyond the burst: the dashboard's request wrapper waits
  // and retries, so all of them succeed.
  const statuses = await page.evaluate(async () => {
    const out: number[] = [];
    for (let i = 0; i < 25; i++) {
      // apiJSON is the dashboard's request wrapper (app.js).
      const json = await (window as unknown as { apiJSON: (u: string) => Promise<{ httpStatus: number }> }).apiJSON("/api/v1/stats");
      out.push(json.httpStatus);
    }
    return out;
  });
  expect(statuses.every(s => s === 200)).toBe(true);
  await expect(page.getByText(/Too many requests: the server asked the dashboard to wait/).first()).toBeVisible();

  // A raw request over the budget sees the 429 itself.
  const raw = await page.evaluate(async () => {
    let last: Response | null = null;
    for (let i = 0; i < 15; i++) last = await fetch("/api/v1/stats");
    return { status: last?.status, retryAfter: last?.headers.get("Retry-After"), body: await last?.json() };
  });
  expect(raw.status).toBe(429);
  expect(Number(raw.retryAfter)).toBeGreaterThanOrEqual(1);
  expect(raw.body.code).toBe("rate_limited");

  // Back to the defaults, so later specs run unthrottled.
  await page.waitForTimeout(2000);
  await save(1200, 300);
});
