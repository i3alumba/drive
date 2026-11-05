import { expect, test, type BrowserContext } from "@playwright/test";
import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const id = "0123456789abcdef0123456789abcdef";
let scratch: string;
let mp4: Buffer;

test.beforeAll(() => {
  scratch = mkdtempSync(join(tmpdir(), "drive-video-browser-"));
  const output = join(scratch, "synthetic.mp4");
  execFileSync("/usr/bin/ffmpeg", [
    "-v",
    "error",
    "-nostdin",
    "-f",
    "lavfi",
    "-i",
    "color=c=black:s=160x120:r=10",
    "-t",
    "1",
    "-c:v",
    "libx264",
    "-threads",
    "1",
    "-pix_fmt",
    "yuv420p",
    "-movflags",
    "+faststart",
    output,
  ]);
  mp4 = readFileSync(output);
});

test.afterAll(() => {
  if (scratch) rmSync(scratch, { recursive: true });
});

async function fixture(context: BrowserContext) {
  const state = {
    posts: 0,
    rawReads: 0,
    ready: true,
    denied: false,
    disconnected: false,
    cancelled: false,
    pendingCancellation: false,
    gone: false,
  };
  await context.addCookies([
    {
      name: "access_token",
      value: "fixture-video",
      url: "http://127.0.0.1:4174",
    },
  ]);
  await context.route("**/api/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    expect(url.searchParams.has("access_token")).toBe(false);
    if (url.pathname === "/api/spaces")
      return route.fulfill({
        json: [{ id: "personal", name: "My files", permission: "edit" }],
      });
    if (url.pathname === "/api/files")
      return route.fulfill({
        json: ["movie.MKV", "movie.AVI"].map((name) => ({
          name,
          path: name,
          size: 100,
          isDir: false,
          lastModified: "2026-01-01T00:00:00Z",
        })),
      });
    if (url.pathname === "/api/view") {
      state.rawReads++;
      return route.fulfill({
        status: 500,
        body: "Original must not be fetched for a file card",
      });
    }
    if (url.pathname === "/api/video-previews" && request.method() === "POST") {
      state.posts++;
      expect(request.headers().authorization).toBe("Bearer fixture-video");
      expect(request.postDataJSON()).toMatchObject({ space: "personal" });
      return route.fulfill({ status: 202, json: { id, status: "preparing" } });
    }
    if (url.pathname === `/api/video-previews/${id}`) {
      if (state.gone)
        return route.fulfill({ status: 404, json: { error: "not_found" } });
      if (state.denied)
        return route.fulfill({ status: 403, json: { error: "denied" } });
      if (request.method() === "DELETE") state.cancelled = true;
      if (state.disconnected) return route.abort();
      return route.fulfill({
        json: {
          id,
          status: state.cancelled
            ? state.pendingCancellation
              ? "cancelling"
              : "cancelled"
            : state.ready
              ? "ready"
              : "preparing",
          ...(state.ready ? { mode: "remux" } : {}),
        },
      });
    }
    if (url.pathname === `/api/video-previews/${id}/content`) {
      expect(request.headers().cookie).toContain("access_token=fixture-video");
      const range = request.headers().range;
      const parsed = range?.match(/^bytes=(\d+)-(\d*)$/);
      const start = parsed ? Number(parsed[1]) : 0;
      const end = parsed?.[2]
        ? Math.min(Number(parsed[2]), mp4.length - 1)
        : mp4.length - 1;
      return route.fulfill({
        status: parsed ? 206 : 200,
        contentType: "video/mp4",
        headers: {
          "accept-ranges": "bytes",
          "cache-control": "no-store",
          ...(parsed
            ? { "content-range": `bytes ${start}-${end}/${mp4.length}` }
            : {}),
        },
        body: mp4.subarray(start, end + 1),
      });
    }
    return route.fulfill({ json: [] });
  });
  return state;
}

