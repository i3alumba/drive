# Remote Drive

Remote file storage integrated into the root project compose stack with:

- **shared root nginx** at `https://drive.i3alumba.ru`
- **Go API service** for object, directory, and torrent operations
- **shared root MinIO S3 storage** for all user data
- **per-user file spaces** under authenticated user IDs
- **read-only or edit sharing** of files/directories with other users
- **React + TypeScript + Material UI frontend** served by nginx

## Authentication and access

The API fails closed when Auth is unconfigured/unavailable. Every authenticated
request checks a live Auth identity with a canonical positive numeric ID and
requires the exact boolean `services.drive: true`; IDs continue to select the
existing per-user storage prefixes. No anonymous storage account is created and
Auth HTTP redirects are rejected.

Browser mutations require an explicit bearer header. Cookie authentication is
read-only (`GET`/`HEAD`), including media elements; a cookie-only cross-site POST
cannot mutate files. Missing/invalid credentials return 401, a missing Drive
grant 403, and unconfigured Auth 503. Upstream validation/network failures
currently return 401 rather than 503; they still fail closed. The frontend
explains access denial without exposing file lists, disables mutations, and
offers retry/Dashboard navigation.

Checks: `go test -race ./api/...`, frontend `npm run build` and
`npm run test:theme` (theme plus mocked access/bearer regressions). The confirmed
unused empty legacy UI stub was removed only after SSO and original-data
acceptance; no storage prefixes, credentials or runtime behavior changed.
Package-wide `go list ./...` / `go test ./...` still fail in untouched legacy
`cmd/` and `internal/` adapters (old `api/internal/...` imports and missing legacy
gRPC/MinIO dependencies). Those adapters/dependencies are not repaired or removed
by the conservative cleanup; active production checks target `./api/...`.

## Run

From the repository root:

```bash
docker compose up --build drive drive-api nginx minio
```

Open:

- App: <https://drive.i3alumba.ru> in production, or `http://drive.i3alumba.ru` when using the plain local nginx listener
- MinIO console: <https://minio.i3alumba.ru> using the root stack MinIO credentials

## Features

- Switch between light and dark themes with the sun/moon button in the header
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

Run the active Go API tests:

```bash
go test ./api/...
go test -race ./api/...
```

Run frontend locally:

```bash
cd frontend
npm install
npm run dev
```

### Color theme

The initial theme follows the operating system. Use the sun/moon button in the
header to choose light or dark mode explicitly. The browser saves this selection
under `drive-color-mode` and synchronizes it between open Drive tabs. Dashboard
has its own independent preference. If browser storage is disabled, switching
still works for the current page, but the selection cannot survive a reload.
File cards, forms, dialogs and previews use the selected theme; image and video
canvases intentionally remain black.

Run the theme regression tests with mocked API data (no production access):

```bash
cd frontend
npm install
npx playwright install chromium
npm run test:theme
npm run build
```

To use an existing Chromium installation instead:
`CHROMIUM_PATH=/usr/bin/google-chrome-stable npm run test:theme`.

## Notes

- User files and directory markers are stored only in the shared root MinIO bucket (`DRIVE_MINIO_BUCKET`, default `drive`) under `users/{auth_user_id}/` prefixes.
- Share metadata is stored in the same bucket at `_drive/shares.json`.
- Torrent downloads use the `drive_torrent_work` compose volume for temporary container storage, then upload results to MinIO and clean up the work directory.
- Exposing torrent clients to untrusted users has security and legal implications; deploy behind authentication and network controls before production use.
