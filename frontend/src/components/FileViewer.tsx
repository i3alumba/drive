import { useEffect, useState } from "react";
import {
  Box,
  Button,
  Dialog,
  DialogContent,
  DialogTitle,
  IconButton,
  Paper,
  Stack,
  Typography,
} from "@mui/material";
import CloseIcon from "@mui/icons-material/Close";
import InsertDriveFileIcon from "@mui/icons-material/InsertDriveFile";
import { apiFetch } from "../api";
import type { DriveObject } from "../types";
import { getKind, iconForKind } from "../utils/fileUtils";

type Props = {
  file: DriveObject | null;
  viewUrl: (path: string) => string;
  previewUrl: (path: string) => string;
  subtitleUrl: (path: string) => string;
  onClose: () => void;
};

export function FileViewer({
  file,
  viewUrl,
  previewUrl,
  subtitleUrl,
  onClose,
}: Props) {
  const [text, setText] = useState<string>("");
  const [textError, setTextError] = useState<string>("");
  const kind = file ? getKind(file.name, false) : "other";
  const src = file ? viewUrl(file.path) : "";

  useEffect(() => {
    setText("");
    setTextError("");
    if (!file || getKind(file.name, false) !== "text") return;
    apiFetch(viewUrl(file.path))
      .then(async (res) => {
        if (!res.ok) throw new Error(await res.text());
        setText(await res.text());
      })
      .catch((err) => setTextError(String(err)));
  }, [file, viewUrl]);

  if (!file) return null;

  return (
    <Dialog open={Boolean(file)} onClose={onClose} maxWidth="xl" fullWidth>
      <DialogTitle sx={{ display: "flex", alignItems: "center", gap: 1 }}>
        {iconForKind(kind, 28)}
        <Typography sx={{ flex: 1 }} noWrap>
          {file.name}
        </Typography>
        <IconButton onClick={onClose}>
          <CloseIcon />
        </IconButton>
      </DialogTitle>
      <DialogContent
        dividers
        sx={{
          minHeight: "70vh",
          bgcolor:
            kind === "image" || kind === "video" ? "grey.950" : undefined,
        }}
      >
        {kind === "image" && (
          <Box
            component="img"
            src={src}
            alt={file.name}
            sx={{
              display: "block",
              maxWidth: "100%",
              maxHeight: "75vh",
              mx: "auto",
              objectFit: "contain",
            }}
          />
        )}
        {kind === "video" && (
          <Stack spacing={1}>
            <Box
              component="video"
              src={src}
              controls
              sx={{ width: "100%", maxHeight: "72vh", bgcolor: "black" }}
            >
              <track
                kind="subtitles"
                src={subtitleUrl(file.path)}
                srcLang="en"
                label="Subtitles"
                default
              />
            </Box>
            <Typography variant="caption" color="grey.400">
              Playback uses browser controls. Quality choices appear when the
              uploaded video contains browser-supported adaptive renditions;
              otherwise the original quality is played.
            </Typography>
          </Stack>
        )}
        {kind === "audio" && (
          <Stack spacing={2} sx={{ p: 2 }}>
            <Typography>{file.name}</Typography>
            <Box component="audio" src={src} controls sx={{ width: "100%" }}>
              <track
                kind="subtitles"
                src={subtitleUrl(file.path)}
                srcLang="en"
                label="Subtitles"
                default
              />
            </Box>
          </Stack>
        )}
        {kind === "text" && (
          <Paper
            variant="outlined"
            sx={{
              p: 2,
              whiteSpace: "pre-wrap",
              fontFamily: "monospace",
              overflow: "auto",
              maxHeight: "72vh",
            }}
          >
            {textError || text || "Loading…"}
          </Paper>
        )}
        {kind === "pdf" && (
          <Box
            component="iframe"
            title={file.name}
            src={src}
            sx={{ border: 0, width: "100%", height: "75vh" }}
          />
        )}
        {kind === "office" && (
          <Stack spacing={1}>
            <Typography variant="body2">
              Document preview is converted to PDF on demand.
            </Typography>
            <Box
              component="iframe"
              title={file.name}
              src={previewUrl(file.path)}
              sx={{ border: 0, width: "100%", height: "75vh" }}
            />
          </Stack>
        )}
        {kind === "other" && (
          <Stack spacing={2} sx={{ py: 8, alignItems: "center" }}>
            <InsertDriveFileIcon sx={{ fontSize: 72 }} />
            <Typography>
              No inline preview is available for this file type.
            </Typography>
            <Button
              variant="contained"
              href={src}
              target="_blank"
              rel="noreferrer"
            >
              Open raw file
            </Button>
          </Stack>
        )}
      </DialogContent>
    </Dialog>
  );
}
