# Progressive MKV/AVI playback (opt-in source feature)

Drive prepares a single H.264/AAC HLS stream with fragmented MP4 segments for
in-site MKV/AVI playback. Each finalized segment is copied into new API-private
storage, hashed and structurally validated before publication. Playback can begin
from that prefix **before conversion of the whole movie finishes**.

**The entire input is still copied from storage first.** This first increment
needs a seekable input: “Reading video from storage…” can take significant time
for large NAS objects. It does not promise playback within seconds or direct
on-demand NAS byte streaming. Seeking is limited to the prepared prefix.
There are no adaptive-quality, subtitle or audio-track selectors. Only the first
video and optional first audio track are selected; codec/browser support is
bounded, not universal. There is no original-file/download fallback in this
player. Existing independent file-management download routes are unchanged.

**HLS processing disabled by default; activation NO-GO until its runtime is ready.** This source does not provision or
qualify a production sandbox controller, enforce hard quotas, build/publish an
image, change a parent gitlink, alter Compose/Kubernetes, or activate anything.
It changes no NAS data, imports, accounts, credentials or TLS configuration.
The API defaults to HTTPS, but an explicitly selected `--legacy-http` profile
preserves the current HTTP8080/Auth8000 production baseline. It is not fallback
or a TLS migration. A compatible partial release may deploy sorting/UI and this
dormant code while HLS preparation remains unavailable; no decoder is launched.
The operator deferred sandbox/quota/native-decoder qualification and any TLS
migration for that partial release, not the fail-closed activation guards.

## User flow

MKV/AVI cards use icons without fetching original media. Opening the viewer does
not start a conversion. **Prepare video stream** explicitly submits one POST.
States distinguish reading/staging, first-segment preparation, ongoing playback,
and completion. An EVENT playlist grows as validated fragments become available;
`ENDLIST` is withheld until conversion and trusted controller teardown complete.
A failed prefix is no longer served as a playable movie.

**Cancel preparation** stops the player immediately and requests cancellation;
“Stopping preparation; waiting for confirmed teardown…” is not a terminal
acknowledgement. Closing the viewer destroys the media loader and aborts local
requests/polling, but does not automatically cancel an accepted preparation.
Autoplay denial offers **Play**, never another conversion. Fatal playback can be
retried explicitly without POST; access/source/expiry denial destroys media and
requires reopening/revalidation.

Known job status can be reconciled using read-only GET. No mutation is retried
automatically. If the initial POST result is lost, the client may not know its
job ID: the current viewer blocks another preparation and directs the user to
operator reconciliation. There is no durable public recovery/list API; reopening
a viewer is not proof that an earlier operation did not execute.

## Authenticated API

Every route performs existing live Auth validation and requires the exact
`services.drive:true` grant. Mutations require bearer authentication; GET/HEAD
may use the existing read-only cookie. Job IDs are not public capabilities.
JWTs never appear in media URLs. All responses use `Cache-Control: no-store`.

- `POST /api/video-previews`: `{ "path": "movie.mkv", "space": "personal" }`;
  202 returns a 32-lowercase-hex ID and status.
- `GET /api/video-previews/{id}`: `staging`, `preparing`, `streaming`, `complete`,
  `cancelling`, `stopping`, `failed` or `cancelled`; includes bounded duration,
  prepared duration, segment count and, once known, `mode: remux|transcode`.
- `GET|HEAD /api/video-previews/{id}/stream.m3u8`: API-generated EVENT playlist
  containing only accepted private assets and actual validated durations.
- `GET|HEAD /api/video-previews/{id}/init.mp4` and
  `/segment-NNNNNN.m4s`: private snapshots, with normal byte Range/206/416 support.
- `DELETE /api/video-previews/{id}`: cancellation or completed-cache disposal;
  an uncertain stopped job can request bounded controller reconciliation.

The legacy `/content` endpoint does not advertise an MP4 for new HLS jobs.
Every status, asset and control access checks the numeric job owner, re-resolves
current personal/shared permissions, and compares source size/modification time.
Read-only incoming shares are supported; revoked shares, changed/missing source
and foreign/expired IDs cannot obtain media. These metadata checks do **not**
prove integrity against same-size/same-time external mutation. The exact staged
input is hashed, without claiming NAS repair or historical integrity acceptance.
There are no Store writes and no cross-job cache adoption/reuse.

Busy admission is 429; unmanaged/insufficient scratch is 507; an unknown execution
or unqualified/unavailable controller is 503. Admission is one job globally per
API manager, without a queue. Ordinary Drive routes are unaffected when disabled.

## Private protocol and media boundary

The exact versioned controller contract is
[`api/internal/videoworker/HLS.md`](../api/internal/videoworker/HLS.md).
It uses HTTP over a private Unix socket, no proxy, redirects or TCP fallback.
Capability checks precede NAS staging. The capability role string is a protocol
check, **not independent evidence of runtime containment**.

Events have fixed names, strict shapes, size/count bounds, contiguous indices,
checksums and timing. Decoder paths/playlists are never published or fetched as
URLs. The API copies each bounded artifact into a fresh inode, checks its exact
hash, validates those private bytes, fsyncs, then publishes without replacement.
Worker rename, close or a checksum alone does not establish immutable ownership:
retained decoder FDs must not mutate the API-private cache.

