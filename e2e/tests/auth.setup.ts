// Spec 1: a fresh server is in setup mode; the setup code from its log creates the
// first administrator, who then signs in. The session is saved for the other specs.
import { randomBytes } from "node:crypto";
import { ADMIN_STATE_FILE } from "../paths.js";
import { ADMIN_USERNAME, expect, test } from "./fixtures.js";

test("setup mode creates the first admin, who signs in", async ({ page, server }) => {
  // Generated per run: nothing but this test ever needs it.
  const password = `e2e-${randomBytes(12).toString("hex")}`;

  await page.goto("/");
  const setup = page.getByRole("region", { name: "Set up mongorescue" });
  await expect(setup).toBeVisible();
  await setup.getByLabel("Setup code").fill(server.setupCode);
  await setup.getByLabel("Username").fill(ADMIN_USERNAME);
  await setup.getByLabel("Password", { exact: true }).fill(password);
  await setup.getByLabel("Confirm password").fill(password);
  await setup.getByRole("button", { name: "Create account" }).click();

  // Setup signs the new administrator in; after logging out, the server is no
  // longer in setup mode and asks for a sign-in instead.
  const userMenu = page.getByRole("button", { name: ADMIN_USERNAME });
  await expect(userMenu).toBeVisible();
  await expect(setup).toBeHidden();
  await userMenu.click();
  await page.getByRole("menuitem", { name: "Log out" }).click();

  const login = page.getByRole("region", { name: "Sign in" });
  await expect(login).toBeVisible();
  await expect(setup).toBeHidden();
  await login.getByLabel("Username").fill(ADMIN_USERNAME);
  await login.getByLabel("Password").fill(password);
  await login.getByRole("button", { name: "Sign in" }).click();

  await expect(login).toBeHidden();
  await expect(userMenu).toBeVisible();
  await expect(page.getByRole("tablist", { name: "Sections" })).toBeVisible();
  await page.context().storageState({ path: ADMIN_STATE_FILE });
});
