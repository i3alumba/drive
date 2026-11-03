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

## NAS backend preparation

`feat/nas-filesystem-storage` contains a candidate Linux/NFS filesystem backend.
The default and deployed backend remains MinIO; no production migration or
switch has been performed. The format, configuration, initialization mode,
completed checks and remaining release gates are recorded in
[docs/NAS-STORAGE.md](docs/NAS-STORAGE.md).

The API binary accepts `--help` and `--init-filesystem`. Initialization is a
separate operator-only mode that requires an empty, already-mounted NFS root and
exits without starting HTTP. Ordinary server startup never initializes storage.
`GET /healthz` is process liveness; anonymous `GET /readyz` checks storage without
returning backend details. Other endpoints remain protected by Auth, except explicitly created, expiring public-download capabilities (fragment exchange and `GET`/`HEAD /api/public/{owner}/{id}`).

The Linux-only [drive-migrate CLI](api/cmd/migrate/README.md) provides private
inventory/backup/restore, manifest-bound copy/resume/final sync and full payload
verification. It never writes/deletes S3 or automatically fences/deploys services.
`api/Dockerfile.arm64` and `frontend/Dockerfile.arm64` support the prepared beta
release; the API requires the exact separately built ARM64 runtime artifact to
be mirrored before the default CI build can run.

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
- Upload files into the current directory with file picker or drag-and-drop; a collapsible transfer widget shows per-file progress, saving, errors and cancellation
- Import a public HTTP(S) file URL into the current personal directory with server-side progress and cancellation
- Create/revoke seven-day public, read-only snapshot download links for personal files
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
- `POST /api/imports` with `{ "url": "https://example.com/file.txt", "path": "docs", "space": "personal" }` — start a URL download (`name` is optional)
- `GET /api/imports` — list only the authenticated user's URL-download jobs
- `DELETE /api/imports/{id}` — cancel an own download; saving returns 409 rather than claiming rollback
- `POST /api/public-links` with `{ "path": "docs/file.txt", "days": 7 }` — create an owner-only snapshot capability (1–30 days)
- `GET /api/public-links?path=docs/file.txt` — list own link metadata, without raw capability tokens
- `DELETE /api/public-links/{id}` — revoke an own link and remove its snapshot, never the original
- `/public/{owner}/{id}#token` — standalone shared-file landing page, without portal login
- `POST /api/public/{owner}/{id}/session` with `{ "token": "..." }` — exchange the fragment capability for a five-minute, snapshot-scoped HttpOnly/Secure/SameSite=Strict cookie
- `GET`/`HEAD /api/public/{owner}/{id}` — native streaming attachment download, requiring that cookie; the identifier alone cannot authorize access
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

## API build toolchain security

The API Dockerfiles use **Go 1.27.3** builders, replacing the obsolete
Go 1.22 standard library. Final verification includes `govulncheck ./api/cmd/server`
with that toolchain: no reachable findings were reported. The inherited Go module
alerts (including SSH-related transitive packages) are not automatically evidence
of code reachable in this API; the frontend production graph audit reported zero
advisories. These checks are not a guarantee of absence of vulnerabilities in the
unchanged OS/aria2/LibreOffice runtime. No module/npm dependency versions or the
pinned ARM64 runtime artifact were changed for this toolchain update.

## Transfers and public links

Computer uploads use **8 MiB parts**, avoiding Cloudflare's 100 MB request limit.
Two files run at a time, with one part in flight per file; same-destination queue
ordering is preserved while an earlier upload is paused. Progress counts durable
server acknowledgements. All parts received means **saving**, not completion:
publication runs asynchronously and the browser polls its result. Pause/resume,
limited transient retries and request deadlines replace an unbounded single POST.
After a reload, choose the original file again; the client verifies the size and
all already accepted part hashes before uploading missing data.

Linux API instances keep private, restart-resumable staging under
`TORRENT_WORK_DIR/.upload-sessions-v1` on the existing work filesystem. The parent
work directory must exist. No NAS initialization, migration, new PVC or format
change is performed. Defaults are **10 GiB per file**, **20 GiB reserved staging**,
**2 GiB free-space headroom**, two active sessions per user/four per process, and
24-hour idle expiry. Optional byte-valued `UPLOAD_MAX_FILE_BYTES`,
`UPLOAD_STAGING_BUDGET_BYTES` and `UPLOAD_FREE_RESERVE_BYTES` override the size
bounds; invalid values fail startup validation. The current format caps a file at
64 GiB (8192 parts). Headroom is a point-in-time guard on a filesystem also used
by torrents, not a guarantee against external writers consuming space.

Cancel reclaims only that session's temporary data. Once saving starts, cancellation
returns a conflict rather than promise rollback. Publication interrupted by a
restart is **uncertain**: check the destination instead of blindly retrying a
replacement. Existing cross-writer replacement semantics are unchanged; no new
compare-and-swap guarantee is claimed. Completed/cancelled metadata receipts are
retained until expiry, with a 1024-session history bound.

Torrent metadata retains its existing 8 MiB multipart endpoint and **Torrent added**
acknowledgement; its payload remains in the Torrent jobs panel. No passwords,
source URLs, capabilities, full files or authentication tokens are saved to
browser storage. See [the chunked-upload protocol and tests](docs/CHUNKED-UPLOADS.md).

