import { useCallback, useEffect, useRef, useState } from "react";
import { apiUpload } from "../api";
import {
  cancelUploadSession,
  listUploadSessions,
  transferChunks,
  UploadHTTPError,
  UploadReceiptMissingError,
  type UploadSession,
} from "../chunkedUploads";

export type UploadRow = {
  id: string;
  name: string;
  status:
    | "queued"
    | "uploading"
    | "saving"
    | "paused"
    | "cancelling"
    | "complete"
    | "failed"
    | "cancelled";
  progress: number | null;
  error?: string;
  torrent: boolean;
  needsFile?: boolean;
  canResume?: boolean;
};
type Task = {
  row: UploadRow;
  file: File | null;
  path: string;
  space: string;
  controller: AbortController;
  running: boolean;
  started: boolean;
  established: boolean;
  order: number;
  stop?: "pause" | "cancel";
};

export function useUploads(onComplete: () => void, onDenied: () => void) {
  const [rows, setRows] = useState<UploadRow[]>([]);
  const tasks = useRef(new Map<string, Task>());
  const running = useRef(0);
  const sequence = useRef(0);
  const alive = useRef(true);
  const callbacks = useRef({ onComplete, onDenied });
  callbacks.current = { onComplete, onDenied };
  const update = useCallback((task: Task, change: Partial<UploadRow>) => {
    Object.assign(task.row, change);
    if (alive.current)
      setRows((current) =>
        current.map((row) => (row.id === task.row.id ? { ...task.row } : row)),
      );
  }, []);
  const denied = (error: Error) => {
    if (
      error.name === "AccessDeniedError" ||
      error.message === "Access to Drive is not granted to this account."
    ) {
      if (alive.current) callbacks.current.onDenied();
    }
  };
  const pump = useCallback(
    function start() {
      if (!alive.current) return;
      for (const task of tasks.current.values()) {
        if (running.current >= 2) break;
        if (task.row.status !== "queued" || task.running) continue;
        // An earlier paused upload still owns its position in this destination's
        // queue. It must be resumed or cancelled before a later replacement.
        if (
          [...tasks.current.values()].some(
            (other) =>
              other !== task &&
              other.order < task.order &&
              other.space === task.space &&
              other.path === task.path &&
              other.row.name === task.row.name &&
              [
                "queued",
                "uploading",
                "saving",
                "paused",
                "cancelling",
              ].includes(other.row.status),
          )
        )
          continue;
        running.current++;
        task.running = true;
        task.started = true;
        const controller = task.controller;
        update(task, { status: "uploading", error: undefined });
        let upload: Promise<unknown>;
        if (task.row.torrent && task.file) {
          const data = new FormData();
          data.append("torrent", task.file);
          data.append("path", task.path);
          data.append("space", task.space);
          upload = apiUpload(
            "/api/torrents",
            data,
            controller.signal,
            (progress) => {
              if (!controller.signal.aborted && task.controller === controller)
                update(task, {
                  progress,
                  status: progress === 1 ? "saving" : "uploading",
                });
            },
          );
        } else {
          upload = transferChunks({
            id: task.row.id,
            established: task.established,
            onEstablished: () => {
              task.established = true;
            },
            file: task.file,
            path: task.path,
            space: task.space,
            signal: controller.signal,
            onStatus: (status: UploadSession) => {
              if (controller.signal.aborted || task.controller !== controller)
                return;
              const received = Object.values(status.parts).reduce(
                (sum, part) => sum + part.size,
                0,
              );
              update(task, {
                progress:
                  status.state === "complete"
                    ? 1
                    : status.size
                      ? received / status.size
                      : 0,
                status:
                  status.state === "complete"
                    ? "complete"
                    : status.state === "committing" ||
                        (status.state === "uploading" &&
                          received === status.size)
                      ? "saving"
                      : "uploading",
                canResume: !["uncertain", "cancelled", "expired"].includes(
                  status.state,
                ),
              });
            },
          });
        }
        upload
          .then(() => {
            if (controller.signal.aborted || task.controller !== controller)
              return;
            update(task, { status: "complete", progress: 1, canResume: false });
            if (alive.current) callbacks.current.onComplete();
          })
          .catch((error: Error) => {
            if (task.controller !== controller || !alive.current) return;
            if (controller.signal.aborted) {
              if (task.stop === "pause") update(task, { status: "paused" });
              // Cancellation is acknowledged separately by the DELETE response.
              return;
            }
            update(task, {
              status: "failed",
              error: error.message,
              canResume:
                error instanceof UploadReceiptMissingError
                  ? false
                  : task.row.canResume,
            });
            denied(error);
          })
          .finally(() => {
            task.running = false;
            running.current--;
            if (
              ["complete", "cancelled"].includes(task.row.status) ||
              (task.row.torrent && task.row.status === "failed")
            )
              tasks.current.delete(task.row.id);
            start();
          });
      }
    },
    [update],
  );

  const enqueue = useCallback(
    (
      files: FileList | File[],
      path: string,
      space: string,
      torrentOnly = false,
    ) => {
      const additions = Array.from(files).map((file) => {
        const torrent =
          torrentOnly || file.name.toLowerCase().endsWith(".torrent");
        const row: UploadRow = {
          id: crypto.randomUUID(),
          name: file.name,
          status: "queued",
          progress: 0,
          torrent,
          canResume: !torrent,
        };
        tasks.current.set(row.id, {
          row,
          file,
          path,
          space,
          controller: new AbortController(),
          running: false,
          started: false,
          established: false,
          order: sequence.current++,
        });
        return { ...row };
      });
      setRows((current) => [...current, ...additions]);
      pump();
    },
    [pump],
  );

  const pause = useCallback(
    (id: string) => {
      const task = tasks.current.get(id);
      if (
        !task ||
        task.row.torrent ||
        !["queued", "uploading"].includes(task.row.status)
      )
        return;
      task.stop = "pause";
      task.controller.abort();
      update(task, { status: "paused", canResume: true });
      pump();
    },
    [pump, update],
  );

  const resume = useCallback(
    (id: string, file?: File) => {
      const task = tasks.current.get(id);
      if (
        !task ||
        !task.row.canResume ||
        !["paused", "failed"].includes(task.row.status)
      )
        return;
      if (file) {
        if (file.name !== task.row.name) {
          update(task, {
            error: "Select the original file with the same name.",
          });
          return;
        }
        task.file = file;
      }
      if (!task.file) {
        update(task, { error: "Select the original file to resume." });
        return;
      }
      task.stop = undefined;
      task.controller = new AbortController();
      update(task, { status: "queued", error: undefined, needsFile: false });
      pump();
    },
    [pump, update],
  );

  const cancel = useCallback(
    (id: string) => {
      const task = tasks.current.get(id);
      if (
        !task ||
        ["complete", "cancelled", "cancelling"].includes(task.row.status)
      )
        return;
      const wasQueued = task.row.status === "queued" && !task.running;
      task.stop = "cancel";
      task.controller.abort();
      if (task.row.torrent || (wasQueued && !task.started)) {
        update(task, {
          status: "cancelled",
          error: undefined,
          canResume: false,
        });
        if (!task.running) tasks.current.delete(id);
        pump();
        return;
      }
      update(task, { status: "cancelling" });
      void cancelUploadSession(id)
        .then(() => {
          update(task, {
            status: "cancelled",
            error: undefined,
            canResume: false,
          });
          if (!task.running) tasks.current.delete(id);
          pump();
        })
        .catch((error: Error) => {
          if (error instanceof UploadHTTPError && error.status === 409) {
            task.stop = undefined;
            task.controller = new AbortController();
            update(task, {
              status: "queued",
              error: "Saving has already started; checking the result.",
            });
            pump();
          } else {
            update(task, {
              status: "failed",
              error:
                "Cancellation was not confirmed. Partial upload data may remain.",
            });
            denied(error);
          }
        });
    },
    [pump, update],
  );

  const cancelAll = useCallback(() => {
    for (const task of tasks.current.values()) cancel(task.row.id);
  }, [cancel]);

  useEffect(() => {
    alive.current = true;
    const controller = new AbortController();
    void listUploadSessions(controller.signal)
      .then((sessions) => {
        if (!alive.current || controller.signal.aborted) return;
        const additions: UploadRow[] = [];
        for (const status of sessions) {
          if (
            tasks.current.has(status.id) ||
            !["uploading", "committing", "uncertain"].includes(status.state)
          )
            continue;
          const row: UploadRow = {
            id: status.id,
            name: status.name,
            torrent: false,
            status:
              status.state === "committing"
                ? "queued"
                : status.state === "uncertain"
                  ? "failed"
                  : "paused",
            progress: status.size
              ? Object.values(status.parts).reduce(
                  (sum, part) => sum + part.size,
                  0,
                ) / status.size
              : 0,
            needsFile: status.state === "uploading",
            canResume: status.state === "uploading",
            error: status.error,
          };
          tasks.current.set(row.id, {
            row,
            file: null,
            path: status.path,
            space: status.space,
            controller: new AbortController(),
            running: false,
            started: true,
            established: true,
            order: -1,
          });
          additions.push({ ...row });
        }
        if (additions.length) setRows((current) => [...current, ...additions]);
        pump();
      })
      .catch((error: Error) => {
        if (!controller.signal.aborted) denied(error);
      });
    return () => {
      alive.current = false;
      controller.abort();
      // Leaving the page pauses transfer; it must NOT delete durable sessions.
      for (const task of tasks.current.values()) task.controller.abort();
    };
  }, [pump]);

  return {
    rows,
    enqueue,
    pause,
    resume,
    cancel,
    cancelAll,
    clear: () =>
      setRows((current) =>
        current.filter(
          (row) =>
            ["queued", "uploading", "saving", "paused", "cancelling"].includes(
              row.status,
            ) ||
            (row.status === "failed" && !row.torrent),
        ),
      ),
  };
}
