import { apiFetch } from "./api";

export type UploadSession = {
  id: string;
  name: string;
  path: string;
  space: string;
  size: number;
  chunkSize: number;
  parts: Record<string, { size: number; sha256: string }>;
  state:
    | "uploading"
    | "committing"
    | "complete"
    | "cancelled"
    | "expired"
    | "uncertain";
  error?: string;
  sha256?: string;
  expiresAt: string;
};

export class UploadHTTPError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly retryAfter = 0,
  ) {
    super(message);
  }
}

function stopped(signal: AbortSignal) {
  if (signal.aborted) throw new DOMException("Upload stopped", "AbortError");
}

function sleep(milliseconds: number, signal: AbortSignal) {
  return new Promise<void>((resolve, reject) => {
    stopped(signal);
    const abort = () => {
      clearTimeout(timer);
      reject(new DOMException("Upload stopped", "AbortError"));
    };
    const timer = setTimeout(() => {
      signal.removeEventListener("abort", abort);
      resolve();
    }, milliseconds);
    signal.addEventListener("abort", abort, { once: true });
  });
}

async function uploadJSON<T>(
  path: string,
  init: RequestInit,
  signal: AbortSignal,
  timeout = 30000,
): Promise<T> {
  for (let attempt = 0; ; attempt++) {
    stopped(signal);
    const controller = new AbortController();
    let timedOut = false;
    const abort = () => controller.abort();
    signal.addEventListener("abort", abort, { once: true });
    const timer = setTimeout(() => {
      timedOut = true;
      controller.abort();
    }, timeout);
    try {
      const response = await apiFetch(path, {
        ...init,
        signal: controller.signal,
      });
      if (!response.ok) {
        let message = `Upload failed (HTTP ${response.status}).`;
        try {
          const body = await response.json();
          if (typeof body.error === "string") message = body.error;
        } catch {
          /* CDN errors can be HTML. */
        }
        throw new UploadHTTPError(
          message,
          response.status,
          Math.min(
            10000,
            Math.max(0, Number(response.headers.get("Retry-After")) * 1000),
          ),
        );
      }
      return (await response.json()) as T;
    } catch (error) {
      stopped(signal);
      const retryable =
        timedOut ||
        error instanceof TypeError ||
        (error instanceof UploadHTTPError &&
          (error.status === 429 ||
            (error.status >= 500 && error.status !== 507)));
      if (!retryable || attempt >= 2)
        throw timedOut
          ? new Error(
              "Upload request timed out. Pause and resume to retry safely.",
            )
          : error;
      await sleep(
        Math.max(
          500 * 2 ** attempt,
          error instanceof UploadHTTPError ? error.retryAfter : 0,
        ),
        signal,
      );
    } finally {
      clearTimeout(timer);
      signal.removeEventListener("abort", abort);
    }
  }
}

function validateSession(session: UploadSession): UploadSession {
  if (
    !session ||
    !/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(
      session.id,
    ) ||
    !Number.isSafeInteger(session.size) ||
    session.size < 0 ||
    session.chunkSize !== 8 * 1024 * 1024 ||
    !session.parts ||
    typeof session.parts !== "object" ||
    ![
      "uploading",
      "committing",
      "complete",
      "cancelled",
      "expired",
      "uncertain",
    ].includes(session.state)
  ) {
    throw new Error("Drive returned an invalid upload session.");
  }
  const count = Math.ceil(session.size / session.chunkSize);
  for (const [key, part] of Object.entries(session.parts)) {
    const index = Number(key);
    if (
      !Number.isInteger(index) ||
      String(index) !== key ||
      index < 0 ||
      index >= count ||
      part.size !==
        Math.min(session.chunkSize, session.size - index * session.chunkSize) ||
      !/^[0-9a-f]{64}$/.test(part.sha256)
    )
      throw new Error("Drive returned an invalid part receipt.");
  }
  return session;
}

async function digest(part: Blob, signal: AbortSignal) {
  stopped(signal);
  if (!globalThis.crypto?.subtle)
    throw new Error(
      "Chunked uploads require HTTPS or localhost for integrity checks.",
    );
  const hash = await crypto.subtle.digest("SHA-256", await part.arrayBuffer());
  stopped(signal);
  return Array.from(new Uint8Array(hash), (byte) =>
    byte.toString(16).padStart(2, "0"),
  ).join("");
}

