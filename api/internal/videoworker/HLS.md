# HLS source implementation contract

This is a **disabled source increment**, not a production controller. Existing
runtime/HTTPS/quota/sandbox gates remain. The first increment stages the complete
seekable source before conversion; it removes the full-conversion wait, not the
full NAS-read wait. No local-download fallback is part of the video UI.

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
routes. No JWT, credentials, source URL or NAS path crosses this port.

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
