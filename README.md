# Remote Drive

Docker Compose managed remote file storage with:

- **nginx gateway** on `http://localhost:8080`
- **Go API service** for object, directory, and torrent operations
- **MinIO S3 storage** for all user data
- **React + TypeScript + Material UI frontend** served by nginx

## Run

```bash
docker compose up --build
```

Open:

- App: <http://localhost:8080>
- MinIO console: <http://localhost:9001> (`minioadmin` / `minioadmin`)

## Features

- Browse MinIO-backed files by directory prefix
- Create directories
- Upload files into the current directory with file picker or drag-and-drop
- Move files and folders by dragging drive items onto folders
- Preview photos, GIFs, videos, audio, text, PDF, Word, and LibreOffice documents inline
- Download and delete files/directories
- Upload `.torrent` files; the Go service downloads the torrent to temporary storage and uploads completed files into MinIO
- Pause, resume, and cancel torrent jobs from the UI or API

## API

- `GET /api/files?path=docs` — list directory
- `POST /api/directories` with `{ "path": "docs/photos" }` — create directory
- `POST /api/upload` multipart fields `file`, `path` — upload file
- `GET /api/download?path=docs/file.txt` — download file
- `GET /api/view?path=docs/file.txt` — inline/range-capable file view for previews and media playback
- `GET /api/preview?path=docs/file.docx` — converted inline preview for office documents
- `DELETE /api/files?path=docs&dir=true` — delete directory recursively
- `DELETE /api/files?path=docs/file.txt` — delete file
- `POST /api/move` with `{ "source": "docs/a.txt", "destination": "archive/a.txt", "isDir": false }` — move file or directory
- `POST /api/torrents` multipart fields `torrent`, `path` — enqueue torrent download
- `GET /api/torrents` — list torrent jobs
- `GET /api/torrents/{id}` — get torrent job
- `POST /api/torrents/{id}/pause` — pause an active torrent job and keep partial data for resume
- `POST /api/torrents/{id}/resume` — resume a paused torrent job
- `POST /api/torrents/{id}/cancel` — cancel a torrent job and remove temporary partial data

## Development

Run Go tests:

```bash
go test ./...
```

Run frontend locally:

```bash
cd frontend
npm install
npm run dev
```

## Notes

- User files and directory markers are stored only in MinIO.
- Torrent downloads use temporary container storage, then upload results to MinIO and clean up the work directory.
- Exposing torrent clients to untrusted users has security and legal implications; deploy behind authentication and network controls before production use.