export async function listUploadSessions(signal: AbortSignal) {
  const sessions = await uploadJSON<UploadSession[]>(
    "/api/uploads",
    {},
    signal,
  );
  if (!Array.isArray(sessions)) throw new Error("Invalid upload session list.");
  return sessions.map(validateSession);
}

export async function cancelUploadSession(id: string) {
  const controller = new AbortController();
  try {
    return validateSession(
      await uploadJSON<UploadSession>(
        `/api/uploads/${encodeURIComponent(id)}`,
        { method: "DELETE" },
        controller.signal,
      ),
    );
  } catch (error) {
    if (error instanceof UploadHTTPError && error.status === 404) return null;
    throw error;
  }
}

export class UploadReceiptMissingError extends Error {
  constructor() {
    super(
      "Upload receipt is unavailable. Verify the destination before starting another upload; resume will not recreate this session.",
    );
  }
}

export async function transferChunks(input: {
  id: string;
  established: boolean;
  onEstablished: () => void;
  file: File | null;
  path: string;
  space: string;
  signal: AbortSignal;
  onStatus: (status: UploadSession) => void;
}) {
  const { file, signal, onStatus } = input;
  const base = `/api/uploads/${encodeURIComponent(input.id)}`;
  let status: UploadSession;
  try {
    const create = !!file && !input.established;
    status = validateSession(
      await uploadJSON<UploadSession>(
        create ? "/api/uploads" : base,
        create
          ? {
              method: "POST",
              headers: { "Content-Type": "application/json" },
              body: JSON.stringify({
                clientId: input.id,
                name: file!.name,
                size: file!.size,
                path: input.path,
                space: input.space,
                contentType: file!.type || "application/octet-stream",
              }),
            }
          : {},
        signal,
      ),
    );
  } catch (error) {
    if (
      input.established &&
      error instanceof UploadHTTPError &&
      error.status === 404
    )
      throw new UploadReceiptMissingError();
    throw error;
  }
  if (
    status.id !== input.id ||
    (file && (status.size !== file.size || status.name !== file.name))
  )
    throw new Error("Selected file does not match this upload session.");
  // Set this before any part/completion request: an established session may
  // only be resumed through GET, never recreated after receipt expiry.
  input.onEstablished();
  onStatus(status);
  if (status.state === "uploading") {
    if (!file)
      throw new Error("Select the original file to resume this upload.");
    // Validate ALL retained parts before sending any new bytes. Name/size alone
    // cannot establish that a reselected local file matches staged content.
    for (const [key, part] of Object.entries(status.parts)) {
      const start = Number(key) * status.chunkSize;
      if (
        (await digest(file.slice(start, start + part.size), signal)) !==
        part.sha256
      )
        throw new Error(
          "Selected file differs from already uploaded parts. Select the original file or cancel this session.",
        );
    }
    onStatus(status);
    const count = Math.ceil(file.size / status.chunkSize);
    for (let index = 0; index < count; index++) {
      stopped(signal);
      if (status.parts[String(index)]) continue;
      const part = file.slice(
        index * status.chunkSize,
        Math.min(file.size, (index + 1) * status.chunkSize),
      );
      const checksum = await digest(part, signal);
      status = validateSession(
        await uploadJSON<UploadSession>(
          `${base}/parts/${index}`,
          {
            method: "PUT",
            headers: {
              "Content-Type": "application/octet-stream",
              "X-Chunk-SHA256": checksum,
            },
            body: part,
          },
          signal,
          5 * 60000,
        ),
      );
      if (
        !status.parts[String(index)] ||
        status.parts[String(index)].sha256 !== checksum
      )
        throw new Error("Drive did not acknowledge the uploaded part.");
      onStatus(status);
    }
    stopped(signal);
    status = validateSession(
      await uploadJSON<UploadSession>(
        `${base}/complete`,
        { method: "POST" },
        signal,
      ),
    );
    onStatus(status);
  }
  const deadline = Date.now() + 35 * 60000;
  while (status.state === "committing") {
    if (Date.now() > deadline)
      throw new Error(
        "Saving is taking too long. Refresh to check the upload; do not upload it again blindly.",
      );
    await sleep(1000, signal);
    status = validateSession(await uploadJSON<UploadSession>(base, {}, signal));
    onStatus(status);
  }
  if (status.state !== "complete")
    throw new Error(status.error || `Upload is ${status.state}.`);
  return status;
}
