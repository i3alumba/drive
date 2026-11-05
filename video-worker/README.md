# Video worker and progressive HLS prototype (source only)

This Linux Unix-socket worker processes private synthetic MKV/AVI scratch inputs.
It has no Store/NAS code, credentials, JWTs, source URL, public listener or queue.
The current Drive player uses progressively published HLS/fMP4; see
[VIDEO-PREVIEWS.md](../docs/VIDEO-PREVIEWS.md) for API/UI semantics and gates.
The entire seekable input is staged before conversion. No image, controller,
quota, deployment, TLS cutover or production activation is supplied here.

**Production NO-GO:** the development handler is permanently unqualified. A
persistent decoder with the incoming parent mounted can observe later jobs after
compromise. Production needs a constrained trusted controller, fresh disposable
job-only sandboxes, real hard quotas and confirmed whole-sandbox teardown.
`exec` working directories, process exit or context cancellation are not sandboxes.

## Go and private wire contracts

The shared versioned contract and bounds are in
[`api/internal/videoworker/HLS.md`](../api/internal/videoworker/HLS.md).

- `Initialize(root)` initializes an existing empty private base.
- `NewClient(socket)` exposes `HLSReady`, `StreamHLS`, `LookupHLS`, `CancelHLS`.
- `HLSIdentity` binds job ID, controller epoch and execution ID, each 32 lowercase
  hex characters. `HLSStatus.ConfirmedFor(expected)` checks exact retained proof.
- `RunDevelopmentHLS(ctx, incoming, id, format, options, maxOutputBytes, emit)`
  produces artifact events and `conversion_end`, **never terminal/quiescence**.
- `NewDevelopmentHLSHandler(incoming, options)` exposes an explicitly development
  capability role and one admitted prototype execution; no second job is admitted
  without a proof mechanism that this handler deliberately does not provide.

The API client accepts only the separately implemented qualified controller role
at `/v1/capabilities`, checks epoch before POST, and does not retry mutations.
`POST /v1/jobs/{id}/events` has correlation headers and strict bounded NDJSON;
GET/DELETE `/v1/jobs/{id}` reconcile only the exact epoch/execution identity.
EOF, restarted epochs, 404, malformed bodies and missing receipts remain unknown.
The role string alone is not proof of production isolation.

The development HTTP handler cannot be used as the current API's controller:
`HLSReady` rejects it **before copying NAS input**. There is no enable flag to
bypass this check, no fabricated teardown receipt and no automatic production
fallback to the legacy whole-MP4 protocol.

For compatibility, the older `NewHandler` and `Client.Process` implement private
`POST /jobs/{id}` with bounded `{format,max_output_bytes}` and a complete
`Result{mode,size,duration}`. Without `--development-hls`, the CLI serves this
legacy handler. It is not selected by the new Drive video API. Its process-level
cancellation also proves no descendant containment.

## Artifact production and validation boundary

The API owns `incoming/{id}/input`. The HLS engine creates exactly one private
`hls/` child, fixed `init.mp4`, `segment-NNNNNN.m4s` and an internal `stream.m3u8`.
Finalized artifacts are recognized from a bounded playlist; temporary files are
not advertised. The internal worker playlist is never served to browsers. The
API generates its own manifest only from independently accepted private copies.

Input must be nonempty, regular, single-link, nonsymlink and size-bounded. Linux
openat2/openat, no-follow checks and inherited input/output-directory FDs pin file
resolution. Unsupported kernel/filesystem behavior fails closed. New output uses
private permissions and no-overwrite rules; unknown/preexisting output is not
adopted or deleted. The engine checks bounded directory inventory, file identity,
size and hashes before emitting events. This is defense in depth, not immutable
ownership against a compromised same-UID decoder.

Publication therefore copies into **new API-private inodes**, verifies hashes
and the strict fMP4 subset, syncs, then publishes. Worker rename/close, hardlinking,
or a retained decoder FD cannot be treated as trusted private publication. Never
hardlink an original. No code writes to the Drive Store.

