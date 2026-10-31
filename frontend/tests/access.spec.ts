import { expect, test } from "@playwright/test";

test("a denied account sees an explicit message, not private file UI, and can retry", async ({
  page,
  context,
}) => {
  let allowed = false;
  let fileRequests = 0;
  await context.route("**/api/**", (route) => {
    const path = new URL(route.request().url()).pathname;
    if (!allowed)
      return route.fulfill({
        status: 403,
        json: { error: "Drive access is not granted" },
      });
    if (path === "/api/spaces")
      return route.fulfill({
        json: [{ id: "personal", name: "My files", permission: "edit" }],
      });
    if (path === "/api/files") {
      fileRequests++;
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
    }
    return route.fulfill({ json: [] });
  });
  await page.goto("/");
  await expect(page.getByRole("alert")).toContainText(
    "Access to Drive is not granted",
  );
  await expect(
    page.getByRole("button", { name: "Create", exact: true }),
  ).toBeDisabled();
  await expect(
    page.getByRole("link", { name: "Dashboard", exact: true }),
  ).toHaveAttribute("href", "https://main.i3alumba.ru/");
  expect(fileRequests).toBe(0);
  allowed = true;
  await page.getByRole("button", { name: "Retry", exact: true }).click();
  await expect(
    page.getByRole("button", { name: /fixture\.txt/ }),
  ).toBeVisible();
  await expect(page.getByRole("alert")).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Create", exact: true }),
  ).toBeEnabled();
});

test("mutations send the explicit bearer header required by the API", async ({
  page,
  context,
  baseURL,
}) => {
  let mutations = 0;
  await context.addCookies([
    { name: "access_token", value: "fixture-access", url: baseURL! },
  ]);
  await context.route("**/api/**", (route) => {
    const req = route.request(),
      path = new URL(req.url()).pathname;
    if (path === "/api/spaces")
      return route.fulfill({
        json: [{ id: "personal", name: "My files", permission: "edit" }],
      });
    if (path === "/api/directories") {
      expect(req.headers().authorization).toBe("Bearer fixture-access");
      expect(req.postDataJSON()).toEqual({ path: "notes" });
      mutations++;
      return route.fulfill({ status: 201, json: {} });
    }
    return route.fulfill({ json: [] });
  });
  await page.goto("/");
  await page.getByLabel("New directory").fill("notes");
  await page.getByRole("button", { name: "Create", exact: true }).click();
  await expect.poll(() => mutations).toBe(1);
});
