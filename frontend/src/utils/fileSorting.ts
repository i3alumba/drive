import type { DriveObject } from "../types";

export type SortKey = "name" | "modified" | "size";
export type SortDirection = "asc" | "desc";

const collator = new Intl.Collator(undefined, {
  numeric: true,
  sensitivity: "base",
});

function modifiedValue(item: DriveObject): number | null {
  const parsed = Date.parse(item.lastModified);
  return Number.isFinite(parsed) ? parsed : null;
}

function sizeValue(item: DriveObject): number | null {
  return typeof item.size === "number" &&
    Number.isFinite(item.size) &&
    item.size >= 0
    ? item.size
    : null;
}

function compareBinaryPath(a: string, b: string): number {
  if (a < b) return -1;
  if (a > b) return 1;
  return 0;
}

/**
 * Returns a new sorted array; the input is never mutated.
 * Directories always precede files regardless of direction. Entries with
 * invalid/missing dates or sizes sort last within their group regardless of
 * direction. Ties resolve by natural name ascending, then binary path.
 */
export function sortDriveObjects(
  items: DriveObject[],
  key: SortKey = "name",
  direction: SortDirection = "asc",
): DriveObject[] {
  const sign = direction === "desc" ? -1 : 1;
  return [...items].sort((a, b) => {
    if (a.isDir !== b.isDir) return a.isDir ? -1 : 1;

    let primary = 0;
    if (key === "name") {
      primary = collator.compare(a.name, b.name) * sign;
    } else {
      const av = key === "modified" ? modifiedValue(a) : sizeValue(a);
      const bv = key === "modified" ? modifiedValue(b) : sizeValue(b);
      if (av === null && bv === null) primary = 0;
      else if (av === null) primary = 1;
      else if (bv === null) primary = -1;
      else primary = (av - bv) * sign;
    }
    if (primary !== 0) return primary;

    const byName = collator.compare(a.name, b.name);
    if (byName !== 0) return byName;
    return compareBinaryPath(a.path, b.path);
  });
}