Only first video and optional first audio are selected. Finite positive duration
is at most six hours. Supported compatible H.264 yuv420p at most 1920×1080 plus
AAC/no audio can remux; other supported input is converted to H.264/AAC, one
encoder/decoder/filter thread, no upscale, bounded dimensions and stripped
metadata/chapters/subtitles/data. Transcoding uses four-second keyframe targets;
each accepted fragment must fit the eight-second limit. There is one rendition,
not adaptive bitrate. Probe/diagnostic output and events are bounded; diagnostics
never cross IPC or logs. Hostile or unsupported media may fail closed after a
prefix; the API then stops advertising it, without manufacturing `ENDLIST`.

Limits: init 1 MiB, fragment 64 MiB, 8192 fragments, six hours, 2 KiB/event,
16384 events, plus aggregate configured bytes. Runtime hard storage quotas remain
mandatory: polling/application accounting cannot bound malicious writers or
unlinked held files. API-private parsing uses pinned mp4ff only after bounded
recursive grammar preflight; this is not full decoded-frame integrity.

## CLI and initialization

Go minimum is 1.24; builders remain pinned to 1.27.3. FFmpeg/FFprobe are separate
worker runtime tools, never added to the API runtime. CLI flags:

- `--root`: required incoming directory, or empty scratch base for initialization
- `--socket`: defaults to `<root>/worker.sock`
- `--ffmpeg`, `--ffprobe`: executable names on PATH by default
- `--timeout`: `1h`
- `--max-input-bytes`, `--max-output-bytes`: each `10737418240` (10 GiB)
- `--init-root`: initialize and exit, no listener/probe/conversion
- `--development-hls`: serve the unqualified one-job HLS prototype; incompatible
  with `--init-root`; **not a production controller or admission bypass**

CLI requires UID 10001 and sets umask 077. Library code changes neither process
umask nor global rlimits. Initialization requires an existing empty UID-owned
0700 nonsymlink base, creates `.video-worker-v1` mode 0600 and `incoming/`, `ready/`
mode 0700, and never repairs/adopts storage. Failed partial initialization is not
speculatively cleaned. Existing sockets are refused rather than unlinked.
Keep socket paths below Linux's 108-byte limit. Control socket mode is 0600.

Synthetic development only, Linux with the pinned Go toolchain and FFmpeg tools:

```sh
go build -o /tmp/video-worker ./api/cmd/video-worker
# An operator separately creates an empty 0700 UID-10001-owned scratch base.
# Both CLI commands run as UID 10001, never against NAS/Drive storage.
/tmp/video-worker --root /tmp/video-scratch --init-root
/tmp/video-worker --root /tmp/video-scratch/incoming --development-hls
# The API deliberately rejects this unqualified development handler.
VIDEO_WORKER_INTEGRATION=1 go test -race -count=1 ./api/...
go vet ./api/...
```

## Docker example: not production isolation

`video-worker/Dockerfile` uses the existing native Go 1.27.3 builder and Debian
bookworm FFmpeg runtime. Build context is repository root. Illustrative commands,
not an instruction to build/publish/run or qualify a production image:

```sh
docker build -f video-worker/Dockerfile -t drive-video-worker:local .
# Requires a separately initialized UID-10001-owned, hard-quota-limited scratch.
docker run --rm --network none --user 10001:10001 --read-only \
  --cap-drop ALL --security-opt no-new-privileges:true \
  --pids-limit 64 --cpus 1 --memory 512m --memory-swap 512m \
  --tmpfs /tmp:rw,noexec,nosuid,nodev,size=16m \
  --mount type=bind,src=/tmp/video-scratch/incoming,dst=/scratch/incoming \
  drive-video-worker:local --root /scratch/incoming --development-hls
```

This persistent development mount is **not safe for tenant media**: it exposes
future incoming jobs and does not prove whole-sandbox teardown. Production must
expose only one job to each disposable decoder, never the incoming parent,
`ready/`, API/controller socket, NAS/Store, secrets, issuer keys, provider
credentials, Docker/root-equivalent sockets or unrelated host paths. The trusted
controller is a separate constrained boundary. Same-Pod placement does not disable
networking. Enforced no-network, CPU/memory/PID/storage controls and native ARM64
runtime/codec/UID/mount qualification remain separate release gates.

Tests generate only tiny local synthetic media and cover private IPC, strict
protocol/identity/limits, artifact lifecycle, parsing, no-overwrite, cancellation,
unknown teardown and browser MSE prefix playback. They neither test compromised
production decoder containment nor authorize runtime, image import/export,
resource changes, data cleanup or deployment.
