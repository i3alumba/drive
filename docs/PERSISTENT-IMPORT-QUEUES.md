# Durable server URL import queues

This protocol document is not deployment or live-provider acceptance evidence.
The separate legacy `/api/imports` endpoint and its response shape remain
in-memory. `/api/import-queues` records **owner-authorized server execution
intent**: after durable HTTP 202, closing the browser/PC, logging out, or expiration
of the accepting JWT does not stop execution. File bytes go from source to server
staging to Store, never through the PC. No JWT, cookie, password or refresh token
is saved or fabricated.

All acceptance, listing and management require the existing live Auth identity
and Drive grant. Mutations require a bearer header; a queue ID is not a capability.
Queues write only the canonical owner's personal `users/{owner}/` space, never
shared spaces. With the current Auth interface, **grant revocation does not
automatically revoke accepted intent**. Cancel or pause before revoking the grant
if work must stop. Management requests after grant loss fail closed while already
accepted work continues until its retained deadline/expiry.

## HTTP API

All queue responses are `Cache-Control: no-store`. Public DTOs contain status,
relative destination paths, names, counts, checksums and safe error codes only:
never source URLs, provider public keys, signed hrefs or internal storage keys.

`POST /api/import-queues` (maximum 12 MiB):

```json
{
  "submissionId": "0123456789abcdef0123456789abcdef",
  "path": "downloads",
  "space": "personal",
  "existing": "skip_verified",
  "sources": [
    {
      "type": "direct",
      "url": "https://example.com/file.bin",
      "name": "file.bin",
      "sha256": "optional-lowercase-64-hex"
    },
    { "type": "yandex", "url": "https://disk.yandex.ru/d/public-share" }
  ]
}
```

`sha256` and direct `name` are optional (omit the illustrative non-hex value).
`existing` defaults to `error`; accepted values are `error|skip_verified`.
Sources number 1–1000. Direct names default to a validated URL basename or
`download`, not a source-chosen Content-Disposition. The selected path must
already be canonical and relative: absolute, empty components, dot segments,
backslashes, controls and invalid UTF-8 are rejected before normalization.

202 is returned only after durable state and authoritative acceptance manifest
writes succeed. Use a random 32-character lowercase hex `submissionId` per
unchanged submission. The same owner/ID/canonical request returns the same receipt;
changed contents return 409, expired receipts 410. Invalid constructed destinations
and duplicate destination names return definitive 422 before registration or
persistence, so the draft can be corrected. A persistence error can occur
after a rename: do not assume a failed/uncertain POST was not accepted. Reconcile
with `GET /api/import-queues/submissions/{submissionId}`, not a new submission ID.
A frozen receipt returns 503 on repeat submission until fenced recovery succeeds.

- `GET /api/import-queues?offset=0&limit=100`: `{queues,total,offset,limit}`.
- `GET /api/import-queues/submissions/{submissionId}`: owner-scoped summary.
- `GET /api/import-queues/{id}`: owner-scoped summary.
- `GET /api/import-queues/{id}/items?offset=0&limit=100`:
  `{items,total,offset,limit}`. Default 100, maximum 500, nonnegative offset.
- `POST /api/import-queues/{id}/pause|resume|cancel`: 202 summary.
- `DELETE /api/import-queues/{id}/items/{itemId}`: 200 item; saving returns 409.
- `POST /api/import-queues/{id}/items/{itemId}/retry`: 202 item, **failed only**.

Pause stops **new admission**, not active work: active downloads and saves may
finish, including an already-admitted inventory page. Resume continues eligible
work. Cancel durably stops new work and cancels pending/downloading items;
**saving may finish** and remains explicitly counted. Neither operation deletes
or rolls back destination data. Individual saving-item cancellation is a conflict.

An explicit failed retry creates a new bounded attempt generation, retaining old
counts/history, total attempts and the original execution deadline. It cannot
extend the deadline or revive a cancelled queue. At most 64 explicit retry
histories are retained per item. `needs_verification` is **not** a download retry:
inspect/verify the destination and use a different target/new intent if needed.
Complete receipts never redownload merely because output was later deleted/moved.

