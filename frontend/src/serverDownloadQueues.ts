import { apiFetch } from "./api";

export interface QueueSummary {
  id: string;
  path: string;
  state: string;
  acceptedAt: string;
  expiresAt: string;
  totalItems: number;
  completedItems: number;
  failedItems: number;
  cancelledItems: number;
  queuedItems: number;
  activeItems: number;
  savingItems: number;
  needsVerificationItems: number;
  totalBytes: number;
  completedBytes: number;
  unknownSizeItems: number;
  discoveryState?: string;
  errorCode?: string;
  normalizedNames?: number;
}

export interface QueueItem {
  id: string;
  queueId: string;
  position: number;
  name: string;
  path: string;
  state: string;
  bytes: number;
  total: number;
  attempts: number;
  totalAttempts?: number;
  sha256?: string;
  nextAttemptAt?: string;
  errorCode?: string;
  reused?: boolean;
}

export interface QueuePage {
  queues: QueueSummary[];
  total: number;
  offset: number;
  limit: number;
}
export interface QueueItemPage {
  items: QueueItem[];
  total: number;
  offset: number;
  limit: number;
}
export interface QueueSubmission {
  submissionId: string;
  path: string;
  space: "personal";
  existing: "error" | "skip_verified";
  sources: { type: "direct" | "yandex"; url: string }[];
}

export class QueueRequestError extends Error {
  constructor(public status: number) {
    // Never display backend text: source locators must remain private.
    super(
      status === 409
        ? "Conflict (409): saving may finish; verify the destination. No overwrite or rollback is promised."
        : `Server queue request failed (HTTP ${status}).`,
    );
  }
}
const root = "/api/import-queues";
export const queuePath = (id: string) => `${root}/${encodeURIComponent(id)}`;
export async function queueRequest<T>(
  path: string,
  init?: RequestInit,
  expectedStatus?: number,
): Promise<T> {
  const response = await apiFetch(path, init);
  if (!response.ok) throw new QueueRequestError(response.status);
  if (expectedStatus && response.status !== expectedStatus)
    throw new Error("Server acceptance acknowledgement unavailable");
  return response.json() as Promise<T>;
}
export const queueApi = {
  list: (offset: number) =>
    queueRequest<QueuePage>(`${root}?offset=${offset}&limit=100`),
  detail: (id: string) => queueRequest<QueueSummary>(queuePath(id)),
  items: (id: string, offset: number) =>
    queueRequest<QueueItemPage>(
      `${queuePath(id)}/items?offset=${offset}&limit=100`,
    ),
  receipt: (id: string) =>
    queueRequest<QueueSummary>(`${root}/submissions/${encodeURIComponent(id)}`),
  submit: (body: QueueSubmission) =>
    queueRequest<QueueSummary>(
      root,
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      },
      202,
    ),
  control: (id: string, action: "pause" | "resume" | "cancel") =>
    queueRequest<QueueSummary>(`${queuePath(id)}/${action}`, {
      method: "POST",
    }),
  cancelItem: (id: string, itemId: string) =>
    queueRequest<QueueItem>(
      `${queuePath(id)}/items/${encodeURIComponent(itemId)}`,
      { method: "DELETE" },
    ),
  retryItem: (id: string, itemId: string) =>
    queueRequest<QueueItem>(
      `${queuePath(id)}/items/${encodeURIComponent(itemId)}/retry`,
      { method: "POST" },
    ),
};

export const pendingSubmissionKey = "drive-pending-import-submission";
export function readPendingSubmission(): string | null {
  try {
    const id = localStorage.getItem(pendingSubmissionKey);
    return id && /^[a-f0-9]{32}$/.test(id) ? id : null;
  } catch {
    return null;
  }
}
export function storePendingSubmission(id: string | null) {
  try {
    if (id) localStorage.setItem(pendingSubmissionKey, id);
    else localStorage.removeItem(pendingSubmissionKey);
  } catch {
    /* The receipt can still be reconciled in this page's memory. */
  }
}
export function newSubmissionId() {
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), (byte) =>
    byte.toString(16).padStart(2, "0"),
  ).join("");
}
