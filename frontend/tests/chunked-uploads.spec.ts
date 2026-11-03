import { createHash, randomUUID } from "node:crypto";
import { mkdir, writeFile } from "node:fs/promises";
import { dirname } from "node:path";
import {
  expect,
  test,
  type Page,
  type Route,
  type TestInfo,
} from "@playwright/test";
import type { UploadSession } from "../src/chunkedUploads";

const chunk = 8 * 1024 * 1024;
const checksum = (data: Buffer) =>
  createHash("sha256").update(data).digest("hex");

async function fixtureFile(info: TestInfo, name: string, data: Buffer) {
  const path = info.outputPath(name);
  await mkdir(dirname(path), { recursive: true });
  await writeFile(path, data);
  return path;
}

async function mockChunks(page: Page, existing: UploadSession[] = []) {
  const sessions = new Map(existing.map((session) => [session.id, session]));
  const parts: { id: string; index: number; bytes: number }[] = [];
  let automaticComplete = true;
  let legacyRequests = 0;
  let creations = 0;
  let onComplete:
    | ((route: Route, session: UploadSession) => Promise<boolean>)
    | undefined;
  let onPart: ((index: number, route: Route) => Promise<boolean>) | undefined;
  await page.context().addCookies([
    {
      name: "access_token",
      value: "fixture-access",
      url: "http://127.0.0.1:4174",
    },
  ]);
  await page.route("**/api/**", async (route) => {
    const request = route.request(),
      path = new URL(request.url()).pathname;
    if (path === "/api/spaces")
      return route.fulfill({
        json: [{ id: "personal", name: "My files", permission: "edit" }],
      });
    if (path === "/api/upload") {
      legacyRequests++;
      return route.fulfill({ status: 500 });
    }
    if (path === "/api/uploads") {
      if (request.method() === "GET")
        return route.fulfill({ json: [...sessions.values()] });
      expect(request.headers().authorization).toBe("Bearer fixture-access");
      creations++;
      const input = request.postDataJSON();
      const session: UploadSession = sessions.get(input.clientId) ?? {
        id: input.clientId,
        name: input.name,
        path: input.path,
        space: input.space,
        size: input.size,
        chunkSize: chunk,
        parts: {},
        state: "uploading",
        expiresAt: new Date(Date.now() + 86400000).toISOString(),
      };
      sessions.set(session.id, session);
      return route.fulfill({ status: 201, json: session });
    }
    const match =
      /^\/api\/uploads\/([^/]+)(?:\/parts\/(\d+)|\/(complete))?$/.exec(path);
    if (!match) return route.fulfill({ json: [] });
    const session = sessions.get(match[1]);
    if (!session)
      return route.fulfill({ status: 404, json: { error: "missing session" } });
    if (match[2] !== undefined) {
      const index = Number(match[2]);
      const data = request.postDataBuffer()!;
      parts.push({ id: session.id, index, bytes: data.length });
      expect(request.headers().authorization).toBe("Bearer fixture-access");
      expect(data.length).toBeLessThanOrEqual(chunk);
      expect(request.headers()["x-chunk-sha256"]).toBe(checksum(data));
      if (onPart && (await onPart(index, route))) return;
      session.parts[String(index)] = {
        size: data.length,
        sha256: checksum(data),
      };
      return route.fulfill({ json: session });
    }
    if (match[3]) {
      if (onComplete && (await onComplete(route, session))) return;
      session.state = automaticComplete ? "complete" : "committing";
      return route.fulfill({
        status: automaticComplete ? 200 : 202,
        json: session,
      });
    }
    if (request.method() === "DELETE") {
      session.state = "cancelled";
      return route.fulfill({ json: session });
    }
    if (session.state === "committing" && automaticComplete)
      session.state = "complete";
    return route.fulfill({ json: session });
  });
  return {
    sessions,
    parts,
    setComplete: (value: boolean) => {
      automaticComplete = value;
    },
    onPart: (callback: (index: number, route: Route) => Promise<boolean>) => {
      onPart = callback;
    },
    legacyCount: () => legacyRequests,
    creationCount: () => creations,
    onComplete: (
      callback: (route: Route, session: UploadSession) => Promise<boolean>,
    ) => {
      onComplete = callback;
    },
  };
}

function picker(page: Page) {
  return page.getByText("Upload file", { exact: true }).locator("input");
}

function recoveredSession(data: Buffer): UploadSession {
  return {
    id: randomUUID(),
    name: "resume.bin",
    path: "",
    space: "personal",
    size: data.length,
    chunkSize: chunk,
    parts: { "0": { size: chunk, sha256: checksum(data.subarray(0, chunk)) } },
    state: "uploading",
    expiresAt: new Date(Date.now() + 86400000).toISOString(),
  };
}

test("ordinary files use bounded parts/checksums and await asynchronous publication", async ({
  page,
}, info) => {
  const api = await mockChunks(page);
  api.setComplete(false);
  await page.goto("/");
  await picker(page).setInputFiles(
    await fixtureFile(info, "chunked.bin", Buffer.alloc(2 * chunk + 3, 0x61)),
  );
  const widget = page.getByRole("region", { name: "Transfers" });
  await expect(widget).toContainText("saving on Drive");
  await expect.poll(() => api.parts.length).toBe(3);
  expect(api.parts.map((part) => part.bytes)).toEqual([chunk, chunk, 3]);
  expect(api.legacyCount()).toBe(0);
  await expect(widget).not.toContainText("complete");
  api.setComplete(true);
  await expect(widget).toContainText("complete");
  await page.getByRole("button", { name: "Clear finished transfers" }).click();
  await expect(widget).toHaveCount(0);
});

