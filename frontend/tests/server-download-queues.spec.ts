import { expect, test, type Page, type Route } from "@playwright/test";

const pendingKey = "drive-pending-import-submission";
const summary = (overrides = {}) => ({
  id: "queue-one",
  path: "",
  state: "running",
  acceptedAt: "2026-01-01",
  expiresAt: "2026-01-08",
  totalItems: 4,
  completedItems: 0,
  failedItems: 1,
  cancelledItems: 0,
  queuedItems: 1,
  activeItems: 1,
  savingItems: 1,
  needsVerificationItems: 1,
  totalBytes: 12,
  completedBytes: 0,
  unknownSizeItems: 1,
  discoveryState: "discovering",
  normalizedNames: 1,
  ...overrides,
});
const item = (id: string, state: string, overrides = {}) => ({
  id,
  queueId: "queue-one",
  position: 1,
  name: `${id}.bin`,
  path: `${id}.bin`,
  state,
  bytes: 12,
  total: 12,
  attempts: 1,
  ...overrides,
});
async function mockDrive(
  page: Page,
  handler: (route: Route, url: URL) => Promise<void>,
) {
  await page.context().addCookies([
    {
      name: "access_token",
      value: "queue-fixture",
      url: "http://127.0.0.1:4174",
    },
  ]);
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname.startsWith("/api/import-queues"))
      return handler(route, url);
    if (url.pathname === "/api/spaces")
      return route.fulfill({
        json: [
          { id: "personal", name: "My files", permission: "edit" },
          { id: "shared", name: "Shared files", permission: "edit" },
        ],
      });
    return route.fulfill({ json: [] });
  });
}
async function open(page: Page) {
  await page.goto("/");
  await page
    .getByRole("button", { name: "Server download queues", exact: true })
    .click();
}
function fulfillList(route: Route, url: URL, queues = [summary()]) {
  if (url.pathname === "/api/import-queues")
    return route.fulfill({
      json: { queues, total: queues.length, offset: 0, limit: 100 },
    });
  if (url.pathname.endsWith("/items"))
    return route.fulfill({
      json: { items: [], total: 0, offset: 0, limit: 100 },
    });
  return route.fulfill({ json: summary() });
}

test("collapsed panel makes no queue requests; bulk and Yandex use personal destination and bearer; reload reads durable server state", async ({
  page,
}) => {
  let requests = 0;
  const bodies: Record<string, unknown>[] = [];
  await mockDrive(page, async (route, url) => {
    requests++;
    if (route.request().method() === "POST") {
      expect(route.request().headers().authorization).toBe(
        "Bearer queue-fixture",
      );
      bodies.push(route.request().postDataJSON());
      return route.fulfill({ status: 202, json: summary() });
    }
    return fulfillList(route, url);
  });
  await page.route("**/api/files?**", (route) =>
    route.fulfill({
      json: [
        {
          name: "notes",
          path: "notes",
          size: 0,
          isDir: true,
          lastModified: "2026-01-01",
        },
      ],
    }),
  );
  await page.goto("/");
  await expect(page.getByLabel("Space")).toHaveValue("personal");
  await page.getByRole("button", { name: /^notes/ }).click();
  expect(requests).toBe(0);
  await page
    .getByRole("button", { name: "Server download queues", exact: true })
    .click();
  const draft = page.getByLabel("Public URLs (one per line)");
  await draft.fill("https://example.com/a\nhttps://example.com/b");
  await page
    .getByRole("button", { name: "Submit server queue", exact: true })
    .click();
  await expect(draft).toHaveValue("");
  expect(bodies[0]).toEqual({
    submissionId: expect.stringMatching(/^[a-f0-9]{32}$/),
    path: "notes",
    space: "personal",
    existing: "skip_verified",
    sources: [
      { type: "direct", url: "https://example.com/a" },
      { type: "direct", url: "https://example.com/b" },
    ],
  });
  await page.getByRole("tab", { name: "Yandex public link" }).click();
  const yandex = page.getByLabel("Stable Yandex public file/folder link");
  await yandex.fill("https://disk.yandex.ru/d/stable-fixture");
  await page
    .getByRole("button", { name: "Submit server queue", exact: true })
    .click();
  await expect(yandex).toHaveValue("");
  expect(bodies[1]).toMatchObject({
    path: "notes",
    space: "personal",
    sources: [
      { type: "yandex", url: "https://disk.yandex.ru/d/stable-fixture" },
    ],
  });
  expect(bodies[0].submissionId).not.toBe(bodies[1].submissionId);
  await page.reload();
  await page
    .getByRole("button", { name: "Server download queues", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "Details queue-one" }),
  ).toBeVisible();
  expect(bodies).toHaveLength(2);
  await expect(page.getByLabel("Public URLs (one per line)")).toHaveValue("");
});

