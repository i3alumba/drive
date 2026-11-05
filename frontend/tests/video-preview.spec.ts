import { expect, test, type BrowserContext } from "@playwright/test";
import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const id = "0123456789abcdef0123456789abcdef";
let scratch: string;
let assets: Record<string, Buffer>;

test.beforeAll(() => {
  scratch = mkdtempSync(join(tmpdir(), "drive-hls-browser-"));
  execFileSync("/usr/bin/ffmpeg", [
    "-v",
    "error",
    "-nostdin",
    "-f",
    "lavfi",
    "-i",
    "color=c=black:s=160x120:r=10",
    "-t",
    "8",
    "-fflags",
    "+bitexact",
    "-map_metadata",
    "-1",
    "-map_metadata:s",
    "-1",
    "-map_chapters",
    "-1",
    "-c:v",
    "libx264",
    "-threads",
    "1",
    "-pix_fmt",
    "yuv420p",
    "-flags:v",
    "+bitexact",
    "-force_key_frames",
    "expr:gte(t,n_forced*4)",
    "-g",
    "40",
    "-sc_threshold",
    "0",
    "-f",
    "hls",
    "-hls_time",
    "4",
    "-hls_segment_type",
    "fmp4",
    "-hls_flags",
    "temp_file+independent_segments",
    "-hls_list_size",
    "0",
    "-hls_playlist_type",
    "event",
    "-hls_fmp4_init_filename",
    "init.mp4",
    "-hls_segment_options",
    "fflags=+bitexact:movflags=+default_base_moof+negative_cts_offsets+skip_sidx:use_editlist=0",
    "-hls_segment_filename",
    join(scratch, "segment-%06d.m4s"),
    join(scratch, "stream.m3u8"),
  ]);
  assets = Object.fromEntries(
    ["init.mp4", "segment-000000.m4s", "segment-000001.m4s"].map((name) => [
      name,
      readFileSync(join(scratch, name)),
    ]),
  );
});

test.afterAll(() => {
  if (scratch) rmSync(scratch, { recursive: true });
});

async function fixture(context: BrowserContext) {
  const state = {
    posts: 0,
    rawReads: 0,
    mediaReads: 0,
    prefixPlaylists: 0,
    phase: "streaming",
    published: 1,
    denied: false,
    disconnected: false,
    gone: false,
    cancelled: false,
    pendingCancellation: false,
  };
  // Exercise actual hls.js/MSE rather than claiming Chromium proves Safari HLS.
  await context.addInitScript(() => {
    const original = HTMLMediaElement.prototype.canPlayType;
    HTMLMediaElement.prototype.canPlayType = function (
      type: string,
    ): CanPlayTypeResult {
      return type.includes("mpegurl") ? "" : original.call(this, type);
    };
  });
  await context.addCookies([
    {
      name: "access_token",
      value: "fixture-video",
      url: "http://127.0.0.1:4174",
    },
  ]);
  const status = () => ({
    id,
    status: state.cancelled
      ? state.pendingCancellation
        ? "cancelling"
        : "cancelled"
      : state.phase,
    ...(state.phase === "streaming" || state.phase === "complete"
      ? { mode: "transcode" }
      : {}),
    duration: 8,
    preparedDuration: state.published * 4,
    segments: state.published,
  });
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
        body: "Original must not be fetched for video fallback/cards",
      });
    }
    if (url.pathname === "/api/video-previews" && request.method() === "POST") {
      state.posts++;
      expect(request.headers().authorization).toBe("Bearer fixture-video");
      expect(request.postDataJSON()).toMatchObject({ space: "personal" });
      return route.fulfill({
        status: 202,
        json: { id, status: "staging", segments: 0 },
      });
    }
    if (url.pathname === `/api/video-previews/${id}`) {
      if (state.denied)
        return route.fulfill({ status: 403, json: { error: "denied" } });
      if (state.gone)
        return route.fulfill({ status: 404, json: { error: "not_found" } });
      if (request.method() === "DELETE") state.cancelled = true;
      if (state.disconnected) return route.abort();
      return route.fulfill({ json: status() });
    }
    if (url.pathname.startsWith(`/api/video-previews/${id}/`)) {
      state.mediaReads++;
      expect(request.headers().cookie).toContain("access_token=fixture-video");
      if (state.denied) return route.fulfill({ status: 403 });
      const name = url.pathname.split("/").at(-1)!;
      if (name === "stream.m3u8") {
        if (state.phase !== "complete") state.prefixPlaylists++;
        const body =
          '#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:8\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:EVENT\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-MAP:URI="init.mp4"\n' +
          Array.from(
            { length: state.published },
            (_, i) =>
              `#EXTINF:4.000000,\nsegment-${String(i).padStart(6, "0")}.m4s\n`,
          ).join("") +
          (state.phase === "complete" ? "#EXT-X-ENDLIST\n" : "");
        return route.fulfill({
          contentType: "application/vnd.apple.mpegurl",
          headers: { "cache-control": "no-store" },
          body,
        });
      }
      const data = assets[name];
      if (!data) return route.fulfill({ status: 404 });
      const parsed = request.headers().range?.match(/^bytes=(\d+)-(\d*)$/);
      const start = parsed ? Number(parsed[1]) : 0;
      const end = parsed?.[2]
        ? Math.min(Number(parsed[2]), data.length - 1)
        : data.length - 1;
      return route.fulfill({
        status: parsed ? 206 : 200,
        contentType: "video/mp4",
        headers: {
          "accept-ranges": "bytes",
          "cache-control": "no-store",
          ...(parsed
            ? { "content-range": `bytes ${start}-${end}/${data.length}` }
            : {}),
        },
        body: data.subarray(start, end + 1),
      });
    }
    return route.fulfill({ json: [] });
  });
  return state;
}