The pinned `mp4ff v0.62.0` parser is reached only after a complete bounded recursive
box/shape/count preflight. The accepted subset is H.264 AVC with optional mono/stereo AAC-LC,
fixed track identities, independent IDR fragments, checked sample ranges and
continuous decode/presentation timing. Both uncropped coded macroblocks and
cropped display dimensions are bounded; ordinary 1920×1088-to-1080 padding is
allowed, not an oversized coded picture hidden by cropping. AAC remux requires
known LC profile, mono/stereo channels and supported sample rate; surround or
unknown audio properties use stereo conversion instead. An optional direct `moov/udta` accepts only
the documented exact 61-byte empty FFmpeg metadata structure; other metadata is
rejected. This is not general MP4 support or decoded-frame/payload equality.
The API never runs FFmpeg/FFprobe.

Bounds: init 1 MiB, each fragment 64 MiB, at most 8192 fragments, each at most
8 seconds, total at most six hours; event lines 2 KiB and at most 16384 events.
Configured aggregate output limits also apply. `hls.js 1.7.4` is self-hosted in the
frontend, with native HLS where available, bounded buffers/retries and same-origin
fixed-asset URL checks. Chromium MSE tests do not qualify Safari native behavior.

## Completion, cancellation and restart

`conversion_end`, FFmpeg exit, socket EOF and HTTP 404 are **not teardown proof**.
Only matching retained controller-epoch/job/execution terminal evidence can
release an accepted execution. Conversion completion and lifecycle quiescence are
separate facts. Success is decided under the manager lock only after private
artifacts are validated/fsynced, matching terminal proof is known and the
execution context is still live. Subsequent known-scratch cleanup and fence
syncing are housekeeping: crossing the deadline there does not invalidate that
success, but exposure/admission release still wait for successful housekeeping.
No hard wall-clock bound or interruptible filesystem cleanup/fsync is promised.
Unknown execution keeps capacity busy and the durable
`.active-preview` fence; restart preserves this fence and does not adopt old jobs.
A wrong controller epoch or execution receipt cannot release it.

Source/private-output copying checks context around bounded 32 KiB reads/writes,
sync and publication. It does not interrupt an already kernel-blocked NAS read,
write or fsync. No copy goroutine is abandoned, and admission stays held until
that I/O actually returns. Active-job status and media serving independently
check the execution context: a deadline denies an earlier prefix even while a
later publication remains blocked, without releasing capacity or deleting files.
Successfully completed caches are unaffected by their finished execution context.
Errors stop further publication; already delivered browser bytes cannot be recalled. Cleanup follows confirmed quiescence and
removes only exact known files plus one fixed HLS output child, never arbitrary
recursive data. Unknown files, stale sockets and orphan incoming jobs are
preserved and fail closed.

Metadata is memory-only. Known completed/failed/cancelled caches expire through
requests and a minute sweeper, with a two-hour TTL from admission. Unknown jobs
are not speculative cleanup targets. Resetting an independently owned,
quiescent scratch volume requires separate operator approval, never NAS cleanup.

## Configuration and resources

Linux only. Empty `VIDEO_PREVIEW_ROOT` keeps the feature off. The root must be an
absolute, nonsymlink, UID-owned 0700 directory, independently initialized with
`--init-root`; it is not Drive/Office/uploads/imports/torrent storage. Initialization
never adopts nonempty storage or repairs ownership. Unix socket paths must fit
Linux's 108-byte limit.

| Environment variable               | Default              |
| ---------------------------------- | -------------------- |
| `VIDEO_PREVIEW_ROOT`               | unset / disabled     |
| `VIDEO_PREVIEW_MAX_INPUT_BYTES`    | 10737418240 (10 GiB) |
| `VIDEO_PREVIEW_MAX_OUTPUT_BYTES`   | 10737418240 (10 GiB) |
| `VIDEO_PREVIEW_BUDGET_BYTES`       | 32212254720 (30 GiB) |
| `VIDEO_PREVIEW_FREE_RESERVE_BYTES` | 2147483648 (2 GiB)   |

Timeout is one hour. Budget must cover max input plus **twice** max output:
source, decoder output and private copies can coexist. Admission inventories
scratch and checks free space/reserve; an exclusive manager lock prevents shared
root ownership. Application accounting and PVC declared capacity are not hard
quotas: unlinked open files, other writers and decoder/crash overhead require
external filesystem/runtime enforcement.

## Local checks and unresolved activation gates

See [worker flags, development installation and Docker examples](../video-worker/README.md).
Go minimum is 1.24; existing builders remain pinned to 1.27.3. For local synthetic
checks, install FFmpeg/FFprobe, frontend dependencies and a compatible Chromium:

```sh
VIDEO_WORKER_INTEGRATION=1 go test -race -count=1 ./api/...
go vet ./api/...
cd frontend
npm ci
npm run build
CHROMIUM_PATH=/usr/bin/google-chrome-stable npm run test:theme
```

Backend tests separately exercise real synthetic FFmpeg through Unix IPC,
private artifact validation and authenticated Range serving. Browser API/auth
are mocked; real tiny fMP4 fragments play through hls.js/MSE while the producer
is marked incomplete, then the manifest grows. These are not one live production
end-to-end acceptance, ARM64 runtime qualification or real NAS integrity proof.

**HLS processing activation remains NO-GO** until independent review and separately approved
qualification establish: a constrained trusted controller; fresh disposable
job-only sandboxes and whole-sandbox teardown evidence; no decoder control/API
socket or tenant-wide input mount; enforced network/resource/real storage quotas;
UID/mount permissions; pinned native ARM64 images/codec runtime; recovery and
capacity approval. Any future HTTPS transport cutover is separate; HTTPS is not
intrinsically required by HLS or sorting and is not part of the compatible
partial release. No Docker/root-
equivalent socket belongs in the API or decoder. Same-Pod placement does not
establish network isolation. The included development handler is permanently
unqualified and cannot manufacture terminal receipts.