test("lost ACK queries receipt before retry, retains same nonce, never stores URLs or body", async ({
  page,
}) => {
  const events: string[] = [];
  const ids: string[] = [];
  let accepted = false;
  await mockDrive(page, async (route, url) => {
    if (url.pathname.includes("/submissions/")) {
      events.push("receipt");
      return route.fulfill({
        status: accepted ? 200 : 404,
        json: accepted ? summary() : {},
      });
    }
    if (route.request().method() === "POST") {
      events.push("post");
      ids.push(route.request().postDataJSON().submissionId);
      if (ids.length === 1) return route.fulfill({ status: 503, json: {} });
      accepted = true;
      return route.fulfill({ status: 202, json: summary() });
    }
    return fulfillList(route, url, []);
  });
  await open(page);
  const draft = page.getByLabel("Public URLs (one per line)");
  await draft.fill("https://example.com/private-source?signature=secret");
  await page
    .getByRole("button", { name: "Submit server queue", exact: true })
    .click();
  await expect(
    page.getByText("Receipt not found yet.", { exact: false }),
  ).toBeVisible();
  const stored = await page.evaluate(() => ({
    local: { ...localStorage },
    session: { ...sessionStorage },
  }));
  expect(stored.local[pendingKey]).toMatch(/^[a-f0-9]{32}$/);
  expect(JSON.stringify(stored)).not.toMatch(
    /private-source|signature|secret|queue-fixture|sources/,
  );
  await page
    .getByRole("button", {
      name: "Retry unchanged submission (check receipt first)",
      exact: true,
    })
    .click();
  await expect(draft).toHaveValue("");
  expect(ids).toHaveLength(2);
  expect(ids[0]).toBe(ids[1]);
  expect(events).toEqual(["post", "receipt", "receipt", "post"]);
  expect(
    await page.evaluate((key) => localStorage.getItem(key), pendingKey),
  ).toBeNull();
});

test("network ACK loss recovers on reload using only submission receipt, no repeat POST", async ({
  page,
}) => {
  let posts = 0;
  let receiptReady = false;
  await mockDrive(page, async (route, url) => {
    if (route.request().method() === "POST") {
      posts++;
      return route.abort("failed");
    }
    if (url.pathname.includes("/submissions/"))
      return route.fulfill({
        status: receiptReady ? 200 : 503,
        json: receiptReady ? summary() : {},
      });
    return fulfillList(route, url);
  });
  await open(page);
  await page
    .getByLabel("Public URLs (one per line)")
    .fill("https://example.com/one");
  await page
    .getByRole("button", { name: "Submit server queue", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "Check submission receipt" }),
  ).toBeEnabled();
  receiptReady = true;
  await page.reload();
  await page
    .getByRole("button", { name: "Server download queues", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "Queue details: queue-one" }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Check submission receipt" }),
  ).toHaveCount(0);
  expect(posts).toBe(1);
});

test("changed destination cannot duplicate unknown intent; shared space still permits owner management", async ({
  page,
}) => {
  let posts = 0;
  let controls = 0;
  await mockDrive(page, async (route, url) => {
    if (url.pathname.includes("/submissions/"))
      return route.fulfill({ status: 404, json: {} });
    if (url.pathname.endsWith("/pause")) {
      controls++;
      return route.fulfill({ status: 202, json: summary({ state: "paused" }) });
    }
    if (route.request().method() === "POST") {
      posts++;
      return route.fulfill({ status: 503, json: {} });
    }
    return fulfillList(route, url);
  });
  await page.route("**/api/files?**", (route) =>
    route.fulfill({
      json: [
        {
          name: "notes",
          path: "notes",
          size: 0,
          isDir: true,
          lastModified: "2026-01-01",
        },
      ],
    }),
  );
  await open(page);
  await page
    .getByLabel("Public URLs (one per line)")
    .fill("https://example.com/one");
  await page
    .getByRole("button", { name: "Submit server queue", exact: true })
    .click();
  await expect(
    page.getByRole("button", {
      name: "Retry unchanged submission (check receipt first)",
      exact: true,
    }),
  ).toBeEnabled();
  await page.getByRole("button", { name: /^notes/ }).click();
  await page
    .getByRole("button", {
      name: "Retry unchanged submission (check receipt first)",
      exact: true,
    })
    .click();
  await expect(
    page.getByText("Unresolved original submission:", { exact: false }),
  ).toBeVisible();
  expect(posts).toBe(1);
  await page.getByLabel("Space", { exact: true }).selectOption("shared");
  await expect(
    page.getByRole("button", {
      name: "Retry unchanged submission (check receipt first)",
      exact: true,
    }),
  ).toBeDisabled();
  await page.getByRole("button", { name: "Details queue-one" }).click();
  await page.getByRole("button", { name: "Pause queue", exact: true }).click();
  await expect.poll(() => controls).toBe(1);
  expect(posts).toBe(1);
});

