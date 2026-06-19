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
