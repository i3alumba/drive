# Isolated video preview worker (source only)

This Linux sidecar converts operator-approved MKV/AVI preview scratch copies to
one MP4. It is **not enabled by this source change**. It has no Store/NAS code,
credentials, JWT, source URL, public listener or internal queue. The opt-in API/UI
integration is described in [VIDEO-PREVIEWS.md](../docs/VIDEO-PREVIEWS.md).
Existing TLS and durable queue behavior are untouched.

## Immutable Go / wire contract

`remote-drive/api/internal/videoworker` exports `InputName="input"`,
`OutputName="output.mp4"`, `SocketName="worker.sock"`, `Options`,
`NewHandler(root, Options) (http.Handler, error)`, `Initialize(root) error`,
`NewClient(socket) *Client`, and
`(*Client).Process(ctx, id, format, maxOutputBytes) (Result, error)`.
`Options` fields: `FFmpeg`, `FFprobe` strings; `MaxInputBytes`, `MaxOutputBytes`
int64; `Timeout` time.Duration. Zero options select CLI defaults below.
`Result` contains JSON `mode` (remux/transcode), `size` (actual bytes), and
`duration` (verified seconds). Errors are `*Error{Code string}`; only safe codes
`busy`, `invalid_request`, `invalid_media`, `unsupported_media`,
`conversion_failed`, `worker_unavailable` cross IPC. Decoder diagnostics are never
returned or logged.

Private HTTP-over-Unix: `POST /jobs/{id}`, ID exactly 32 lowercase hex digits;
body <=1 KiB, only `{"format":"mkv"|"avi","max_output_bytes":positive_int}`.
No query strings, paths, source URLs or credentials. Success 200 returns Result;
errors return `{"code":"..."}` (busy 429, invalid_request 400,
worker_unavailable 503, media/conversion failures 422). A single global slot
rejects concurrent conversions rather than queuing. Caller cancellation and the
worker timeout target the directly launched process; they do not prove descendant
termination or per-job containment. The Unix client ignores proxy environment,
never follows redirects, never dials TCP, caps response bodies/headers at 4 KiB,
uses a 3s connect and 65m response-header bound; caller context is the overall
request deadline (callers should always supply one).

## Files and confinement

The API owns creation/cleanup of `incoming/{id}/input`. Input must be nonempty,
regular, single-link, non-symlink and within the bound. Jobs must be real
directories. Linux openat2 (no symlink resolution), openat/O_NOFOLLOW, inherited
input/output FDs, exclusive partial creation and renameat2/RENAME_NOREPLACE
protect path handling. Unsupported kernels/filesystems fail closed. Unknown
files are neither removed nor overwritten. Only the worker's exact created
`output.part.mp4` inode is removed on failure. Success atomically publishes
`output.mp4`; originals remain unchanged. Output files are 0600. Initialization
creates 0700 directories; the API must create private 0700 job directories and
0600 inputs. CLI umask is 077 (library never changes process umask/rlimits).

API mounts the **whole scratch base** including sibling `incoming/` and `ready/`.
Worker mounts **only incoming**, not the base or ready. After success the API
copies the bounded output into a **new private inode** in ready, then removes its
known incoming files. Renaming or hardlinking the worker output would be unsafe:
retained worker FDs could still mutate the completed cache. Publication through
the authenticated API happens only after copying and syncing. API and worker
scratch access remains a trusted boundary during processing; FD pinning does
not prevent a same-UID writer modifying an already-open input. Never use
hardlinks to originals.

**exec cwd is not a sandbox.** Decoder file-protocol access remains possible
inside its container. Required runtime confinement: uid/gid 10001; only the
incoming scratch bind writable; read-only container root; no NAS/Store, API
secrets, issuer/signing keys, service credentials or host filesystem mounts;
network disabled, all capabilities dropped, no-new-privileges and externally
configured memory/CPU/PID/storage quotas. Do not substitute host execution as a
production security boundary. No port is exposed. Keep the socket private to the
API/worker UID; mode is 0600. Preexisting sockets are refused, never unlinked on
startup; operators must investigate stale artifacts rather than auto-cleaning.

## CLI and initialization

Flags (no environment configuration):

- `--root`: required incoming directory, or empty scratch base for init mode
- `--socket`: default `<root>/worker.sock`
- `--ffmpeg`, `--ffprobe`: default executable names on PATH
- `--timeout`: default `1h`
- `--max-input-bytes`, `--max-output-bytes`: each default `10737418240` (10 GiB)
- `--init-root`: initialize and exit; no listener, probes or conversion