Summary fields: `id,path,state,acceptedAt,expiresAt,totalItems,completedItems,
failedItems,cancelledItems,queuedItems,activeItems,savingItems,
needsVerificationItems,totalBytes,completedBytes,unknownSizeItems`; optional
`discoveryState,errorCode,normalizedNames`. Unknown lengths use total 0 plus
`unknownSizeItems`, not negative bytes. Item fields: `id,queueId,position,name,
path,state,bytes,total,attempts,totalAttempts`, optionally `sha256,nextAttemptAt,
errorCode,reused`. Download progress is volatile; lifecycle checkpoints are durable.

Item states: `queued,resolving,downloading,saving,retry_wait,complete,failed,
cancelled,expired,needs_verification`. Summary states additionally distinguish
`discovering,running,paused,cancelling,cancelled,complete,partial,expired,
needs_verification,blocked`. `blocked` or discovery failure is not success.

## Storage protocol and recovery

Existing Store/private ACLs are the baseline. **NAS envelopes and the S3 adapter
do not provide application encryption.** Stable source URLs are private,
access-controlled server metadata, not cryptographically encrypted. Storage
administrators/readers with backend access can read them; S3 confidentiality also
depends on existing bucket policy. No new secret, key, database or volume exists.
Ordinary file APIs remain beneath personal/shared prefixes, outside this reserved
namespace:

```text
_drive/import-queues/{owner}/{queueID}/manifest.json
_drive/import-queues/{owner}/{queueID}/state.json
```

The immutable acceptance manifest contains schema/identity/fingerprint, canonical
request, accepted time and seven-day expiry. The bounded atomic state snapshot
contains all committed item records, source-index locators, retained attempts and
deadlines, control state, collision identities and inventory page checkpoints.
Sources are referenced by index, not duplicated per child. State is written
**before** the manifest; only manifest existence commits acceptance. Orphan state
without a manifest is never executable. Inventory page records and checkpoint
advance together in one state snapshot before any child becomes executable.

Each lifecycle/control/page transition writes a complete atomic snapshot; progress
ticks do not write metadata. This favors a simple transaction boundary over
metadata efficiency: large queues have whole-snapshot write amplification, and
no throughput benchmark is claimed. Hard limits are **16 MiB encoded per
snapshot**, **128 MiB retained manifests+snapshots per process**, **10,000 retained
items per owner**, and **50,000 process-wide** (pending provider intents also
reserve records). Capacity failures occur before replacement/commit; discovery
limits are visible, never silent truncation. Receipts are not silently evicted or
recreated at expiry; retained metadata still consumes capacity. There is no new
garbage-collection/admin deletion endpoint.

Any metadata write error freezes the queue in that process: readback alone does
not prove directory fsync. Fenced startup validates schema, owner, identity,
fingerprint, revision and paths and successfully **recommits** readback before
dispatch. Missing/corrupt committed state blocks the queue, never reconstructs
pending work. Unknown retained capacity from corrupt state closes new admission;
healthy already-committed independent items can still finish. Malformed acceptance
catalogs fail queue initialization closed.

Startup enables queues only **after the existing Linux URL-import work lock is
acquired**, before dispatch. No durable queues run on unfenced/non-Linux APIs;
legacy import compatibility remains. The work-directory flock fences only
processes sharing that directory, not separate work volumes; the existing
single-writer deployment assumption remains necessary. A blocked Store/NFS call
retains its worker slot even after context timeout; no replacement goroutines are
spawned to bypass it.

Before invoking destination Put, the server durably checkpoints `saving` with
exact personal relative target, computed full SHA-256 and actual size. Failed
saving checkpoint means **no Put**. Put invocation errors, saving after a crash,
or a missing completion write are publication-uncertain, never automatic Put
replay. Startup saving recovery streams the destination through **full SHA-256**
and checks exact size: matching bytes become complete; absent/different/unreadable
or outage results become `needs_verification`. An outage is not evidence of
absence. Directory publication uncertainty is conservatively needs-verification.
A Put error reported during normal execution stays needs-verification; it is not
silently retried as a download.