async function openVideo(
  page: import("@playwright/test").Page,
  name = "movie.MKV",
) {
  await page.goto("/");
  await page
    .getByRole("button", { name: new RegExp(`^${name.replace(".", "\\.")}`) })
    .click();
  await expect(
    page.getByRole("link", { name: /original|download/i }),
  ).toHaveCount(0);
}

for (const name of ["movie.MKV", "movie.AVI"]) {
  test(`${name}: actual MSE playback begins from a prefix before conversion completes`, async ({
    page,
    context,
  }) => {
    const state = await fixture(context);
    await openVideo(page, name);
    expect(state.posts).toBe(0);
    expect(state.rawReads).toBe(0);
    await page
      .getByRole("button", { name: "Prepare video stream", exact: true })
      .click();
    const video = page.getByLabel("Prepared video stream");
    await expect(video).toBeVisible();
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
    expect(state.phase).toBe("streaming");
    expect(state.published).toBe(1);
    expect(state.prefixPlaylists).toBeGreaterThan(0);
    await expect(
      page.getByText("Playing available segments; preparation continues…"),
    ).toBeVisible();
    state.published = 2;
    state.phase = "complete";
    await expect(page.getByText("Preparation complete.")).toBeVisible();
    expect(state.posts).toBe(1);
    expect(state.rawReads).toBe(0);
  });
}

test("source staging is shown honestly without advertising playable media", async ({
  page,
  context,
}) => {
  const state = await fixture(context);
  state.phase = "staging";
  state.published = 0;
  await openVideo(page);
  await page
    .getByRole("button", { name: "Prepare video stream", exact: true })
    .click();
  await expect(page.getByText("Reading video from storage…")).toBeVisible();
  await expect(page.getByLabel("Prepared video stream")).toHaveCount(0);
  expect(state.mediaReads).toBe(0);
  state.phase = "streaming";
  state.published = 1;
  await expect(page.getByLabel("Prepared video stream")).toBeVisible();
  expect(state.posts).toBe(1);
});

