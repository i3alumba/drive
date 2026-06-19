import {
  type DragEvent,
  useCallback,
  useEffect,
  useMemo,
  useState,
} from "react";
import {
  AppBar,
  Box,
  Button,
  Container,
  Paper,
  Stack,
  TextField,
  Toolbar,
  Typography,
} from "@mui/material";
import CloudUploadIcon from "@mui/icons-material/CloudUpload";
import { apiFetch } from "../api";
import { FileCard } from "../components/FileCard";
import { FileViewer } from "../components/FileViewer";
import { ShareDialog } from "../components/ShareDialog";
import { TorrentJobRow } from "../components/TorrentJobRow";
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
  const canEdit = activeSpace?.permission !== "read";
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
    const spacesRes = await apiFetch(`${api}/api/spaces`);
    const nextSpaces = (await spacesRes.json()) as Space[];
    setSpaces(nextSpaces);
    if (!nextSpaces.some((candidate) => candidate.id === space)) {
      setSpace("personal");
      return;
    }
    const res = await apiFetch(
      `${api}/api/files?path=${encodeURIComponent(path)}&space=${encodeURIComponent(space)}`,
    );
    setItems(await res.json());
    const jobRes = await apiFetch(`${api}/api/torrents`);
    setJobs(await jobRes.json());
  }, [path, space]);

  useEffect(() => {
    refresh();
  }, [refresh]);
  useEffect(() => {
    if (selected) return;
    const id = setInterval(refresh, 4000);
    return () => clearInterval(id);
  }, [refresh, selected]);

  async function upload(
    file: File,
    endpoint: "/api/upload" | "/api/torrents",
    field: "file" | "torrent",
  ) {
    const data = new FormData();
    data.append(field, file);
    data.append("path", path);
    data.append("space", space);
    await apiFetch(endpoint, { method: "POST", body: data });
    await refresh();
  }

  async function uploadFiles(files: FileList | File[], targetPath = path) {
    for (const file of Array.from(files)) {
      const endpoint = file.name.toLowerCase().endsWith(".torrent")
        ? "/api/torrents"
        : "/api/upload";
      const field = endpoint === "/api/torrents" ? "torrent" : "file";
      const data = new FormData();
      data.append(field, file);
      data.append("path", targetPath);
      data.append("space", space);
      await apiFetch(endpoint, { method: "POST", body: data });
    }
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
      onDragOver={handleRootDragOver}
      onDrop={handleRootDrop}
      onDragLeave={() => setDropTarget(null)}
    >
      <AppBar position="static">
        <Toolbar>
          <Typography variant="h6">Remote Drive</Typography>
        </Toolbar>
      </AppBar>
      <Container sx={{ py: 4 }}>
        <Stack spacing={3}>
          <Paper sx={{ p: 2 }}>
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
                value={space}
                onChange={(event) => changeSpace(String(event.target.value))}
                sx={{ p: 1, borderRadius: 1, borderColor: "divider" }}
              >
                {spaces.map((candidate) => (
                  <option key={candidate.id} value={candidate.id}>
                    {candidate.name} ({candidate.permission})
                  </option>
                ))}
              </Box>
              <Button onClick={goUp} disabled={!path}>
                Up
              </Button>
              <Typography>/ {breadcrumbs.join(" / ")}</Typography>
            </Box>
          </Paper>

          <Paper
            sx={{
              p: 2,
              border: "2px dashed",
              borderColor: dropTarget ? "primary.main" : "divider",
              bgcolor: dropTarget ? "primary.50" : undefined,
            }}
            onDragOver={handleRootDragOver}
            onDrop={handleRootDrop}
          >
            <Stack
              direction={{ xs: "column", sm: "row" }}
              spacing={2}
              sx={{ alignItems: { sm: "center" } }}
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
                  onChange={(e) =>
                    e.target.files && uploadFiles(e.target.files)
                  }
                />
              </Button>
              <Button disabled={!canEdit} component="label" variant="outlined">
                Upload torrent
                <input
                  hidden
                  type="file"
                  accept=".torrent"
                  onChange={(e) =>
                    e.target.files?.[0] &&
                    upload(e.target.files[0], "/api/torrents", "torrent")
                  }
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

          <Box
            sx={{
              display: "grid",
              gridTemplateColumns: "repeat(auto-fill, minmax(190px, 1fr))",
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
              />
            ))}
          </Box>

          <Paper sx={{ p: 2 }}>
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
