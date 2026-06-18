import React, { DragEvent, useCallback, useEffect, useMemo, useState } from 'react';
import { createRoot } from 'react-dom/client';
import {
  AppBar,
  Box,
  Button,
  Card,
  CardActionArea,
  CardContent,
  CardMedia,
  Chip,
  Container,
  Dialog,
  DialogContent,
  DialogTitle,
  IconButton,
  LinearProgress,
  Paper,
  Stack,
  TextField,
  Toolbar,
  Tooltip,
  Typography,
} from '@mui/material';
import AudioFileIcon from '@mui/icons-material/AudioFile';
import CloseIcon from '@mui/icons-material/Close';
import DeleteIcon from '@mui/icons-material/Delete';
import DescriptionIcon from '@mui/icons-material/Description';
import CloudUploadIcon from '@mui/icons-material/CloudUpload';
import FolderIcon from '@mui/icons-material/Folder';
import ImageIcon from '@mui/icons-material/Image';
import InsertDriveFileIcon from '@mui/icons-material/InsertDriveFile';
import MovieIcon from '@mui/icons-material/Movie';
import PauseIcon from '@mui/icons-material/Pause';
import PlayArrowIcon from '@mui/icons-material/PlayArrow';
import StopCircleIcon from '@mui/icons-material/StopCircle';

type DriveObject = { name: string; path: string; size: number; lastModified: string; isDir: boolean };
type TorrentJob = { id: string; name: string; targetDir: string; status: string; progress: number; error?: string };
type FileKind = 'image' | 'video' | 'audio' | 'text' | 'pdf' | 'office' | 'other';

const api = '';
const driveDragType = 'application/x-drive-item';
const textExtensions = new Set(['txt', 'md', 'json', 'csv', 'log', 'xml', 'yaml', 'yml', 'go', 'ts', 'tsx', 'js', 'jsx', 'py', 'rs', 'java', 'c', 'cpp', 'h', 'css', 'html']);
const officeExtensions = new Set(['doc', 'docx', 'odt', 'ods', 'odp', 'ppt', 'pptx', 'xls', 'xlsx', 'rtf']);

