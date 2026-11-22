# HLS source implementation contract

**Execution mode update:** explicitly selected local jobs now run in the main API
container under the API UID/mount permissions, without a sandbox. See
[local configuration, initialization and accepted risks](../../../docs/LOCAL-VIDEO.md).
Default execution is disabled. Sandbox qualification/NO-GO statements below remain
requirements for remote mode, not for the deliberately selected local alternative.

This is a **disabled source increment**, not a production controller. Existing
runtime/HTTPS/quota/sandbox gates remain. The default v1 lane stages the complete
seekable source before conversion. An explicitly opted-in v3 lane uses bounded
remote input without staging; no qualified production controller is supplied. No local-download fallback is part of the video UI.

## Fixed media subset

Use one fMP4 event rendition, nominal four-second fragments, fixed public target
duration eight seconds. Bounds: initialization 1 MiB, media fragment 64 MiB,
8,192 fragments, six hours media, configured aggregate output ceiling, 2 KiB per
NDJSON record and 16,384 records. Reject long fragments rather than changing the
target duration or silently replacing an already published timeline. Remux is
permitted only when private initialization/fragments pass the same narrow codec,
timing and independent-IDR checks as transcoding. Transcoding is capped at
1080p/no upscale, 30 fps, H.264/yuv420p, optional AAC-LC stereo, forced closed GOPs
around four seconds. No quality selector or arbitrary-future seeking is promised.

Fixed artifact basenames are `init.mp4` and `segment-%06d.m4s`. Filenames are
computed from validated indexes, never received from an event or copied from a
worker playlist. The API generates its own playlist using only its immutable,
validated published-prefix ledger. `ENDLIST` requires confirmed final success.

The validator is pinned mp4ff behind independent complete grammar/count preflight
on exact **API-private copied bytes**, not the mutable worker path. The detailed
bounded subset is retained in the independent Astra design review. No FFmpeg is
run in the API process.

## Client and controller port

The private HTTP transport remains Unix-only, with no proxy, redirect or TCP
fallback. Public browser requests are separate, authenticated owner-scoped API
routes. The v1 port carries no source URL. The v3 source descriptor carries a private
execution-secret broker URL; no JWT, NAS key/path or storage credentials cross
the port. Do not log descriptors or URLs.

Production client methods:

- `HLSReady(ctx) (controllerEpoch string, error)` — GET `/v1/capabilities`;
  requires `{v:1,isolation:"isolated-job-controller-v1",controller_epoch:hex32}`.
- `StreamHLS(ctx, identity HLSIdentity, format string, maxOutputBytes int64,
accept func(HLSEvent) error) (HLSSequence, error)` — one POST
  `/v1/jobs/{jobID}/events`, strict JSON `format`, `max_output_bytes`,
  `controller_epoch`, `execution_id`. The API creates the execution ID before
  sending and persists its incoming fence. No POST retries.
- `LookupHLS(ctx, identity) (HLSStatus, error)` — GET
  `/v1/jobs/{jobID}?controller_epoch=...&execution_id=...`.
- `CancelHLS(ctx, identity) (HLSStatus, error)` — DELETE the same fixed route and
  identity. Idempotent cancellation intent, never a termination receipt itself.

`HLSStatus` has `Identity HLSIdentity`, `State` (`running`, `stopping`, `quiescent`,
`unknown`) and optional `Terminal *HLSEvent`. Its `ConfirmedFor(expected)` accepts
only a matching retained controller terminal receipt. Private JSON status uses
`v`, flat identity fields, `state` and optional `terminal`; reject unknown keys,
malformed nested terminal records and mismatches. Bound every control response.

Response headers `X-Video-Controller-Epoch` and `X-Video-Execution-ID` must match
before accepting any stream frame. These are correlation values, not secrets or
capabilities. A restart, identity mismatch, 404, failed request or EOF without a
matching terminal receipt means **unknown**, never absence of execution.

Frames: heartbeat, started, init, segment, conversion_end, terminal, as implemented
by `hls_protocol.go`. `conversion_end` reports conversion outcome/totals only.
`terminal` additionally binds job/epoch/execution and quiescence="confirmed".
Only the trusted lifecycle controller can supply that receipt. It is not created
from FFmpeg exit, a development-runner assertion or a boolean configuration flag.

On failure/cancel, stop publication; reconcile GET/DELETE through a separate
bounded control context. Do not release admission, clean active incoming files,
complete the playlist or remove the durable fence until both local I/O and
confirmed execution teardown have finished. Unknown execution remains fenced.

## Development artifact runner

