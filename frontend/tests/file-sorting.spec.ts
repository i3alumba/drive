import { expect, test, type Page } from "@playwright/test";
import {
  sortDriveObjects,
  type SortDirection,
  type SortKey,
} from "../src/utils/fileSorting";
import type { DriveObject } from "../src/types";

function file(name: string, overrides: Partial<DriveObject> = {}): DriveObject {
  return {
    name,
    path: name,
    size: 0,
    lastModified: "2026-01-01T00:00:00Z",
    isDir: false,
    ...overrides,
  };
}

function names(items: DriveObject[]): string[] {
  return items.map((item) => item.name);
}

test.describe("sortDriveObjects", () => {
  const directions: SortDirection[] = ["asc", "desc"];

  test("sorts by name with natural numeric ordering in both directions", () => {
    const items = [file("video10.mp4"), file("video2.mp4"), file("Video1.mp4")];
    expect(names(sortDriveObjects(items, "name", "asc"))).toEqual([
      "Video1.mp4",
      "video2.mp4",
      "video10.mp4",
    ]);
    expect(names(sortDriveObjects(items, "name", "desc"))).toEqual([
      "video10.mp4",
      "video2.mp4",
      "Video1.mp4",
    ]);
  });

  test("defaults to name ascending", () => {
    const items = [file("b.txt"), file("a.txt")];
    expect(names(sortDriveObjects(items))).toEqual(["a.txt", "b.txt"]);
  });

  test("sorts by date modified in both directions", () => {
    const items = [
      file("new.txt", { lastModified: "2026-03-01T00:00:00Z" }),
      file("old.txt", { lastModified: "2020-01-01T00:00:00Z" }),
      file("mid.txt", { lastModified: "2023-06-15T12:00:00Z" }),
    ];
    expect(names(sortDriveObjects(items, "modified", "asc"))).toEqual([
      "old.txt",
      "mid.txt",
      "new.txt",
    ]);
    expect(names(sortDriveObjects(items, "modified", "desc"))).toEqual([
      "new.txt",
      "mid.txt",
      "old.txt",
    ]);
  });

  test("sorts by size in both directions", () => {
    const items = [
      file("big", { size: 300 }),
      file("small", { size: 1 }),
      file("medium", { size: 42 }),
    ];
    expect(names(sortDriveObjects(items, "size", "asc"))).toEqual([
      "small",
      "medium",
      "big",
    ]);
    expect(names(sortDriveObjects(items, "size", "desc"))).toEqual([
      "big",
      "medium",
      "small",
    ]);
  });

  for (const direction of directions) {
    test(`directories always come first (${direction})`, () => {
      const items = [
        file("aaa.txt"),
        file("zzz-dir", { isDir: true }),
        file("mmm.txt"),
        file("aaa-dir", { isDir: true }),
      ];
      const keys: SortKey[] = ["name", "modified", "size"];
      for (const key of keys) {
        const sorted = sortDriveObjects(items, key, direction);
        expect(sorted.slice(0, 2).every((item) => item.isDir)).toBe(true);
        expect(sorted.slice(2).every((item) => !item.isDir)).toBe(true);
      }
    });

    test(`invalid dates and sizes go last within their group (${direction})`, () => {
      const byModified = sortDriveObjects(
        [
          file("bad-date", { lastModified: "not-a-date" }),
          file("good", { lastModified: "2020-01-01T00:00:00Z" }),
          file("missing-date", { lastModified: "" }),
        ],
        "modified",
        direction,
      );
      expect(names(byModified)[0]).toBe("good");
      expect(names(byModified).slice(1).sort()).toEqual([
        "bad-date",
        "missing-date",
      ]);

      const bySize = sortDriveObjects(
        [
          file("negative", { size: -5 }),
          file("good", { size: 10 }),
          file("nan", { size: Number.NaN }),
        ],
        "size",
        direction,
      );
      expect(names(bySize)[0]).toBe("good");
      expect(names(bySize).slice(1).sort()).toEqual(["nan", "negative"]);
    });

    test(`ties fall back to natural name ascending then binary path (${direction})`, () => {
      const same = "2026-01-01T00:00:00Z";
      const items = [
        file("item10", { path: "b/item10", size: 5, lastModified: same }),
        file("item2", { path: "b/item2", size: 5, lastModified: same }),
        file("item2", { path: "a/item2", size: 5, lastModified: same }),
      ];
      for (const key of ["modified", "size"] as SortKey[]) {
        const sorted = sortDriveObjects(items, key, direction);
        expect(sorted.map((item) => item.path)).toEqual([
          "a/item2",
          "b/item2",
          "b/item10",
        ]);
      }
    });
  }

  test("never mutates the input array", () => {
    const items = [file("b"), file("a"), file("c", { isDir: true })];
    const before = items.map((item) => item.name);
    const sorted = sortDriveObjects(items, "size", "desc");
    expect(items.map((item) => item.name)).toEqual(before);
    expect(sorted).not.toBe(items);
  });
});

const fixtureFiles: DriveObject[] = [
  file("folder", { isDir: true }),
  file("video10.mp4", { size: 10, lastModified: "2026-02-01T00:00:00Z" }),
  file("video2.mp4", { size: 500, lastModified: "2026-01-01T00:00:00Z" }),
];

const insideFolder: DriveObject[] = [
  file("b.txt", { path: "folder/b.txt" }),
  file("a.txt", { path: "folder/a.txt" }),
];

