import { expect, test } from "@playwright/test";

test("each downloading torrent shows its own peers and speed, including zero", async ({
  page,
}) => {
  let paused = false;
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
            id: "first",
            name: "first.iso",
            targetDir: "users/42/",
            status: paused ? "paused" : "downloading",
            progress: 0.5,
            connectedPeers: 53,
            downloadSpeedBytesPerSecond: 1572864,
            etaSeconds: 20,
          },
          {
            id: "second",
            name: "second.iso",
            targetDir: "users/42/",
            status: "downloading",
            progress: 0.1,
            connectedPeers: 0,
            downloadSpeedBytesPerSecond: 0,
          },
          {
            id: "older-api",
            name: "older.iso",
            targetDir: "users/42/",
            status: "downloading",
            progress: 0,
          },
        ],
      });
    return route.fulfill({ json: [] });
  });
  await page.goto("/");
  const first = page.getByRole("article", {
    name: "Torrent first.iso",
    exact: true,
  });
  const second = page.getByRole("article", {
    name: "Torrent second.iso",
    exact: true,
  });
  await expect(first).toContainText("Peers: 53");
  await expect(first).toContainText("1.5 MiB/s");
  await expect(first).toContainText("ETA 20s");
  await expect(second).toContainText("Peers: 0");
  await expect(second).toContainText("0 B/s");
  await expect(second).not.toContainText("53");
  await expect(
    page.getByRole("article", { name: "Torrent older.iso", exact: true }),
  ).toContainText("Peers: —");
  paused = true;
  // Deliberately retain stale API numbers to verify that paused rows cannot
  // display a former transfer speed or peer count as though still downloading.
  await expect(first).toContainText("paused", { timeout: 10000 });
  await expect(first).not.toContainText("Peers:");
  await expect(first).not.toContainText("MiB/s");
  await expect(first).not.toContainText("ETA");
});