for (const name of ["movie.MKV", "movie.AVI"]) {
  test(`${name}: explicit preparation and native MP4 playback`, async ({
    page,
    context,
  }) => {
    const state = await fixture(context);
    await page.goto("/");
    await page
      .getByRole("button", { name: new RegExp(`^${name.replace(".", "\\.")}`) })
      .click();
    expect(state.posts).toBe(0);
    expect(state.rawReads).toBe(0);
    await page
      .getByRole("button", { name: "Prepare video preview", exact: true })
      .click();
    const video = page.getByLabel("Prepared video preview");
    await expect(video).toHaveAttribute("controls", "");
    await expect
      .poll(() =>
        video.evaluate((element) => (element as HTMLVideoElement).readyState),
      )
      .toBeGreaterThanOrEqual(2);
    await video.evaluate(async (element) => {
      const media = element as HTMLVideoElement;
      media.muted = true;
      await media.play();
    });
    await expect
      .poll(() =>
        video.evaluate((element) => (element as HTMLVideoElement).currentTime),
      )
      .toBeGreaterThan(0);
    await expect(
      page.getByRole("link", { name: "Open original file" }),
    ).toHaveAttribute("href", new RegExp("/api/view\\?"));
    expect(state.posts).toBe(1);
    expect(state.rawReads).toBe(0);
  });
}

test("cancel preparation without modifying the original", async ({
  page,
  context,
}) => {
  const state = await fixture(context);
  state.ready = false;
  await page.goto("/");
  await page.getByRole("button", { name: /^movie\.MKV/ }).click();
  await page
    .getByRole("button", { name: "Prepare video preview", exact: true })
    .click();
  await page.getByRole("button", { name: "Cancel preparation" }).click();
  await expect(page.getByText("Preparation cancelled.")).toBeVisible();
  expect(state.cancelled).toBe(true);
  expect(state.posts).toBe(1);
  expect(state.rawReads).toBe(0);
});

test("cancellation request is not displayed as completed while I/O is still stopping", async ({
  page,
  context,
}) => {
  const state = await fixture(context);
  state.ready = false;
  state.pendingCancellation = true;
  await page.goto("/");
  await page.getByRole("button", { name: /^movie\.MKV/ }).click();
  await page
    .getByRole("button", { name: "Prepare video preview", exact: true })
    .click();
  await page.getByRole("button", { name: "Cancel preparation" }).click();
  await expect(page.getByText("Stopping preparation…")).toBeVisible();
  await expect(page.getByText("Preparation cancelled.")).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Prepare video preview", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Cancel preparation" }),
  ).toBeDisabled();
  state.pendingCancellation = false;
  await expect(page.getByText("Preparation cancelled.")).toBeVisible();
  expect(state.posts).toBe(1);
});

test("revoked access clears preview controls and never starts another conversion", async ({
  page,
  context,
}) => {
  const state = await fixture(context);
  state.denied = true;
  await page.goto("/");
  await page.getByRole("button", { name: /^movie\.MKV/ }).click();
  await page
    .getByRole("button", { name: "Prepare video preview", exact: true })
    .click();
  await expect(
    page.getByText("Access to this video is no longer granted."),
  ).toBeVisible();
  await expect(page.getByLabel("Prepared video preview")).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Prepare video preview", exact: true }),
  ).toHaveCount(0);
  expect(state.posts).toBe(1);
});

test("expired or foreign-owner status clears the old preview without replay", async ({
  page,
  context,
}) => {
  const state = await fixture(context);
  state.gone = true;
  await page.goto("/");
  await page.getByRole("button", { name: /^movie\.MKV/ }).click();
  await page
    .getByRole("button", { name: "Prepare video preview", exact: true })
    .click();
  await expect(
    page.getByText(
      "Video preparation is unavailable. You can still download the original.",
    ),
  ).toBeVisible();
  await expect(page.getByLabel("Prepared video preview")).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Check preview status" }),
  ).toHaveCount(0);
  expect(state.posts).toBe(1);
});

test("lost status uses read-only reconciliation, never automatic POST retry", async ({
  page,
  context,
}) => {
  const state = await fixture(context);
  state.disconnected = true;
  await page.goto("/");
  await page.getByRole("button", { name: /^movie\.MKV/ }).click();
  await page
    .getByRole("button", { name: "Prepare video preview", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "Check preview status" }),
  ).toBeVisible();
  expect(state.posts).toBe(1);
  state.disconnected = false;
  await page.getByRole("button", { name: "Check preview status" }).click();
  await expect(page.getByLabel("Prepared video preview")).toBeVisible();
  expect(state.posts).toBe(1);
});
