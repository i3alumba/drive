# Private seekable input (development only, NO-GO)

**Execution mode update:** explicitly selected local jobs now run in the main API
container under the API UID/mount permissions, without a sandbox. See
[local configuration, initialization and accepted risks](../../../docs/LOCAL-VIDEO.md).
Default execution is disabled. Sandbox qualification/NO-GO statements below remain
requirements for remote mode, not for the deliberately selected local alternative.

`Reader` is exercised through real FFprobe/FFmpeg by
`videoworker.RunDevelopmentRemoteHLS`, using a fresh loopback listener and a
versioned trusted `DevelopmentSource` selection. It is **not an enabled public
streaming lane**. The default API lane still stages input; the opt-in v3 lane does not. The development controller stays
permanently unqualified. Admission, auth, share checks and playback are unchanged.

Each instance binds one selected storage key and known size to a random 256-bit
execution capability and a mandatory deadline. GET forwards only an explicit,
bounded byte ranges to `Store.GetObjectRange`; HEAD does not touch storage. Single
closed/open/suffix ranges return 206; invalid or unsatisfiable ranges return 416.
An absent range returns 200 and open-ended ranges return 206, including when
they exceed `MaxRequestBytes`: this limit now bounds **each backend chunk**.
Reservations happen before each chunk, never for the entire requested remainder.
This accommodates actual FFprobe/FFmpeg initial access and reconnects without
unbounded backend reads. Budget exhaustion before headers returns 429; exhaustion
after headers terminates the incomplete response (the decoder must fail closed).
Concurrent excess returns 429;
there is no queued work. Reservations are never refunded, even after errors.
No file staging, full hashing, redirects, arbitrary URL fetching, or cross-job
cache is implemented here. Response transports must implement write deadlines;
unsupported transports fail closed. Each HTTP response uses the explicit job deadline, not a fixed five-second
write lifetime. Revocation force-expires that
deadline so a decoder that stops reading cannot retain handler admission. Storage backends retain their own integrity behavior.

The path is a secret execution capability: do not log or expose it to browsers.
Revoke cancels in-flight storage contexts, closes acquired readers, and rejects
subsequent requests. A replacement execution must get a new Reader, and its
controller must stop and join the old execution **before** replacement admission.
An unchanged size is not an object identity proof: callers must preserve existing
source selection/immutability fences and revalidate live authorization/shares.
The Store must honor cancellation and support concurrent Close to interrupt reads.

## Required before wiring or qualification

- Qualify the v3 controller contract described in `videoworker/HLS.md`.
  The opt-in server lane now binds remote input to job/epoch/execution and limits,
  rechecks live owner/share/metadata on reads, and joins source handlers before
  releasing admission. Nonzero-start generations use accurate video transcoding;
  start/origin/full-duration and replacement identity are included in v3.
- Bind this handler on a private job-scoped listener, never the public API router;
  restrict it to the decoder's namespace. Do not accept caller-supplied URLs.
- Qualify decoder sandbox egress so it can reach **only** this broker endpoint:
  reject redirects/proxies and arbitrary destinations. An FFmpeg HTTP protocol
  whitelist limits protocols, **not destinations**, and is not an egress policy.
- Qualify the development FFprobe/FFmpeg path for a production disposable
  controller. The runner now independently selects video copy plus AAC stereo
  audio conversion (`copy_video`), whole-stream remux or video transcode. Actual
  synthetic MKV/AVI/MP4/MOV tests cover these modes. AVI fixtures use global H264
  headers: repeated/configuration-changing NALs remain validation failures, not
  silently filtered or retried. Bounded unsuitable-GOP generation fallback is
  not automatic: explicit video-transcode replacement is available instead.
  Validation/security failures never trigger automatic fallback.
- Implement new immutable job/assets for out-of-prefix seeks, normalized local
  timelines, actual origin/duration, and old execution termination before new
  admission. No cross-job reuse.
- Preserve direct playback; implement explicit MP4/MOV preparation fallback,
  whole-duration seek UI/debounce, accepted-mutation deduplication, and stale
  callback guards.
- Exercise actual FFmpeg MKV/AVI/MP4/MOV fixtures, start-before-all-indexed-input
  reads, browser progressive playback/seeks, live-share revocation, cancellation,
  controller generations, and qualified sandbox/network boundaries end to end.

Activation remains **NO-GO**. Historical NAS-to-beta corruption is out of scope.
