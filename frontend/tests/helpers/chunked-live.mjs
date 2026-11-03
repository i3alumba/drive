import { chromium } from "../../node_modules/playwright-core/index.mjs";
import { mkdtemp, mkdir, open, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

const base = new URL(process.argv[2]);
if (base.protocol !== "http:" || base.hostname !== "127.0.0.1")
  throw new Error("Owned loopback fixture only");
const home = await mkdtemp(join(tmpdir(), "du-"));
const file = join(home, "large-fixture.bin");
const size = 101 * 1024 * 1024 + 3;
let browser;
let stage = "fixture";
try {
  const output = await open(file, "wx", 0o600);
  try {
    const block = Buffer.alloc(1024 * 1024, 0x61);
    for (
      let remaining = size;
      remaining > 0;
      remaining -= Math.min(remaining, block.length)
    )
      await output.writeFile(
        block.subarray(0, Math.min(remaining, block.length)),
      );
  } finally {
    await output.close();
  }
  await mkdir(join(home, "tmp"));
  const env = {
    HOME: home,
    XDG_CONFIG_HOME: join(home, "config"),
    XDG_CACHE_HOME: join(home, "cache"),
    TMPDIR: join(home, "tmp"),
  };
  for (const key of ["PATH", "LANG", "LC_ALL", "TZ"])
    if (process.env[key]) env[key] = process.env[key];
  stage = "sandboxed_launch";
  browser = await chromium.launch({
    executablePath: process.env.CHROMIUM_PATH || "/opt/google/chrome/chrome",
    headless: true,
    chromiumSandbox: true,
    env,
  });
  const context = await browser.newContext({ ignoreHTTPSErrors: false });
  await context.addCookies([
    { name: "access_token", value: "fixture-browser", url: base.origin },
  ]);
  const page = await context.newPage();
  const indexes = [];
  let legacy = 0;
  let blocked = false;
  let first = true;
  let release;
  const gate = new Promise((resolve) => {
    release = resolve;
  });
  await context.route("**/*", async (route) => {
    const url = new URL(route.request().url());
    if (url.origin !== base.origin) return route.abort();
    if (url.pathname === "/api/upload") legacy++;
    const match = /\/parts\/(\d+)$/.exec(url.pathname);
    if (match) {
      indexes.push(Number(match[1]));
      if (match[1] === "1" && first) {
        first = false;
        blocked = true;
        await gate;
      }
    }
    await route.continue().catch(() => {}); // a deliberately paused request can already be aborted
  });
  stage = "initial_upload";
  await page.goto(base.origin);
  await page
    .getByText("Upload file", { exact: true })
    .locator("input")
    .setInputFiles(file);
  const deadline = Date.now() + 30000;
  while (!blocked && Date.now() < deadline)
    await new Promise((resolve) => setTimeout(resolve, 10));
  if (!blocked) throw new Error("Part gate not reached");
  stage = "pause";
  await page
    .getByRole("button", { name: "Pause large-fixture.bin", exact: true })
    .click();
  release();
  stage = "reload_resume";
  await page.reload();
  const reselect = page.getByLabel("Choose file to resume large-fixture.bin", {
    exact: true,
  });
  await reselect.waitFor({ state: "attached" });
  await reselect.setInputFiles(file);
  stage = "publication";
  await page
    .getByRole("region", { name: "Transfers" })
    .getByText("complete", { exact: true })
    .waitFor({ timeout: 60000 });
  if (legacy !== 0 || indexes.filter((index) => index === 0).length !== 1)
    throw new Error("Unexpected retransmission or legacy upload");
  console.log(
    JSON.stringify({
      ok: true,
      sandbox: true,
      bytes: size,
      first_part_requests: 1,
      pause_reload_resume: true,
      legacy_requests: legacy,
      real_api: true,
    }),
  );
  await context.close();
} catch (error) {
  const diagnostic =
    stage === "sandboxed_launch"
      ? String(error.message)
          .split("\n")
          .filter(
            (line) =>
              /FATAL|sandbox|Failed|error while|executable.*exist|not found/i.test(
                line,
              ) && !line.includes("<launching>"),
          )
          .slice(0, 6)
          .map((line) => line.replaceAll(home, "<owned-fixture>"))
          .join("\n")
      : undefined;
  console.log(
    JSON.stringify({
      ok: false,
      stage,
      error_type: error.name,
      launch_diagnostic: diagnostic,
    }),
  );
  process.exitCode = 1;
} finally {
  if (browser) await browser.close();
  await rm(home, { recursive: true, force: true });
}
