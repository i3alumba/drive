# Remote Drive

Remote file storage integrated into the root project compose stack with:

- **shared root nginx** at `https://drive.i3alumba.ru`
- **Go API service** for object, directory, and torrent operations
- **shared root MinIO S3 storage** for all user data
- **per-user file spaces** under authenticated user IDs
- **read-only or edit sharing** of files/directories with other users
- **React + TypeScript + Material UI frontend** served by nginx

## Run

From the repository root:

```bash
docker compose up --build drive drive-api nginx minio
```

Open:

- App: <https://drive.i3alumba.ru> in production, or `http://drive.i3alumba.ru` when using the plain local nginx listener
- MinIO console: <https://minio.i3alumba.ru> using the root stack MinIO credentials

## Features

- Browse each user's private MinIO-backed file space
- Switch into file spaces shared by other users
- Share files/directories with read-only or edit permissions
- Create directories
- Upload files into the current directory with file picker or drag-and-drop
- Move files and folders by dragging drive items onto folders
- Preview photos, GIFs, videos, audio, text, PDF, Word, and LibreOffice documents inline
- Download and delete files/directories
- Upload `.torrent` files; the Go service downloads the torrent to temporary storage and uploads completed files into MinIO
- Pause, resume, and cancel torrent jobs from the UI or API

## API

- `GET /api/spaces` — list the current user's personal space and incoming shared spaces
- `GET /api/shares` — list incoming and outgoing shares
- `POST /api/shares` with `{ "path": "docs", "isDir": true, "targetUsername": "alice", "permission": "read" }` — share a path
- `DELETE /api/shares/{id}` — remove an outgoing share
- `GET /api/files?path=docs&space=personal` — list directory
- `POST /api/directories?space=personal` with `{ "path": "docs/photos" }` — create directory
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

Run Go API tests:

```bash
go test ./api/...
```

Run frontend locally:

```bash
cd frontend
npm install
npm run dev
```

## Notes

- User files and directory markers are stored only in the shared root MinIO bucket (`DRIVE_MINIO_BUCKET`, default `drive`) under `users/{auth_user_id}/` prefixes.
- Share metadata is stored in the same bucket at `_drive/shares.json`.
- Torrent downloads use the `drive_torrent_work` compose volume for temporary container storage, then upload results to MinIO and clean up the work directory.
- Exposing torrent clients to untrusted users has security and legal implications; deploy behind authentication and network controls before production use.
