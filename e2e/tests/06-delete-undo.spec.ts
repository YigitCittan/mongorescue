// Spec 6b: deletes are soft. The backups spec 6 bulk-deleted wait in the "Deleted"
// view; one is undone, deleted again from its row (the confirmation explains the
// grace period instead of "cannot be undone") and undone once more, then deleted
// for the specs that follow.
import { type Locator, type Page } from "@playwright/test";
import { expect, test } from "./fixtures.js";

async function rowAction(page: Page, row: Locator, action: string): Promise<void> {
  await row.getByRole("button", { name: "More actions" }).click();
  await page.getByRole("menuitem", { name: action, exact: true }).click();
}

test("deletes a backup softly and undoes it from the Deleted view", async ({ page, services }) => {
  await page.goto("/");
  await page.getByRole("tab", { name: /^Backups/ }).click();
  const backups = page.getByRole("tabpanel", { name: "Backups" });
  const rows = backups.getByRole("row").filter({ hasText: services.database });

  // Spec 6 deleted both backups: hidden from the list, waiting in the Deleted view.
  await expect(rows).toHaveCount(0);
  const deletedView = backups.getByRole("button", { name: "Deleted", exact: true });
  await deletedView.click();
  await expect(deletedView).toHaveAttribute("aria-pressed", "true");
  await expect(rows).toHaveCount(2);
  await expect(rows.first()).toContainText("Deleted");

  // Undo restores the backup to its earlier state.
  const undo = page.waitForResponse(res => res.request().method() === "POST" && /\/api\/v1\/backups\/[^/]+\/undelete$/.test(new URL(res.url()).pathname));
  await rowAction(page, rows.first(), "Undo delete");
  const confirm = page.getByRole("alertdialog", { name: "Undo delete" });
  await confirm.getByRole("button", { name: "Undo delete" }).click();
  expect((await undo).status()).toBe(200);
  await expect(rows).toHaveCount(1);
  await deletedView.click();
  await expect(deletedView).toHaveAttribute("aria-pressed", "false");
  await expect(rows).toHaveCount(1);
  await expect(rows).toContainText("Succeeded");

  // Deleting it again says until when it can be undone.
  const del = page.waitForResponse(res => res.request().method() === "DELETE" && new URL(res.url()).pathname.startsWith("/api/v1/backups/"));
  await rowAction(page, rows.first(), "Delete");
  const dialog = page.getByRole("alertdialog", { name: "Delete backup?" });
  await expect(dialog).toContainText("you can undo the deletion until then");
  await expect(dialog).toContainText("Recoverable until");
  await expect(dialog).not.toContainText("This cannot be undone");
  await dialog.getByRole("button", { name: "Delete" }).click();
  const answer = await del;
  expect(answer.status()).toBe(200);
  const body = await answer.json();
  expect(body.data.status).toBe("deleted");
  expect(body.data.purge_after).toBeTruthy();
  await expect(rows).toHaveCount(0);

  // ... and is undone once more from the Deleted view, then deleted for good measure.
  await deletedView.click();
  await expect(rows).toHaveCount(2);
  await rowAction(page, rows.first(), "Undo delete");
  await page.getByRole("alertdialog", { name: "Undo delete" }).getByRole("button", { name: "Undo delete" }).click();
  await expect(rows).toHaveCount(1);
  await deletedView.click();
  await expect(rows).toHaveCount(1);
  await rowAction(page, rows.first(), "Delete");
  await page.getByRole("alertdialog", { name: "Delete backup?" }).getByRole("button", { name: "Delete" }).click();
  await expect(rows).toHaveCount(0);
});
