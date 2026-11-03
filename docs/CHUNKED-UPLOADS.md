# Chunked uploads

This protocol replaces single-request computer uploads with authenticated,
restart-resumable sessions. Source publication and production deployment are
separate operator-authorized steps. Cloudflare Flexible, HTTP origin transport,
ingress, certificates, NAS format, volume identities and PVCs remain unchanged. `.torrent` metadata retains its separate 8 MiB API.

## Protocol

- `POST /api/uploads`: JSON `name`, `path`, `space`, `size`, `contentType`;
  optional canonical UUIDv4 `clientId` makes creation retries idempotent for the
  same owner/destination/size within retained history. The first content type is
  retained. Returns 201 with private session status and an 8 MiB chunk size.
- `GET /api/uploads`: list only the current user's recoverable sessions.
- `GET /api/uploads/{id}`: status and acknowledged part sizes/SHA-256 digests.
- `PUT /api/uploads/{id}/parts/{index}`: binary body, with `X-Chunk-SHA256`;
  exact expected length, bounded request body, durable acknowledgement.
- `POST /api/uploads/{id}/complete`: return 202 and commit asynchronously;
  repeated completion of the same session must never spawn another commit.
- `DELETE /api/uploads/{id}`: cancel and reclaim only that session's staging;
  refuse cancellation while publication is in progress rather than promise
  rollback of an already committed object.

Every operation validates live Auth and the immutable session owner. Session
IDs are not capabilities. Creation and completion use the existing personal and
editable-shared-space path resolver; completion rechecks the original destination
and permissions. No bearer, cookie or password is persisted in metadata.

## Persistence and limits

Use a private `.upload-sessions-v1` subdirectory of the existing torrent work
filesystem, never the original NAS object tree and never a new/migrated volume.
Torrent jobs use their own UUID directories; neither manager may clean the
other's work. Stage files are 0600 and directories 0700; reject symlinks and
unsafe identifiers. Persist part data before its receipt, using fsync and atomic
metadata replacement. Hold an exclusive manager lock to prevent two processes
from mutating the same staging root.

Candidate defaults: 10 GiB maximum file, 20 GiB reserved aggregate staging,
2 GiB minimum filesystem headroom, two active sessions per user and four per
process, 24 hours idle expiry. Reservation uses declared file size, not just
received bytes. Account for temporary retry parts and ongoing torrent work;
check free space on every part. Read-only beta inventory before implementation
found 230,414,417,920 bytes available on the existing work filesystem; this is a
point-in-time observation, not a persistent guarantee or permission to fill it.
Byte limits are configurable through `UPLOAD_MAX_FILE_BYTES`,
`UPLOAD_STAGING_BUDGET_BYTES` and `UPLOAD_FREE_RESERVE_BYTES` (or manager options
in tests); invalid values fail startup validation. The format caps a file at
64 GiB/8192 parts. Tests exercise quota failures with small limits. Cleanup is bounded, restricted to recognized manager-owned sessions,
and never walks/deletes user objects or unknown torrent directories.

## Completion and recovery

Never assemble the entire file in memory. Verify ordered parts and calculate a
whole-file digest, then stream through the existing Store publication contract.
The filesystem backend already stages an object and atomically replaces it;
failed uploads must preserve an existing object. Keep ordinary destination
replacement semantics; do not claim a new cross-writer no-clobber guarantee for
imports, torrents or independent clients.

Initial creation and resume are distinct. After a validated creation response,
all continuation uses GET, never another create POST. A missing established
receipt (including expiry after a lost completion acknowledgement) requires
checking the destination; it must not recreate and overwrite a later file.
Only an initial creation whose response was never accepted can retry its
idempotent create request: no part/publication can have started at that point.
Creation publishes a complete private manifest atomically; a directory-sync
failure is reconciled on the next same-ID attempt without requiring restart.
On startup, the exclusive lock also protects recovery of pre-rename initialization
directories: empty, complete-manifest and partial atomic-write cases are reclaimed
before applying the retained-session inventory limit. Unknown contents and
symlinks are preserved and fail recovery closed, never recursively deleted.

After a restart, incomplete sessions resume from durable receipts. A session
interrupted during publication is uncertain, not automatically retried: reconcile
against the expected completed object or report an explicit conflict without
blindly overwriting a subsequently changed destination. Never persist a token
to automatically resume an authenticated commit after restart.

## Browser

Use File.slice and bounded Web Crypto SHA-256 per part. Show uploading versus
server-side saving separately. Retry transient failures only, with bounded
backoff and per-request deadlines; authentication, integrity and size failures
are not infinitely retried. Pause stops new parts and aborts in-flight requests
without deleting accepted data. Cancel calls the session DELETE endpoint.

After reload, list own incomplete sessions and ask for the local file again.
Verify its size and all retained part digests before resuming; a filename match
alone is insufficient. Do not persist tokens or full files in browser storage.
Progress must distinguish durable server acknowledgement from transient bytes
sent. Existing same-destination queue ordering must remain intact.

## Acceptance gates

- Empty/small files, >100 MiB and multi-GiB streams; exact final digest.
- Missing, short, oversized, corrupted, conflicting, duplicate and concurrent parts.
- Disconnect, bounded retry, pause/resume, cancellation and idle expiry.
- Recovery after restart and uncertain-publication handling without blind rewrite.
- Quota/free-space errors, cleanup failures and preservation of an old destination.
- Owner isolation, revoked grants, shared-space boundaries, traversal/symlink attacks.
- Existing imports, public links, multipart compatibility and actual torrent tests.
- Frontend race/error/progress tests and live API browser fixture, without production
  accounts, personal data, signed URLs or credential artifacts.

Run Go race/vet/build, frontend locked install/build/browser tests and targeted
formatting. `DRIVE_QA_PREVIEW=1` runs browser tests against the built bundle instead
of a memory-intensive development bundler. The real browser/API fixture is:

```sh
cd frontend && npm ci && npm run build && cd ..
DRIVE_UPLOAD_BROWSER_INTEGRATION=1 GOTOOLCHAIN=go1.27.3 go test -race ./api/internal/server -run '^TestChunkedUploadBrowserActualAPI$' -count=1 -v
```

It requires Node, Linux and sandbox-capable Chromium (`CHROMIUM_PATH` can select
an installed executable). It uploads 101 MiB + 3 bytes through the actual compiled
frontend and Go handlers, pauses, reloads and reselects the file, verifies that
accepted part zero is not retransmitted, and compares the whole-file SHA-256.
The Auth service and storage are owned loopback/temp fixtures, not production;
this is not production NFS/Cloudflare acceptance. Host sandbox protections are
not disabled. The opt-in fixture is skipped in ordinary Go runs.
Independent review is separate from implementation. Commits, push, PRs and
production deployment require explicit operator authorization; passing tests do
not grant it. Release authorization does not authorize production test accounts,
NAS initialization/migration, or changes to original user data.

For the additional 2 GiB streaming fixture (temporary local disk only):

```sh
DRIVE_UPLOAD_LARGE_INTEGRATION=1 GOTOOLCHAIN=go1.27.3 go test -race ./api/internal/server -run '^TestUploadManagerSmallEmptyAndLargeStreaming$' -count=1
```