test("cancel during streaming stops the player without claiming teardown is done", async ({
  page,
  context,
}) => {
  const state = await fixture(context);
  state.pendingCancellation = true;
  await openVideo(page);
  await page
    .getByRole("button", { name: "Prepare video stream", exact: true })
    .click();
  await expect(page.getByLabel("Prepared video stream")).toBeVisible();
  await page.getByRole("button", { name: "Cancel preparation" }).click();
  await expect(
    page.getByText("Stopping preparation; waiting for confirmed teardown…"),
  ).toBeVisible();
  await expect(page.getByLabel("Prepared video stream")).toHaveCount(0);
  await expect(page.getByText("Preparation cancelled.")).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Prepare video stream", exact: true }),
  ).toHaveCount(0);
  state.pendingCancellation = false;
  await expect(page.getByText("Preparation cancelled.")).toBeVisible();
  expect(state.posts).toBe(1);
});

test("revoked access destroys media and suppresses conversion replay", async ({
  page,
  context,
}) => {
  const state = await fixture(context);
  await openVideo(page);
  await page
    .getByRole("button", { name: "Prepare video stream", exact: true })
    .click();
  await expect(page.getByLabel("Prepared video stream")).toBeVisible();
  state.denied = true;
  await expect(
    page.getByText(/Access to (this video|this stream)/),
  ).toBeVisible();
  await expect(page.getByLabel("Prepared video stream")).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Prepare video stream", exact: true }),
  ).toHaveCount(0);
  expect(state.posts).toBe(1);
  expect(state.rawReads).toBe(0);
});

test("lost status reconciles only with GET and never repeats POST", async ({
  page,
  context,
}) => {
  const state = await fixture(context);
  state.disconnected = true;
  await openVideo(page);
  await page
    .getByRole("button", { name: "Prepare video stream", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "Check stream status" }),
  ).toBeVisible();
  expect(state.posts).toBe(1);
  state.disconnected = false;
  await page.getByRole("button", { name: "Check stream status" }).click();
  await expect(page.getByLabel("Prepared video stream")).toBeVisible();
  expect(state.posts).toBe(1);
});

test("foreign or expired status clears the old stream without a download fallback", async ({
  page,
  context,
}) => {
  const state = await fixture(context);
  state.gone = true;
  await openVideo(page);
  await page
    .getByRole("button", { name: "Prepare video stream", exact: true })
    .click();
  await expect(
    page.getByText("Video streaming is currently unavailable."),
  ).toBeVisible();
  await expect(page.getByLabel("Prepared video stream")).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Prepare video stream", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("link", { name: /original|download/i }),
  ).toHaveCount(0);
  expect(state.posts).toBe(1);
});

test("autoplay rejection offers Play rather than another conversion", async ({
  page,
  context,
}) => {
  const state = await fixture(context);
  await page.addInitScript(() => {
    const original = HTMLMediaElement.prototype.play;
    let first = true;
    HTMLMediaElement.prototype.play = function () {
      if (first) {
        first = false;
        return Promise.reject(
          new DOMException("Fixture autoplay denied", "NotAllowedError"),
        );
      }
      return original.call(this);
    };
  });
  await openVideo(page);
  await page
    .getByRole("button", { name: "Prepare video stream", exact: true })
    .click();
  await page.getByRole("button", { name: "Play", exact: true }).click();
  await expect
    .poll(() =>
      page
        .getByLabel("Prepared video stream")
        .evaluate((element) => (element as HTMLVideoElement).currentTime),
    )
    .toBeGreaterThan(0);
  expect(state.posts).toBe(1);
});

test("failed playable prefix stops rather than manufacturing a completed movie", async ({
  page,
  context,
}) => {
  const state = await fixture(context);
  await openVideo(page);
  await page
    .getByRole("button", { name: "Prepare video stream", exact: true })
    .click();
  await expect(page.getByLabel("Prepared video stream")).toBeVisible();
  state.phase = "failed";
  await expect(
    page.getByText(/Video preparation stopped before completion/),
  ).toBeVisible();
  await expect(page.getByLabel("Prepared video stream")).toHaveCount(0);
  await expect(page.getByText("Preparation complete.")).toHaveCount(0);
  expect(state.posts).toBe(1);
});