Initialize requires an **existing, empty, nonsymlink scratch base**. It requires the base already have mode 0700 and makes only
`.video-worker-v1` (0600) and `incoming/`, `ready/` (0700).
It rejects all nonempty bases, including already initialized ones. It performs
no ownership repair, mount setup, PVC initialization or storage migration.
Provision ownership to 10001 out of band before initialization; init failures may
leave newly created artifacts and do not trigger speculative cleanup.

Local synthetic-only development (Go 1.27.3, Linux, FFmpeg/FFprobe installed):

```sh
# From repository root; use the specified Go 1.27.3 binary as `go`.
go build -o /tmp/video-worker ./api/cmd/video-worker
# Run both modes as UID 10001, using an already-created fresh private base:
/tmp/video-worker --root /tmp/video-preview-scratch --init-root
# Run as uid 10001, with synthetic data only when outside container confinement:
/tmp/video-worker --root /tmp/video-preview-scratch/incoming
VIDEO_WORKER_INTEGRATION=1 go test -race -count=1 ./api/internal/videoworker ./api/cmd/video-worker
go vet ./api/internal/videoworker ./api/cmd/video-worker
```

`video-worker/Dockerfile` uses a native Go 1.27.3 build and Debian bookworm FFmpeg
runtime. Build context is repository root. No build/publish/run has been performed
by this implementation. The following persistent-worker example is **synthetic
development only, not activation-ready for tenant media**. A fresh disposable
job sandbox and confirmed whole-sandbox teardown are required before production.
Illustrative Docker commands (not executed):

```sh
docker build -f video-worker/Dockerfile -t drive-video-worker:local .
# Requires initialized, UID-10001-owned, dedicated quota-limited scratch.
docker run --rm --network none --user 10001:10001 --read-only \
  --cap-drop ALL --security-opt no-new-privileges:true \
  --pids-limit 64 --cpus 1 --memory 512m --memory-swap 512m \
  --tmpfs /tmp:rw,noexec,nosuid,nodev,size=16m \
  --mount type=bind,src=/tmp/video-preview-scratch/incoming,dst=/scratch/incoming \
  drive-video-worker:local --root /scratch/incoming
```

The development bind above does not prevent a compromised decoder retaining
execution and observing future incoming jobs. Production needs a constrained
trusted controller and a fresh sandbox exposing only the current job, never the
incoming parent, ready cache or control socket; admission stays fenced until
whole-sandbox teardown is confirmed. No runtime/controller, architecture emulation,
image import/export or deployment is provisioned by this source example.

## Conversion and acceptance limits

Input probing is capped at 15s, restricted to AVI or Matroska demuxing and
file/pipe protocols. Only first video and optional first audio are selected.
Duration must be finite, positive, <=6h. Compatible H264 yuv420p <=1920x1080 plus
AAC/no audio is remuxed; otherwise first video becomes libx264 CRF23 yuv420p,
one decoder/encoder/filter thread, no upscale, aspect-preserving even dimensions
within 1920x1080, and first audio AAC stereo 128k. Subtitles/data, metadata and
chapters are stripped. Output is faststart MP4, never HLS or multiple renditions.

FFmpeg logs and probe output are bounded. `-fs` plus post-validation bounds output;
the final 64 KiB below the requested cap is conservatively rejected because
FFmpeg can exit zero at its cap. Consequently very small caps cannot succeed.
Output must independently probe as one H264 yuv420p video plus optional AAC,
with known duration within 2s of input and no shortening greater than max(100ms,
1%); the small allowance accounts for container/audio padding. This is structural
validation, not a complete decoded-frame equality/integrity proof. Scratch quotas
remain necessary: output trailers/faststart can temporarily overshoot `-fs`, and
process crashes can leave private partials which startup deliberately preserves.

Tests cover strict requests, Unix IPC/deadlines/response bounds, file and job
symlinks, hardlinks, missing/empty/oversize inputs, no-overwrite, initialization,
concurrency admission, corrupt media, and failure cleanup. Opt-in tests generate
only tiny synthetic MKV/AVI, exercise real remux/transcode, cap rejection and
cancellation; no production/provider/NAS data is used.

Activation gates belong to the parent/operator: independent review;
resource/confinement and Unix permissions validation; scratch
budget/free-space/cancellation/cache lifecycle acceptance; browser MP4 playback;
explicit runtime/deployment approval. This component alone authorizes none of
those actions and supplies no readiness or public-serving endpoint.
