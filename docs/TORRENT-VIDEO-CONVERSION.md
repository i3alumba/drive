# Opt-in torrent video derivatives

The Drive checkbox **Convert downloaded videos for browser playback — keep
originals** is initially unchecked. Its value is captured when each torrent is
enqueued, including generic uploads and drops. Later checkbox changes do not
change queued jobs. Ordinary uploads are unaffected.

`POST /api/torrents` accepts one multipart `convertVideos` field containing
literally `true` or `false`. Missing means false for legacy clients. Repeated,
file-valued, empty, or other values are rejected before starting aria2. Existing
personal-space ownership and bearer authorization still apply.

Conversion is available only in the already configured `VIDEO_EXECUTION_MODE=local`
API with FFmpeg and ffprobe installed. Disabled/remote mode or unavailable tools
reject opt-in explicitly; this feature does not enable local execution or change
production configuration. Main shares one local-media admission token between
preview and conversion. Preview fails busy rather than queueing behind conversion.

After downloading, originals are published normally. Known completed local inputs
are retained without a second input copy or NAS staging. Up to four conversion
jobs (including the active job) are admitted, with one active conversion job.
Inputs and a full possible 32 GiB output must fit a conservative 64 GiB retained
budget. Consequently large jobs may be rejected even below the individual 64 GiB
input ceiling. Admission and each file require actual torrent-work free space of
at least 40 GiB (32 GiB output plus 8 GiB reserve); a declared volume capacity is
not treated as a quota or available space. Enumeration is bounded to 4096 regular
files and 64 GiB total; symlinks and nonregular files are rejected.

Supported candidates are MKV, AVI, MP4/M4V, and MOV, checked by actual bounded
probing. The first non-attached video and default (otherwise first) audio track
are selected. H264 yuv420p video within 1920×1080 is copied; other video is encoded
to H264 yuv420p, even dimensions, aspect-preserving, at most 1080p, without
upscaling. AAC audio is copied; other audio becomes stereo AAC. Silent videos stay
silent. Derivatives omit subtitles and other tracks; originals preserve them.
HDR/unsupported tone mapping is rejected rather than silently washing out colors.

Each file gets a fresh queue-owned 24-hour deadline shared by conversion and
publication, independent of the download clock, with 60-second probes and a six-hour media-duration ceiling. Outputs are
limited to 32 GiB, finalized as faststart MP4, probed for codec/tracks/duration,
and decoded to EOF before publication. This uses MP4 validation, not HLS grammar
validation. Child diagnostics are bounded and never returned as media paths or
URLs in public errors.

Derivatives are siblings named `<bounded-stem>.browser-<job-id>-<file-index>.mp4`.
Publication is atomic create-only: filesystem `RENAME_NOREPLACE`, or MinIO
`If-None-Match: *`, including multipart completion. Backend conditional-write
support is required; there is no Stat-then-Put fallback. MinIO multipart writes use
sequential bounded 16 MiB parts and retain the condition explicitly at completion,
rather than the pinned SDK high-level path that discards conditional options.
Failed uploads synchronously attempt to abort only their owned upload ID with a
separate five-second cleanup deadline. Abort failure is reported as
`publication_cleanup_failed`; an incomplete remote upload may remain. No existing
object is deleted, and ambiguous completion is not retried as a new publication. Originals retain the
existing normal-overwrite behavior. Existing derivatives are not replaced.

Job responses include `convertVideos`, `conversionStatus`, and per-file
`conversionResults`, including skips and mixed success. Originals remain available
when conversion fails or is cancelled. Cancellation joins active children and
storage calls (including abort) before cleaning only the job's owned work.
Deadline expiry requests cancellation; uninterruptible storage must actually join
before media admission or retained resources are released. Already-published
originals/derivatives are not rolled back. There is no automatic retry.

Jobs and the immutable opt-in exist **only in API memory**. Pause/resume retains
the option. Durable torrent journaling and restoration after API restart are
explicitly deferred; restarting the API does not restore conversion queues.
