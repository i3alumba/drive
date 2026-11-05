import { useEffect, useRef, useState } from "react";
import { Alert, Box, Button, Stack } from "@mui/material";
import Hls, { type ErrorData, type LoadPolicy } from "hls.js";

type Props = {
  jobID: string;
  onUnavailable: (message: string, denied: boolean) => void;
};

const boundedLoadPolicy: LoadPolicy = {
  default: {
    maxTimeToFirstByteMs: 5000,
    maxLoadTimeMs: 30000,
    timeoutRetry: { maxNumRetry: 1, retryDelayMs: 500, maxRetryDelayMs: 500 },
    errorRetry: {
      maxNumRetry: 1,
      retryDelayMs: 500,
      maxRetryDelayMs: 500,
      shouldRetry: (_config, _count, _timeout, response, retry) =>
        retry &&
        (response?.code === undefined ||
          response.code === 0 ||
          response.code >= 500),
    },
  },
};

export function VideoStreamPlayer({ jobID, onUnavailable }: Props) {
  const video = useRef<HTMLVideoElement | null>(null);
  const [needsPlay, setNeedsPlay] = useState(false);

  useEffect(() => {
    const media = video.current;
    if (!media) return;
    let disposed = false;
    let hls: Hls | null = null;
    const prefix = `/api/video-previews/${jobID}/`;
    const source = `${prefix}stream.m3u8`;
    const stop = () => {
      hls?.stopLoad();
      hls?.destroy();
      hls = null;
      media.pause();
      media.removeAttribute("src");
      media.load();
    };
    const fail = (message: string, denied = false) => {
      if (disposed) return;
      stop();
      onUnavailable(message, denied);
    };
    const play = () => {
      void media.play().catch(() => {
        // Autoplay rejection is not conversion failure and must never POST again.
        if (!disposed) setNeedsPlay(true);
      });
    };
    const loaded = () => play();
    const playing = () => setNeedsPlay(false);
    const nativeError = () =>
      fail(
        "The stream could not be played or access expired. Reopen the file to check its status.",
      );
    media.addEventListener("playing", playing);

    if (media.canPlayType("application/vnd.apple.mpegurl")) {
      media.addEventListener("loadedmetadata", loaded);
      media.addEventListener("error", nativeError);
      media.src = source;
      media.load();
    } else if (Hls.isSupported()) {
      hls = new Hls({
        autoStartLoad: false,
        startPosition: 0,
        lowLatencyMode: false,
        maxBufferLength: 12,
        maxMaxBufferLength: 24,
        maxBufferSize: 32 * 1024 * 1024,
        backBufferLength: 8,
        manifestLoadPolicy: boundedLoadPolicy,
        playlistLoadPolicy: boundedLoadPolicy,
        fragLoadPolicy: boundedLoadPolicy,
        xhrSetup(xhr, url) {
          const target = new URL(url, window.location.href);
          const asset = target.pathname.slice(prefix.length);
          if (
            target.origin !== window.location.origin ||
            !target.pathname.startsWith(prefix) ||
            target.search ||
            target.hash ||
            target.username ||
            target.password ||
            !/^(stream\.m3u8|init\.mp4|segment-\d{6}\.m4s)$/.test(asset)
          )
            throw new Error("Invalid private media URL");
          xhr.withCredentials = true;
        },
      });
      hls.on(Hls.Events.MEDIA_ATTACHED, () => hls?.loadSource(source));
      hls.on(Hls.Events.MANIFEST_PARSED, () => {
        hls?.startLoad(0);
        play();
      });
      hls.on(Hls.Events.ERROR, (_event, data: ErrorData) => {
        const status = data.response?.code;
        if (
          status === 401 ||
          status === 403 ||
          status === 404 ||
          status === 409 ||
          status === 410
        ) {
          fail(
            "Access to this stream is no longer available, or the source changed.",
            true,
          );
        } else if (data.fatal) {
          fail(
            "Playback stopped before the stream finished. Check preparation status before trying again.",
          );
        }
      });
      hls.attachMedia(media);
    } else {
      fail("This browser does not support the available streaming player.");
    }
    return () => {
      disposed = true;
      media.removeEventListener("playing", playing);
      media.removeEventListener("loadedmetadata", loaded);
      media.removeEventListener("error", nativeError);
      stop();
    };
  }, [jobID, onUnavailable]);

  return (
    <Stack spacing={1}>
      <Box
        component="video"
        ref={video}
        aria-label="Prepared video stream"
        controls
        controlsList="nodownload"
        playsInline
        preload="metadata"
        sx={{ width: "100%", maxHeight: "68vh", bgcolor: "black" }}
      />
      {needsPlay && (
        <>
          <Alert severity="info">
            The stream is ready. Press Play to start playback.
          </Alert>
          <Button
            onClick={() => {
              void video.current
                ?.play()
                .then(() => setNeedsPlay(false))
                .catch(() => setNeedsPlay(true));
            }}
          >
            Play
          </Button>
        </>
      )}
    </Stack>
  );
}
