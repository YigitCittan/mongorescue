// Spec 23: users and roles. The administrator adds an operator, promotes the user to
// administrator, demotes the user again after confirming (the demotion asks first,
// the promotion does not), and deletes the user after confirming. The last
// administrator and the signed-in user cannot be changed or deleted.
import { randomBytes } from "node:crypto";
import { ADMIN_USERNAME, expect, test } from "./fixtures.js";

const USER = "e2e-operator";

test("adds a user, changes the role up and down, and deletes the user", async ({ page }) => {
  const password = `e2e-${randomBytes(12).toString("hex")}`;
  await page.goto("/");
  await page.getByRole("tab", { name: "Settings" }).click();
  await page.getByRole("button", { name: "Users", exact: true }).click();
  const messages = page.getByRole("region", { name: "Messages" });

  // The signed-in administrator is the only administrator: no role change, no delete.
  const self = page.getByRole("row").filter({ hasText: ADMIN_USERNAME });
  await expect(self.getByRole("combobox", { name: `Role of ${ADMIN_USERNAME}` })).toBeDisabled();
  await expect(self.getByRole("button", { name: "Delete" })).toBeDisabled();
  await expect(self.getByText("You", { exact: true })).toBeVisible();

  // Add an operator.
  await page.getByRole("button", { name: "Add user" }).first().click();
  const dialog = page.getByRole("dialog", { name: "Add user" });
  await dialog.getByLabel("Username").fill(USER);
  await dialog.getByRole("combobox", { name: "Role" }).selectOption("operator");
  await dialog.getByLabel("Password", { exact: true }).fill(password);
  await dialog.getByLabel("Confirm password").fill(password);
  await dialog.getByRole("button", { name: "Add user" }).click();
  await expect(dialog).toBeHidden();
  const row = page.getByRole("row").filter({ hasText: USER });
  const role = row.getByRole("combobox", { name: `Role of ${USER}` });
  await expect(role).toHaveValue("operator");

  // Promote: no confirmation.
  await role.selectOption("admin");
  await expect(messages.getByText(`${USER} is now Admin`, { exact: true })).toBeVisible();
  await expect(role).toHaveValue("admin");

  // Demote: asks first. Cancelling keeps the role.
  await role.selectOption("viewer");
  const confirm = page.getByRole("alertdialog", { name: "Change role" });
  await expect(confirm).toBeVisible();
  await confirm.getByRole("button", { name: "Cancel" }).click();
  await expect(confirm).toBeHidden();
  await expect(role).toHaveValue("admin");

  await role.selectOption("viewer");
  await confirm.getByRole("button", { name: "Confirm" }).click();
  await expect(messages.getByText(`${USER} is now Viewer`, { exact: true })).toBeVisible();
  await expect(role).toHaveValue("viewer");

  // Delete asks first.
  await row.getByRole("button", { name: "Delete" }).click();
  const del = page.getByRole("alertdialog", { name: "Delete" });
  await expect(del.getByText(`Delete user ${USER}? Their sessions end immediately.`, { exact: true })).toBeVisible();
  await del.getByRole("button", { name: "Delete", exact: true }).click();
  await expect(messages.getByText("User deleted", { exact: true })).toBeVisible();
  await expect(page.getByRole("row").filter({ hasText: USER })).toHaveCount(0);
});
