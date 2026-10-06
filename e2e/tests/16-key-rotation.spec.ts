// Spec 16: Settings → Security → Key rotation. The section shows the secret.key
// fingerprint, and rotating the S3 credentials of the MinIO target to a key that
// does not exist fails its write probe and leaves the target working. secret.key
// itself is not rotated here: it would sign the shared session out (the unit and
// integration suites cover it).
import { TARGET_NAME, expect, test } from "./fixtures.js";

test("shows the key rotation section and refuses credentials that fail a probe", async ({ page }) => {
  await page.goto("/");
  await page.getByRole("tab", { name: "Settings" }).click();
  await page.getByRole("button", { name: "Security", exact: true }).click();
  const section = page.locator("#keyrot-section");
  await expect(section.getByRole("heading", { name: "Key rotation" })).toBeVisible();
  await expect(section.getByText(/Current fingerprint: [0-9a-f]{32}/)).toBeVisible();
  await expect(section.getByRole("button", { name: "Rotate secret.key" })).toBeEnabled();

  await section.getByLabel("Storage target").selectOption({ label: TARGET_NAME });
  await section.getByLabel("Access key ID").fill("e2e-no-such-key");
  await section.getByLabel("Secret access key").fill("e2e-wrong-secret-value");
  await section.getByRole("button", { name: "Test and rotate" }).click();
  const confirm = page.getByRole("alertdialog", { name: `Rotate the credentials of ${TARGET_NAME}?` });
  await confirm.getByRole("button", { name: "Test and rotate" }).click();
  await expect(page.getByText(/failed the write probe; nothing was changed/)).toBeVisible();
  await expect(page.getByText("e2e-wrong-secret-value")).toHaveCount(0);

  // The target still works with its old credentials.
  const targets = await page.evaluate(async () => (await (await fetch("/api/v1/storage-targets")).json()).data);
  const target = targets.find((x: { name: string }) => x.name === TARGET_NAME);
  expect(target.s3.access_key_id).not.toBe("e2e-no-such-key");
});
