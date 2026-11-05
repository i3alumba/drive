import { useEffect, useRef, useState } from "react";
import {
  Alert,
  Box,
  Button,
  CircularProgress,
  Stack,
  Typography,
} from "@mui/material";
import { AccessDeniedError, apiFetch } from "../api";

type Preview = {
  id: string;
  status:
    | "preparing"
    | "cancelling"
    | "ready"
    | "failed"
    | "cancelled"
    | "unknown";
  mode?: "remux" | "transcode";
};

type Props = {
  path: string;
  space: string;
  originalUrl: string;
};

function parsePreview(value: unknown): Preview {
  if (!value || typeof value !== "object")
    throw new Error("Invalid preview status");
  const data = value as Record<string, unknown>;
  if (
    typeof data.id !== "string" ||
    !/^[0-9a-f]{32}$/.test(data.id) ||
    !["preparing", "cancelling", "ready", "failed", "cancelled"].includes(
      String(data.status),
    ) ||
    (data.status === "ready" &&
      data.mode !== "remux" &&
      data.mode !== "transcode")
  )
    throw new Error("Invalid preview status");
  return {
    id: data.id,
    status: data.status as Preview["status"],
    mode: data.mode as Preview["mode"],
  };
}

function responseError(status: number): string {
  if (status === 429) return "The video worker is busy. Try again later.";
  if (status === 507)
    return "Not enough private preview space. Try later or download the original.";
  if (status === 503 || status === 404)
    return "Video preparation is unavailable. You can still download the original.";
  if (status === 409)
    return "The original changed or this preview expired. Open the file again.";
  return "This video could not be prepared. The container, codecs, size or duration may not be supported.";
}