`RunDevelopmentHLS(ctx, root, id, format, options, maxOutputBytes, emit)` produces
artifacts and conversion events only, returning `HLSConversion` with outcome,
safe code, segment count and duration. It can never construct a terminal teardown
receipt. All subprocess/watcher/heartbeat work is joined before returning; this
is still not descendant/sandbox containment.

An explicitly named `--development-hls` CLI mode may serve
`NewDevelopmentHLSHandler(root, options)`. Capabilities permanently report a
**development** role, rejected by production `Client.HLSReady`. It may demonstrate
one progressive job, but does not acknowledge quiescence or admit a second job
in the same handler. GET/DELETE remain unqualified/unknown. No API option accepts
that development role. Tests may use owned synthetic controller fixtures; those
are not selectable by production configuration.

Real FFmpeg synthetic fixtures, context cancellation, aggregate/partial-file
bounds, strict events, unsafe paths, retained-FD mutation, controller identity
mismatch/unknown teardown, and actual progressive MSE playback must be tested.
Production controller/admission, real hard quota, native decoder ARM64 and
network/sandbox teardown remain **NO-GO for processing activation** until qualified.
A separately approved compatible legacy-HTTP application release can deliver
sorting/UI with processing OFF; HLS does not intrinsically require a TLS cutover.

## Opt-in bounded remote input contract (v3, activation NO-GO)

`VideoPreviewOptions.RemoteInput` defaults to false. This lane is explicitly
configured in code only; no runtime manifests or production controller are
provided. It accepts MKV/AVI/MP4/MOV. The default staged lane remains MKV/AVI.
A missing/legacy/unqualified v3 capability is rejected before any source read;
there is no fallback to staging or to a development decoder.

GET `/v3/capabilities` must return exactly:

```json
{
  "v": 3,
  "isolation": "isolated-range-source-egress-controller-v3",
  "controller_epoch": "11111111111111111111111111111111",
  "source": "bounded-http-range-timeline-v3",
  "egress": "selected-broker-only-no-redirect-no-proxy"
}
```

These strings are **trusted controller assertions**, not automated qualification
proof. The permanently unqualified development handler never advertises them.
Operators must qualify fresh disposable execution, no incoming-parent access,
hard scratch quota, and egress to only the selected broker, with no proxies or
redirect following. HTTP protocol whitelists do not constrain destinations.

POST `/v3/jobs/{jobID}/events` carries `RemoteHLSRequest`:

- `v`: 3;
- `identity`: exact `job_id`, `controller_epoch`, `execution_id`;
- `format`: `mkv`, `avi`, `mp4` or `mov`;
- `max_output_bytes`: the existing output bound;
- `source`: `v` (3), identical `identity`, `endpoint` (private HTTP capability),
  `size`, `deadline` (RFC3339 timestamp), `max_read_bytes`, `chunk_bytes`,
  `concurrency`, `start_time` (finite seconds, 0 <= start < 21600), and
  `force_transcode` (explicit bool).

The controller must bind URL, limits and source identity to exactly this
execution and reject mismatches before starting. No arbitrary caller URL is
accepted by the API. Currently the API binds a fresh loopback IPv4 listener;
only literal loopback descriptor endpoints are supported. This is not network
namespace isolation and may be unreachable from a real isolated sandbox. Do not
activate it by relaxing egress or adding public routes.

The source capability selects only the API-authorized key. Source HTTP requests
and backend chunks recheck live owner Drive permission, share/path resolution,
size and modification time. HEAD performs these checks without reading source
bytes. GET supports single ranges with bounded backend chunks; initial absent
or open-ended ranges do not reserve the full remainder. Cumulative reservations
include failed reads (eight times source size), chunks are 64 KiB, concurrency
is two. No full source hash/staged input is created. Existing metadata identity
limitations remain; same-size mutations with unchanged modification time are
not newly solved here.

Response write deadlines equal the job deadline and expire immediately on job
or request cancellation. Healthy responses may progress for more than five
seconds. Remote FFprobe shares the explicit job/source deadline, rather than
the legacy local-file 15-second probe cap. The decoder must honor that deadline;
probe and conversion together remain bounded by the execution timeout.

The output NDJSON, identity headers, strict private-byte validation and
immutable asset publication remain v1. Modes now include `copy_video` (compatible
H264 copied; incompatible audio converted to AAC-LC stereo), in addition to
`remux` and `transcode`. Validation/security errors never trigger conversion
fallback. Existing v1 GET/DELETE lifecycle routes retain the same identity and
confirmed-quiescence requirements. EOF or source cancellation is not a teardown
receipt. Source handlers are revoked and joined before completion can release
admission; unknown teardown retains the existing execution fence.

