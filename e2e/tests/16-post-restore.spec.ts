// Spec 16: an administrator edits the post-restore commands of a connection: the
// editor refuses invalid JSON and commands that are not allowed, saves valid ones,
// and an empty editor removes them again.
import { CONNECTION_NAME, expect, test } from "./fixtures.js";

const ERASURE = [{ database: "*", command: { delete: "users", deletes: [{ q: { _id: { $in: [42] } }, limit: 0 }] } }];

test("edits the post-restore commands of a connection", async ({ page }) => {
  await page.goto("/");
  await page.getByRole("tab", { name: /^Connections/ }).click();
  const row = page.getByRole("tabpanel", { name: "Connections" }).getByRole("row").filter({ hasText: CONNECTION_NAME });
  await row.getByRole("button", { name: "Edit" }).click();

  const dialog = page.getByRole("dialog", { name: "Edit connection" });
  await expect(dialog).toBeVisible();
  const editor = dialog.getByRole("textbox", { name: "Post-restore commands" });
  await expect(editor).toHaveValue("");

  await editor.fill("[{");
  await expect(dialog.getByText(/^Not valid JSON/)).toBeVisible();
  await editor.fill(JSON.stringify([{ database: "*", command: { applyOps: [] } }]));
  await expect(dialog.getByText("Item 1: applyOps is not allowed.", { exact: false })).toBeVisible();

  await editor.fill(JSON.stringify(ERASURE, null, 2));
  await expect(dialog.getByText(/is not allowed|Not valid JSON/)).toBeHidden();
  await dialog.getByRole("button", { name: "Save" }).click();
  await expect(dialog).toBeHidden({ timeout: 30_000 });

  const stored = async () => page.evaluate(async (name) => {
    const list = (await (await fetch("/api/v1/connections")).json()).data as Array<Record<string, unknown>>;
    return list.find(c => c.name === name)?.post_restore_commands;
  }, CONNECTION_NAME);
  expect(await stored()).toEqual(ERASURE);

  // The editor shows them again; emptying it removes them.
  await row.getByRole("button", { name: "Edit" }).click();
  await expect(dialog).toBeVisible();
  await expect(editor).toHaveValue(JSON.stringify(ERASURE, null, 2));
  await editor.fill("");
  await dialog.getByRole("button", { name: "Save" }).click();
  await expect(dialog).toBeHidden({ timeout: 30_000 });
  expect(await stored()).toBeUndefined();
});
