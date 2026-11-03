import { useCallback, useEffect, useRef, useState } from "react";
import { apiUpload } from "../api";

export type UploadRow = {
  id: string;
  name: string;
  status:
    "queued" | "uploading" | "saving" | "complete" | "failed" | "cancelled";
  progress: number | null;
  error?: string;
  torrent: boolean;
};
type Task = {
  row: UploadRow;
  file: File;
  path: string;
  space: string;
  controller: AbortController;
};

export function useUploads(onComplete: () => void, onDenied: () => void) {
  const [rows, setRows] = useState<UploadRow[]>([]);
  const tasks = useRef(new Map<string, Task>());
  const running = useRef(0);
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
  const pump = useCallback(
    function start() {
      if (!alive.current) return;
      for (const task of tasks.current.values()) {
        if (running.current >= 2) break;
        if (task.row.status !== "queued") continue;
        // Preserve selection order for overwrites of the same destination.
        if (
          [...tasks.current.values()].some(
            (other) =>
              other !== task &&
              other.space === task.space &&
              other.path === task.path &&
              other.file.name === task.file.name &&
              ["uploading", "saving"].includes(other.row.status),
          )
        )
          continue;
        running.current++;
        update(task, { status: "uploading" });
        const data = new FormData();
        data.append(task.row.torrent ? "torrent" : "file", task.file);
        data.append("path", task.path);
        data.append("space", task.space);
        apiUpload(
          task.row.torrent ? "/api/torrents" : "/api/upload",
          data,
          task.controller.signal,
          (progress) => {
            if (
              !task.controller.signal.aborted &&
              ["uploading", "saving"].includes(task.row.status)
            )
              update(task, {
                progress,
                status: progress === 1 ? "saving" : "uploading",
              });
          },
        )
          .then(() => {
            update(task, { status: "complete", progress: 1 });
            if (alive.current) callbacks.current.onComplete();
          })
          .catch((error: Error) => {
            update(task, {
              status: task.controller.signal.aborted ? "cancelled" : "failed",
              error: task.controller.signal.aborted ? undefined : error.message,
            });
            if (
              error.name === "AccessDeniedError" ||
              error.message ===
                "Access to Drive is not granted to this account."
            )
              if (alive.current) callbacks.current.onDenied();
          })
          .finally(() => {
            tasks.current.delete(task.row.id);
            running.current--;
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
        const row: UploadRow = {
          id: crypto.randomUUID(),
          name: file.name,
          status: "queued",
          progress: 0,
          torrent: torrentOnly || file.name.toLowerCase().endsWith(".torrent"),
        };
        tasks.current.set(row.id, {
          row,
          file,
          path,
          space,
          controller: new AbortController(),
        });
        return { ...row };
      });
      setRows((current) => [...current, ...additions]);
      pump();
    },
    [pump],
  );
  const cancel = useCallback(
    (id: string) => {
      const task = tasks.current.get(id);
      if (!task) return;
      task.controller.abort();
      if (task.row.status === "queued") {
        update(task, { status: "cancelled" });
        tasks.current.delete(id);
      }
    },
    [update],
  );
  const cancelAll = useCallback(() => {
    for (const task of tasks.current.values()) {
      task.controller.abort();
      update(task, { status: "cancelled" });
    }
    tasks.current.clear();
  }, [update]);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
      for (const task of tasks.current.values()) task.controller.abort();
      tasks.current.clear();
    };
  }, []);
  return {
    rows,
    enqueue,
    cancel,
    cancelAll,
    clear: () =>
      setRows((current) =>
        current.filter((row) =>
          ["queued", "uploading", "saving"].includes(row.status),
        ),
      ),
  };
}
