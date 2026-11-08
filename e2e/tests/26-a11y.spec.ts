// Spec 26: automated accessibility scan (axe-core, WCAG 2.0/2.1 A and AA) of the
// sign-in screen, the overview, backups, restores and settings, in light and dark
// mode. Critical and serious violations fail the test unless they are allow-listed,
// with a reason, in a11y-allowlist.ts.
import AxeBuilder from "@axe-core/playwright";
import type { Page } from "@playwright/test";
import { ALLOWED_VIOLATIONS } from "./a11y-allowlist.js";
import { expect, test } from "./fixtures.js";

type Theme = "light" | "dark";

const FAILING_IMPACTS = ["critical", "serious"];

// violations runs axe on the page and returns every non-allow-listed critical or serious violation.
async function violations(page: Page, name: string): Promise<string[]> {
  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
    .analyze();
  const failures: string[] = [];
  for (const v of results.violations) {
    if (!v.impact || !FAILING_IMPACTS.includes(v.impact)) continue;
    for (const node of v.nodes) {
      const selector = node.target.join(" ");
      const allowed = ALLOWED_VIOLATIONS.some(
        a =>
          a.rule === v.id &&
          (!a.pages || a.pages.some(p => name.startsWith(p))) &&
          (!a.selector || selector.includes(a.selector)),
      );
      if (!allowed) failures.push(`[${v.impact}] ${v.id}: ${selector}\n    ${(node.failureSummary ?? "").replace(/\n/g, "\n    ")}`);
    }
  }
  return failures.map(f => `${name}: ${f}`);
}

// scan fails the test on every non-allow-listed critical or serious violation.
async function scan(page: Page, name: string): Promise<void> {
  expect(await violations(page, name), `axe violations on ${name}`).toEqual([]);
}

// useTheme makes the dashboard start in the theme (the stored preference wins over
// the system setting) before any script runs.
async function useTheme(page: Page, theme: Theme): Promise<void> {
  await page.emulateMedia({ colorScheme: theme });
  await page.addInitScript(t => window.localStorage.setItem("mongorescue_theme", t), theme);
}

for (const theme of ["light", "dark"] as const) {
  test.describe(`${theme} mode`, () => {
    test("sign-in screen has no serious violations", async ({ browser, server }) => {
      const context = await browser.newContext({ baseURL: server.baseURL, storageState: { cookies: [], origins: [] } });
      try {
        const page = await context.newPage();
        await useTheme(page, theme);
        await page.goto("/");
        await expect(page.getByRole("region", { name: "Sign in" })).toBeVisible();
        await scan(page, `login (${theme})`);
      } finally {
        await context.close();
      }
    });

    for (const tab of ["Overview", "Backups", "Restores"] as const) {
      test(`${tab} tab has no serious violations`, async ({ page }) => {
        await useTheme(page, theme);
        await page.goto("/");
        await page.getByRole("tab", { name: new RegExp(`^${tab}`) }).click();
        const panel = page.getByRole("tabpanel", { name: tab });
        await expect(panel).toBeVisible();
        if (tab !== "Overview") await expect(panel.getByRole("row").nth(1)).toBeVisible();
        await scan(page, `${tab.toLowerCase()} (${theme})`);
      });
    }

    test("restore dialog has no serious violations", async ({ page, services }) => {
      await useTheme(page, theme);
      await page.goto("/");
      await page.getByRole("tab", { name: /^Backups/ }).click();
      const backups = page.getByRole("tabpanel", { name: "Backups" });
      await backups
        .getByRole("row")
        .filter({ hasText: services.database })
        .filter({ hasText: "Succeeded" })
        .first()
        .getByRole("button", { name: "Restore" })
        .click();
      await expect(page.getByRole("dialog", { name: "Restore backup" })).toBeVisible();
      await scan(page, `restore dialog (${theme})`);
    });

    test("settings sections have no serious violations", async ({ page }) => {
      await useTheme(page, theme);
      await page.goto("/");
      await page.getByRole("tab", { name: "Settings" }).click();
      const nav = page.getByRole("navigation", { name: "Settings" });
      await expect(nav).toBeVisible();
      const sections = await nav
        .getByRole("button")
        .evaluateAll(els => els.map(el => (el as HTMLElement).dataset.section ?? ""));
      expect(sections.length).toBeGreaterThan(4);
      // Every section is scanned before the test fails, so one run lists them all.
      const failures: string[] = [];
      for (const section of sections) {
        await nav.locator(`[data-section="${section}"]`).click();
        failures.push(...(await violations(page, `settings ${section} (${theme})`)));
      }
      expect(failures, "axe violations in the settings sections").toEqual([]);
    });
  });
}
