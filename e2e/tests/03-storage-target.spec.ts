// Spec 3: add the MinIO bucket as an S3 storage target, test it (write, read,
// delete) and make it the default, so the backup spec writes to MinIO.
import { TARGET_NAME, expect, test } from "./fixtures.js";

test("adds an S3 storage target and makes it the default", async ({ page, services }) => {
  await page.goto("/");
  await page.getByRole("tab", { name: "Settings" }).click();
  const settings = page.getByRole("tabpanel", { name: "Settings" });
  await settings.getByRole("navigation", { name: "Settings" }).getByRole("button", { name: "Storage" }).click();
  const targets = settings.getByRole("region", { name: "Storage targets" });
  await targets.getByRole("button", { name: "Add target" }).click();

  const dialog = page.getByRole("dialog", { name: "New storage target" });
  await expect(dialog).toBeVisible();
  await dialog.getByRole("textbox", { name: "Name" }).fill(TARGET_NAME);
  await dialog.getByRole("combobox", { name: "Provider" }).selectOption({ label: "MinIO" });
  await dialog.getByRole("textbox", { name: "Endpoint" }).fill(services.s3Endpoint);
  await dialog.getByRole("textbox", { name: "Bucket" }).fill(services.s3Bucket);
  await dialog.getByRole("textbox", { name: "Access key ID" }).fill(services.s3AccessKey);
  await dialog.getByRole("textbox", { name: "Secret access key" }).fill(services.s3SecretKey);
  await dialog.getByRole("checkbox", { name: "Use path-style URLs" }).check();
  await dialog.getByRole("checkbox", { name: "Use as default for new backups" }).check();

  await dialog.getByRole("button", { name: "Test storage" }).click();
  await expect(dialog.getByText(/Write, read and delete succeeded/)).toBeVisible({ timeout: 30_000 });
  await dialog.getByRole("button", { name: "Save" }).click();
  await expect(dialog).toBeHidden();

  const row = targets.getByRole("row").filter({ hasText: TARGET_NAME });
  await expect(row).toBeVisible();
  await expect(row.getByRole("cell").first()).toContainText("Default");
  await expect(row).toContainText(services.s3Bucket);
});