function App() {
  const [path, setPath] = useState('');
  const [items, setItems] = useState<DriveObject[]>([]);
  const [newDir, setNewDir] = useState('');
  const [jobs, setJobs] = useState<TorrentJob[]>([]);
  const [selected, setSelected] = useState<DriveObject | null>(null);
  const [dropTarget, setDropTarget] = useState<string | null>(null);
  const breadcrumbs = useMemo(() => path.split('/').filter(Boolean), [path]);

  const refresh = useCallback(async () => {
    const res = await fetch(`${api}/api/files?path=${encodeURIComponent(path)}`);
    setItems(await res.json());
    const jobRes = await fetch(`${api}/api/torrents`);
    setJobs(await jobRes.json());
  }, [path]);

  useEffect(() => { refresh(); }, [refresh]);
  useEffect(() => {
    if (selected) return;
    const id = setInterval(refresh, 4000);
    return () => clearInterval(id);
  }, [refresh, selected]);

  async function upload(file: File, endpoint: '/api/upload' | '/api/torrents', field: 'file' | 'torrent') {
    const data = new FormData();
    data.append(field, file);
    data.append('path', path);
    await fetch(endpoint, { method: 'POST', body: data });
    await refresh();
  }

  async function uploadFiles(files: FileList | File[], targetPath = path) {
    for (const file of Array.from(files)) {
      const endpoint = file.name.toLowerCase().endsWith('.torrent') ? '/api/torrents' : '/api/upload';
      const field = endpoint === '/api/torrents' ? 'torrent' : 'file';
      const data = new FormData();
      data.append(field, file);
      data.append('path', targetPath);
      await fetch(endpoint, { method: 'POST', body: data });
    }
    await refresh();
  }

  async function createDir() {
    if (!newDir.trim()) return;
    const dirPath = [path, newDir.trim()].filter(Boolean).join('/');
    await fetch('/api/directories', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ path: dirPath }) });
    setNewDir('');
    await refresh();
  }

  async function remove(item: DriveObject) {
    await fetch(`/api/files?path=${encodeURIComponent(item.path)}&dir=${item.isDir}`, { method: 'DELETE' });
    if (selected?.path === item.path) setSelected(null);
    await refresh();
  }

  async function controlTorrent(id: string, action: 'pause' | 'resume' | 'cancel') {
    await fetch(`/api/torrents/${encodeURIComponent(id)}/${action}`, { method: 'POST' });
    await refresh();
  }

  async function moveItem(item: DriveObject, destinationDir: string) {
    const destination = joinPath(destinationDir, baseName(item.path));
    if (destination === item.path || destination.startsWith(item.path + '/')) return;
    await fetch('/api/move', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ source: item.path, destination, isDir: item.isDir }),
    });
    if (selected?.path === item.path) setSelected(null);
    await refresh();
  }

  function open(item: DriveObject) {
    if (item.isDir) setPath(item.path);
    else setSelected(item);
  }

  function goUp() { setPath(breadcrumbs.slice(0, -1).join('/')); }

  function handleRootDragOver(event: DragEvent) {
    if (event.dataTransfer.types.includes('Files') || event.dataTransfer.types.includes(driveDragType)) {
      event.preventDefault();
      setDropTarget(path || '/');
    }
  }

  async function handleRootDrop(event: DragEvent) {
    event.preventDefault();
    setDropTarget(null);
    const dragged = readDraggedItem(event);
    if (dragged) await moveItem(dragged, path);
    else if (event.dataTransfer.files.length > 0) await uploadFiles(event.dataTransfer.files, path);
  }

  return <Box onDragOver={handleRootDragOver} onDrop={handleRootDrop} onDragLeave={() => setDropTarget(null)}>
    <AppBar position="static"><Toolbar><Typography variant="h6">Remote Drive</Typography></Toolbar></AppBar>
    <Container sx={{ py: 4 }}>
      <Stack spacing={3}>
        <Paper sx={{ p: 2 }}>
          <Box sx={{ display: 'flex', gap: 1, alignItems: 'center', flexWrap: 'wrap' }}>
            <Button onClick={goUp} disabled={!path}>Up</Button>
            <Typography>/ {breadcrumbs.join(' / ')}</Typography>
          </Box>
        </Paper>

        <Paper
          sx={{ p: 2, border: '2px dashed', borderColor: dropTarget ? 'primary.main' : 'divider', bgcolor: dropTarget ? 'primary.50' : undefined }}
          onDragOver={handleRootDragOver}
          onDrop={handleRootDrop}
        >
          <Stack direction={{ xs: 'column', sm: 'row' }} spacing={2} sx={{ alignItems: { sm: 'center' } }}>
            <Button component="label" variant="contained" startIcon={<CloudUploadIcon />}>Upload file<input hidden multiple type="file" onChange={e => e.target.files && uploadFiles(e.target.files)} /></Button>
            <Button component="label" variant="outlined">Upload torrent<input hidden type="file" accept=".torrent" onChange={e => e.target.files?.[0] && upload(e.target.files[0], '/api/torrents', 'torrent')} /></Button>
            <TextField size="small" label="New directory" value={newDir} onChange={e => setNewDir(e.target.value)} />
            <Button onClick={createDir}>Create</Button>
            <Typography variant="body2" color="text.secondary">Drag files here to upload, or drag drive items onto folders to move them.</Typography>
          </Stack>
        </Paper>

        <Box sx={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(190px, 1fr))', gap: 2 }}>
          {items.map(item => <FileCard key={item.path} item={item} onOpen={() => open(item)} onDelete={() => remove(item)} onMove={moveItem} />)}
        </Box>

        <Paper sx={{ p: 2 }}>
          <Typography variant="h6">Torrent jobs</Typography>
          {jobs.map(job => <TorrentJobRow key={job.id} job={job} onControl={controlTorrent} />)}
        </Paper>
      </Stack>
    </Container>
    <FileViewer file={selected} onClose={() => setSelected(null)} />
  </Box>;
}

function TorrentJobRow({ job, onControl }: { job: TorrentJob; onControl: (id: string, action: 'pause' | 'resume' | 'cancel') => Promise<void> }) {
  const canPause = job.status === 'queued' || job.status === 'downloading';
  const canResume = job.status === 'paused';
  const canCancel = !['complete', 'cancelled'].includes(job.status);

  return <Box sx={{ my: 1 }}>
    <Stack direction={{ xs: 'column', sm: 'row' }} spacing={1} sx={{ alignItems: { sm: 'center' } }}>
      <Box sx={{ flex: 1 }}>
        <Typography>{job.name}: {job.status} {job.error}</Typography>
        <LinearProgress variant="determinate" value={Math.round(job.progress * 100)} />
      </Box>
      {canPause && <Button size="small" startIcon={<PauseIcon />} onClick={() => onControl(job.id, 'pause')}>Pause</Button>}
      {canResume && <Button size="small" startIcon={<PlayArrowIcon />} onClick={() => onControl(job.id, 'resume')}>Resume</Button>}
      {canCancel && <Button size="small" color="error" startIcon={<StopCircleIcon />} onClick={() => onControl(job.id, 'cancel')}>Cancel</Button>}
    </Stack>
  </Box>;
}

