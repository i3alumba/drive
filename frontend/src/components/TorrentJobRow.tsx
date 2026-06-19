import { Box, Button, LinearProgress, Stack, Typography } from "@mui/material";
import PauseIcon from "@mui/icons-material/Pause";
import PlayArrowIcon from "@mui/icons-material/PlayArrow";
import StopCircleIcon from "@mui/icons-material/StopCircle";
import type { TorrentJob } from "../types";

type Props = {
  job: TorrentJob;
  onControl: (
    id: string,
    action: "pause" | "resume" | "cancel",
  ) => Promise<void>;
};

export function TorrentJobRow({ job, onControl }: Props) {
  const canPause = job.status === "queued" || job.status === "downloading";
  const canResume = job.status === "paused";
  const canCancel = !["complete", "cancelled"].includes(job.status);

  return (
    <Box sx={{ my: 1 }}>
      <Stack
        direction={{ xs: "column", sm: "row" }}
        spacing={1}
        sx={{ alignItems: { sm: "center" } }}
      >
        <Box sx={{ flex: 1 }}>
          <Typography>
            {job.name}: {job.status} {job.error}
          </Typography>
          <Typography variant="caption" color="text.secondary">
            {Math.round(job.progress * 100)}%
            {job.downloadSpeedBytesPerSecond
              ? ` · ${formatBytesPerSecond(job.downloadSpeedBytesPerSecond)}`
              : ""}
            {job.etaSeconds ? ` · ETA ${formatDuration(job.etaSeconds)}` : ""}
          </Typography>
          <LinearProgress
            variant="determinate"
            value={Math.round(job.progress * 100)}
          />
        </Box>
        {canPause && (
          <Button
            size="small"
            startIcon={<PauseIcon />}
            onClick={() => onControl(job.id, "pause")}
          >
            Pause
          </Button>
        )}
        {canResume && (
          <Button
            size="small"
            startIcon={<PlayArrowIcon />}
            onClick={() => onControl(job.id, "resume")}
          >
            Resume
          </Button>
        )}
        {canCancel && (
          <Button
            size="small"
            color="error"
            startIcon={<StopCircleIcon />}
            onClick={() => onControl(job.id, "cancel")}
          >
            Cancel
          </Button>
        )}
      </Stack>
    </Box>
  );
}

function formatBytesPerSecond(bytesPerSecond: number) {
  const units = ["B/s", "KB/s", "MB/s", "GB/s", "TB/s"];
  let value = bytesPerSecond;
  let unitIndex = 0;
  while (value >= 1024 && unitIndex < units.length - 1) {
    value /= 1024;
    unitIndex += 1;
  }
  const precision = value >= 10 || unitIndex === 0 ? 0 : 1;
  return `${value.toFixed(precision)} ${units[unitIndex]}`;
}

function formatDuration(totalSeconds: number) {
  const seconds = Math.max(0, Math.round(totalSeconds));
  const hours = Math.floor(seconds / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  const remainingSeconds = seconds % 60;
  if (hours > 0) return `${hours}h ${minutes}m`;
  if (minutes > 0) return `${minutes}m ${remainingSeconds}s`;
  return `${remainingSeconds}s`;
}
