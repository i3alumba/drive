import { expect, test } from "@playwright/test";

async function mockDrive(page: import("@playwright/test").Page) {
  await page.context().addCookies([
    {
      name: "access_token",
      value: "fixture-access",
      url: "http://127.0.0.1:4174",
    },
  ]);
  await page.route("**/api/**", async (route) => {
    const request = route.request(),
      url = new URL(request.url());
    if (url.pathname === "/api/spaces")
      return route.fulfill({
        json: [{ id: "personal", name: "My files", permission: "edit" }],
      });
    if (url.pathname === "/api/files")
      return route.fulfill({
        json: [
          {
            name: "fixture.txt",
            path: "fixture.txt",
            size: 12,
            isDir: false,
            lastModified: "2026-01-01",
          },
        ],
      });
    return route.fulfill({ json: [] });
  });
}

test("legacy torrent metadata shows sent progress and waits for server acknowledgement", async ({
  page,
}) => {
  await mockDrive(page);
  await page.addInitScript(() => {
    const pending: any[] = [];
    (window as any).fixtureUploads = pending;
    class FixtureXHR {
      upload: any = {};
      status = 0;
      onload: any;
      onerror: any;
      onabort: any;
      headers: Record<string, string> = {};
      target = "";
      data: any;
      open(_method: string, url: string) {
        this.target = url;
      }
      setRequestHeader(key: string, value: string) {
        this.headers[key] = value;
      }
      send(data: FormData) {
        this.data = data;
        pending.push(this);
      }
      abort() {
        this.onabort?.();
      }
    }
    (window as any).XMLHttpRequest = FixtureXHR;
  });
  await page.goto("/");
  await page
    .getByText("Upload file", { exact: true })
    .locator("input")
    .setInputFiles({
      name: "sample.torrent",
      mimeType: "text/plain",
      buffer: Buffer.from("fixture"),
    });
  await page.setViewportSize({ width: 360, height: 760 });
  const widget = page.getByRole("region", { name: "Transfers" });
  await expect(widget).toContainText("sample.torrent");
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth),
  ).toBeLessThanOrEqual(360);
  expect(
    await page.evaluate(() => {
      const data = (window as any).fixtureUploads[0].data;
      return { path: data.get("path"), space: data.get("space") };
    }),
  ).toEqual({ path: "", space: "personal" });
  expect(
    await page.evaluate(
      () => (window as any).fixtureUploads[0].headers.Authorization,
    ),
  ).toBe("Bearer fixture-access");
  await page.evaluate(() =>
    (window as any).fixtureUploads[0].upload.onprogress({
      lengthComputable: true,
      total: 100,
      loaded: 25,
    }),
  );
  await expect(widget).toContainText("25%");
  await page.evaluate(() => (window as any).fixtureUploads[0].upload.onload());
  await expect(widget).toContainText("saving on Drive");
  await page.evaluate(() => {
    const xhr = (window as any).fixtureUploads[0];
    xhr.status = 201;
    xhr.onload();
  });
  await expect(widget).toContainText("Torrent added");
  await page.getByRole("button", { name: "Clear finished transfers" }).click();
  await expect(widget).toHaveCount(0);
});

test("failed legacy metadata uploads are not reported as success and cancellation aborts the request", async ({
  page,
}) => {
  await mockDrive(page);
  await page.addInitScript(() => {
    const pending: any[] = [];
    (window as any).fixtureUploads = pending;
    class FixtureXHR {
      upload: any = {};
      status = 0;
      onload: any;
      onabort: any;
      open() {}
      setRequestHeader() {}
      send() {
        pending.push(this);
      }
      abort() {
        this.onabort?.();
      }
    }
    (window as any).XMLHttpRequest = FixtureXHR;
  });
  await page.goto("/");
  const picker = page
    .getByText("Upload file", { exact: true })
    .locator("input");
  await picker.setInputFiles([
    {
      name: "failed.torrent",
      mimeType: "text/plain",
      buffer: Buffer.from("a"),
    },
    {
      name: "cancel.torrent",
      mimeType: "text/plain",
      buffer: Buffer.from("b"),
    },
  ]);
  await expect(
    page.getByRole("button", { name: "Cancel cancel.torrent", exact: true }),
  ).toBeVisible();
  await page.evaluate(() => {
    const xhr = (window as any).fixtureUploads[0];
    xhr.status = 500;
    xhr.onload();
  });
  await expect(page.getByRole("region", { name: "Transfers" })).toContainText(
    "Upload failed (HTTP 500)",
  );
  await page
    .getByRole("button", { name: "Cancel cancel.torrent", exact: true })
    .click();
  await expect(page.getByRole("region", { name: "Transfers" })).toContainText(
    "cancelled",
  );
});

test("URL import carries destination/bearer and displays server-side download progress", async ({
  page,
}) => {
  await mockDrive(page);
  let started = false;
  await page.route("**/api/imports", async (route) => {
    if (route.request().method() === "POST") {
      expect(route.request().headers().authorization).toBe(
        "Bearer fixture-access",
      );
      expect(route.request().postDataJSON()).toEqual({
        url: "https://example.com/test.txt",
        path: "",
        space: "personal",
      });
      started = true;
      return route.fulfill({ status: 202, json: { id: "import-1" } });
    }
    return route.fulfill({
      json: started
        ? [
            {
              id: "import-1",
              name: "test.txt",
              path: "",
              status: "downloading",
              bytes: 50,
              total: 100,
            },
          ]
        : [],
    });
  });
  await page.goto("/");
  await page
    .getByLabel("External file URL")
    .fill("https://example.com/test.txt");
  await page
    .getByRole("button", { name: "Download to Drive", exact: true })
    .click();
  await expect(page.getByRole("region", { name: "Transfers" })).toContainText(
    "50%",
  );
});

test("owner can create, copy and revoke a public snapshot link", async ({
  page,
}) => {
  await mockDrive(page);
  await page.route("**/api/public-links**", async (route) => {
    if (route.request().method() === "POST") {
      expect(route.request().postDataJSON()).toEqual({
        path: "fixture.txt",
        days: 7,
      });
      return route.fulfill({
        status: 201,
        json: {
          link: { id: "link-1", path: "fixture.txt", expiresAt: "2027-01-01" },
          downloadUrl: `/public/42/${"a".repeat(64)}#${"b".repeat(64)}`,
        },
      });
    }
    if (route.request().method() === "DELETE") {
      expect(route.request().headers().authorization).toBe(
        "Bearer fixture-access",
      );
      return route.fulfill({ status: 204 });
    }
    return route.fulfill({ json: [] });
  });
  await page.goto("/");
  await page
    .getByRole("button", { name: "Public link fixture.txt", exact: true })
    .click();
  await expect(page.getByRole("dialog")).toContainText("Anyone with the link");
  await page
    .getByRole("button", { name: "Create public link", exact: true })
    .click();
  await expect(page.getByLabel("Public download URL")).toHaveValue(
    `http://127.0.0.1:4174/public/42/${"a".repeat(64)}#${"b".repeat(64)}`,
  );
  await page.getByRole("button", { name: "Revoke link", exact: true }).click();
  await expect(page.getByLabel("Public download URL")).toHaveCount(0);
});