Tests use an explicitly trusted fake Unix controller and real synthetic FFmpeg;
its advertised capabilities do not establish actual sandbox qualification.
Generation replacement and whole-film seek UI are implemented as described below.
Production activation remains **NO-GO**.

## Seek generations and source clock

Public POST `/api/video-previews` additionally accepts `startTime` (finite seconds,
0 inclusive to 21600 exclusive), `forceTranscode`, and `replaceID`. Missing start
means zero. These fields require the opt-in v3 lane. The source probe still reads
the original full source, never a pretrimmed staged file. `started` reports:

- `source_duration_us`: full normalized movie duration from the source probe;
- `origin_us`: requested origin in the source's zero-based movie clock;
- `expected_duration_us`: source duration minus origin, used for output validation.

The source's nonzero container timestamp offset is handled by FFmpeg's relative
input seeking; it is not added to the movie clock. For nonzero origins, accurate
input seeking decodes/discards preceding source frames and **always transcodes**
video, using a closed-GOP output with no B-frame reordering. Source B-frames are
also preflighted to the video-transcode path even at zero origin. This avoids
promising keyframe-copy precision or introducing a mux reorder offset relative
to AAC priming. Compatible zero-origin/no-B-frame H264 may still be copied,
independently from audio conversion. Track timestamps are not independently
zeroed: their relative timing is preserved. Priming/frame quantization can leave
small track offsets; synthetic tests bound AV start/end skew and compare the
first decoded sought frame to the same source-clock frame. This is not a claim
of sample-exact arbitrary-VFR editing or arbitrary codec support.

API status `duration` is the **full source duration**, `origin` is this generation's
source-clock origin, `preparedDuration` is its locally prepared extent, and
`seekable` advertises v3 generation support. HLS assets use a normalized local
clock. A seek within the current prepared extent only updates the player clock.
Outside that extent, the UI debounces intent, stops old playback, DELETEs active
preparation, and GET-polls until a terminal status before issuing one replacement
POST. An unconfirmed teardown, network ambiguity or authorization denial never
causes a replacement POST. Unknown accepted POSTs are never repeated.

A replacement needs the same owner, selected source and space, plus an existing
quiescent predecessor. The predecessor must have no accepted replacement. The
API still checks its global one-job admission and execution fence. A replacement
gets a new job ID, execution capability and immutable assets; old URLs are never
rebound to the new generation. Duplicate predecessor replacement is rejected.
Old player/status callbacks are isolated by job identity. Source budget and
cancellation remain per-generation, with no cross-job cache.

There is **no automatic unsuitable-GOP fallback**: validation/hash/identity/auth/
sandbox errors are never classified as benign compatibility failures. An explicit
“Prepare with video transcoding” action requests one new force-transcode
generation after old teardown. The user can inspect a failed attempt before
choosing that action; no hidden retry loop exists.

MP4/MOV retain direct browser playback. Only a browser unsupported-source verdict
plus a successful authenticated source HEAD offers the explicit preparation
button. Decode errors, auth/network failure and autoplay rejection do not
activate this fallback. Browser tests exercise this distinction and generation
lifecycle with mocked API status plus real synthetic HLS playback; they are not
a production sandbox end-to-end qualification.

Acquired source metadata is checked before any source body reaches the decoder,
not just by a prior Stat. Filesystem readers return their bound envelope mtime.
MinIO ranged GETs are version/ETag-bound, acquired with a one-byte prefix, and
checked against pinned response metadata; the prefix is returned only after
those checks. No whole-file hashing/staging is introduced. Same-size changes
that preserve both identity metadata and checksum/version remain outside this
metadata-bound contract.

### Bounded SDR color metadata

Strict init validation permits one optional `colr` leaf under `avc1` only:
canonical `nclx` (19 bytes total) or `nclc` (18 bytes total), with each color enum
explicitly limited to BT709 (1) or unspecified (2). Reserved `nclx` bits, ICC
profiles, other enums, duplicate/trailing/extended/zero-size forms are rejected.
When present, bounded full SPS VUI parsing checks specified color/range signalling
for contradictions. `nclc` does not assert range; missing VUI and unspecified
values are not treated as BT709 assertions. Existing no-colr behavior is unchanged.
This accepts metadata, not HDR, tone mapping, or pixel-integrity claims.

Synthetic source-backed local API tests cover BT709 H264 plus AC3 stereo and 5.1:
no-B-frame input uses video copy with AAC stereo, while B-frame input uses the
actual source-backed full-transcode decision. Staged copy helpers are not proof
of source-backed API behavior.