test("pause/resume retains acknowledged parts and resends only missing data", async ({
  page,
}, info) => {
  const api = await mockChunks(page);
  let release!: () => void;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  let blocked = false,
    first = true;
  api.onPart(async (index, route) => {
    if (index !== 1 || !first) return false;
    first = false;
    blocked = true;
    await gate;
    await route.abort("aborted").catch(() => {});
    return true;
  });
  await page.goto("/");
  await picker(page).setInputFiles(
    await fixtureFile(info, "pause.bin", Buffer.alloc(2 * chunk + 1, 0x62)),
  );
  await expect.poll(() => blocked).toBe(true);
  await page
    .getByRole("button", { name: "Pause pause.bin", exact: true })
    .click();
  const widget = page.getByRole("region", { name: "Transfers" });
  await expect(widget).toContainText("paused");
  release();
  await page
    .getByRole("button", { name: "Resume pause.bin", exact: true })
    .click();
  await expect(widget).toContainText("complete");
  expect(api.parts.filter((part) => part.index === 0)).toHaveLength(1);
  expect(api.parts.filter((part) => part.index === 1)).toHaveLength(2);
});

test("reselection after reload verifies retained hashes before sending new parts", async ({
  page,
}, info) => {
  const data = Buffer.alloc(chunk + 7, 0x63);
  const session = recoveredSession(data);
  const api = await mockChunks(page, [session]);
  await page.goto("/");
  const input = page.getByLabel("Choose file to resume resume.bin", {
    exact: true,
  });
  await expect(input).toHaveCount(1);
  await input.setInputFiles(
    await fixtureFile(
      info,
      "wrong/resume.bin",
      Buffer.alloc(data.length, 0x64),
    ),
  );
  await expect(page.getByRole("alert")).toContainText(
    "differs from already uploaded parts",
  );
  expect(api.parts).toHaveLength(0);
  await input.setInputFiles(
    await fixtureFile(info, "original/resume.bin", data),
  );
  await expect(page.getByRole("region", { name: "Transfers" })).toContainText(
    "complete",
  );
  expect(api.parts.map((part) => part.index)).toEqual([1]);
});

test("transient part failures retry without duplicating accepted data", async ({
  page,
}) => {
  const api = await mockChunks(page);
  let first = true;
  api.onPart(async (_index, route) => {
    if (!first) return false;
    first = false;
    await route.fulfill({
      status: 503,
      json: { error: "temporary fixture failure" },
    });
    return true;
  });
  await page.goto("/");
  await picker(page).setInputFiles({
    name: "retry.bin",
    mimeType: "application/octet-stream",
    buffer: Buffer.from("owned fixture"),
  });
  await expect(page.getByRole("region", { name: "Transfers" })).toContainText(
    "complete",
  );
  expect(api.parts).toHaveLength(2);
  expect([...api.sessions.values()][0].parts).toHaveProperty("0");
});

test("cancelling a recovered upload discards staging without completing it", async ({
  page,
}) => {
  const data = Buffer.alloc(chunk + 1, 0x65),
    session = recoveredSession(data);
  const api = await mockChunks(page, [session]);
  await page.goto("/");
  await page
    .getByRole("button", { name: "Cancel resume.bin", exact: true })
    .click();
  await expect(page.getByRole("region", { name: "Transfers" })).toContainText(
    "cancelled",
  );
  expect(api.sessions.get(session.id)?.state).toBe("cancelled");
  expect(api.parts).toHaveLength(0);
});

test("lost completion receipt plus expiry never recreates or overwrites on resume", async ({
  page,
}) => {
  const api = await mockChunks(page);
  let destination = "original",
    completions = 0;
  api.onComplete(async (route, session) => {
    if (session.state !== "complete") destination = "uploaded fixture";
    session.state = "complete";
    completions++;
    await route.abort("failed"); // server committed; client never receives the acknowledgement
    return true;
  });
  await page.goto("/");
  const file = {
    name: "receipt.bin",
    mimeType: "application/octet-stream",
    buffer: Buffer.from("owned fixture"),
  };
  await picker(page).setInputFiles(file);
  await expect.poll(() => completions).toBe(3);
  const input = page.getByLabel("Choose file to resume receipt.bin", {
    exact: true,
  });
  await expect(input).toHaveCount(1);
  expect(destination).toBe("uploaded fixture");
  api.sessions.clear(); // simulate receipt GC after 24 hours
  destination = "subsequent writer replacement";
  await input.setInputFiles(file);
  await expect(page.getByRole("alert")).toContainText(
    "resume will not recreate this session",
  );
  expect(api.creationCount()).toBe(1);
  expect(api.parts).toHaveLength(1);
  expect(completions).toBe(3);
  expect(destination).toBe("subsequent writer replacement");
  await expect(input).toHaveCount(0);
});