test("unknown initial POST cannot be blindly submitted again", async ({
  page,
  context,
}) => {
  const state = await fixture(context);
  await context.route("**/api/video-previews", async (route) => {
    state.posts++;
    await route.abort();
  });
  await openVideo(page);
  await page
    .getByRole("button", { name: "Prepare video stream", exact: true })
    .click();
  await expect(
    page.getByText(/preparation request result is unknown/),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Prepare video stream", exact: true }),
  ).toHaveCount(0);
  expect(state.posts).toBe(1);
});

test("stale status cannot restart media while cancellation acknowledgement is delayed", async ({
  page,
  context,
}) => {
  const state = await fixture(context);
  state.pendingCancellation = true;
  let resolveDelete = () => {};
  const release = new Promise<void>((resolve) => {
    resolveDelete = resolve;
  });
  let holdingDelete = false;
  let staleReads = 0;
  await context.route(`**/api/video-previews/${id}`, async (route) => {
    if (route.request().method() !== "DELETE") {
      if (holdingDelete) staleReads++;
      return route.fallback();
    }
    holdingDelete = true;
    await release;
    state.cancelled = true;
    return route.fulfill({
      json: {
        id,
        status: "cancelling",
        segments: 1,
        duration: 8,
        preparedDuration: 4,
      },
    });
  });
  await openVideo(page);
  await page
    .getByRole("button", { name: "Prepare video stream", exact: true })
    .click();
  await expect(page.getByLabel("Prepared video stream")).toBeVisible();
  try {
    await page.getByRole("button", { name: "Cancel preparation" }).click();
    await expect.poll(() => staleReads).toBeGreaterThan(0);
    await expect(
      page.getByText("Stopping preparation; waiting for confirmed teardown…"),
    ).toBeVisible();
    await expect(page.getByLabel("Prepared video stream")).toHaveCount(0);
    await expect(
      page.getByRole("button", { name: "Retry playback" }),
    ).toHaveCount(0);
  } finally {
    resolveDelete();
  }
  expect(state.posts).toBe(1);
});

test("unsupported streaming browser fails in site without suggesting a download", async ({
  page,
  context,
}) => {
  const state = await fixture(context);
  await context.addInitScript(() => {
    for (const name of [
      "MediaSource",
      "ManagedMediaSource",
      "WebKitMediaSource",
    ])
      Object.defineProperty(window, name, {
        value: undefined,
        configurable: true,
      });
  });
  await openVideo(page);
  await page
    .getByRole("button", { name: "Prepare video stream", exact: true })
    .click();
  await expect(
    page.getByText(
      "This browser does not support the available streaming player.",
    ),
  ).toBeVisible();
  await expect(page.getByLabel("Prepared video stream")).toHaveCount(0);
  await expect(
    page.getByRole("link", { name: /original|download/i }),
  ).toHaveCount(0);
  expect(state.posts).toBe(1);
  expect(state.rawReads).toBe(0);
});

test("closing viewer tears down media but does not cancel or repeat an accepted conversion", async ({
  page,
  context,
}) => {
  const state = await fixture(context);
  await openVideo(page);
  await page
    .getByRole("button", { name: "Prepare video stream", exact: true })
    .click();
  const video = page.getByLabel("Prepared video stream");
  await expect(video).toBeVisible();
  const handle = await video.elementHandle();
  await page.getByRole("button", { name: "Close viewer" }).click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  expect(
    await handle!.evaluate(
      (element) =>
        (element as HTMLVideoElement).paused && !element.hasAttribute("src"),
    ),
  ).toBe(true);
  await handle!.dispose();
  expect(state.cancelled).toBe(false);
  expect(state.posts).toBe(1);
});
