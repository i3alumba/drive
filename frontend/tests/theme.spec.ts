import { expect, test, type Page } from "@playwright/test";

async function expectTheme(page: Page, mode: "light" | "dark") {
  await expect(page.locator("html")).toHaveClass(new RegExp(`\\b${mode}\\b`));
  await expect(page.locator("body")).toHaveCSS(
    "background-color",
    mode === "dark" ? "rgb(11, 18, 32)" : "rgb(244, 247, 251)",
  );
  await expect(
    page.getByRole("button", {
      name: `Switch to ${mode === "dark" ? "light" : "dark"} theme`,
    }),
  ).toBeEnabled();
}

test.beforeEach(async ({ context }) => {
  // All Drive requests stay local to these fixtures; no files are uploaded or changed.
  await context.route("**/api/**", (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/spaces") {
      return route.fulfill({
        json: [{ id: "personal", name: "My files", permission: "edit" }],
      });
    }
    if (path === "/api/files") {
      return route.fulfill({
        json: [
          {
            name: "notes.txt",
            path: "notes.txt",
            size: 12,
            isDir: false,
            lastModified: "2026-01-01T00:00:00Z",
          },
        ],
      });
    }
    if (path === "/api/view") {
      return route.fulfill({
        contentType: "text/plain",
        body: "Saved content",
      });
    }
    return route.fulfill({ json: [] });
  });
});

for (const mode of ["light", "dark"] as const) {
  test(`initially follows the ${mode} system theme`, async ({ page }) => {
    await page.emulateMedia({ colorScheme: mode });
    await page.goto("/");
    await expectTheme(page, mode);
  });
}

test("switches both ways, preserves form input and persists after reload", async ({
  page,
}) => {
  await page.goto("/");
  await page.getByLabel("New directory").fill("keep-me");
  await page.getByRole("button", { name: "Switch to dark theme" }).click();
  await expectTheme(page, "dark");
  await expect(page.getByLabel("New directory")).toHaveValue("keep-me");
  await expect
    .poll(() =>
      page.evaluate(() => localStorage.getItem("i3alumba-color-mode")),
    )
    .toBe("dark");
  await page.reload();
  await expectTheme(page, "dark");
  await page.getByRole("button", { name: "Switch to light theme" }).click();
  await page.reload();
  await expectTheme(page, "light");
});

test("follows system changes until an explicit choice overrides them", async ({
  page,
}) => {
  await page.goto("/");
  await expectTheme(page, "light");
  await page.emulateMedia({ colorScheme: "dark" });
  await expectTheme(page, "dark");
  await page.getByRole("button", { name: "Switch to light theme" }).click();
  await page.emulateMedia({ colorScheme: "light" });
  await page.emulateMedia({ colorScheme: "dark" });
  await expectTheme(page, "light");
});

test("synchronizes the saved choice between tabs", async ({
  page,
  context,
}) => {
  await page.goto("/");
  const second = await context.newPage();
  await second.goto("/");
  await expectTheme(second, "light");
  await page.getByRole("button", { name: "Switch to dark theme" }).click();
  await expectTheme(second, "dark");
});

test("still switches when browser storage is unavailable", async ({ page }) => {
  await page.addInitScript(() => {
    Object.defineProperty(document, "cookie", {
      get() {
        throw new DOMException("Cookies disabled", "SecurityError");
      },
      set() {
        throw new DOMException("Cookies disabled", "SecurityError");
      },
    });
    Object.defineProperty(window, "localStorage", {
      get() {
        throw new DOMException("Storage disabled", "SecurityError");
      },
    });
  });
  await page.goto("/");
  await expectTheme(page, "light");
  await page.getByRole("button", { name: "Switch to dark theme" }).click();
  await expectTheme(page, "dark");
});

test("the file viewer and native space selector follow both themes", async ({
  page,
}) => {
  await page.goto("/");
  for (const mode of ["dark", "light"] as const) {
    await page.getByRole("button", { name: `Switch to ${mode} theme` }).click();
    await expectTheme(page, mode);
    await expect(page.locator("select")).toHaveCSS(
      "background-color",
      mode === "dark" ? "rgb(18, 30, 48)" : "rgb(255, 255, 255)",
    );
    await page.getByRole("button", { name: /^notes\.txt/ }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText("Saved content")).toBeVisible();
    await expect(dialog).toHaveCSS(
      "background-color",
      mode === "dark" ? "rgb(18, 30, 48)" : "rgb(255, 255, 255)",
    );
    await page.keyboard.press("Escape");
    await expect(dialog).not.toBeVisible();
  }
});

test("the toggle is visible and keyboard accessible on mobile", async ({
  page,
}) => {
  await page.setViewportSize({ width: 320, height: 640 });
  await page.goto("/");
  const button = page.getByRole("button", { name: "Switch to dark theme" });
  await expect(button).toBeVisible();
  const rect = await button.boundingBox();
  expect(rect!.x + rect!.width).toBeLessThanOrEqual(320);
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth),
  ).toBeLessThanOrEqual(320);
  await button.focus();
  await page.keyboard.press("Enter");
  await expectTheme(page, "dark");
});

