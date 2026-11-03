import {
  type DragEvent,
  useCallback,
  useEffect,
  useMemo,
  useState,
} from "react";
import {
  Alert,
  AppBar,
  Box,
  Button,
  Container,
  Link,
  Paper,
  Stack,
  TextField,
  Toolbar,
  Typography,
} from "@mui/material";
import CloudUploadIcon from "@mui/icons-material/CloudUpload";
import { AccessDeniedError, apiFetch } from "../api";
import { FileCard } from "../components/FileCard";
import { ThemeToggle } from "../components/ThemeToggle";
import { FileViewer } from "../components/FileViewer";
import { ShareDialog } from "../components/ShareDialog";
import { TorrentJobRow } from "../components/TorrentJobRow";
import { useUploads } from "../components/useUploads";
import { TransferWidget, type ImportJob } from "../components/TransferWidget";
import { PublicLinkDialog } from "../components/PublicLinkDialog";
import { ServerDownloadQueues } from "../components/ServerDownloadQueues";
import type { DriveObject, Space, TorrentJob } from "../types";
import {
  baseName,
  driveDragType,
  joinPath,
  readDraggedItem,
} from "../utils/fileUtils";

const api = "";

export function DrivePage() {
  const [path, setPath] = useState("");
  const [imports, setImports] = useState<ImportJob[]>([]);
  const [remoteUrl, setRemoteUrl] = useState("");
  const [remoteName, setRemoteName] = useState("");
  const [importError, setImportError] = useState("");
  const [publicItem, setPublicItem] = useState<DriveObject | null>(null);
  const [publicOpen, setPublicOpen] = useState(false);
  const [accessDenied, setAccessDenied] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [items, setItems] = useState<DriveObject[]>([]);
  const [newDir, setNewDir] = useState("");
  const [jobs, setJobs] = useState<TorrentJob[]>([]);
  const [selected, setSelected] = useState<DriveObject | null>(null);
  const [dropTarget, setDropTarget] = useState<string | null>(null);
  const [spaces, setSpaces] = useState<Space[]>([
    { id: "personal", name: "My files", permission: "edit" },
  ]);
  const [space, setSpace] = useState("personal");
  const [shareItem, setShareItem] = useState<DriveObject | null>(null);
  const [shareTarget, setShareTarget] = useState("");
  const [sharePermission, setSharePermission] = useState<"read" | "edit">(
    "read",
  );
  const breadcrumbs = useMemo(() => path.split("/").filter(Boolean), [path]);
  const activeSpace = spaces.find((candidate) => candidate.id === space);
  const canEdit =
    !accessDenied && !loadError && activeSpace?.permission === "edit";
  const withSpace = useCallback(
    (url: string) =>
      `${url}${url.includes("?") ? "&" : "?"}space=${encodeURIComponent(space)}`,
    [space],
  );
  const viewUrlFor = useCallback(
    (itemPath: string) =>
      withSpace(`/api/view?path=${encodeURIComponent(itemPath)}`),
    [withSpace],
  );
  const previewUrlFor = useCallback(
    (itemPath: string) =>
      withSpace(`/api/preview?path=${encodeURIComponent(itemPath)}`),
    [withSpace],
  );
  const subtitleUrlFor = useCallback(
    (itemPath: string) =>
      withSpace(
        `/api/view?path=${encodeURIComponent(itemPath.replace(/\.[^/.]+$/, ".vtt"))}`,
      ),
    [withSpace],
  );

  const refresh = useCallback(async () => {
    try {
      const spacesRes = await apiFetch(`${api}/api/spaces`);
      if (!spacesRes.ok) throw new Error("Spaces unavailable");
      const nextSpaces = (await spacesRes.json()) as Space[];
      if (!Array.isArray(nextSpaces))
        throw new Error("Invalid spaces response");
      setSpaces(nextSpaces);
      if (!nextSpaces.some((candidate) => candidate.id === space)) {
        setSpace("personal");
        return;
      }
      const res = await apiFetch(
        `${api}/api/files?path=${encodeURIComponent(path)}&space=${encodeURIComponent(space)}`,
      );
      if (!res.ok) throw new Error("Files unavailable");
      setItems(await res.json());
      const jobRes = await apiFetch(`${api}/api/torrents`);
      if (!jobRes.ok) throw new Error("Torrent jobs unavailable");
      setJobs(await jobRes.json());
      const importRes = await apiFetch("/api/imports");
      if (!importRes.ok) throw new Error("URL downloads unavailable");
      setImports(await importRes.json());
      setAccessDenied(false);
      setLoadError(null);
    } catch (error) {
      if (error instanceof AccessDeniedError) {
        setAccessDenied(true);
        setSpaces([]);
        setItems([]);
        setJobs([]);
        setImports([]);
        setPublicItem(null);
        setPublicOpen(false);
        setSelected(null);
        setShareItem(null);
        setLoadError(error.message);
      } else {
        setLoadError("Drive is temporarily unavailable. Please retry.");
      }
    }
  }, [path, space]);

  const transfers = useUploads(
    () => void refresh(),
    () => void refresh(),
  );
  useEffect(() => {
    if (accessDenied) transfers.cancelAll();
  }, [accessDenied, transfers.cancelAll]);
  useEffect(() => {
    refresh();
  }, [refresh]);
  useEffect(() => {
    if (accessDenied) return;
    const id = setInterval(refresh, 4000);
    return () => clearInterval(id);
  }, [refresh, accessDenied]);

  async function uploadFiles(files: FileList | File[], targetPath = path) {
    if (canEdit) transfers.enqueue(files, targetPath, space);
  }

  async function importURL() {
    setImportError("");
    try {
      const response = await apiFetch("/api/imports", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          url: remoteUrl,
          path,
          space,
          ...(remoteName.trim() ? { name: remoteName.trim() } : {}),
        }),
      });
      if (!response.ok) throw new Error();
      setRemoteUrl("");
      setRemoteName("");
      await refresh();
    } catch {
      setImportError(
        "Could not start download. Use a public HTTP(S) file URL; up to 1 GiB and two active downloads per user.",
      );
    }
  }
  async function cancelImport(id: string) {
    const response = await apiFetch(`/api/imports/${encodeURIComponent(id)}`, {
      method: "DELETE",
    });
    if (!response.ok)
      setImportError("Could not cancel download; it may already be saving.");
    await refresh();
  }

  async function createDir() {
    if (!newDir.trim()) return;
    const dirPath = [path, newDir.trim()].filter(Boolean).join("/");
    await apiFetch(withSpace("/api/directories"), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ path: dirPath }),
    });
    setNewDir("");
    await refresh();
  }

  async function remove(item: DriveObject) {
    await apiFetch(
      withSpace(
        `/api/files?path=${encodeURIComponent(item.path)}&dir=${item.isDir}`,
      ),
      {
        method: "DELETE",
      },
    );
    if (selected?.path === item.path) setSelected(null);
    await refresh();
  }

  async function controlTorrent(
    id: string,
    action: "pause" | "resume" | "cancel",
  ) {
    await apiFetch(`/api/torrents/${encodeURIComponent(id)}/${action}`, {
      method: "POST",
    });
    await refresh();
  }

  async function moveItem(item: DriveObject, destinationDir: string) {
    const destination = joinPath(destinationDir, baseName(item.path));
    if (destination === item.path || destination.startsWith(item.path + "/"))
      return;
    await apiFetch(withSpace("/api/move"), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        source: item.path,
        destination,
        isDir: item.isDir,
      }),
    });
    if (selected?.path === item.path) setSelected(null);
    await refresh();
  }

  async function createShare() {
    if (!shareItem || !shareTarget.trim()) return;
    await apiFetch("/api/shares", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        path: shareItem.path,
        isDir: shareItem.isDir,
        targetUsername: shareTarget.trim(),
        permission: sharePermission,
      }),
    });
    setShareItem(null);
    setShareTarget("");
    setSharePermission("read");
    await refresh();
  }

  function changeSpace(nextSpace: string) {
    setSpace(nextSpace);
    setPath("");
    setSelected(null);
  }

  function open(item: DriveObject) {
    if (item.isDir) setPath(item.path);
    else setSelected(item);
  }

  function goUp() {
    setPath(breadcrumbs.slice(0, -1).join("/"));
  }

  function handleRootDragOver(event: DragEvent) {
    if (!canEdit) return;
    if (
      event.dataTransfer.types.includes("Files") ||
      event.dataTransfer.types.includes(driveDragType)
    ) {
      event.preventDefault();
      setDropTarget(path || "/");
    }
  }

  async function handleRootDrop(event: DragEvent) {
    event.preventDefault();
    setDropTarget(null);
    if (!canEdit) return;
    const dragged = readDraggedItem(event);
    if (dragged) await moveItem(dragged, path);
    else if (event.dataTransfer.files.length > 0)
      await uploadFiles(event.dataTransfer.files, path);
  }

  return (
    <Box
      sx={{ minHeight: "100dvh" }}
      onDragOver={handleRootDragOver}
      onDrop={handleRootDrop}
      onDragLeave={() => setDropTarget(null)}
    >
      <AppBar position="static">
        <Toolbar sx={{ width: "100%", maxWidth: 1280, mx: "auto", gap: 2 }}>
          <Link
            href="https://main.i3alumba.ru/"
            underline="none"
            color="inherit"
            sx={{ fontWeight: 700, py: 1 }}
          >
            i3alumba
          </Link>
          <Typography component="h1" variant="h6" sx={{ flexGrow: 1 }}>
            Drive
          </Typography>
          <ThemeToggle />
        </Toolbar>
      </AppBar>
      <Container maxWidth={false} sx={{ maxWidth: 1280, py: { xs: 2, sm: 4 } }}>
        {loadError && (
          <Alert
            severity="error"
            sx={{ mb: 2 }}
            action={
              <Box sx={{ display: "flex", gap: 1 }}>
                <Button color="inherit" onClick={() => void refresh()}>
                  Retry
                </Button>
                <Button color="inherit" href="https://main.i3alumba.ru/">
                  Dashboard
                </Button>
              </Box>
            }
          >
            {loadError}
          </Alert>
        )}
        <Stack spacing={3}>
          <Paper variant="outlined" sx={{ p: 2 }}>
            <Box
              sx={{
                display: "flex",
                gap: 1,
                alignItems: "center",
                flexWrap: "wrap",
              }}
            >
              <Typography variant="body2" color="text.secondary">
                Space
              </Typography>
              <Box
                component="select"
                aria-label="Space"
                value={space}
                onChange={(event) => changeSpace(String(event.target.value))}
                sx={{
                  p: 1,
                  minHeight: 40,
                  maxWidth: "100%",
                  borderRadius: 1,
                  borderColor: "divider",
                  bgcolor: "background.paper",
                  color: "text.primary",
                }}
              >
                {spaces.map((candidate) => (
                  <option key={candidate.id} value={candidate.id}>
                    {candidate.name} ({candidate.permission})
                  </option>
                ))}
              </Box>
              <Button
                disabled={accessDenied || !!loadError}
                onClick={() => {
                  setPublicItem(null);
                  setPublicOpen(true);
                }}
              >
                Manage public links
              </Button>
              <Button onClick={goUp} disabled={!path}>
                Up
              </Button>
              <Typography sx={{ overflowWrap: "anywhere", minWidth: 0 }}>
                / {breadcrumbs.join(" / ")}
              </Typography>
            </Box>
          </Paper>

          <Paper
            sx={{
              p: 2,
              border: "2px dashed",
              borderColor: dropTarget ? "primary.main" : "divider",
              bgcolor: dropTarget ? "action.selected" : undefined,
            }}
            onDragOver={handleRootDragOver}
            onDrop={handleRootDrop}
          >
            <Stack
              direction={{ xs: "column", sm: "row" }}
              spacing={2}
              sx={{ alignItems: { sm: "center" }, flexWrap: "wrap", rowGap: 2 }}
            >
              <Button
                disabled={!canEdit}
                component="label"
                variant="contained"
                startIcon={<CloudUploadIcon />}
              >
                Upload file
                <input
                  hidden
                  multiple
                  type="file"
                  onChange={(e) => {
                    if (e.target.files) void uploadFiles(e.target.files);
                    e.target.value = "";
                  }}
                />
              </Button>
              <Button
                disabled={!canEdit || space !== "personal"}
                component="label"
                variant="outlined"
              >
                Upload torrent
                <input
                  hidden
                  type="file"
                  accept=".torrent"
                  onChange={(e) => {
                    if (e.target.files?.[0])
                      transfers.enqueue([e.target.files[0]], path, space, true);
                    e.target.value = "";
                  }}
                />
              </Button>
              <TextField
                disabled={!canEdit}
                size="small"
                label="New directory"
                value={newDir}
                onChange={(e) => setNewDir(e.target.value)}
              />
              <Button disabled={!canEdit} onClick={createDir}>
                Create
              </Button>
              <Typography variant="body2" color="text.secondary">
                Drag files here to upload, or drag drive items onto folders to
                move them.
              </Typography>
            </Stack>
          </Paper>

          <Paper variant="outlined" sx={{ p: 2 }}>
            <Stack direction={{ xs: "column", sm: "row" }} spacing={1}>
              <TextField
                fullWidth
                label="External file URL"
                type="url"
                value={remoteUrl}
                onChange={(event) => setRemoteUrl(event.target.value)}
                disabled={!canEdit || space !== "personal"}
              />
              <TextField
                label="Save as (optional)"
                value={remoteName}
                onChange={(event) => setRemoteName(event.target.value)}
                disabled={!canEdit || space !== "personal"}
              />
              <Button
                disabled={!canEdit || space !== "personal" || !remoteUrl.trim()}
                onClick={() => void importURL()}
              >
                Download to Drive
              </Button>
            </Stack>
            {importError && <Alert severity="error">{importError}</Alert>}
          </Paper>

          <ServerDownloadQueues
            path={path}
            canSubmit={canEdit && space === "personal"}
            accessAllowed={!accessDenied && !loadError}
            accessDenied={accessDenied}
          />

          <Box
            sx={{
              display: "grid",
              gridTemplateColumns:
                "repeat(auto-fill, minmax(min(190px, 100%), 1fr))",
              gap: 2,
            }}
          >
            {items.map((item) => (
              <FileCard
                key={item.path}
                item={item}
                canEdit={canEdit}
                canShare={space === "personal"}
                viewUrl={viewUrlFor(item.path)}
                onOpen={() => open(item)}
                onDelete={() => remove(item)}
                onMove={moveItem}
                onShare={() => setShareItem(item)}
                onPublicLink={
                  space === "personal" && !item.isDir
                    ? () => {
                        setPublicItem(item);
                        setPublicOpen(true);
                      }
                    : undefined
                }
              />
            ))}
          </Box>

          <Paper variant="outlined" sx={{ p: 2 }}>
            <Typography variant="h6">Torrent jobs</Typography>
            {jobs.map((job) => (
              <TorrentJobRow
                key={job.id}
                job={job}
                onControl={controlTorrent}
              />
            ))}
          </Paper>
        </Stack>
      </Container>
      {!accessDenied && (
        <TransferWidget
          uploads={transfers.rows}
          imports={imports}
          onCancel={transfers.cancel}
          onPause={transfers.pause}
          onResume={transfers.resume}
          onCancelImport={(id) => void cancelImport(id)}
          onClear={transfers.clear}
        />
      )}
      <PublicLinkDialog
        item={publicItem}
        open={publicOpen && !accessDenied}
        onClose={() => {
          setPublicItem(null);
          setPublicOpen(false);
        }}
      />
      <FileViewer
        file={selected}
        viewUrl={viewUrlFor}
        previewUrl={previewUrlFor}
        subtitleUrl={subtitleUrlFor}
        onClose={() => setSelected(null)}
      />
      <ShareDialog
        item={shareItem}
        target={shareTarget}
        permission={sharePermission}
        onTargetChange={setShareTarget}
        onPermissionChange={setSharePermission}
        onClose={() => setShareItem(null)}
        onShare={createShare}
      />
    </Box>
  );
}
