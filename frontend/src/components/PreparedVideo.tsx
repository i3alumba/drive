import { useCallback, useEffect, useRef, useState } from "react";
import {
  Alert,
  Button,
  CircularProgress,
  Stack,
  Slider,
  Typography,
} from "@mui/material";
import { AccessDeniedError, apiFetch } from "../api";
import { VideoStreamPlayer } from "./VideoStreamPlayer";

type PreviewState =
  | "staging"
  | "preparing"
  | "streaming"
  | "complete"
  | "cancelling"
  | "stopping"
  | "failed"
  | "cancelled"
  | "unknown";
type Preview = {
  id: string;
  status: PreviewState;
  mode?: "remux" | "copy_video" | "transcode";
  duration: number;
  origin: number;
  seekable: boolean;
  preparedDuration: number;
  segments: number;
};
type Props = { path: string; space: string };
const activeStates = new Set<PreviewState>([
  "staging",
  "preparing",
  "streaming",
  "cancelling",
  "stopping",
]);

function parsePreview(value: unknown): Preview {
  if (!value || typeof value !== "object")
    throw new Error("Invalid stream status");
  const data = value as Record<string, unknown>;
  const states = [
    "staging",
    "preparing",
    "streaming",
    "complete",
    "cancelling",
    "stopping",
    "failed",
    "cancelled",
  ];
  if (
    typeof data.id !== "string" ||
    !/^[0-9a-f]{32}$/.test(data.id) ||
    !states.includes(String(data.status)) ||
    ((data.status === "streaming" || data.status === "complete") &&
      data.mode !== "remux" &&
      data.mode !== "copy_video" &&
      data.mode !== "transcode")
  )
    throw new Error("Invalid stream status");
  const origin = data.origin ?? 0;
  if (
    typeof origin !== "number" ||
    !Number.isFinite(origin) ||
    origin < 0 ||
    origin >= 21600
  )
    throw new Error("Invalid stream origin");
  const duration = data.duration ?? 0;
  const preparedDuration = data.preparedDuration ?? 0;
  const segments = data.segments ?? 0;
  if (
    typeof duration !== "number" ||
    !Number.isFinite(duration) ||
    duration < 0 ||
    duration > 21600 ||
    typeof preparedDuration !== "number" ||
    !Number.isFinite(preparedDuration) ||
    preparedDuration < 0 ||
    preparedDuration > 21602 ||
    typeof segments !== "number" ||
    !Number.isInteger(segments) ||
    segments < 0 ||
    segments > 8192 ||
    ((data.status === "streaming" || data.status === "complete") &&
      (segments === 0 || duration <= 0))
  )
    throw new Error("Invalid stream status");
  return {
    id: data.id,
    status: data.status as PreviewState,
    mode: data.mode as Preview["mode"],
    duration,
    origin,
    seekable: data.seekable === true,
    preparedDuration,
    segments,
  };
}

function responseError(status: number): string {
  if (status === 429) return "Video preparation is busy. Try again later.";
  if (status === 507)
    return "Not enough private preparation space. Try again later.";
  if (status === 503 || status === 404)
    return "Video streaming is currently unavailable.";
  if (status === 409)
    return "The source changed or this stream expired. Reopen the file.";
  return "This video could not be prepared with the supported codecs, size and duration limits.";
}

function mediaTime(seconds: number): string {
  const rounded = Math.max(0, Math.floor(seconds));
  return `${Math.floor(rounded / 60)}:${String(rounded % 60).padStart(2, "0")}`;
}

