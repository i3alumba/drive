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

Checks: package-wide Go commands below, frontend `npm run build` and
`npm run test:theme` (theme plus mocked access/bearer regressions).

The unsupported prototype in root `cmd/` and `internal/`, including its hardcoded
identity stub and experimental gRPC interface, has been retired. The old root
`Dockerfile` was removed with it. The supported server remains
`api/cmd/server`, built with `api/Dockerfile`; existing Compose/CI entrypoints,
SSO checks, storage prefixes and credentials are unchanged. No legacy gRPC or
MinIO dependencies were added to make the prototype compile.

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

Run checks for the entire Go module from `drive/`:

```bash
go list ./...
go build ./...
go test ./...
go test -race ./...
go vet ./...
```

Build the supported API container from `drive/` (plain `docker build .` is no
longer supported):

```bash
docker build -f api/Dockerfile -t remote-drive-api .
```

Run frontend locally:

```bash
cd frontend
npm ci
npm run dev
```

### Color theme

The shared i3alumba UI contract v1 uses identical `frontend/src/ui/sharedTheme.ts`
and `themeStorage.ts` modules across services. Light/dark palettes, controls,
header and responsive surfaces are shared; image and video canvases stay black.
The initial theme follows the system unless a saved preference exists. The
sun/moon button chooses an explicit mode without changing file or form state.

Only the non-sensitive `i3alumba-theme=light|dark|system` cookie is shared with
`Domain=i3alumba.ru`, `Path=/`, `SameSite=Lax`, a one-year lifetime and `Secure`
on HTTPS. It is not authentication; no authentication cookies or API behavior
are changed. The cookie wins over `i3alumba-color-mode` localStorage; the legacy
`drive-color-mode` preference seeds it when no shared preference exists. Tabs
synchronize via storage events; sibling subdomains synchronize via one-second
cookie polling and focus/pageshow checks. Invalid preferences are ignored. When
cookies/storage are denied, switching still works in memory for the current page.
Standalone deployments use a host-only theme cookie outside i3alumba.ru.

Run the theme regression tests with mocked API data (no production access):

```bash
cd frontend
npm ci
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
