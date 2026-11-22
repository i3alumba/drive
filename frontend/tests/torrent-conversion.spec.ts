import { expect, test } from "@playwright/test";

test("torrent conversion is off by default and captured when enqueued", async ({
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
    (window as any).conversionRequests = requests;
    class XHR {
      upload = {};
      status = 0;
      onload?: () => void;
      onabort?: () => void;
      open() {}
      setRequestHeader() {}
      send(data: FormData) {
        requests.push({
          xhr: this,
          flag: data.getAll("convertVideos"),
          name: (data.get("torrent") as File).name,
        });
      }
      abort() {
        this.onabort?.();
      }
    }
    (window as any).XMLHttpRequest = XHR;
  });
  await page.goto("/");
  const checkbox = page.getByRole("checkbox", {
    name: "Convert downloaded videos for browser playback — keep originals",
  });
  await expect(checkbox).not.toBeChecked();
  await checkbox.check();
  await page
    .getByText("Upload file", { exact: true })
    .locator("input")
    .setInputFiles(
      ["first.torrent", "second.torrent"].map((name) => ({
        name,
        mimeType: "application/x-bittorrent",
        buffer: Buffer.from("synthetic metadata"),
      })),
    );
  await expect
    .poll(() => page.evaluate(() => (window as any).conversionRequests.length))
    .toBe(2);
  // A dropped torrent waits behind the two active metadata uploads.
  const transfer = await page.evaluateHandle(() => {
    const data = new DataTransfer();
    data.items.add(
      new File(["synthetic metadata"], "queued.torrent", {
        type: "application/x-bittorrent",
      }),
    );
    return data;
  });
  await page
    .getByText("Drag files here to upload", { exact: false })
    .dispatchEvent("drop", { dataTransfer: transfer });
  await checkbox.uncheck();
  await page.evaluate(() => {
    const request = (window as any).conversionRequests[0];
    request.xhr.status = 202;
    request.xhr.onload();
  });
  await expect
    .poll(() => page.evaluate(() => (window as any).conversionRequests.length))
    .toBe(3);
  expect(
    await page.evaluate(() =>
      (window as any).conversionRequests.map((request: any) => request.flag),
    ),
  ).toEqual([["true"], ["true"], ["true"]]);
  await page.evaluate(() => {
    for (const request of (window as any).conversionRequests.slice(1)) {
      request.xhr.status = 202;
      request.xhr.onload();
    }
  });
  await page
    .getByText("Upload torrent", { exact: true })
    .locator("input")
    .setInputFiles({
      name: "off.torrent",
      mimeType: "application/x-bittorrent",
      buffer: Buffer.from("synthetic metadata"),
    });
  await expect
    .poll(() => page.evaluate(() => (window as any).conversionRequests.length))
    .toBe(4);
  expect(
    await page.evaluate(() => (window as any).conversionRequests[3].flag),
  ).toEqual(["false"]);
});

test("per-file mixed failures and cancellation preserve visible successful derivatives", async ({
  page,
}) => {
  await page.route("**/api/**", (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/spaces")
      return route.fulfill({
        json: [{ id: "personal", name: "My files", permission: "edit" }],
      });
    if (path === "/api/torrents")
      return route.fulfill({
        json: [
          {
            id: "mixed",
            name: "mixed.torrent",
            status: "complete",
            progress: 1,
            convertVideos: true,
            conversionStatus: "partial",
            conversionResults: [
              { name: "movie.browser-job-0.mp4", status: "complete" },
              { name: "hdr.mkv", status: "failed", error: "unsupported_hdr" },
              { name: "notes.txt", status: "skipped" },
            ],
          },
          {
            id: "cancelled",
            name: "cancelled.torrent",
            status: "cancelled",
            progress: 1,
            convertVideos: true,
            conversionStatus: "cancelled",
            conversionResults: [
              { name: "saved.browser-job-0.mp4", status: "complete" },
            ],
          },
        ],
      });
    return route.fulfill({ json: [] });
  });
  await page.goto("/");
  const mixed = page.getByRole("article", {
    name: "Torrent mixed.torrent",
    exact: true,
  });
  await expect(mixed).toContainText("Originals kept · Conversion: partial");
  await expect(mixed).toContainText("movie.browser-job-0.mp4: complete");
  await expect(mixed).toContainText("hdr.mkv: failed · unsupported_hdr");
  await expect(mixed).toContainText("notes.txt: skipped");
  const cancelled = page.getByRole("article", {
    name: "Torrent cancelled.torrent",
    exact: true,
  });
  await expect(cancelled).toContainText("Conversion: cancelled");
  await expect(cancelled).toContainText("saved.browser-job-0.mp4: complete");
  await expect(
    cancelled.getByRole("button", { name: "Cancel", exact: true }),
  ).toHaveCount(0);
});