Resolving/downloading recovery restarts the current file **from zero**, only
because the committed publication boundary proves Put impossible. Attempts,
backoff and deadline are retained; paused/cancelled intent survives restart.
Scratch names bind queue/item/attempt before creation. FD-relative no-follow,
UID/single-link-checked files use the existing private import root. Only exact
referenced scratch files are reclaimed; anonymous `.url-*`, upload/torrent and
unknown files are untouched. Failure/cancellation never deletes destination files.

## Scheduling, limits and source rules

Legacy and durable imports share **two active per owner/four process-wide**,
**10 GiB per file/20 GiB staging reservations/2 GiB free reserve**, and the existing
URL-import configuration (`URL_IMPORT_MAX_FILE_BYTES`,
`URL_IMPORT_STAGING_BUDGET_BYTES`, `URL_IMPORT_FREE_RESERVE_BYTES`,
`URL_IMPORT_TIMEOUT_SECONDS`). No new runtime configuration is introduced.
URL-import and computer-upload budgets remain independent. Headroom cannot reserve
space against external writers.

The scheduler rotates owners, selecting FIFO eligible items, skipping
paused/backoff/busy destinations; eligible queue demand prevents repeated legacy
requests bypassing queued owners (legacy retains bounded 429 admission). Known
import destinations serialize across both APIs. Destination existence is checked
immediately before saving; no known replacement is allowed. **This is not CAS
against ordinary uploads, moves, torrents or other writers.**

There are three automatic whole-file attempts with persisted 2/4-second backoff,
and a first-start + existing timeout (normally two hours) deadline, capped by the
fixed seven-day queue expiry. Restarts/retries do not reset it. Discovery pages
also retain a three-attempt budget/backoff and source traversal deadline. One
failed source does not block independent sources/items. Checksum-bound downloads
reuse the unchanged **16 MiB ranges/shared five attempts per range**. Direct source
expiry cannot magically refresh.

`skip_verified` only reuses a known existing file after expected size and expected
SHA are both available and full server-side streaming verification matches. No
SHA, wrong hash/size, or unverifiable content means conflict, never overwrite.
This uses no PC download and does not treat a size-only match as verification.

Yandex public file/folder inventory is asynchronous after 202, using only the fixed
HTTPS `cloud-api.yandex.net/v1/disk/public/resources` and `/download` APIs.
Supported public share hosts: `disk.yandex.ru`, `disk.yandex.com`,
`disk.yandex.com.tr`, `yadi.sk`; `/d/` and `/i/` share forms. No provider credentials
or browser cookies are used. Pagination uses locally constructed limit/offset,
not arbitrary metadata next links. JSON is bounded to 2 MiB/page, at most 200
entries/page, 10,000 committed pages, 32 path components, 255 bytes/component and
900-byte relative paths. Top folder **contents** go under the chosen destination;
nested/empty directories are preserved. Provider paths reject traversal before
normalization. Destination component edge whitespace is trimmed only with
`normalizedNames` warning; distinct paths normalizing to the same destination
conflict, as do multiple sources targeting the same path. Original provider
names/paths remain private for lookup.

Each file attempt rechecks stable metadata size/SHA and freshly resolves a signed
href; changed expected metadata fails as source mutation. Signed `file`/`href`
responses are never persisted. Metadata/download clients retain public-IP
validation/pinning, normal TLS verification, guarded redirects, no HTTPS downgrade,
no environment proxy and no credential/cookie/referrer forwarding. Provider
protocol is tested with bounded fixtures only: **external protocol/live acceptance
has not been separately verified or authorized**.

## Local validation boundary

`GOTOOLCHAIN=go1.27.3 go test -race ./api/internal/server ./api/internal/config`
includes focused queue fault/limit/provider/scheduling tests and
`TestImportQueueLocalHTTPDisconnectAuthExpiryAndRestart`: real owned loopback
HTTP Auth/source/API, real fsync/rename temp disk Store fixture, client disconnect,
new Server/manager loading durable records after lock release, and execution
without a valid original JWT. This is **LOCAL fixture integration**, not a real
browser, external provider, production NAS, network-outage or power-loss test.
Existing opt-in large/torrent/browser/NAS tests remain separate and unchanged.