export function PreparedVideo({ path, space, originalUrl }: Props) {
  const [job, setJob] = useState<Preview | null>(null);
  const [problem, setProblem] = useState("");
  const [denied, setDenied] = useState(false);
  const [requesting, setRequesting] = useState(false);
  const action = useRef<AbortController | null>(null);
  const jobID = job?.id;
  const jobStatus = job?.status;

  useEffect(() => () => action.current?.abort(), []);

  useEffect(() => {
    if (!jobID || (jobStatus !== "preparing" && jobStatus !== "cancelling"))
      return;
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    const id = jobID;
    async function poll() {
      try {
        const response = await apiFetch(`/api/video-previews/${id}`, {
          signal: controller.signal,
          cache: "no-store",
        });
        if (response.status === 404 || response.status === 409) {
          if (!controller.signal.aborted) {
            setJob(null);
            setProblem(responseError(response.status));
          }
          return;
        }
        if (!response.ok) throw new Error(responseError(response.status));
        const next = parsePreview(await response.json());
        if (next.id !== id) throw new Error("Invalid preview status");
        if (controller.signal.aborted) return;
        setJob(next);
        if (next.status === "preparing" || next.status === "cancelling")
          timer = setTimeout(poll, 2000);
      } catch (error) {
        if (controller.signal.aborted) return;
        if (error instanceof AccessDeniedError) {
          setDenied(true);
          setJob(null);
          setProblem("Access to this video is no longer granted.");
        } else {
          setJob({ id, status: "unknown" });
          setProblem(
            "Preview status could not be checked. Preparation may still be running; no new conversion was started.",
          );
        }
      }
    }
    void poll();
    return () => {
      controller.abort();
      clearTimeout(timer);
    };
  }, [jobID, jobStatus]);

  async function control(method: "POST" | "DELETE" | "GET") {
    action.current?.abort();
    const controller = new AbortController();
    action.current = controller;
    setRequesting(true);
    setProblem("");
    try {
      const url =
        method === "POST"
          ? "/api/video-previews"
          : `/api/video-previews/${job?.id}`;
      const response = await apiFetch(url, {
        method,
        signal: controller.signal,
        cache: "no-store",
        ...(method === "POST"
          ? {
              headers: { "Content-Type": "application/json" },
              body: JSON.stringify({ path, space }),
            }
          : {}),
      });
      if (
        method !== "POST" &&
        (response.status === 404 || response.status === 409)
      ) {
        if (!controller.signal.aborted) {
          setJob(null);
          setProblem(responseError(response.status));
        }
        return;
      }
      if (!response.ok) throw new Error(responseError(response.status));
      const next = parsePreview(await response.json());
      if (method !== "POST" && next.id !== job?.id)
        throw new Error("Invalid preview status");
      if (!controller.signal.aborted) setJob(next);
    } catch (error) {
      if (controller.signal.aborted) return;
      if (error instanceof AccessDeniedError) {
        setDenied(true);
        setJob(null);
        setProblem("Access to this video is no longer granted.");
      } else if (error instanceof TypeError) {
        setProblem(
          "The request may have been accepted, but its result is unknown. Nothing was retried automatically. Check status or try later.",
        );
      } else {
        setProblem(
          error instanceof Error ? error.message : "Video preparation failed.",
        );
      }
    } finally {
      if (!controller.signal.aborted) setRequesting(false);
    }
  }

  return (
    <Stack spacing={2} sx={{ p: 2 }}>
      {problem && <Alert severity="warning">{problem}</Alert>}
      {job?.status === "ready" && !denied ? (
        <>
          <Box
            component="video"
            aria-label="Prepared video preview"
            src={`/api/video-previews/${job.id}/content`}
            controls
            playsInline
            preload="metadata"
            onError={() => {
              setJob(null);
              setProblem(
                "Playback failed or access expired. You can still download the original if access is granted.",
              );
            }}
            sx={{ width: "100%", maxHeight: "68vh", bgcolor: "black" }}
          />
          <Typography variant="caption" color="grey.400">
            {job.mode === "remux"
              ? "Compatible streams repackaged without re-encoding."
              : "Compatible H.264/AAC preview; conversion can reduce quality."}{" "}
            The original is unchanged. Only the first video/audio track is used;
            embedded subtitles are not included.
          </Typography>
        </>
      ) : (
        <>
          <Typography color="grey.300">
            MKV/AVI needs a compatible MP4 preview before playback. The original
            is not modified.
          </Typography>
          {(job?.status === "preparing" || job?.status === "cancelling") && (
            <Stack direction="row" spacing={2} sx={{ alignItems: "center" }}>
              <CircularProgress size={24} />
              <Typography color="grey.300" role="status">
                {job.status === "cancelling"
                  ? "Stopping preparation…"
                  : "Preparing video…"}
              </Typography>
              <Button
                disabled={requesting || job.status === "cancelling"}
                onClick={() => void control("DELETE")}
              >
                Cancel preparation
              </Button>
            </Stack>
          )}
          {job?.status === "failed" && (
            <Alert severity="warning">
              Preparation failed. This media may be invalid, unsupported, too
              large or exceed the time limit.
            </Alert>
          )}
          {job?.status === "cancelled" && (
            <Typography color="grey.300">Preparation cancelled.</Typography>
          )}
          {job?.status === "unknown" && (
            <Button
              disabled={requesting || denied}
              onClick={() => void control("GET")}
            >
              Check preview status
            </Button>
          )}
          {(!job || job.status === "failed" || job.status === "cancelled") &&
            !denied && (
              <Button
                variant="contained"
                disabled={requesting}
                onClick={() => void control("POST")}
              >
                {requesting ? "Requesting preview…" : "Prepare video preview"}
              </Button>
            )}
          <Typography variant="caption" color="grey.400">
            Preparation is explicit, not started by file cards. Closing this
            viewer stops status checks, not an accepted conversion.
          </Typography>
        </>
      )}
      {!denied && (
        <Button href={originalUrl} target="_blank" rel="noreferrer">
          Open original file
        </Button>
      )}
    </Stack>
  );
}
