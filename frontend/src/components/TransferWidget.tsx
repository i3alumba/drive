import { useState } from "react";
import {
  Alert,
  Box,
  Button,
  LinearProgress,
  Paper,
  Stack,
  Typography,
} from "@mui/material";
import type { UploadRow } from "./useUploads";
export type ImportJob = {
  id: string;
  name: string;
  path: string;
  status: string;
  bytes: number;
  total: number;
  error?: string;
};
export function TransferWidget({
  uploads,
  imports,
  onCancel,
  onPause,
  onResume,
  onCancelImport,
  onClear,
}: {
  uploads: UploadRow[];
  imports: ImportJob[];
  onCancel: (id: string) => void;
  onPause: (id: string) => void;
  onResume: (id: string, file?: File) => void;
  onCancelImport: (id: string) => void;
  onClear: () => void;
}) {
  const [collapsed, setCollapsed] = useState(false);
  const [hidden, setHidden] = useState(new Set<string>());
  const visibleImports = imports.filter((row) => !hidden.has(row.id));
  if (!uploads.length && !visibleImports.length) return null;
  const active =
    uploads.filter((row) =>
      ["queued", "uploading", "saving", "cancelling"].includes(row.status),
    ).length +
    visibleImports.filter((row) =>
      ["downloading", "saving"].includes(row.status),
    ).length;
  return (
    <Paper
      component="section"
      aria-label="Transfers"
      elevation={8}
      sx={{
        position: "fixed",
        right: { xs: 8, sm: 24 },
        bottom: 16,
        width: { xs: "calc(100% - 16px)", sm: 380 },
        maxWidth: 480,
        zIndex: 1200,
        p: 2,
      }}
    >
      <Stack
        direction="row"
        sx={{ alignItems: "center", justifyContent: "space-between" }}
      >
        <Typography component="h2" variant="subtitle1">
          Transfers ({active} active)
        </Typography>
        <Button
          size="small"
          aria-expanded={!collapsed}
          onClick={() => setCollapsed(!collapsed)}
        >
          {collapsed ? "Expand" : "Collapse"}
        </Button>
      </Stack>
      {!collapsed && (
        <>
          <Box sx={{ maxHeight: "40vh", overflowY: "auto" }}>
            {uploads.map((row) => (
              <Box key={row.id} sx={{ py: 1 }}>
                <Typography noWrap title={row.name}>
                  {row.name}
                </Typography>
                <Typography variant="caption">
                  {row.status === "saving"
                    ? "Sent — saving on Drive…"
                    : row.status === "complete" && row.torrent
                      ? "Torrent added"
                      : row.status}{" "}
                  {["uploading", "paused"].includes(row.status) &&
                  row.progress !== null
                    ? `${Math.floor(row.progress * 100)}%`
                    : ""}
                </Typography>
                {[
                  "queued",
                  "uploading",
                  "saving",
                  "paused",
                  "cancelling",
                ].includes(row.status) && (
                  <>
                    <LinearProgress
                      aria-label={`Upload progress ${row.name}`}
                      variant={
                        row.progress === null || row.status === "saving"
                          ? "indeterminate"
                          : "determinate"
                      }
                      value={(row.progress ?? 0) * 100}
                    />
                    {(["queued", "uploading", "paused"].includes(row.status) ||
                      (row.torrent && row.status === "saving")) && (
                      <Button size="small" onClick={() => onCancel(row.id)}>
                        Cancel {row.name}
                      </Button>
                    )}
                    {!row.torrent &&
                      ["queued", "uploading"].includes(row.status) && (
                        <Button size="small" onClick={() => onPause(row.id)}>
                          Pause {row.name}
                        </Button>
                      )}
                  </>
                )}
                {!row.torrent &&
                  ["paused", "failed"].includes(row.status) &&
                  row.canResume &&
                  (row.needsFile || row.status === "failed" ? (
                    <Button component="label" size="small">
                      Choose file to resume {row.name}
                      <input
                        type="file"
                        hidden
                        aria-label={`Choose file to resume ${row.name}`}
                        onChange={(event) => {
                          const file = event.target.files?.[0];
                          event.target.value = "";
                          if (file) onResume(row.id, file);
                        }}
                      />
                    </Button>
                  ) : (
                    <Button size="small" onClick={() => onResume(row.id)}>
                      Resume {row.name}
                    </Button>
                  ))}
                {!row.torrent && row.status === "failed" && (
                  <Button size="small" onClick={() => onCancel(row.id)}>
                    Discard partial upload {row.name}
                  </Button>
                )}
                {row.error && <Alert severity="error">{row.error}</Alert>}
              </Box>
            ))}
            {visibleImports.map((row) => (
              <Box key={row.id} sx={{ py: 1 }}>
                <Typography noWrap>{row.name}</Typography>
                <Typography variant="caption">
                  URL: {row.status}{" "}
                  {row.status === "downloading" && row.total > 0
                    ? `${Math.min(100, Math.floor((row.bytes / row.total) * 100))}%`
                    : ""}
                </Typography>
                {["downloading", "saving"].includes(row.status) && (
                  <>
                    <LinearProgress
                      aria-label={`Download progress ${row.name}`}
                      variant={
                        row.total > 0 && row.status === "downloading"
                          ? "determinate"
                          : "indeterminate"
                      }
                      value={
                        row.total > 0
                          ? Math.min(100, (row.bytes / row.total) * 100)
                          : 0
                      }
                    />
                    {row.status === "downloading" && (
                      <Button
                        size="small"
                        onClick={() => onCancelImport(row.id)}
                      >
                        Cancel {row.name}
                      </Button>
                    )}
                  </>
                )}
                {row.error && <Alert severity="error">{row.error}</Alert>}
              </Box>
            ))}
          </Box>
          <Button
            size="small"
            onClick={() => {
              onClear();
              setHidden(
                (current) =>
                  new Set([
                    ...current,
                    ...imports
                      .filter(
                        (row) =>
                          !["downloading", "saving"].includes(row.status),
                      )
                      .map((row) => row.id),
                  ]),
              );
            }}
          >
            Clear finished transfers
          </Button>
        </>
      )}
    </Paper>
  );
}