test("owner access loss clears queue data and stops controls/polling; accepted server work continues", async ({
  page,
}) => {
  let denied = false;
  let queueRequests = 0;
  await mockDrive(page, async (route, url) => {
    queueRequests++;
    if (denied) return route.fulfill({ status: 403, json: {} });
    return fulfillList(route, url);
  });
  await open(page);
  await page.getByRole("button", { name: "Details queue-one" }).click();
  await expect(
    page.getByRole("button", { name: "Pause queue", exact: true }),
  ).toBeEnabled();
  denied = true;
  await page.getByRole("button", { name: "Refresh server queues" }).click();
  await expect(
    page.getByText("Drive access is unavailable.", { exact: false }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Submit server queue", exact: true }),
  ).toBeDisabled();
  await expect(
    page.getByRole("button", { name: "Details queue-one" }),
  ).toHaveCount(0);
  const count = queueRequests;
  await page.waitForTimeout(4500);
  expect(queueRequests).toBe(count);
});

test("pagination defaults to 100, empty/error recovery and unknown state are visible", async ({
  page,
}) => {
  const queries: string[] = [];
  let unavailable = true;
  await mockDrive(page, async (route, url) => {
    if (url.pathname === "/api/import-queues") {
      queries.push(url.search);
      if (unavailable) return route.fulfill({ status: 500, json: {} });
      const second = url.searchParams.get("offset") === "100";
      return route.fulfill({
        json: {
          queues: second
            ? []
            : [summary({ state: "future_state", errorCode: "source_failed" })],
          total: second ? 101 : 101,
          offset: second ? 100 : 0,
          limit: 100,
        },
      });
    }
    if (url.pathname.endsWith("/items")) {
      queries.push(url.search);
      const second = url.searchParams.get("offset") === "100";
      return route.fulfill({
        json: {
          items: second ? [] : [item("unknown", "future_item_state")],
          total: 101,
          offset: second ? 100 : 0,
          limit: 100,
        },
      });
    }
    return route.fulfill({ json: summary({ state: "future_state" }) });
  });
  await open(page);
  await expect(
    page.getByText("Server queue request failed (HTTP 500).", { exact: true }),
  ).toBeVisible();
  unavailable = false;
  await page.getByRole("button", { name: "Refresh server queues" }).click();
  await expect(
    page.getByText("Unknown state: future_state (not confirmed complete)", {
      exact: false,
    }),
  ).toBeVisible();
  await expect(
    page.getByText("Source/queue error: source_failed.", { exact: false }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Details queue-one" }).click();
  await expect(
    page.getByText("Unknown state: future_item_state", { exact: false }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Next items", exact: true }).click();
  await expect(
    page.getByText("No items on this page; discovery may still be running."),
  ).toBeVisible();
  await page.getByRole("button", { name: "Next queues", exact: true }).click();
  await expect(
    page.getByText("No server queues.", { exact: true }),
  ).toBeVisible();
  expect(queries).toContain("?offset=0&limit=100");
  expect(queries).toContain("?offset=100&limit=100");
});

test("pause/resume/cancel, saving conflict, failed-only retry and needs_verification safety", async ({
  page,
}) => {
  let state = "running";
  const actions: string[] = [];
  await mockDrive(page, async (route, url) => {
    if (route.request().method() !== "GET") {
      expect(route.request().headers().authorization).toBe(
        "Bearer queue-fixture",
      );
      actions.push(`${route.request().method()} ${url.pathname}`);
      // The item became saving after it was rendered queued: server wins.
      if (route.request().method() === "DELETE")
        return route.fulfill({ status: 409, json: {} });
      if (url.pathname.endsWith("/pause")) state = "paused";
      if (url.pathname.endsWith("/resume")) state = "running";
      if (url.pathname.endsWith("/cancel")) state = "cancelling";
      return route.fulfill({
        status: 202,
        json: url.pathname.endsWith("/retry")
          ? item("failed", "queued")
          : summary({ state }),
      });
    }
    if (url.pathname.endsWith("/items"))
      return route.fulfill({
        json: {
          items: [
            item("failed", "failed", { errorCode: "destination_conflict" }),
            item("uncertain", "needs_verification"),
            item("saving", "saving"),
            item("pending", "queued"),
            item("waiting", "retry_wait", { nextAttemptAt: "2026-01-02" }),
          ],
          total: 5,
          offset: 0,
          limit: 100,
        },
      });
    if (url.pathname === "/api/import-queues")
      return route.fulfill({
        json: { queues: [summary({ state })], total: 1, offset: 0, limit: 100 },
      });
    return route.fulfill({ json: summary({ state }) });
  });
  await open(page);
  await page.getByRole("button", { name: "Details queue-one" }).click();
  await expect(
    page.getByText("Saving — not complete, even if all bytes arrived."),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Cancel item saving.bin", exact: true }),
  ).toBeDisabled();
  await expect(
    page.getByRole("button", { name: /Retry failed item/ }),
  ).toHaveCount(1);
  await expect(page.getByText("Retry waiting until 2026-01-02")).toBeVisible();
  await expect(
    page.getByText("Item error: destination_conflict"),
  ).toBeVisible();
  await page.getByRole("button", { name: "Pause queue", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "Resume queue", exact: true }),
  ).toBeEnabled();
  await page.getByRole("button", { name: "Resume queue", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "Pause queue", exact: true }),
  ).toBeEnabled();
  await page
    .getByRole("button", { name: "Retry failed item failed.bin", exact: true })
    .click();
  await page
    .getByRole("button", { name: "Cancel item pending.bin", exact: true })
    .click();
  await expect(
    page.getByText(
      "Conflict (409): saving may finish; verify the destination. No overwrite or rollback is promised.",
      { exact: true },
    ),
  ).toBeVisible();
  await page
    .getByRole("button", {
      name: "Cancel queue (saving may finish)",
      exact: true,
    })
    .click();
  await expect(
    page.getByRole("button", {
      name: "Cancel queue (saving may finish)",
      exact: true,
    }),
  ).toBeDisabled();
  await expect.poll(() => actions.length).toBe(5);
  expect(actions).toContain(
    "POST /api/import-queues/queue-one/items/failed/retry",
  );
  expect(actions.join(" ")).not.toContain("uncertain/retry");
});

test("definitive rejection preserves unchanged submission ID; editing the draft creates a new intent", async ({
  page,
}) => {
  const bodies: { submissionId: string; sources: { url: string }[] }[] = [];
  await mockDrive(page, async (route, url) => {
    if (route.request().method() === "POST") {
      bodies.push(route.request().postDataJSON());
      return route.fulfill({ status: 400, json: {} });
    }
    return fulfillList(route, url, []);
  });
  await open(page);
  const draft = page.getByLabel("Public URLs (one per line)");
  const submit = page.getByRole("button", {
    name: "Submit server queue",
    exact: true,
  });
  await draft.fill("https://example.com/first");
  await submit.click();
  await expect(
    page.getByText("Server queue request failed (HTTP 400).", { exact: true }),
  ).toBeVisible();
  await submit.click();
  await expect.poll(() => bodies.length).toBe(2);
  expect(bodies[0].submissionId).toBe(bodies[1].submissionId);
  await draft.fill("https://example.com/changed");
  await submit.click();
  await expect.poll(() => bodies.length).toBe(3);
  expect(bodies[2].submissionId).not.toBe(bodies[1].submissionId);
});

test("expired authentication reuses existing apiFetch login behavior without token refresh", async ({
  page,
}) => {
  let queueRequests = 0;
  // The existing auth redirect is fulfilled locally, never by a live portal.
  await page.route("https://main.i3alumba.ru/**", (route) =>
    route.fulfill({
      contentType: "text/html",
      body: "<h1>Local login fixture</h1>",
    }),
  );
  await mockDrive(page, async (route) => {
    queueRequests++;
    return route.fulfill({ status: 401, json: {} });
  });
  await open(page);
  await expect(page).toHaveURL("https://main.i3alumba.ru/login");
  await expect(
    page.getByRole("heading", { name: "Local login fixture" }),
  ).toBeVisible();
  expect(queueRequests).toBe(1);
});

test("422 duplicate destinations are definitive and correctable without weakening 409 recovery", async ({
  page,
}) => {
  const bodies: { submissionId: string; sources: { url: string }[] }[] = [];
  let receipts = 0;
  await mockDrive(page, async (route, url) => {
    if (url.pathname.includes("/submissions/")) {
      receipts++;
      return route.fulfill({ status: 404, json: {} });
    }
    if (route.request().method() === "POST") {
      bodies.push(route.request().postDataJSON());
      return route.fulfill({
        status: bodies.length === 1 ? 422 : 202,
        json:
          bodies.length === 1 ? { error: "destination_collision" } : summary(),
      });
    }
    return fulfillList(route, url, []);
  });
  await open(page);
  const draft = page.getByLabel("Public URLs (one per line)");
  await draft.fill(
    "https://one.example/file.bin\nhttps://two.example/file.bin",
  );
  await page
    .getByRole("button", { name: "Submit server queue", exact: true })
    .click();
  await expect(
    page.getByText("Server queue request failed (HTTP 422).", { exact: true }),
  ).toBeVisible();
  await expect(draft).toBeEnabled();
  await expect(draft).toHaveValue(
    "https://one.example/file.bin\nhttps://two.example/file.bin",
  );
  expect(
    await page.evaluate((key) => localStorage.getItem(key), pendingKey),
  ).toBeNull();
  await draft.fill(
    "https://one.example/file.bin\nhttps://two.example/other.bin",
  );
  await page
    .getByRole("button", { name: "Submit server queue", exact: true })
    .click();
  await expect(draft).toHaveValue("");
  expect(bodies).toHaveLength(2);
  expect(bodies[1].submissionId).not.toBe(bodies[0].submissionId);
  expect(receipts).toBe(0);
});

test("failed initial acceptance and unavailable receipt never acknowledge a blocked placeholder", async ({
  page,
}) => {
  let submittedId = "";
  await mockDrive(page, async (route, url) => {
    if (route.request().method() === "POST") {
      submittedId = route.request().postDataJSON().submissionId;
      return route.fulfill({ status: 503, json: {} });
    }
    if (url.pathname.includes("/submissions/"))
      return route.fulfill({ status: 503, json: {} });
    return fulfillList(route, url, []);
  });
  await open(page);
  const draft = page.getByLabel("Public URLs (one per line)");
  await draft.fill("https://example.com/retained-private-source");
  await page
    .getByRole("button", { name: "Submit server queue", exact: true })
    .click();
  await expect(
    page.getByText("Server queue request failed (HTTP 503).", { exact: true }),
  ).toBeVisible();
  await expect(draft).toHaveValue(
    "https://example.com/retained-private-source",
  );
  await expect(
    page.getByRole("button", { name: "Check submission receipt" }),
  ).toBeEnabled();
  await expect(
    page.getByRole("heading", { name: /Queue details:/ }),
  ).toHaveCount(0);
  expect(
    await page.evaluate((key) => localStorage.getItem(key), pendingKey),
  ).toBe(submittedId);
});

for (const status of [404, 410]) {
  test(`reload receipt ${status} can only be forgotten with explicit local-only confirmation`, async ({
    page,
  }) => {
    const id = "a".repeat(32);
    let posts = 0;
    await page.addInitScript(({ key, id }) => localStorage.setItem(key, id), {
      key: pendingKey,
      id,
    });
    await mockDrive(page, async (route, url) => {
      if (route.request().method() !== "GET") posts++;
      if (url.pathname.includes("/submissions/"))
        return route.fulfill({ status, json: {} });
      return fulfillList(route, url, []);
    });
    await open(page);
    await expect(
      page.getByRole("button", { name: "Check submission receipt" }),
    ).toBeVisible();
    await expect(
      page.getByText("The original queue may still run.", { exact: false }),
    ).toBeVisible();
    await page
      .getByRole("button", { name: "Forget receipt tracking", exact: true })
      .click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toContainText("does not cancel or pause server work");
    await dialog.getByRole("button", { name: "Keep tracking" }).click();
    expect(
      await page.evaluate((key) => localStorage.getItem(key), pendingKey),
    ).toBe(id);
    await page
      .getByRole("button", { name: "Forget receipt tracking", exact: true })
      .click();
    await dialog
      .getByRole("button", { name: "Confirm forget tracking" })
      .click();
    await expect(page.getByLabel("Public URLs (one per line)")).toBeEnabled();
    await expect(page.getByLabel("Public URLs (one per line)")).toHaveValue("");
    expect(
      await page.evaluate((key) => localStorage.getItem(key), pendingKey),
    ).toBeNull();
    expect(posts).toBe(0);
    const storage = await page.evaluate(() =>
      JSON.stringify([{ ...localStorage }, { ...sessionStorage }]),
    );
    expect(storage).not.toMatch(/https:|sources|url|queue-fixture/);
  });
}

async function driveAccessFixture(page: Page, denied: () => boolean) {
  await page.route("**/api/spaces", (route) =>
    denied()
      ? route.fulfill({ status: 403, json: {} })
      : route.fulfill({
          json: [{ id: "personal", name: "My files", permission: "edit" }],
        }),
  );
}
async function detectDriveDenialAndRecover(
  page: Page,
  setDenied: (value: boolean) => void,
) {
  setDenied(true);
  await expect(
    page.getByText("Access to Drive is not granted to this account.", {
      exact: true,
    }),
  ).toBeVisible({ timeout: 8000 });
  setDenied(false);
  await page.getByRole("button", { name: "Retry", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "Create", exact: true }),
  ).toBeEnabled();
}

test("Drive-level denial while collapsed terminally clears owner A details and retry body before owner B recovery", async ({
  page,
}) => {
  let denied = false;
  let ownerB = false;
  let posts = 0;
  let requestsUnderB = 0;
  await mockDrive(page, async (route, url) => {
    if (ownerB) {
      requestsUnderB++;
      return route.fulfill({ status: 404, json: {} });
    }
    if (route.request().method() === "POST") {
      posts++;
      return route.fulfill({ status: 503, json: {} });
    }
    if (url.pathname.includes("/submissions/"))
      return route.fulfill({ status: 404, json: {} });
    if (url.pathname.endsWith("/items"))
      return route.fulfill({
        json: {
          items: [item("owner-a-private-item", "queued")],
          total: 1,
          offset: 0,
          limit: 100,
        },
      });
    return fulfillList(route, url);
  });
  await driveAccessFixture(page, () => denied);
  await open(page);
  const panel = page.getByRole("button", {
    name: "Server download queues",
    exact: true,
  });
  await page.getByRole("button", { name: "Details queue-one" }).click();
  await expect(
    page.getByText("1. owner-a-private-item.bin", { exact: false }),
  ).toBeVisible();
  const draft = page.getByLabel("Public URLs (one per line)");
  await draft.fill("https://example.com/owner-a-private-url");
  await page
    .getByRole("button", { name: "Submit server queue", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "Check submission receipt" }),
  ).toBeEnabled();
  await panel.click();
  await panel.click();
  await expect(draft).toHaveValue("https://example.com/owner-a-private-url");
  await expect(
    page.getByText("1. owner-a-private-item.bin", { exact: false }),
  ).toBeVisible();
  await panel.click();
  await detectDriveDenialAndRecover(page, (value) => {
    denied = value;
    if (!value) ownerB = true;
  });
  await panel.click();
  await expect(draft).toHaveValue("");
  await expect(
    page.getByText("owner-a-private-item.bin", { exact: false }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("heading", { name: /Queue details:/ }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", {
      name: "Retry unchanged submission (check receipt first)",
      exact: true,
    }),
  ).toBeDisabled();
  await expect(
    page.getByText("Queue controls and polling stopped;", { exact: false }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Forget receipt tracking", exact: true })
    .click();
  await page.getByRole("button", { name: "Confirm forget tracking" }).click();
  expect(
    await page.evaluate((key) => localStorage.getItem(key), pendingKey),
  ).toBeNull();
  expect(posts).toBe(1);
  expect(requestsUnderB).toBe(0);
});

async function settleLateAcceptance(
  page: Page,
  delayed: "receipt" | "post",
  release: () => void,
) {
  const response = page.waitForResponse((response) => {
    const path = new URL(response.url()).pathname;
    return delayed === "post"
      ? path === "/api/import-queues" &&
          response.request().method() === "POST" &&
          response.status() === 202
      : path.includes("/submissions/") && response.status() === 200;
  });
  release();
  await (await response).finished();
  // Let the fetch JSON continuation and its React updates run before asserting absence.
  await page.evaluate(
    () =>
      new Promise<void>((resolve) =>
        requestAnimationFrame(() => requestAnimationFrame(() => resolve())),
      ),
  );
}

for (const delayed of ["receipt", "post"] as const) {
  test(`delayed ${delayed} acceptance cannot cross collapsed Drive denial and owner recovery`, async ({
    page,
  }) => {
    let denied = false;
    let release!: () => void;
    let started = false;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    let posts = 0;
    await mockDrive(page, async (route, url) => {
      if (route.request().method() === "POST") {
        posts++;
        if (delayed === "post") {
          started = true;
          await gate;
          return route.fulfill({
            status: 202,
            json: summary({ id: "old-owner-receipt" }),
          });
        }
        return route.fulfill({ status: 503, json: {} });
      }
      if (url.pathname.includes("/submissions/")) {
        started = true;
        await gate;
        return route.fulfill({ json: summary({ id: "old-owner-receipt" }) });
      }
      return fulfillList(route, url, []);
    });
    await driveAccessFixture(page, () => denied);
    await open(page);
    await page
      .getByLabel("Public URLs (one per line)")
      .fill("https://example.com/owner-a-source");
    await page
      .getByRole("button", { name: "Submit server queue", exact: true })
      .click();
    await expect.poll(() => started).toBe(true);
    const nonce = await page.evaluate(
      (key) => localStorage.getItem(key),
      pendingKey,
    );
    await page
      .getByRole("button", { name: "Server download queues", exact: true })
      .click();
    await detectDriveDenialAndRecover(page, (value) => {
      denied = value;
    });
    await settleLateAcceptance(page, delayed, release);
    await page
      .getByRole("button", { name: "Server download queues", exact: true })
      .click();
    await expect(page.getByLabel("Public URLs (one per line)")).toHaveValue("");
    await expect(
      page.getByRole("heading", { name: /Queue details:/ }),
    ).toHaveCount(0);
    await expect(
      page.getByRole("button", { name: "Check submission receipt" }),
    ).toBeDisabled();
    expect(
      await page.evaluate((key) => localStorage.getItem(key), pendingKey),
    ).toBe(nonce);
    expect(posts).toBe(1);
  });

  test(`explicit forgetting fences a late ${delayed} callback from a newer pending draft`, async ({
    page,
  }) => {
    let release!: () => void;
    let started = false;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    const ids: string[] = [];
    await mockDrive(page, async (route, url) => {
      if (route.request().method() === "POST") {
        ids.push(route.request().postDataJSON().submissionId);
        if (ids.length === 1 && delayed === "post") {
          started = true;
          await gate;
          return route.fulfill({
            status: 202,
            json: summary({ id: "stale-acceptance" }),
          });
        }
        return route.fulfill({ status: 503, json: {} });
      }
      if (url.pathname.includes("/submissions/")) {
        if (url.pathname.endsWith(ids[0]) && delayed === "receipt") {
          started = true;
          await gate;
          return route.fulfill({ json: summary({ id: "stale-acceptance" }) });
        }
        return route.fulfill({ status: 503, json: {} });
      }
      return fulfillList(route, url, []);
    });
    await open(page);
    const draft = page.getByLabel("Public URLs (one per line)");
    await draft.fill("https://example.com/original-private-source");
    await page
      .getByRole("button", { name: "Submit server queue", exact: true })
      .click();
    await expect.poll(() => started).toBe(true);
    await page
      .getByRole("button", { name: "Forget receipt tracking", exact: true })
      .click();
    await expect(page.getByRole("dialog")).toContainText(
      "could duplicate work",
    );
    await page.getByRole("button", { name: "Confirm forget tracking" }).click();
    await draft.fill("https://example.com/new-private-source");
    await page
      .getByRole("button", { name: "Submit server queue", exact: true })
      .click();
    await expect(
      page.getByText("Server queue request failed (HTTP 503).", {
        exact: true,
      }),
    ).toBeVisible();
    await expect.poll(() => ids.length).toBe(2);
    await settleLateAcceptance(page, delayed, release);
    await expect(draft).toHaveValue("https://example.com/new-private-source");
    await expect(
      page.getByRole("heading", { name: /Queue details:/ }),
    ).toHaveCount(0);
    expect(
      await page.evaluate((key) => localStorage.getItem(key), pendingKey),
    ).toBe(ids[1]);
    expect(ids[0]).not.toBe(ids[1]);
    expect(
      await page.evaluate(() =>
        JSON.stringify([{ ...localStorage }, { ...sessionStorage }]),
      ),
    ).not.toMatch(/private-source|sources|https:/);
  });
}

test("a selected queue rejected with 404 clears cached details and items", async ({
  page,
}) => {
  let missing = false;
  await mockDrive(page, async (route, url) => {
    if (url.pathname === "/api/import-queues/queue-one" && missing)
      return route.fulfill({ status: 404, json: {} });
    if (url.pathname.endsWith("/items"))
      return route.fulfill({
        json: {
          items: [item("cached-private-item", "queued")],
          total: 1,
          offset: 0,
          limit: 100,
        },
      });
    return fulfillList(route, url);
  });
  await open(page);
  await page.getByRole("button", { name: "Details queue-one" }).click();
  await expect(
    page.getByText("1. cached-private-item.bin", { exact: false }),
  ).toBeVisible();
  missing = true;
  await page.getByRole("button", { name: "Refresh server queues" }).click();
  await expect(
    page.getByText("Server queue request failed (HTTP 404).", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("heading", { name: /Queue details:/ }),
  ).toHaveCount(0);
  await expect(
    page.getByText("cached-private-item.bin", { exact: false }),
  ).toHaveCount(0);
});

test("ambiguous 409 submission_changed never unlocks or auto-forgets the original intent", async ({
  page,
}) => {
  let posts = 0;
  let nonce = "";
  await mockDrive(page, async (route, url) => {
    if (route.request().method() === "POST") {
      posts++;
      nonce = route.request().postDataJSON().submissionId;
      return route.fulfill({
        status: 409,
        json: { error: "submission_changed" },
      });
    }
    if (url.pathname.includes("/submissions/"))
      return route.fulfill({ status: 404, json: {} });
    return fulfillList(route, url, []);
  });
  await open(page);
  const draft = page.getByLabel("Public URLs (one per line)");
  await draft.fill("https://example.com/uncertain-original");
  await page
    .getByRole("button", { name: "Submit server queue", exact: true })
    .click();
  await expect(
    page.getByText("Receipt not found yet.", { exact: false }),
  ).toBeVisible();
  await expect(draft).toBeDisabled();
  await expect(draft).toHaveValue("https://example.com/uncertain-original");
  await page.getByRole("button", { name: "Check submission receipt" }).click();
  await expect(
    page.getByRole("button", { name: "Check submission receipt" }),
  ).toBeEnabled();
  expect(posts).toBe(1);
  expect(
    await page.evaluate((key) => localStorage.getItem(key), pendingKey),
  ).toBe(nonce);
});

test("in-flight owner A item reads cannot install data after collapsed Drive denial and recovery", async ({
  page,
}) => {
  let denied = false;
  let release!: () => void;
  let started = false;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  await mockDrive(page, async (route, url) => {
    if (url.pathname.endsWith("/items")) {
      started = true;
      await gate;
      return route.fulfill({
        json: {
          items: [item("stale-owner-a-item", "queued")],
          total: 1,
          offset: 0,
          limit: 100,
        },
      });
    }
    return fulfillList(route, url);
  });
  await driveAccessFixture(page, () => denied);
  await open(page);
  await page.getByRole("button", { name: "Details queue-one" }).click();
  await expect.poll(() => started).toBe(true);
  await page
    .getByRole("button", { name: "Server download queues", exact: true })
    .click();
  await detectDriveDenialAndRecover(page, (value) => {
    denied = value;
  });
  const response = page.waitForResponse((response) =>
    new URL(response.url()).pathname.endsWith("/items"),
  );
  release();
  await (await response).finished();
  await page
    .getByRole("button", { name: "Server download queues", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: /Queue details:/ }),
  ).toHaveCount(0);
  await expect(
    page.getByText("stale-owner-a-item", { exact: false }),
  ).toHaveCount(0);
});