function FileCard({ item, onOpen, onDelete, onMove }: { item: DriveObject; onOpen: () => void; onDelete: () => void; onMove: (item: DriveObject, destinationDir: string) => Promise<void> }) {
  const [isDropTarget, setIsDropTarget] = useState(false);
  const kind = getKind(item.name, item.isDir);
  const src = viewUrl(item.path);

  function handleDragStart(event: DragEvent) {
    event.dataTransfer.effectAllowed = 'move';
    event.dataTransfer.setData(driveDragType, JSON.stringify(item));
  }

  function handleDragOver(event: DragEvent) {
    if (!item.isDir || !event.dataTransfer.types.includes(driveDragType)) return;
    event.preventDefault();
    event.stopPropagation();
    setIsDropTarget(true);
  }

  async function handleDrop(event: DragEvent) {
    if (!item.isDir) return;
    event.preventDefault();
    event.stopPropagation();
    setIsDropTarget(false);
    const dragged = readDraggedItem(event);
    if (dragged) await onMove(dragged, item.path);
  }

  return <Card
    variant="outlined"
    draggable
    onDragStart={handleDragStart}
    onDragOver={handleDragOver}
    onDrop={handleDrop}
    onDragLeave={() => setIsDropTarget(false)}
    sx={{ borderColor: isDropTarget ? 'primary.main' : undefined, borderWidth: isDropTarget ? 2 : 1 }}
  >
    <CardActionArea onClick={onOpen}>
      <Box sx={{ height: 128, bgcolor: isDropTarget ? 'primary.50' : 'grey.100', display: 'flex', alignItems: 'center', justifyContent: 'center', overflow: 'hidden' }}>
        {kind === 'image' && <CardMedia component="img" image={src} alt={item.name} sx={{ width: '100%', height: '100%', objectFit: 'cover' }} />}
        {kind === 'video' && <CardMedia component="video" src={src} muted preload="metadata" sx={{ width: '100%', height: '100%', objectFit: 'cover' }} />}
        {kind !== 'image' && kind !== 'video' && iconForKind(kind, 56)}
      </Box>
      <CardContent sx={{ pb: 1 }}>
        <Tooltip title={item.name}><Typography noWrap>{item.name}</Typography></Tooltip>
        <Typography variant="caption" color="text.secondary">{item.isDir ? 'Directory' : `${Math.round(item.size / 1024)} KB`}</Typography>
      </CardContent>
    </CardActionArea>
    <Box sx={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', px: 1, pb: 1 }}>
      <Chip size="small" label={item.isDir ? 'folder' : kind} />
      <IconButton size="small" onClick={(event) => { event.stopPropagation(); onDelete(); }}><DeleteIcon fontSize="small" /></IconButton>
    </Box>
  </Card>;
}

function FileViewer({ file, onClose }: { file: DriveObject | null; onClose: () => void }) {
  const [text, setText] = useState<string>('');
  const [textError, setTextError] = useState<string>('');
  const kind = file ? getKind(file.name, false) : 'other';
  const src = file ? viewUrl(file.path) : '';

  useEffect(() => {
    setText('');
    setTextError('');
    if (!file || getKind(file.name, false) !== 'text') return;
    fetch(viewUrl(file.path)).then(async res => {
      if (!res.ok) throw new Error(await res.text());
      setText(await res.text());
    }).catch(err => setTextError(String(err)));
  }, [file]);

  if (!file) return null;

  return <Dialog open={Boolean(file)} onClose={onClose} maxWidth="xl" fullWidth>
    <DialogTitle sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
      {iconForKind(kind, 28)}
      <Typography sx={{ flex: 1 }} noWrap>{file.name}</Typography>
      <IconButton onClick={onClose}><CloseIcon /></IconButton>
    </DialogTitle>
    <DialogContent dividers sx={{ minHeight: '70vh', bgcolor: kind === 'image' || kind === 'video' ? 'grey.950' : undefined }}>
      {kind === 'image' && <Box component="img" src={src} alt={file.name} sx={{ display: 'block', maxWidth: '100%', maxHeight: '75vh', mx: 'auto', objectFit: 'contain' }} />}
      {kind === 'video' && <Stack spacing={1}>
        <Box component="video" src={src} controls sx={{ width: '100%', maxHeight: '72vh', bgcolor: 'black' }}>
          <track kind="subtitles" src={subtitleUrl(file.path)} srcLang="en" label="Subtitles" default />
        </Box>
        <Typography variant="caption" color="grey.400">Playback uses browser controls. Quality choices appear when the uploaded video contains browser-supported adaptive renditions; otherwise the original quality is played.</Typography>
      </Stack>}
      {kind === 'audio' && <Stack spacing={2} sx={{ p: 2 }}>
        <Typography>{file.name}</Typography>
        <Box component="audio" src={src} controls sx={{ width: '100%' }}>
          <track kind="subtitles" src={subtitleUrl(file.path)} srcLang="en" label="Subtitles" default />
        </Box>
      </Stack>}
      {kind === 'text' && <Paper variant="outlined" sx={{ p: 2, whiteSpace: 'pre-wrap', fontFamily: 'monospace', overflow: 'auto', maxHeight: '72vh' }}>{textError || text || 'Loading…'}</Paper>}
      {kind === 'pdf' && <Box component="iframe" title={file.name} src={src} sx={{ border: 0, width: '100%', height: '75vh' }} />}
      {kind === 'office' && <Stack spacing={1}>
        <Typography variant="body2">Document preview is converted to PDF on demand.</Typography>
        <Box component="iframe" title={file.name} src={previewUrl(file.path)} sx={{ border: 0, width: '100%', height: '75vh' }} />
      </Stack>}
      {kind === 'other' && <Stack spacing={2} sx={{ py: 8, alignItems: 'center' }}>
        <InsertDriveFileIcon sx={{ fontSize: 72 }} />
        <Typography>No inline preview is available for this file type.</Typography>
        <Button variant="contained" href={src} target="_blank" rel="noreferrer">Open raw file</Button>
      </Stack>}
    </DialogContent>
  </Dialog>;
}

function getKind(name: string, isDir: boolean): FileKind | 'folder' {
  if (isDir) return 'folder';
  const ext = name.split('.').pop()?.toLowerCase() ?? '';
  if (['jpg', 'jpeg', 'png', 'gif', 'webp', 'bmp', 'svg'].includes(ext)) return 'image';
  if (['mp4', 'webm', 'ogg', 'ogv', 'mov', 'm4v'].includes(ext)) return 'video';
  if (['mp3', 'wav', 'flac', 'm4a', 'aac', 'oga', 'opus'].includes(ext)) return 'audio';
  if (ext === 'pdf') return 'pdf';
  if (textExtensions.has(ext)) return 'text';
  if (officeExtensions.has(ext)) return 'office';
  return 'other';
}

function iconForKind(kind: FileKind | 'folder', size: number) {
  const sx = { fontSize: size, color: 'text.secondary' };
  if (kind === 'folder') return <FolderIcon sx={{ ...sx, color: 'warning.main' }} />;
  if (kind === 'image') return <ImageIcon sx={sx} />;
  if (kind === 'video') return <MovieIcon sx={sx} />;
  if (kind === 'audio') return <AudioFileIcon sx={sx} />;
  if (kind === 'text' || kind === 'pdf' || kind === 'office') return <DescriptionIcon sx={sx} />;
  return <InsertDriveFileIcon sx={sx} />;
}

function readDraggedItem(event: DragEvent): DriveObject | null {
  const raw = event.dataTransfer.getData(driveDragType);
  if (!raw) return null;
  try { return JSON.parse(raw) as DriveObject; } catch { return null; }
}

function baseName(path: string) { return path.split('/').filter(Boolean).pop() ?? path; }
function joinPath(dir: string, name: string) { return [dir.replace(/\/$/, ''), name].filter(Boolean).join('/'); }
function viewUrl(path: string) { return `/api/view?path=${encodeURIComponent(path)}`; }
function previewUrl(path: string) { return `/api/preview?path=${encodeURIComponent(path)}`; }
function subtitleUrl(path: string) { return `/api/view?path=${encodeURIComponent(path.replace(/\.[^/.]+$/, '.vtt'))}`; }

createRoot(document.getElementById('root')!).render(<App />);
