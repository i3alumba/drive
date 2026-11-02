# drive-migrate (Linux)

Operator-only migration/backup tool for Drive's existing S3 bucket and the candidate
NFS filesystem backend. It preserves exact canonical keys, MIME, subsecond S3
mtime, size and SHA256. S3 access is **read-only**: this tool cannot create a bucket,
write/delete source objects, deploy workloads, stop writers or switch services.

**Candidate, not released.** Production use requires the release gates in
[../../../docs/NAS-STORAGE.md](../../../docs/NAS-STORAGE.md), independent review,
a private NAS share and an actually fenced final source. Public/anonymous NAS
access is not suitable for real user data. Local tests and synthetic NFS acceptance
are not authorization to skip these gates.

## Install / local execution

From `drive/`, using the repository's Go 1.22 toolchain:

```sh
go build -o /tmp/drive-migrate ./api/cmd/migrate
/tmp/drive-migrate --help
umask 077
mkdir -p "$HOME/.local/state/drive-migration"
```

Keep manifests/backups outside Git, Docker build contexts and CI artifacts.
They contain private filenames/data. New outputs use mode `0600`, fsync and atomic
no-overwrite publication; failed writes do not publish a completed artifact.
Input files must be regular, non-symlink, with no group/other permissions.
Output parents must exist and must already be private/trusted. The tool does not
make public NFS/SMB identity mapping private by applying `0600`.

## Flags and environment

| Flag                                | Purpose                                                                          |
| ----------------------------------- | -------------------------------------------------------------------------------- |
| `--mode`                            | Required: one of the modes below; no default operation                           |
| `--manifest`                        | New inventory output, otherwise private input manifest                           |
| `--archive`                         | New backup tar output, or existing offline backup input                          |
| `--previous-manifest`               | Required only for final `sync`; authorizes exact old destination versions        |
| `--writers-frozen`                  | Required for `sync`; operator acknowledgement, **not** an automatic writer fence |
| `--endpoint`, `--bucket`            | Explicit S3 source, defaults only from `MINIO_ENDPOINT` / `MINIO_BUCKET`         |
| `--destination-root`, `--volume-id` | Defaults from `FILESYSTEM_ROOT` / `FILESYSTEM_VOLUME_ID`                         |
| `--timeout`                         | Go context deadline, default `24h`; e.g. `48h`. Hard NFS syscalls may outlive it |
| `--help`                            | Print help without connecting to S3/NAS                                          |

S3 credentials are accepted **only** through `MINIO_ACCESS_KEY` and
`MINIO_SECRET_KEY`, injected by the existing secure operator mechanism. There are
no default credentials and no credential flags. Do not paste credentials into
chat, shell history, commands or files. `MINIO_USE_SSL` is a strict optional bool
(default false for the existing internal S3 endpoint). Source endpoint/bucket/TLS
are bound into the manifest; switching them is not implicit adoption.

A bucket with enabled or suspended versioning is refused: historical versions
need a separate archival plan, not silent flattening. Unsupported/noncanonical
keys, duplicate keys, invalid MIME, oversized native path components and source
drift fail closed before migration certification. ETags are concurrency tokens,
**not** assumed to be MD5. Payloads are actually hashed.

## Modes / operator sequence

| Mode             | Reads                                | Writes                                                          |
| ---------------- | ------------------------------------ | --------------------------------------------------------------- |
| `inventory`      | S3 metadata + all payloads           | New private JSON manifest                                       |
| `backup`         | Manifest + S3 metadata/payloads      | New protected tar/PAX archive                                   |
| `verify-backup`  | Entire archive, offline              | Only count/status report                                        |
| `restore-backup` | Entire verified archive, offline     | Missing NAS objects; only exact verified matches may be resumed |
| `copy`           | Manifest + S3 + NAS                  | Missing NAS objects; conflicts/unknown objects refused          |
| `sync`           | Final/previous manifests + S3 + NAS  | Proven replacements and removals of obsolete **NAS** versions   |
| `verify`         | Manifest + all NAS metadata/payloads | Only count/status report                                        |
| `verify-source`  | Manifest + S3 metadata/key set       | Only count/status report; not a writer fence                    |

Reports contain counts/bytes, not filenames, credentials, raw vendor response
bodies or the manifest itself. Success exits 0; invalid commands/failed operations
exit nonzero. No successful report by itself certifies a global live snapshot.

```sh
# Credentials and MINIO_ENDPOINT / MINIO_BUCKET already injected securely.
# Source remains unchanged throughout all commands.
state="$HOME/.local/state/drive-migration"
/tmp/drive-migrate --mode=inventory --manifest="$state/initial.json"
/tmp/drive-migrate --mode=backup --manifest="$state/initial.json" \
  --archive="$state/initial.tar"
/tmp/drive-migrate --mode=verify-backup --archive="$state/initial.tar"

# Only after creating and explicitly initializing an empty PRIVATE NFS root
# with drive-api --init-filesystem. This tool never initializes a destination.
/tmp/drive-migrate --mode=copy --manifest="$state/initial.json" \
  --destination-root=/mnt/private-drive/root --volume-id='<expected-uuid>'
```

