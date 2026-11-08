// Spec 22: notification channels. The administrator creates a webhook channel that
// points at a receiver started by the test, sends a test notification (the receiver
// sees the request), edits the channel and deletes it after confirming.
import { createServer, type IncomingMessage } from "node:http";
import type { AddressInfo } from "node:net";
import { expect, test } from "./fixtures.js";

const CHANNEL = "e2e-webhook";
const RENAMED = "e2e-webhook-renamed";

test("creates, tests, edits and deletes a webhook channel", async ({ page }) => {
  // The receiver of the test notification: an HTTP server on a random loopback port.
  const received: IncomingMessage[] = [];
  const receiver = createServer((req, res) => {
    received.push(req);
    req.resume();
    res.writeHead(204).end();
  });
  await new Promise<void>(resolve => receiver.listen(0, "127.0.0.1", resolve));
  const url = `http://127.0.0.1:${(receiver.address() as AddressInfo).port}/hook`;

  try {
    await page.goto("/");
    await page.getByRole("tab", { name: /^Notifications/ }).click();
    const panel = page.getByRole("tabpanel", { name: "Notifications" });
    const messages = page.getByRole("region", { name: "Messages" });

    // Create.
    await panel.getByRole("button", { name: "Add channel" }).first().click();
    const dialog = page.getByRole("dialog", { name: "New channel" });
    await expect(dialog.getByRole("combobox", { name: "Type" })).toHaveValue("webhook");
    await dialog.getByLabel("Name", { exact: true }).fill(CHANNEL);
    await dialog.getByLabel("Endpoint URL").fill(url);
    await dialog.getByRole("button", { name: "Save" }).click();
    await expect(dialog).toBeHidden();
    await expect(messages.getByText("Channel saved", { exact: true })).toBeVisible();
    const row = panel.getByRole("row").filter({ hasText: CHANNEL });
    await expect(row).toHaveCount(1);
    await expect(row.getByRole("cell", { name: "Webhook", exact: true })).toBeVisible();
    await expect(row.getByText("Enabled", { exact: true })).toBeVisible();
    await expect(row.getByText("Never", { exact: true })).toBeVisible();

    // Test: the receiver gets one POST and the row shows the delivery.
    await row.getByRole("button", { name: "Send test" }).click();
    await expect(messages.getByText("Test notification delivered", { exact: true })).toBeVisible();
    expect(received).toHaveLength(1);
    expect(received[0].method).toBe("POST");
    expect(received[0].url).toBe("/hook");
    // The cell reads "Delivered · <when>", so it is matched by its class and a prefix.
    await expect(row.locator(".delivery")).toHaveText(/^Delivered/);

    // Edit: rename, and the dialog shows the saved values.
    await row.getByRole("button", { name: "Edit" }).click();
    const edit = page.getByRole("dialog", { name: /channel/i });
    await expect(edit.getByLabel("Name", { exact: true })).toHaveValue(CHANNEL);
    // The path of a webhook URL can carry a token, so it comes back masked.
    await expect(edit.getByLabel("Endpoint URL")).toHaveValue(`${new URL(url).origin}/******`);
    await edit.getByLabel("Name", { exact: true }).fill(RENAMED);
    await edit.getByRole("button", { name: "Save" }).click();
    await expect(edit).toBeHidden();
    await expect(panel.getByRole("row").filter({ hasText: RENAMED })).toHaveCount(1);
    await expect(panel.getByRole("row").filter({ hasText: CHANNEL, hasNotText: RENAMED })).toHaveCount(0);

    // Delete asks first; cancelling keeps the channel.
    const renamed = panel.getByRole("row").filter({ hasText: RENAMED });
    await renamed.getByRole("button", { name: "Delete" }).click();
    const confirm = page.getByRole("alertdialog", { name: "Delete" });
    await expect(confirm.getByText("Delete this channel? It will also be removed from every rule that uses it.", { exact: true })).toBeVisible();
    await confirm.getByRole("button", { name: "Cancel" }).click();
    await expect(confirm).toBeHidden();
    await expect(renamed).toHaveCount(1);

    await renamed.getByRole("button", { name: "Delete" }).click();
    await confirm.getByRole("button", { name: "Delete", exact: true }).click();
    await expect(messages.getByText("Channel deleted", { exact: true })).toBeVisible();
    await expect(panel.getByRole("row").filter({ hasText: RENAMED })).toHaveCount(0);
    await expect(panel.getByText("No channels yet. Add a webhook, Telegram, e-mail or SMS destination.", { exact: true })).toBeVisible();
  } finally {
    await new Promise<void>(resolve => receiver.close(() => resolve()));
  }
});