async function mockDrive(
  page: Page,
  permission: "read" | "edit" = "edit",
  files: () => DriveObject[] = () => fixtureFiles,
) {
  await page.context().route("**/api/**", (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/spaces")
      return route.fulfill({
        json: [{ id: "personal", name: "My files", permission }],
      });
    if (url.pathname === "/api/files") {
      const path = url.searchParams.get("path") ?? "";
      return route.fulfill({
        json: path === "folder" ? insideFolder : files(),
      });
    }
    return route.fulfill({ json: [] });
  });
}

async function fileOrder(page: Page, candidates: string[]): Promise<string[]> {
  return page.$$eval(
    "button",
    (buttons, wanted) =>
      buttons
        .map(
          (button) =>
            wanted.find((name) =>
              (button.textContent ?? "").startsWith(name),
            ) ?? "",
        )
        .filter((name) => name !== ""),
    candidates,
  );
}

test.describe("drive page sort controls", () => {
  const order = ["folder", "video10.mp4", "video2.mp4"];

  test("sorts by each key and direction, keeping folders first", async ({
    page,
  }) => {
    await mockDrive(page);
    await page.goto("/");

    // Default: name ascending, folder first, natural numeric order.
    await expect
      .poll(() => fileOrder(page, order))
      .toEqual(["folder", "video2.mp4", "video10.mp4"]);
    await expect(
      page.getByRole("combobox", { name: "Sort files by" }),
    ).toHaveText("Name");

    // Switch to size; direction (ascending) is preserved.
    await page.getByRole("combobox", { name: "Sort files by" }).click();
    await page.getByRole("option", { name: "Size" }).click();
    await expect
      .poll(() => fileOrder(page, order))
      .toEqual(["folder", "video10.mp4", "video2.mp4"]);

    // Toggle direction; aria-label advertises the next action.
    const toggle = page.getByRole("button", { name: "Sort descending" });
    await expect(toggle).toHaveText("Ascending");
    await toggle.click();
    const descending = page.getByRole("button", { name: "Sort ascending" });
    await expect(descending).toHaveText("Descending");
    await expect
      .poll(() => fileOrder(page, order))
      .toEqual(["folder", "video2.mp4", "video10.mp4"]);

    // Date modified, descending: folder stays first.
    await page.getByRole("combobox", { name: "Sort files by" }).click();
    await page.getByRole("option", { name: "Date modified" }).click();
    await expect
      .poll(() => fileOrder(page, order))
      .toEqual(["folder", "video10.mp4", "video2.mp4"]);
  });

  test("keeps selection across background refresh and folder navigation", async ({
    page,
  }) => {
    let refreshedPayload = false;
    await mockDrive(page, "edit", () =>
      refreshedPayload
        ? [fixtureFiles[0], file("aaa.txt", { size: 1 }), fixtureFiles[1]]
        : fixtureFiles,
    );
    await page.goto("/");
    await page.getByRole("combobox", { name: "Sort files by" }).click();
    await page.getByRole("option", { name: "Size" }).click();
    await expect(
      page.getByRole("combobox", { name: "Sort files by" }),
    ).toHaveText("Size");

    // Background refresh (4s polling interval) keeps the sort applied.
    refreshedPayload = true;
    await expect
      .poll(() => fileOrder(page, ["folder", "aaa.txt", "video10.mp4"]), {
        timeout: 15_000,
      })
      .toEqual(["folder", "aaa.txt", "video10.mp4"]);
    await expect(
      page.getByRole("combobox", { name: "Sort files by" }),
    ).toHaveText("Size");

    // Folder navigation keeps the selection too.
    await page.getByRole("button", { name: /^folder/ }).click();
    await expect
      .poll(() => fileOrder(page, ["a.txt", "b.txt"]))
      .toEqual(["a.txt", "b.txt"]);
    await expect(
      page.getByRole("combobox", { name: "Sort files by" }),
    ).toHaveText("Size");
  });

  test("sorting works in a read-only shared space", async ({ page }) => {
    await mockDrive(page, "read");
    await page.goto("/");
    await expect(
      page.getByRole("combobox", { name: "Sort files by" }),
    ).toBeEnabled();
    await expect(
      page.getByRole("button", { name: "Sort descending" }),
    ).toBeEnabled();
    await page.getByRole("combobox", { name: "Sort files by" }).click();
    await page.getByRole("option", { name: "Size" }).click();
    await expect
      .poll(() => fileOrder(page, order))
      .toEqual(["folder", "video10.mp4", "video2.mp4"]);
  });

  test("controls wrap at 360px and the space selector stays the only native select", async ({
    page,
  }) => {
    await page.setViewportSize({ width: 360, height: 700 });
    await mockDrive(page);
    await page.goto("/");
    await expect(
      page.getByRole("combobox", { name: "Sort files by" }),
    ).toBeVisible();
    await expect(
      page.getByRole("button", { name: "Sort descending" }),
    ).toBeVisible();
    // MUI select must not introduce a second native <select> (theme tests).
    await expect(page.locator("select")).toHaveCount(1);
  });

  test("controls are disabled when the drive fails to load", async ({
    page,
  }) => {
    await page
      .context()
      .route("**/api/**", (route) =>
        route.fulfill({ status: 403, json: { error: "denied" } }),
      );
    await page.goto("/");
    await expect(
      page.getByRole("combobox", { name: "Sort files by" }),
    ).toBeDisabled();
    await expect(
      page.getByRole("button", { name: "Sort descending" }),
    ).toBeDisabled();
  });
});
