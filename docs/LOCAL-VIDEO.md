# Explicit local video execution

The deliberately selected `local` mode runs FFprobe and FFmpeg **inside the main
Drive API container**. This is the user-approved alternative to a sandbox, not a
qualified remote controller. No Kubernetes Job, sidecar, host policy or fake
isolation advertisement is involved. The default remains `disabled`; an
unavailable remote controller never falls back to local execution.

## Trust and limitations

Decoders inherit the API UID, environment and mount permissions. A decoder
vulnerability can compromise the API and access its mounted data/secrets. Process
groups, timeouts, private directory checks, protocol whitelists and output parsing
are **not containment**. Decoder network destinations are not restricted by an
HTTP protocol whitelist in local mode. No hostile-descendant containment or
whole-container quiescence is claimed: a malicious child could escape its process
group. Application disk accounting is polling/admission control, **not a hard
filesystem quota**. Container resource limits apply to the shared API, not an
independent decoder sandbox.

The local adapter admits one job, starts a tracked probe followed by a tracked
conversion process group, kills the group on cancellation/deadline and waits and
reaps its direct tracked children. Same-group leftovers are also signaled at
completion. It reports only process-local completion, not a sandbox teardown
receipt. The API then revokes and joins all selected-source HTTP handlers before
releasing admission. Blocked storage calls retain admission until they return.
Do not clear an unknown execution fence to work around a blocked or suspect job.

Auth/bearer validation, live share checks, acquired source version/metadata checks,
range budgets, source deadlines and strict private artifact/hash/codec validation
are unchanged. No full source staging or input hashing is needed. Seek generations,
independent video/audio modes and explicit video-transcode retry use the existing
v3 timeline contract. No automatic security/validation-error fallback exists.

## Configuration and operator initialization

- `VIDEO_EXECUTION_MODE=disabled|local|remote`: one exact value, default `disabled`.
  Invalid or combined values fail startup. Local and remote server options cannot
  be combined. `remote` retains the qualified Unix-controller requirements.
- `VIDEO_PREVIEW_ROOT`: absolute, separately owned scratch directory, independent
  of the NAS object root and torrent work directory (overlapping paths and resolved
  symlink aliases are rejected). A separately sized persistent scratch volume is
  an operator choice, not supplied by this source change.
- `VIDEO_PREVIEW_MAX_INPUT_BYTES`: default 10737418240 (10 GiB).
- `VIDEO_PREVIEW_MAX_OUTPUT_BYTES`: default 10737418240 (10 GiB).
- `VIDEO_PREVIEW_BUDGET_BYTES`: default 32212254720 (30 GiB).
- `VIDEO_PREVIEW_FREE_RESERVE_BYTES`: default 2147483648 (2 GiB).

Limits are integer byte counts. Admission conservatively reserves input plus twice
output even though local input is not staged. Current server job timeout is one
hour, completed retention two hours, one active job; the input broker allows
64-KiB backend chunks, concurrency two and cumulative reads up to eight times the
selected input size. Probe and conversion share the job/source deadline. These
bounds are not hard memory/disk isolation. Operator-selected larger caps must fit
the independently sized scratch volume and shared API resources.

Before enabling execution, the operator creates an **empty** scratch directory
owned by the container API UID/GID (image defaults 10001:10001), mode 0700. With
that directory mounted, run the one-shot command as the same UID:

```sh
VIDEO_PREVIEW_ROOT=/path/to/private-video drive-api --init-video-root
```

Initialization exits without opening storage, TLS, auth services or workers. It
creates only `.video-worker-v1`, `incoming` and `ready`; it never repairs ownership,
clears data, adopts a nonempty path or initializes during normal startup. This flag
cannot be combined with filesystem initialization or serving transport flags.
Normal startup requires the initialized private layout and takes `.api.lock`.

The persistent `.active-preview` marker records admitted identity before execution.
An API restart generates a new process-local epoch and cannot reconstruct/reap old
jobs from an in-memory receipt. A surviving marker keeps video admission fenced;
it is never auto-cleared. Operator investigation is required to establish that old
execution and source I/O have stopped before any explicit recovery. Persistent
scratch and the active marker must survive Pod replacement if restart fencing is
to remain meaningful. Losing scratch loses that durable evidence.

## Images and tools

Local startup runs fixed, bounded `ffmpeg -version` and `ffprobe -version` commands
without media input or network access. Missing/failing tools fail local startup;
there is no fallback to staged/remote decoding.

`api/Dockerfile` runtime-base retains `aria2`, `ca-certificates` and `libreoffice`
and now also installs Debian `ffmpeg` (which supplies FFprobe). For a native ARM64
runtime build on an ARM64 builder, the operator can build the runtime-only target:

```sh
docker build --target runtime-base -f api/Dockerfile -t <reviewed-runtime-tag> .
```

The selected runtime was built on gamma using an isolated BuildKit builder with
bundled ARM64 emulation, without global binfmt or Docker configuration changes,
and published as the immutable pin in `api/Dockerfile.arm64`:

```text
10.200.73.2:5000/vendor/drive-runtime-arm64@sha256:1d5fc53e8b65a9a0bc9b1d456591f604e6131f983889a6e22007cd0b3b221d16
```

Native beta checks verified ARM64, UID/GID 10001, aria2, LibreOffice, CA roots,
FFmpeg/FFprobe, and H.264/AAC plus VP9/Opus encode/probe/decode. Remote manifest
bytes were hashed independently. This qualifies the runtime tools, not the final
API image, scratch mount, application rollout or production activation. Jenkins
builds the final API image using this published pin; no target RUN or host binfmt
registration is needed in the ordinary CI cross-build.

The permanently unqualified development Unix handler remains unqualified. Remote
qualification NO-GO statements elsewhere apply to remote mode; they do not impose
a sandbox requirement on the explicitly selected, risk-accepted local mode.