export function PreparedVideo({ path, space }: Props) {
  const [job, setJob] = useState<Preview | null>(null);
  const [problem, setProblem] = useState("");
  const [blocked, setBlocked] = useState(false);
  const [requesting, setRequesting] = useState(false);
  const [playerStopped, setPlayerStopped] = useState(false);
  const [seekTarget, setSeekTarget] = useState(0);
  const [localSeek, setLocalSeek] = useState<number | undefined>();
  const seekTimer = useRef<ReturnType<typeof setTimeout> | undefined>(
    undefined,
  );
  const mutationBusy = useRef(false);
  const currentJob = useRef<string | undefined>(undefined);
  currentJob.current = job?.id;
  const action = useRef<AbortController | null>(null);
  const cancellationRequested = useRef(false);
  const jobID = job?.id;
  const jobState = job?.status;

  useEffect(
    () => () => {
      action.current?.abort();
      clearTimeout(seekTimer.current);
    },
    [],
  );
  const unavailable = useCallback((message: string, denied: boolean) => {
    setProblem(message);
    setPlayerStopped(true);
    if (denied) {
      setBlocked(true);
      setJob(null);
    }
  }, []);

  useEffect(() => {
    if (!jobID || !jobState || !activeStates.has(jobState) || blocked) return;
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    const id = jobID;
    async function poll() {
      try {
        const response = await apiFetch(`/api/video-previews/${id}`, {
          signal: controller.signal,
          cache: "no-store",
        });
        if (controller.signal.aborted || currentJob.current !== id) return;
        if (!response.ok) {
          if (
            response.status === 404 ||
            response.status === 409 ||
            response.status === 410
          ) {
            if (!controller.signal.aborted)
              unavailable(responseError(response.status), true);
            return;
          }
          throw new Error(responseError(response.status));
        }
        const next = parsePreview(await response.json());
        if (next.id !== id) throw new Error("Invalid stream status");
        if (controller.signal.aborted || currentJob.current !== id) return;
        setJob(
          cancellationRequested.current && activeStates.has(next.status)
            ? { ...next, status: "cancelling" }
            : next,
        );
        if (activeStates.has(next.status)) timer = setTimeout(poll, 2000);
      } catch (error) {
        if (controller.signal.aborted || currentJob.current !== id) return;
        if (error instanceof AccessDeniedError) {
          unavailable("Access to this video is no longer granted.", true);
        } else {
          setJob((previous) =>
            previous?.id === id ? { ...previous, status: "unknown" } : previous,
          );
          setProblem(
            "Stream status is unknown. Preparation may still be running; no new conversion was started.",
          );
          setPlayerStopped(true);
        }
      }
    }
    void poll();
    return () => {
      controller.abort();
      clearTimeout(timer);
    };
  }, [jobID, jobState, blocked, unavailable]);

  async function control(
    method: "POST" | "DELETE" | "GET",
    replacement?: {
      startTime: number;
      forceTranscode: boolean;
      replaceID: string;
    },
  ) {
    if (mutationBusy.current) return;
    mutationBusy.current = true;
    action.current?.abort();
    const controller = new AbortController();
    action.current = controller;
    let responseAccepted = false;
    setRequesting(true);
    setProblem("");
    if (method === "DELETE") {
      cancellationRequested.current = true;
      setPlayerStopped(true);
      // Stop media and fence a stale in-flight status response immediately;
      // this is cancellation intent, never a terminal acknowledgement.
      setJob((previous) =>
        previous && activeStates.has(previous.status)
          ? { ...previous, status: "cancelling" }
          : previous,
      );
    }
    try {
      const response = await apiFetch(
        method === "POST"
          ? "/api/video-previews"
          : `/api/video-previews/${job?.id}`,
        {
          method,
          signal: controller.signal,
          cache: "no-store",
          ...(method === "POST"
            ? {
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ path, space, ...replacement }),
              }
            : {}),
        },
      );
      if (!response.ok) {
        if (method !== "POST" && [404, 409, 410].includes(response.status)) {
          if (!controller.signal.aborted)
            unavailable(responseError(response.status), true);
          return;
        }
        throw new Error(responseError(response.status));
      }
      responseAccepted = true;
      const next = parsePreview(await response.json());
      if (method !== "POST" && next.id !== job?.id)
        throw new Error("Invalid stream status");
      if (!controller.signal.aborted) {
        if (method === "POST") {
          cancellationRequested.current = false;
          currentJob.current = next.id;
          setLocalSeek(undefined);
        }
        setJob(
          cancellationRequested.current && activeStates.has(next.status)
            ? { ...next, status: "cancelling" }
            : next,
        );
        if (
          (method === "POST" || method === "GET") &&
          !cancellationRequested.current
        )
          setPlayerStopped(false);
      }
    } catch (error) {
      if (controller.signal.aborted) return;
      if (error instanceof AccessDeniedError)
        unavailable("Access to this video is no longer granted.", true);
      else if (
        method === "POST" &&
        (error instanceof TypeError || responseAccepted)
      ) {
        setBlocked(true);
        setProblem(
          "The preparation request result is unknown. It was not retried. Check with the operator before starting another preparation.",
        );
      } else if (error instanceof TypeError)
        setProblem(
          "The request result is unknown. Nothing was retried automatically. Check status before trying again.",
        );
      else
        setProblem(
          error instanceof Error ? error.message : "Video preparation failed.",
        );
    } finally {
      mutationBusy.current = false;
      if (!controller.signal.aborted) setRequesting(false);
    }
  }

  async function replaceAt(target: number, force = false) {
    if (
      !job ||
      blocked ||
      mutationBusy.current ||
      currentJob.current !== job.id
    )
      return;
    const old = job;
    if (
      !force &&
      target >= old.origin &&
      target < old.origin + old.preparedDuration
    ) {
      setLocalSeek(target - old.origin);
      return;
    }
    mutationBusy.current = true;
    setRequesting(true);
    setPlayerStopped(true);
    cancellationRequested.current = true;
    setJob({ ...old, status: "cancelling" });
    action.current?.abort();
    const controller = new AbortController();
    action.current = controller;
    try {
      let status = old;
      if (activeStates.has(old.status)) {
        const response = await apiFetch(`/api/video-previews/${old.id}`, {
          method: "DELETE",
          signal: controller.signal,
        });
        if (!response.ok) throw new Error(responseError(response.status));
        status = parsePreview(await response.json());
      }
      for (let count = 0; activeStates.has(status.status); count++) {
        if (count >= 120)
          throw new Error(
            "Old execution teardown is unconfirmed. No replacement was submitted.",
          );
        await new Promise((resolve) => setTimeout(resolve, 500));
        if (controller.signal.aborted) return;
        const response = await apiFetch(`/api/video-previews/${old.id}`, {
          signal: controller.signal,
          cache: "no-store",
        });
        if (!response.ok) throw new Error(responseError(response.status));
        status = parsePreview(await response.json());
        if (status.id !== old.id) throw new Error("Invalid stream identity");
      }
      if (controller.signal.aborted) return;
      mutationBusy.current = false;
      await control("POST", {
        startTime: target,
        forceTranscode: force,
        replaceID: old.id,
      });
    } catch (error) {
      if (controller.signal.aborted) return;
      if (error instanceof AccessDeniedError)
        unavailable("Access to this video is no longer granted.", true);
      else {
        setBlocked(true);
        setProblem(
          error instanceof Error
            ? error.message
            : "Replacement was not submitted.",
        );
      }
    } finally {
      mutationBusy.current = false;
      if (!controller.signal.aborted) setRequesting(false);
    }
  }
  const playerUnavailable = useCallback(
    (message: string, denied: boolean) => {
      if (currentJob.current === jobID) unavailable(message, denied);
    },
    [jobID, unavailable],
  );

  const playable =
    job &&
    (job.status === "streaming" || job.status === "complete") &&
    !blocked;
  return (
    <Stack spacing={2} sx={{ p: 2 }}>
      {problem && <Alert severity="warning">{problem}</Alert>}
      {playable && !playerStopped && (
        <VideoStreamPlayer
          key={job.id}
          jobID={job.id}
          onUnavailable={playerUnavailable}
          seekTime={localSeek}
        />
      )}
      {playable && playerStopped && !cancellationRequested.current && (
        <Button
          onClick={() => {
            setProblem("");
            setPlayerStopped(false);
          }}
        >
          Retry playback
        </Button>
      )}
      {job && activeStates.has(job.status) && (
        <Stack
          direction="row"
          spacing={2}
          sx={{ alignItems: "center", flexWrap: "wrap" }}
        >
          <CircularProgress size={24} />
          <Typography color="grey.300" role="status">
            {job.status === "staging"
              ? "Reading video from storage…"
              : job.status === "preparing"
                ? "Preparing the first video segments…"
                : job.status === "streaming"
                  ? "Playing available segments; preparation continues…"
                  : "Stopping preparation; waiting for confirmed teardown…"}
          </Typography>
          <Button
            disabled={
              requesting ||
              job.status === "cancelling" ||
              job.status === "stopping"
            }
            onClick={() => void control("DELETE")}
          >
            Cancel preparation
          </Button>
        </Stack>
      )}
      {job && job.duration > 0 && (
        <Typography variant="caption" color="grey.400">
          Prepared {mediaTime(job.preparedDuration)} of{" "}
          {mediaTime(job.duration)}. Current stream begins at{" "}
          {mediaTime(job.origin)}.
        </Typography>
      )}
      {job && job.seekable && job.duration > 0 && !blocked && (
        <Stack>
          <Typography>
            Seek movie: {mediaTime(seekTarget)} of {mediaTime(job.duration)}
          </Typography>
          <Slider
            aria-label="Seek movie"
            min={0}
            max={Math.max(0, job.duration - 0.1)}
            step={0.1}
            value={seekTarget}
            disabled={requesting}
            onChange={(_event, value) => {
              const target = value as number;
              setSeekTarget(target);
              clearTimeout(seekTimer.current);
              seekTimer.current = setTimeout(() => void replaceAt(target), 400);
            }}
          />
        </Stack>
      )}
      {job?.status === "complete" && (
        <Typography color="grey.300">Preparation complete.</Typography>
      )}
      {job?.status === "failed" && (
        <Alert severity="warning">
          Video preparation stopped before completion. The media may be
          unsupported or exceed processing limits.
        </Alert>
      )}
      {job &&
        job.seekable &&
        (job.status === "failed" || playerStopped) &&
        !blocked && (
          <Button
            disabled={requesting}
            onClick={() => void replaceAt(job.origin, true)}
          >
            Prepare with video transcoding
          </Button>
        )}
      {job?.status === "cancelled" && (
        <Typography color="grey.300">Preparation cancelled.</Typography>
      )}
      {job?.status === "unknown" && (
        <Button
          disabled={requesting || blocked}
          onClick={() => void control("GET")}
        >
          Check stream status
        </Button>
      )}
      {(!job || job.status === "failed" || job.status === "cancelled") &&
        !blocked && (
          <Button
            variant="contained"
            disabled={requesting}
            onClick={() => void control("POST")}
          >
            {requesting ? "Requesting stream…" : "Prepare video stream"}
          </Button>
        )}
      <Typography variant="caption" color="grey.400">
        The configured controller prepares a private stream. Playback then
        starts from the first completed segments, without waiting for the whole
        conversion. The original is unchanged. Closing this viewer stops
        polling, not accepted preparation. No quality selector or embedded
        subtitles are provided.
      </Typography>
    </Stack>
  );
}
