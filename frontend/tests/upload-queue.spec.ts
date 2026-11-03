import { expect, test } from "@playwright/test";

test("only two computer uploads run concurrently; cancelling queued work sends no request", async ({
  page,
}) => {
  await page.route("**/api/**", (route) =>
    new URL(route.request().url()).pathname === "/api/spaces"
      ? route.fulfill({
          json: [{ id: "personal", name: "My files", permission: "edit" }],
        })
      : route.fulfill({ json: [] }),
  );
  await page.addInitScript(() => {
    const requests: any[] = [];
    (window as any).uploads = requests;
    class XHR {
      upload: any = {};
      status = 0;
      onload: any;
      onabort: any;
      open() {}
      setRequestHeader() {}
      send() {
        requests.push(this);
      }
      abort() {
        this.onabort?.();
      }
    }
    (window as any).XMLHttpRequest = XHR;
  });
  await page.goto("/");
  await page
    .getByText("Upload file", { exact: true })
    .locator("input")
    .setInputFiles(
      ["first.txt", "second.txt", "queued.txt"].map((name) => ({
        name,
        mimeType: "text/plain",
        buffer: Buffer.from("owned fixture"),
      })),
    );
  await expect
    .poll(() => page.evaluate(() => (window as any).uploads.length))
    .toBe(2);
  await expect(page.getByRole("region", { name: "Transfers" })).toContainText(
    "queued",
  );
  await page
    .getByRole("button", { name: "Cancel queued.txt", exact: true })
    .click();
  await page.evaluate(() => {
    for (const xhr of (window as any).uploads) {
      xhr.status = 201;
      xhr.onload();
    }
  });
  await expect(page.getByRole("region", { name: "Transfers" })).toContainText(
    "cancelled",
  );
  expect(await page.evaluate(() => (window as any).uploads.length)).toBe(2);
});

test("same-destination uploads are serialized in selection order", async ({
  page,
}) => {
  await page.route("**/api/**", (route) =>
    new URL(route.request().url()).pathname === "/api/spaces"
      ? route.fulfill({
          json: [{ id: "personal", name: "My files", permission: "edit" }],
        })
      : route.fulfill({ json: [] }),
  );
  await page.addInitScript(() => {
    const requests: any[] = [];
    (window as any).uploads = requests;
    class XHR {
      upload: any = {};
      status = 0;
      onload: any;
      onabort: any;
      open() {}
      setRequestHeader() {}
      send() {
        requests.push(this);
      }
      abort() {
        this.onabort?.();
      }
    }
    (window as any).XMLHttpRequest = XHR;
  });
  await page.goto("/");
  await page
    .getByText("Upload file", { exact: true })
    .locator("input")
    .setInputFiles(
      ["first payload", "second payload"].map((text) => ({
        name: "same.txt",
        mimeType: "text/plain",
        buffer: Buffer.from(text),
      })),
    );
  await expect
    .poll(() => page.evaluate(() => (window as any).uploads.length))
    .toBe(1);
  await page.evaluate(() => {
    const xhr = (window as any).uploads[0];
    xhr.status = 201;
    xhr.onload();
  });
  await expect
    .poll(() => page.evaluate(() => (window as any).uploads.length))
    .toBe(2);
  await page.evaluate(() => {
    const xhr = (window as any).uploads[1];
    xhr.status = 201;
    xhr.onload();
  });
  await expect(page.getByRole("region", { name: "Transfers" })).toContainText(
    "0 active",
  );
});
