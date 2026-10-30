import { expect, test, type Page } from "@playwright/test";

async function expectTheme(page: Page, mode: "light" | "dark") {
  await expect(page.locator("html")).toHaveClass(new RegExp(`\\b${mode}\\b`));
  await expect(page.locator("body")).toHaveCSS(
    "background-color",
    mode === "dark" ? "rgb(18, 18, 18)" : "rgb(255, 255, 255)",
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
    .poll(() => page.evaluate(() => localStorage.getItem("drive-color-mode")))
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
      mode === "dark" ? "rgb(18, 18, 18)" : "rgb(255, 255, 255)",
    );
    await page.getByRole("button", { name: /notes\.txt/ }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText("Saved content")).toBeVisible();
    await expect(dialog).toHaveCSS(
      "background-color",
      mode === "dark" ? "rgb(18, 18, 18)" : "rgb(255, 255, 255)",
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
  await button.focus();
  await page.keyboard.press("Enter");
  await expectTheme(page, "dark");
});
