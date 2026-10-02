// Spec 10: single sign-on through Keycloak (only with E2E_KEYCLOAK=1, which starts
// Keycloak with the realm of internal/integration/testdata). The administrator
// configures the provider in Settings → Single sign-on, tests it and maps groups to
// roles; a person then signs in with the button on the sign-in page through
// Keycloak's login form and lands in the dashboard with the mapped role. The users
// table marks the single sign-on user, whose role is managed by the provider, and a
// failed sign-in is explained on the sign-in page.
import { expect, test } from "./fixtures.js";

const KEYCLOAK_URL = process.env.MONGORESCUE_E2E_KEYCLOAK_URL || "";

test.skip(!KEYCLOAK_URL, "MONGORESCUE_E2E_KEYCLOAK_URL is not set (run with E2E_KEYCLOAK=1 make test-e2e)");

test("people sign in through Keycloak with the role of their group", async ({ page, browser, server }) => {
  // The administrator configures single sign-on.
  await page.goto("/");
  await page.getByRole("tab", { name: "Settings" }).click();
  await page.getByRole("button", { name: "Single sign-on", exact: true }).click();
  const panel = page.locator("#settings-sso");
  await expect(panel.getByLabel("Callback URL")).toHaveValue(`${server.baseURL}/auth/oidc/callback`);
  await panel.getByLabel("Button label").fill("Keycloak");
  await panel.getByLabel("Issuer URL").fill(`${KEYCLOAK_URL}/realms/mongorescue`);
  await panel.getByRole("button", { name: "Test provider" }).click();
  await expect(panel.getByText(/Provider found: .*RS256/)).toBeVisible();
  await panel.getByLabel("Client ID").fill("mongorescue");
  await panel.getByLabel("Client secret").fill("mongorescue-it-client-secret");
  for (const [group, role] of [["backup-admins", "admin"], ["backup-ops", "operator"]]) {
    await panel.getByRole("button", { name: "Add mapping" }).click();
    await panel.getByRole("textbox", { name: "Group", exact: true }).last().fill(group);
    await panel.getByRole("combobox", { name: "Role", exact: true }).last().selectOption(role);
  }
  await panel.getByLabel("Enable single sign-on").check();
  await panel.getByRole("button", { name: "Save" }).click();
  await expect(panel.getByText("Saved")).toBeVisible();

  const context = await browser.newContext({ baseURL: server.baseURL, storageState: { cookies: [], origins: [] } });
  try {
    // A failed sign-in comes back with a code, explained and removed from the address bar.
    const visitor = await context.newPage();
    await visitor.goto("/?oidc_error=no_role");
    const login = visitor.getByRole("region", { name: "Sign in" });
    await expect(login.getByText("Your account has no access to MongoRescue.", { exact: false })).toBeVisible();
    await expect(visitor).not.toHaveURL(/oidc_error/);

    // bob signs in with the button, through Keycloak's login form.
    await login.getByRole("link", { name: "Sign in with Keycloak" }).click();
    await visitor.locator("#username").fill("bob");
    await visitor.locator("#password").fill("bob-it-password-1");
    await visitor.locator("#kc-login").click();
    await expect(visitor.getByRole("button", { name: "bob" })).toBeVisible();
    const me = await visitor.evaluate(async () => (await (await fetch("/api/v1/auth/me")).json()).data);
    expect({ role: me.role, provider: me.user.auth_provider }).toEqual({ role: "operator", provider: "oidc" });
  } finally {
    await context.close();
  }

  // The users table marks bob, whose role follows the group mappings.
  await page.getByRole("button", { name: "Users", exact: true }).click();
  const row = page.getByRole("row").filter({ hasText: "bob" });
  await expect(row.getByText("SSO", { exact: true })).toBeVisible();
  const role = row.getByRole("combobox", { name: "Role of bob" });
  await expect(role).toHaveValue("operator");
  await expect(role).toBeDisabled();
  await expect(role).toHaveAttribute("title", "Managed by your identity provider");
  await expect(row.getByRole("button", { name: "Change password" })).toHaveCount(0);

  // Single sign-on is turned off again, so the sign-in page is as before.
  await page.getByRole("button", { name: "Single sign-on", exact: true }).click();
  await panel.getByLabel("Enable single sign-on").uncheck();
  await panel.getByRole("button", { name: "Save" }).click();
  await expect(panel.getByText("Saved")).toBeVisible();
});