test("migrates the legacy explicit preference into shared storage and cookie", async ({
  page,
  context,
}) => {
  await page.addInitScript(() =>
    localStorage.setItem("drive-color-mode", "dark"),
  );
  await page.goto("/");
  await expectTheme(page, "dark");
  expect(
    await page.evaluate(() => localStorage.getItem("i3alumba-color-mode")),
  ).toBe("dark");
  const cookie = (await context.cookies()).find(
    (item) => item.name === "i3alumba-theme",
  );
  expect(cookie).toMatchObject({
    value: "dark",
    path: "/",
    sameSite: "Lax",
    httpOnly: false,
  });
});

test("cookie wins over shared and legacy local storage", async ({
  page,
  context,
  baseURL,
}) => {
  await context.addCookies([
    { name: "i3alumba-theme", value: "dark", url: baseURL! },
  ]);
  await page.addInitScript(() => {
    localStorage.setItem("drive-color-mode", "light");
    localStorage.setItem("i3alumba-color-mode", "light");
  });
  await page.goto("/");
  await expectTheme(page, "dark");
});

test("invalid shared preferences are ignored", async ({
  page,
  context,
  baseURL,
}) => {
  await context.addCookies([
    { name: "i3alumba-theme", value: "invalid", url: baseURL! },
  ]);
  await page.addInitScript(() =>
    localStorage.setItem("i3alumba-color-mode", "invalid"),
  );
  await page.goto("/");
  await expectTheme(page, "light");
});

test("subdomain theme cookie syncs without sharing authentication", async ({
  page,
  context,
}) => {
  // Virtual HTTPS hosts: all app assets and APIs are local/mocked, never public services.
  await page.route(
    /^https:\/\/drive\.i3alumba\.ru\/(?!api\/)/,
    async (route) => {
      const url = new URL(route.request().url());
      const response = await route.fetch({
        url: `http://127.0.0.1:4174${url.pathname}${url.search}`,
      });
      await route.fulfill({ response });
    },
  );
  await context.route("https://main.i3alumba.ru/**", (route) =>
    route.fulfill({
      contentType: "text/html",
      body: "<!doctype html><title>Cookie fixture</title>",
    }),
  );
  await page.goto("https://drive.i3alumba.ru/");
  await page.getByRole("button", { name: "Switch to dark theme" }).click();
  await expectTheme(page, "dark");
  const cookie = (await context.cookies()).find(
    (item) => item.name === "i3alumba-theme",
  );
  expect(cookie).toMatchObject({
    domain: ".i3alumba.ru",
    secure: true,
    sameSite: "Lax",
    path: "/",
    value: "dark",
  });
  expect(cookie!.expires - Date.now() / 1000).toBeGreaterThan(31535000);
  expect((await context.cookies()).map((item) => item.name)).toEqual([
    "i3alumba-theme",
  ]);
  const sibling = await context.newPage();
  await sibling.goto("https://main.i3alumba.ru/");
  await sibling.evaluate(() => {
    document.cookie =
      "i3alumba-theme=light; Domain=i3alumba.ru; Path=/; Secure; SameSite=Lax";
  });
  await expectTheme(page, "light");
  await sibling.evaluate(() => {
    document.cookie =
      "i3alumba-theme=system; Domain=i3alumba.ru; Path=/; Secure; SameSite=Lax";
  });
  await page.evaluate(() => window.dispatchEvent(new Event("focus")));
  await page.emulateMedia({ colorScheme: "dark" });
  await expectTheme(page, "dark");
});

test("share controls preserve input across themes", async ({ page }) => {
  await page.goto("/");
  await page
    .getByRole("button", { name: "Share notes.txt", exact: true })
    .click();
  const dialog = page.getByRole("dialog");
  await dialog.getByLabel("Target username").fill("fixture-user");
  await dialog.getByLabel("Share permission").selectOption("edit");
  await page.evaluate(() => {
    document.cookie = "i3alumba-theme=dark; Path=/";
    window.dispatchEvent(new Event("focus"));
  });
  await expect(dialog).toHaveCSS("background-color", "rgb(18, 30, 48)");
  await expect(dialog.getByLabel("Target username")).toHaveValue(
    "fixture-user",
  );
  await expect(dialog.getByLabel("Share permission")).toHaveValue("edit");
});

test("image and video preview canvases remain black in both themes", async ({
  page,
}) => {
  await page.route("**/api/files?**", (route) =>
    route.fulfill({
      json: [
        {
          name: "photo.png",
          path: "photo.png",
          size: 12,
          isDir: false,
          lastModified: "2026-01-01",
        },
        {
          name: "clip.mp4",
          path: "clip.mp4",
          size: 12,
          isDir: false,
          lastModified: "2026-01-01",
        },
      ],
    }),
  );
  await page.goto("/");
  for (const mode of ["dark", "light"] as const) {
    await page.getByRole("button", { name: `Switch to ${mode} theme` }).click();
    for (const name of ["photo.png", "clip.mp4"]) {
      await page.getByRole("button", { name: new RegExp(`^${name}`) }).click();
      const dialog = page.getByRole("dialog");
      await expect(dialog.locator(".MuiDialogContent-root")).toHaveCSS(
        "background-color",
        "rgb(0, 0, 0)",
      );
      await page.keyboard.press("Escape");
      await expect(dialog).not.toBeVisible();
    }
  }
});