Live pre-copy may fail on source drift, leaving only individually verified copies.
Do not erase the partial destination: after a real freeze, a final manifest can
replace only versions proven by `initial.json`. Unknown destination keys/versions
abort **before any mutation**. The backend's lifetime writer lock prevents an API
process from serving/writing this destination concurrently with the migration.

Before final inventory, explicitly fence **all** source writers: old Drive API,
torrent jobs and any manual S3 writer. Confirm no new Pod/process can restart as a
writer. This is an operator/deployment task, not an effect of a CLI flag. Take and
verify a coherent final backup after the freeze. Check actual storage capacity;
declared Kubernetes PV sizes are not quotas or free-space guarantees.

```sh
# Only after the actual source write fence and confirmed available backup space.
/tmp/drive-migrate --mode=inventory --manifest="$state/final.json"
/tmp/drive-migrate --mode=backup --manifest="$state/final.json" \
  --archive="$state/final.tar"
/tmp/drive-migrate --mode=verify-backup --archive="$state/final.tar"
/tmp/drive-migrate --mode=sync --manifest="$state/final.json" \
  --previous-manifest="$state/initial.json" --writers-frozen \
  --destination-root=/mnt/private-drive/root --volume-id='<expected-uuid>'
/tmp/drive-migrate --mode=verify --manifest="$state/final.json" \
  --destination-root=/mnt/private-drive/root --volume-id='<expected-uuid>'
/tmp/drive-migrate --mode=verify-source --manifest="$state/final.json"
```

Only then may a separately reviewed rollout serve the NAS volume. Retain S3 and
backups. After NAS receives new writes, rollback requires reverse synchronization;
pointing to stale S3 would lose those new changes. No rollback deployment or
source deletion is automated by this tool.

## Backup restore

Backup tar entries include the manifest plus `objects/<key>` with MIME/SHA256 PAX
records and original mtime. **Do not blindly run `tar -x`:** S3 keys `foo` and
`foo/bar` can coexist but cannot both be plain extracted files. The restore mode
reads logical objects instead, verifies the full archive before any destination
write, refuses unsafe entry types/extra objects and checks the resulting dataset.
It neither contacts S3 nor needs S3 credentials.

```sh
/tmp/drive-migrate --mode=restore-backup --archive="$state/final.tar" \
  --destination-root=/mnt/separate-private-restore/root \
  --volume-id='<explicitly-initialized-restore-volume-uuid>'
```

Test restore into a **separate** private volume, never a running Drive root.

## Docker

```sh
# Build context is drive/, not api/.
docker build -f api/Dockerfile.migrate -t drive-migrate:candidate .
docker run --rm drive-migrate:candidate --help

# Offline verification needs no credentials/network. Match UID to artifact owner.
docker run --rm --network=none --read-only --user "$(id -u):$(id -g)" \
  --mount "type=bind,src=$HOME/.local/state/drive-migration,dst=/work,readonly" \
  drive-migrate:candidate --mode=verify-backup --archive=/work/final.tar
```

For S3 modes inject credential **environment names**, not inline values:
`--env MINIO_ENDPOINT --env MINIO_BUCKET --env MINIO_ACCESS_KEY --env MINIO_SECRET_KEY`.
Give only the necessary private artifact/NFS mounts and S3 network path. The image
is a nonroot, static Go binary in scratch with CA certificates, no shell. Build/run
natively on beta for ARM64 or use an explicitly configured multi-platform builder.
Do not mount Docker sockets, host root, arbitrary Secrets or production volumes
for a local test.

## Tests

From `drive/`:

```sh
GOTOOLCHAIN=go1.22.12 go test -race -count=1 ./...
GOTOOLCHAIN=go1.22.12 go vet ./...
```

Unit tests use synthetic Source/Destination fixtures and a loopback S3 HTTP mock;
assert no S3 writes, conditional ETag GETs, subsecond timestamp preservation,
copy/resume/fence/conflict behavior, backup corruption/truncation/trailing bytes,
actual restore/resume, private artifacts and CLI modes/credential sanitization.
Storage tests verify import checksum-before-publication and original mtime.

`TestNASMigrationAcceptance` is opt-in only. It requires the same independently
created UUID-labelled disposable parent/ownership token as storage's NFS fixture
and runs from UID/GID `10001:10001`; it never uses real S3 credentials/data. It
checks NFS copy/resume, exact metadata/hash, tar backup/restore, file-prefix
collisions and final sync. Never point this fixture at an existing application
root. Reboot/reclaim, power loss, cross-host fencing and network outage are not
proved by this scoped test.
