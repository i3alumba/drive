import { expect, test } from "@playwright/test";

const id = "a".repeat(64),
  token = "b".repeat(64);
test("shared fragment is removed before exchange; native download has no token URI or portal bearer", async ({
  page,
  context,
  baseURL,
}) => {
  await context.addCookies([
    { name: "access_token", value: "fixture-access", url: baseURL! },
  ]);
  let exchanges = 0,
    privateRequests = 0;
  page.on("request", (request) => {
    expect(request.url()).not.toContain(token);
  });
  await page.route("**/api/**", async (route) => {
    const request = route.request(),
      path = new URL(request.url()).pathname;
    if (path === `/api/public/42/${id}/session`) {
      expect(request.method()).toBe("POST");
      expect(request.postDataJSON()).toEqual({ token });
      expect(request.headers().authorization).toBeUndefined();
      exchanges++;
      return route.fulfill({ status: 204 });
    }
    if (path === `/api/public/42/${id}`) {
      expect(request.method()).toBe("GET");
      return route.fulfill({
        headers: {
          "Content-Type": "application/octet-stream",
          "Content-Disposition": "attachment; filename=snapshot.txt",
        },
        body: "owned fixture",
      });
    }
    privateRequests++;
    return route.fulfill({ status: 401 });
  });
  await page.goto(`/public/42/${id}#${token}`);
  await expect(
    page.getByRole("heading", { name: "Shared Drive file" }),
  ).toBeVisible();
  expect(page.url()).not.toContain("#");
  expect(exchanges).toBe(0);
  const download = page.waitForEvent("download");
  await page
    .getByRole("button", { name: "Download file", exact: true })
    .click();
  expect((await download).suggestedFilename()).toBe("snapshot.txt");
  expect(exchanges).toBe(1);
  expect(privateRequests).toBe(0);
});
test("missing fragment or revoked capability does not navigate to login or fetch a file", async ({
  page,
}) => {
  let exchanges = 0;
  await page.route("**/api/**", (route) => {
    exchanges++;
    return route.fulfill({ status: 404 });
  });
  await page.goto(`/public/42/${id}`);
  await expect(
    page.getByRole("button", { name: "Download file", exact: true }),
  ).toBeDisabled();
  expect(exchanges).toBe(0);
  await page.goto(`/public/42/${id}#${token}`);
  await page
    .getByRole("button", { name: "Download file", exact: true })
    .click();
  await expect(page.getByRole("alert")).toContainText(
    "Link expired, revoked or unavailable",
  );
  expect(exchanges).toBe(1);
  expect(new URL(page.url()).pathname).toBe(`/public/42/${id}`);
});
test("owner-wide link management remains available after original files are gone", async ({
  page,
}) => {
  let revoked = false;
  await page.route("**/api/**", (route) => {
    const request = route.request(),
      path = new URL(request.url()).pathname;
    if (path === "/api/spaces")
      return route.fulfill({
        json: [{ id: "personal", name: "My files", permission: "edit" }],
      });
    if (path === "/api/public-links" && request.method() === "GET") {
      expect(new URL(request.url()).search).toBe("");
      return route.fulfill({
        json: [
          {
            id: "cleanup-id",
            path: "deleted-original.txt",
            expiresAt: "2027-01-01",
            cleanupOnly: true,
          },
        ],
      });
    }
    if (
      path === "/api/public-links/cleanup-id" &&
      request.method() === "DELETE"
    ) {
      revoked = true;
      return route.fulfill({ status: 204 });
    }
    return route.fulfill({ json: [] });
  });
  await page.goto("/");
  await page
    .getByRole("button", { name: "Manage public links", exact: true })
    .click();
  await expect(page.getByRole("dialog")).toContainText("cleanup required");
  await expect(
    page.getByRole("button", { name: "Create public link", exact: true }),
  ).toHaveCount(0);
  await page.getByRole("button", { name: "Revoke link", exact: true }).click();
  expect(revoked).toBe(true);
});