Each actively downloading torrent shows its own connected-peer count
(`connectedPeers`, aria2's `CN` connection count, not tracker-advertised swarm
population), current speed in B/s or binary KiB/s/MiB/s, and ETA when available.
Zero peers and zero speed remain visible. Older API responses without a peer
count show a dash. Rates/peers/ETA reset when pausing, cancelling, saving, completing
or failing; inactive rows do not display stale transfer measurements.

URL imports are personal-space only, up to **1 GiB**, ten minutes, two active jobs
per user and four per API process. Files are staged privately before storage
publication; a truncated/oversized/failed download is not published. Before saving,
the importer checks for an existing destination and refuses known replacements;
use **Save as (optional)** to choose another name. This check is not an atomic
no-clobber primitive across other browser/API writers: avoid simultaneous writes
to the same path. Jobs and their
progress are in memory (not a restart-resumable queue); completed history is bounded.
Only HTTP(S), ports 80/443 and public destinations are allowed. Every connection
resolves and checks all IPs, then dials a checked literal; redirects are checked,
HTTPS downgrades rejected, and private/loopback/link-local/metadata/reserved networks
blocked. Environment proxies, cookies, credentials and signed-source referrers are
not forwarded. Signed query strings are accepted but never returned in job JSON.
This protection is for the new URL importer, not a claim that arbitrary torrent
metadata has equivalent network isolation.

Public links are explicit **anyone-with-the-link** capabilities; they intentionally
do not require Auth, including when the owner later loses portal access. Revoke
before removing the owner's Drive grant if those links must stop immediately.
The UI defaults to seven days. The token is shown only on creation: copy it before
closing; stored link metadata contains only its SHA-256 lookup ID. The long-lived
token is **only in the URL fragment**, which browsers do not send in HTTP request
URIs. The landing page consumes/removes the fragment and exchanges it in a JSON
POST body. The resulting cookie lasts at most five minutes and is signed with an
ephemeral per-process key, scoped to that owner/snapshot, HttpOnly, Secure and
SameSite=Strict. Downloads stream natively to disk, not through a large JS Blob.
Expiry and revocation are rechecked on each download. After an API restart the
recipient can renew with the original shared link. Public links require HTTPS
(loopback browser testing is separate); never downgrade Secure cookies. Links download
an immutable **separate copy**, so later replacements cannot accidentally expose
new private content. Deleting/moving the original does **not** revoke this snapshot.
Revoke links explicitly to remove their copies; expiration denies new downloads
but does not automatically reclaim storage. There is one snapshot copy in flight
per process and a 100-link owner limit (revoke old/expired links to free slots).
An already-started download cannot be recalled. Downloads are attachments with
no-store/no-referrer/nosniff headers, not public inline previews or directory shares.
The owner-wide **Manage public links** entry point includes deleted/moved originals
and unlinked snapshots whose earlier cleanup failed. Revocation/data cleanup is
idempotent and retryable even after metadata was removed. Failed creation cleanup
uses an independent bounded context rather than an already-cancelled request.

Treat the shared fragment, exchange body and short-lived cookie as credentials:
never log them or export them to analytics/tracing. The long-lived capability is
not in any request URI; identifier-only proxy paths are not authorization. Review
body/header logging at actual ingress/CDN hops before release. No proxy/network/
deployment configuration is changed by this feature.

Snapshot data/metadata use `_drive/public-link-data/{owner}/{hash}` and
`_drive/public-links/{owner}/{hash}.json`; original `users/{id}/` prefixes and existing
user-to-user sharing stay unchanged. The implementation uses the common Store
interface for both existing backends; no NFS initialization, migration, remount or
production cutover is performed.

### Real torrent regression tests

With `aria2c` available on PATH, run from `drive/`:

```sh
DRIVE_TORRENT_INTEGRATION=1 go test -race -count=1 ./api/internal/torrent
```

These opt-in tests generate three tiny legal torrents (single file, nested multi-file,
and multi-piece Unicode text), serve their payloads from a loopback BEP-19 webseed,
run **real aria2**, and compare every uploaded byte/path. Additional real tests check
pause/resume and cancellation cleanup. DHT, LPD, PEX and public trackers are disabled;
no public swarm, production storage or original user data is needed. The tests fail
if explicitly enabled without aria2; ordinary runs skip only this opt-in integration.
Torrent metadata is limited to 8 MiB and torrent jobs require a personal space;
shared-folder computer uploads and existing user-to-user sharing remain available.
This prevents creating cross-owner torrent jobs that cannot be safely listed or
controlled by the initiating user under the existing prefix-based job contract.
This does not establish public-tracker/peer reachability in production. Runtime
containers already include aria2; this feature adds no application dependencies.

## Notes

- User files and directory markers are stored only in the shared root MinIO bucket (`DRIVE_MINIO_BUCKET`, default `drive`) under `users/{auth_user_id}/` prefixes.
- Share metadata is stored in the same bucket at `_drive/shares.json`.
- Torrent downloads use the `drive_torrent_work` compose volume for temporary container storage, then upload results to MinIO and clean up the work directory.
- Exposing torrent clients to untrusted users has security and legal implications; deploy behind authentication and network controls before production use.
