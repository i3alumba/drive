# MKV/AVI video previews (opt-in source feature)

Drive can prepare a compatible MP4 for MKV/AVI files before native browser
playback. Compatible H.264/AAC streams are remuxed; other supported streams are
converted to H.264/AAC, up to 1920×1080. Original files and download routes remain
unchanged. There is no HLS, adaptive-quality selector, embedded-subtitle support,
audio-track selector, DRM support or guarantee that every codec/browser works.
Only the first video and optional first audio track are used.

**Disabled by default. This source change does not build/publish images, modify
Compose/Kubernetes, provision scratch, or enable/deploy the worker.** It does not
resume imports, change NAS data/permissions, or modify TLS configuration.

## User flow

MKV/AVI cards show a video icon, without fetching original media or automatically
starting conversions. Open a file and press **Prepare video preview**. Preparation
finishes before playback begins. **Cancel preparation** requests cancellation;
the UI shows **Stopping preparation** until outstanding work actually ends, then
reports **Preparation cancelled**. Closing the viewer stops polling but does not
cancel a job.
The original-file action remains available whenever access is granted.

A lost status response can be checked again with a read-only GET. The client never
retries POST automatically. A lost initial POST response can leave an accepted job
whose ID was not received; there is no durable receipt/recovery API. Wait before
explicitly trying again. Global admission and scratch limits still apply.

## Authenticated API

All routes use existing live Auth validation and the exact `services.drive:true`
grant. Mutations require an explicit bearer token; existing cookie authentication
is permitted only for GET/HEAD. IDs are not public capabilities and contain no JWT.
Responses are `Cache-Control: no-store`.

- `POST /api/video-previews`: strict JSON `{ "path": "movie.mkv", "space": "personal" }`;
  202 returns `id`, `status`, `createdAt`, `expiresAt`.
- `GET /api/video-previews/{id}`: status `preparing`, `cancelling`, `ready`, `failed` or `cancelled`;
  ready additionally reports `mode: remux|transcode`.
- `GET|HEAD /api/video-previews/{id}/content`: private MP4; byte Range/206 and 416
  use the standard Go content-serving implementation. Only ready jobs are served.
- `DELETE /api/video-previews/{id}`: cancel preparation or discard a ready preview.

Every status/content/control access checks the authenticated numeric owner and
re-resolves current personal/shared access. Read-only incoming shares can be
previewed; revoked shares cannot. Changed size/modification time or missing source
prevents serving cached content. This metadata check is **not** a proof against
same-size/same-time external NAS mutation. No cross-job cache reuse is performed.
The exact staged snapshot is hashed, without claiming to verify or repair NAS
headers or historical payload-integrity incidents.

Unknown/cross-owner IDs return 404. Disabled feature/worker failures fail closed;
ordinary Drive routes are unaffected when the feature is not configured. Busy
admission returns 429, insufficient/unmanaged scratch returns 507. There is one
conversion globally per API/worker process, no internal waiting queue.

## Explicit API configuration

Linux only. An unset/empty `VIDEO_PREVIEW_ROOT` leaves the feature off. A configured
root must be an absolute, nonsymlink, UID-owned private directory, independently
initialized with the worker's `--init-root`. It is **not** an existing Drive storage,
Office, uploads, imports or torrent directory. No automatic creation/adoption,
ownership repair or migration is performed. Keep the Unix socket pathname below
Linux's 108-byte limit.

| Environment variable               | Default              |
| ---------------------------------- | -------------------- |
| `VIDEO_PREVIEW_ROOT`               | unset / disabled     |
| `VIDEO_PREVIEW_MAX_INPUT_BYTES`    | 10737418240 (10 GiB) |
| `VIDEO_PREVIEW_MAX_OUTPUT_BYTES`   | 10737418240 (10 GiB) |
| `VIDEO_PREVIEW_BUDGET_BYTES`       | 32212254720 (30 GiB) |
| `VIDEO_PREVIEW_FREE_RESERVE_BYTES` | 2147483648 (2 GiB)   |

TTL is two hours from admission; preparation timeout is one hour. Configuration
requires budget ≥ max input + **twice** max output: publication copies decoder
output into a new API-private inode, so source, worker output and private copy can
coexist. Admission inventories actual private cache bytes and checks filesystem
free space/reserve. A held lock prevents a second API manager using the same root.
External hard storage quotas remain mandatory; process-held unlinked files,
FFmpeg trailer/faststart overhead and other filesystem writers cannot be bounded
by application accounting alone.

Source/output copying checks context before and after each bounded 32 KiB
read/write operation and before/after output sync/publication. This stops further
copying after cancellation without draining the entire source. It does **not**
interrupt an already kernel-blocked filesystem/NAS read, write or fsync. Admission
stays fenced until that I/O returns; no copying goroutine is abandoned and no
terminal cancellation receipt is issued while it is still active.

The development worker mounts only `incoming/`; API also accesses sibling `ready/`.
This is not production per-job containment: activation requires a constrained
controller with fresh disposable, job-only decoder sandboxes and confirmed
teardown, not this persistent tenant-wide decoder mount. After success,
the API verifies output size, makes/syncs its own private copy, then removes exact
known incoming files. Renaming/hardlinking worker output is deliberately avoided:
retained decoder FDs must not mutate completed cache. No Store writes occur.

Known completed/failed/cancelled jobs expire on requests and a one-minute sweeper.
Cleanup removes only exact known preview files, never recursively deletes unknown
data. Metadata is in memory: restarts do not adopt old jobs or serve orphan cache.
Orphan incoming directories block new admission; unknown files and stale sockets
are preserved for investigation. Operators may reset only an independently owned,
quiescent preview scratch volume under separate approval, never Drive/NAS data.

## Worker, tests and activation gates

See [worker installation, flags, local/Docker examples and restrictions](../video-worker/README.md).
The API image and its existing ARM64 runtime pin are unchanged; FFmpeg lives in a
separate new worker image. Native ARM64 runtime/codec/resource testing remains a
release gate, not something an amd64 test or cross-build proves.

Local synthetic checks from the repository root (Go 1.27.3):

```sh
VIDEO_WORKER_INTEGRATION=1 go test -race -count=1 ./api/...
go vet ./api/...
cd frontend
npm ci
npm run build
CHROMIUM_PATH=/usr/bin/google-chrome-stable npm run test:theme
```

Browser tests mock API/auth/storage; the video suite generates and actually plays
a tiny local synthetic MP4. Backend integration separately exercises real FFmpeg
through the Unix socket and authenticated API/Range boundary. Neither validates
production mounts, NAS integrity, all codecs or real ARM hardware.

**Deployment remains gated** on independent security review, scratch capacity and
recovery approval, pinned native worker images, live Unix UID/mount permissions,
resource/no-network/read-only/capability confinement, and scoped rollout approval.
A same-Pod sidecar does not by itself disable networking. A long-lived decoder
container is not per-job process isolation: production must additionally review
cross-job process-lifetime/descendant containment rather than assuming Go context
cancellation alone contains a compromised decoder. Do not activate this feature
with access to NAS, API secrets, issuer keys, host paths or provider credentials.
