# Подготовка Drive к beta + NAS

Статус: неопубликованный кандидат Drive в `feat/nas-filesystem-storage`.
Отдельно разрешённые операционные изменения NAS описаны ниже.
Commit/push/PR и deployment не выполнялись. Рабочие Drive/MinIO и существующие
данные NAS не менялись. По отдельным разрешениям исправлен NSM startup beta/NAS и
пройдена ограниченная scratch NFS-приёмка; временные NAS-каталоги/mount удалены.
Независимое static review и повторная проверка двух исправлений выполнены;
production migration не выполнялась, эксплуатационные gates остаются.

## Реализация

- `STORAGE_BACKEND=minio` остаётся значением по умолчанию. Неизвестный backend
  завершает запуск с ошибкой; fallback между filesystem и MinIO отсутствует.
- HTTP/shares/torrent consumers не зависят от типов MinIO SDK. Сохраняются
  `users/{auth_user_id}/`, `_drive/shares.json`, Auth и API.
- Linux filesystem backend требует существующий NFS root, canonical ненулевой
  volume UUID, marker и исправный эксклюзивный writer lock. Local/nolock mounts
  не разрешены; mount options должны подтверждать `local_lock=none`.
- Root открывается через directory fds без symlink traversal. Действия проверяют
  mount ID, inode, UUID marker и идентичность lockfile. Потеря/замена mount не
  разрешает запись в локальный каталог beta. Автоматического mkdir root нет.
- Объект публикуется одним атомарным rename после fsync; последующий fsync
  каталога обязателен. Неудачный upload не заменяет предыдущий объект.
- Полное чтение проверяет SHA256; повреждённый последний chunk не отдаётся как
  завершённый файл. Частичный Range не доказывает целостность всего объекта.
- File/directory moves копируют перед удалением источника. Directory move не
  атомарен как целое; сбой может оставить дубликаты/частично скопированную цель.
  Перекрывающиеся деревья (предок/потомок) запрещены до каких-либо изменений.
- Share metadata при outage, malformed JSON или checksum failure не сбрасывается
  в пустой список. Только missing-object error разрешает первоначальный список.
- `/healthz` не зависит от NAS; `/readyz` возвращает общий статус storage без
  деталей. Одновременно допускается один readiness worker: hard NFS syscall,
  переживший HTTP/context timeout, не создаёт новые зависшие workers каждым probe.

## Формат NAS (кандидат v1)

Это **объектный контейнер**, а не каталог обычных документов для прямого открытия
на NAS. Drive API отдаёт исходные байты без служебного заголовка.

```text
<root>/
  .drive-volume-id
  .drive-lock
  objects/users/42/example.txt/.drive-object
  objects/_drive/shares.json/.drive-object
```

Каждый `.drive-object` содержит 8192-byte header block: magic `DRIVEFS1`, big-endian
uint32 длины JSON, JSON с volume/key/MIME/size/modified/SHA256 и padding; затем
неизменённые bytes документа. MIME и данные публикуются совместно, без пары
несогласованных sidecar-файлов. Поддерживаются S3-ключи `foo` и `foo/bar` одновременно.

Physical key components обычно совпадают с logical names. В начале компонента
`~` экранирует `~`, `.drive-object`, `.drive-tmp-*` и `.nfs*`. Служебные regular
`.drive-tmp-*` и Linux NFS silly files (`.nfs` + ровно 24 lowercase hex digits)
игнорируются при listing/walk, но не удаляются приложением как garbage. Symlinks
под этими именами не считаются безопасными temporary-файлами.
Ограничения: canonical UTF-8 key до 1024 bytes; physical component до 255 bytes;
MIME до 1024 bytes. До миграции выявить все неподдерживаемые/noncanonical ключи и
metadata, а не нормализовать/пропускать их молча. Формат ещё не зафиксирован выпуском.

## Конфигурация и режимы API binary

| Настройка              | Значение по умолчанию | Назначение                                        |
| ---------------------- | --------------------- | ------------------------------------------------- |
| `STORAGE_BACKEND`      | `minio`               | `minio` или явно выбранный `filesystem`           |
| `FILESYSTEM_ROOT`      | пусто                 | существующий выделенный NFS-каталог; только Linux |
| `FILESYSTEM_VOLUME_ID` | пусто                 | заранее выбранный canonical UUID, не UUID nil     |

Обычный запуск требует уже инициализированный каталог. Bootstrap — отдельная
явная операция; не выполнять на production до завершения перечисленных gates.

```sh
# Только после подготовки private NFS share/mount и выделенного пустого root.
# UUID выбирается один раз и затем хранится в конфигурации/backup inventory.
export STORAGE_BACKEND=filesystem
export FILESYSTEM_ROOT=/mnt/drive-nas/dedicated-root
export FILESYSTEM_VOLUME_ID='<canonical-nonzero-uuid>'

go build -o /tmp/drive-api ./api/cmd/server
/tmp/drive-api --help
/tmp/drive-api --init-filesystem  # init и exit; HTTP не запускается
/tmp/drive-api                    # обычный API server; Auth env по README
```

Существующий Dockerfile API содержит тот же binary и initialization mode:

```sh
docker build -f api/Dockerfile -t drive-api:nas-candidate .
# src обязан уже быть корректным NFS root, а не пустым локальным mountpoint.
docker run --rm --mount type=bind,src=/mnt/drive-nas/dedicated-root,dst=/data \
  -e STORAGE_BACKEND=filesystem -e FILESYSTEM_ROOT=/data \
  -e FILESYSTEM_VOLUME_ID="$FILESYSTEM_VOLUME_ID" \
  drive-api:nas-candidate --init-filesystem
```

Эта инструкция не подтверждает сборку/приёмку runtime image. Для обычного Docker
API запуска также нужны существующие Auth env/network и torrent work storage.
После частичного bootstrap root не очищать автоматически: init требует пустоту
и отказывается автоматически усыновлять неизвестные данные/восстанавливать marker.

## Реальная NFS-проверка beta/NAS: scoped PASS

Beta — `aarch64`, direct link `enP4p65s0 = 192.168.254.1/30`. По явным разрешениям
`rpc-statd.service` на beta запущен, затем диагностирован NAS-side NSM. NAS — Linux
3.2.26 armv7l; `rpc.statd` был установлен, но отключён через `NEED_STATD=no`.
До исправления NLM возвращал `ENOLCK`, хотя `100021/nlockmgr` был зарегистрирован.
Backend корректно отказывался работать; `nolock` fallback не использовался.

Административный доступ предоставил оператор через одноразовый SSH ControlMaster,
без передачи пароля агенту и без постоянных новых SSH keys. Legacy `ssh-rsa`
разрешался только этому соединению; host key checking не отключалось. NAS NSM
запущен штатным `start-stop-daemon`, без restart NFS/kernel workers или NAS.
После запуска `100024/status` зарегистрирован TCP/UDP на NAS и beta; lock работает.

Сохранён **единственный config delta** `/etc/default/nfs-common`:
`NEED_STATD=no` → `NEED_STATD=yes`; IDMAPD/GSSD не включались. Перед заменой
проверен SHA256 baseline, сохранён backup с исходными правами/владельцем:

```text
/etc/default/nfs-common.drive-before-nsm-271978d0-cae5-4504-8758-3ed7d39e37b7
original: d6b444d5e7e3bf2b13dea38871c7c12eeb2792fc59a05fe4c91e0cdaf0387d54
updated:  179cfc6d92fdf73efed2fb61c77d82108ea4d95ef3379cbc8ed9155fa9ffadbe
```

Backup/staging file и parent directory синхронизированы, config заменён атомарно.
Штатный `/etc/rc2.d/S20nfs-common` уже существовал; текущий NAS runlevel — 2.
Фактический reboot и сохранение настроек после firmware regeneration **не проверялись**.
На beta persistent enable/config и firewall не менялись.

Тест после запуска NSM обнаружил source bug: при overwrite открытого объекта Linux
NFS создаёт `.nfs…` silly file, который listing ошибочно считал посторонним node.
Исправлено точечным распознаванием regular `.nfs` + 24 hex; symlinks/неизвестные
lookalikes не разрешены. `.nfs*` logical components экранируются, kernel-owned
files не удаляются. Добавлены unit regressions и native pinned-reader tests.

Последний ARM64 binary собран Go 1.22.12 и выполнен на beta от UID/GID `10001:10001`.
Opt-in `TestNASFilesystemAcceptance` разрешает только UUID-labelled scratch parent
с заранее созданным ownership marker; создаёт/удаляет собственный volume.
Временный mount `192.168.254.2:/nfs/Public`:
`vers=3,proto=tcp,hard,timeo=50,retrans=2,local_lock=none,nosuid,nodev,noexec`.

**PASS, 1.90 s:** initialization/directory fsync, отказ второму процессу и получение
lock после освобождения, SHA256/MIME, byte/suffix Range, failed-overwrite preservation,
20 atomic replacements с четырьмя readers, открытый старый reader при overwrite,
listing и чтение при delete открытого объекта, directory/file move, close/reopen,
delete/list. Последний log: `/tmp/drive-nfs-source.XfdR6L/nfs.log` (временный локальный
артефакт, не backup). Предыдущий green run — `/tmp/drive-nfs-source.nPY3LC/nfs.log`.

Собственные NAS scratch folders и beta temp mounts/binaries удаляются harness.
Это **не** sustained benchmark, cross-host lock/fencing test, server reboot/reclaim,
power-loss durability, network outage, private-share security или production rollout.
Существующие NAS данные, Public share/export settings и рабочие Drive/MinIO не менялись.
Одноразовый административный SSH ControlMaster после проверки закрыт; credentials
агенту не передавались и постоянные новые SSH keys не устанавливались.

## Private export / empty root prepared (not deployed)

По разрешению оператора создан **новый** `/DataVolume/drive-private` на большом
разделе `/dev/sda4`. UID/GID 10001 были свободны; только новый каталог получил
owner `10001:10001`, `0700`. Public/SMB anonymous UID `65534:1000` проверен локально:
read/write/traverse denied. Существующие Public данные/права не менялись.

Добавлен `/etc/exports.d/drive-private.exports`, только beta `192.168.254.1`,
`rw,sync,secure,root_squash,all_squash,anonuid=10001,anongid=10001,no_subtree_check`.
Активация и проверка парсинга выполнялись только для этого host/path; глобальный
`exportfs -ra` и restart NFS не выполнялись. Fragment SHA256:
`810bb747b73ccb069866a77ef7aafa0ad569bd7d6938485dadad1ef2a6e5bfbc`.
Исходный `/etc/exports` сохранил SHA256
`8d4514e4eb49137ff04e56395c1a0e5cea13766771b2ff389f7faaa1e984de3d`.
Kernel options подтверждают `sync,secure,secure_locks`; старые NFS worker PIDs те же.

Оба native tests повторены на private export:
`/tmp/drive-private-nfs-check.lIsBYc/storage.log` — PASS 1.70 s;
`/tmp/drive-private-nfs-check.lIsBYc/migration.log` — PASS 0.89 s.
Все synthetic fixtures и временные NFS mounts удалены.

Private root **уже инициализирован** явным `drive-api --init-filesystem` с UUID
`f282b27a-7711-4dc4-b2c9-b66db7acf79d`. Контейнер ARM64 запускался UID/GID 10001,
network-none, read-only root filesystem и только writable NFS bind. Log:
`/tmp/drive-private-root-init.log`. Проверены control files/objects directory,
owner/modes/UUID и **ноль импортированных объектов**. Не повторять init и не
очищать root. Рабочие S3-объекты ещё не копировались. Временный NAS admin
ControlMaster после завершения подготовки закрыт; для штатного copy по NFS
дальше не требуется NAS root login, пока review не потребует изменения настроек.

Reboot/firmware persistence, отказ по реальному другому IP, полный SMB security
acceptance ещё не доказаны. Static source review описано ниже; это не разрешение
на production. До оставшихся release gates не применять Deployment и не переключать данные.

## Перед production

1. Private NFS export/новый root созданы и scoped-tested выше. Независимо проверить
   host trust boundary/SMB mappings и конфигурацию: Public/0777 не является storage
   для Drive. Не расширять ACL и не делать mass chown существующих данных.
2. Согласованный NFSv3 client runtime beta, remote locking/callback/recovery,
   directory fsync, restart и outage acceptance на собственных scratch objects.
   NFS hard mount может блокировать syscall после context cancellation; bounded
   readiness не превращает такой mount в быстрый cancellable I/O и не даёт failover.
3. Static independent review выполнено; исходные P1/P2 исправлены и прошли
   scoped follow-up GO. Review не является эксплуатационной приёмкой.
4. ARM64 API/UI/migration containers собраны на beta; aria2/nginx и фактический
   RTF→PDF через preview handler проверены. Точный runtime artifact зеркалирован;
   default amd64 CI cross-build API против private remote digest прошёл (см. ниже).
5. App-scoped manifests: beta placement, NAS PV/PVC, beta-local torrent storage,
   one writer/Recreate, readiness. Общий MinIO не удалять/переносить.
6. Manifest-bound inventory/backup/copy/sync/verify/restore реализованы, включая
   сохранение точного MIME/mtime и checksum-before-publish import. Предварительная
   read-only production инвентаризация и защищённый backup: 1 объект / 285 bytes.
   Это не final frozen snapshot; после реального writer fence переснять и сверить.
7. Публикация Drive feature → dev → master; затем доступный удалённо parent gitlink
   и два parent PR. Parent CI/Kubernetes не редактировать в чужой dirty ветке.
8. Только после gates: копирование, короткий freeze записи, final sync/hash verify,
   переключение и smoke checks. Оригинальный S3 сохранить для отката. Изменения
   после переключения требуют обратной синхронизации для безопасного rollback.

## Migration / ARM64 preparation (not deployed)

[Migration CLI](../api/cmd/migrate/README.md): Linux, private 0600 manifests/archives,
read-only S3 interface, no bucket creation/source writes, conditional ETag GETs,
subsecond LIST timestamp preservation, exact checksum/MIME/mtime imports. Unknown
or conflicting destination versions are refused before mutation. Final sync
requires a previous verified manifest and explicit acknowledgement of a real
source fence; the flag itself does not stop writers. Backup restores are verified
before touching destination and never extract arbitrary tar paths to host paths.

`TestNASMigrationAcceptance` прошёл на beta/NFS (synthetic source, UID/GID 10001):
copy/resume, точные SHA256/MIME/subsecond mtime, backup/restore, file-prefix collisions,
shares и final sync. Log: `/tmp/drive-nfs-migration.YcH6xT/nfs.log`, PASS 1.40 s.

Реальные S3 credentials были внедрены только в память процесса из существующего
операторского механизма; ни значения, ни filenames не выведены. Предварительные
private artifacts вне Git:
`~/.local/state/drive-nas-migration/c3884b71-9ab6-44db-873b-ed7caf808934/`.
Backup проверен на gamma и после переноса в этот независимый operator state path.
Никаких записей/удалений в S3, остановки API или переключения пока не было.

Native beta build tags используют suffix
`nas-7083a99d-6b42-4a89-a432-b42c39572b67`. API/migration/UI подтверждены как
linux/arm64; API RTF→PDF acceptance прошёл также с cross-build Dockerfile и local
runtime override. Исправлены writable home/UID:GID 10001 и per-preview office
profile. Frontend ARM Dockerfile сохраняет Node 22.23.4, использует multiarch index
с теми же amd64/arm64 child digests и `npm ci --include=optional`; package/lock
versions не менялись. Проверенный runtime-only OCI artifact опубликован без
изменения OCI index в `10.200.73.2:5000/vendor/drive-runtime-arm64`:
`sha256:e0ee86c6c976140ac565645c9464c642f10e2fcfcf56ee9f2d6f3394b14fc2ac`.
Remote manifest bytes повторно проверены по SHA256, ARM64 entry присутствует.
Штатный `drive_images.build_one` на gamma/x86_64 собрал API с default AMD64 Go
builder и этим remote runtime digest; snapshot и ARM64 image checks прошли.
Build ID `71af71be-16c8-4754-bdcc-3c5cc307fe66`; log на gamma:
`/tmp/drive-ci-cross-71af71be-16c8-4754-bdcc-3c5cc307fe66/reports/build-drive-api-arm64.log`.
Существующие registry credentials нужны уже на build stage, не только push.
Они подавались в памяти, scoped config был на tmpfs; global QEMU/binfmt,
Docker config, Jenkins credentials/config и monitor/generic builders не менялись.

Parent подготовка из dev изолирована в `/tmp/public-page-drive-nas-deployment`,
ветка `feat/drive-nas-deployment`. Canonical child code — здесь; parent test copy
на отдельной `feat/nas-deployment-source-checks`. Чужие parent changes не переносились.
Подготовлены новые Retain NAS/work PV/PVC, beta placement, filesystem config,
readiness, isolated Drive ARM cache family и operator-fenced cutover gate.
`ci/image_build.py`/monitor ARM policy/Jenkinsfile не менялись. Ни один manifest не
применён; parent pin пока не обновлялся. Runbook — parent `docs/DRIVE-NAS-CUTOVER.md`.

## Independent review outcome

По прямому запросу оператора открыты отдельные kitty/Pi sessions Astra/high с
read-only tools, без bash/MCP/изменений/production доступа. Первое review дало
NO-GO: P1 — target ARM64 native Node require при default AMD64 builder; P2 —
checkpoint UUID проверялся против live ConfigMap вместо desired supporting docs.

P1 исправлен удалением hardcoded binding require; реальная UI сборка прошла на
default AMD64 и native ARM64 builders. P2 исправлен разрешением фактического
configMapKeyRef целевого API из prepared supporting.json, с exact name/key/namespace,
canonical nonzero UUID и отказом неоднозначным/optional/literal references.
Добавлены mocked-main regressions. Parent CI теперь 108 PASS, Kubernetes 47 PASS.

Follow-up reviewer закрыл оба замечания, дал scoped **GO для исправлений**, новых
конкретных P0/P1/P2 в этих путях не нашёл. Сам команды не выполнял; production
приёмки не заявлял. Reports (temporary local artifacts):
`/tmp/drive-nas-independent-review.yBYLCJ/phase1-report.md` и `phase2-report.md`.
Дополнительное incremental Astra/high review scoped private-runtime build login
дало GO, новых конкретных P0/P1/P2 не найдено. Тесты проверяют password-stdin,
0700, сохранение только plugin paths, сохранность новых auths в build/inspect,
одинаковый scoped config, cleanup после ошибок и побайтовую неизменность исходного
config. Report: `/tmp/drive-nas-independent-review.yBYLCJ/phase3-report.md`.
Runtime mirror и штатный default API cross-build выполнены; Git source release
идёт по двум последовательным PR, application cutover ещё не выполнялся.

## Локальные проверки

`GOTOOLCHAIN=go1.22.12 go test -race -count=1 ./...`, `go vet ./...`,
`git diff --check`. Go 1.22.12 также собрал статические Linux amd64/arm64 binaries;
`--help` выполнен на amd64. Darwin arm64 прошёл только compile check (filesystem
mode на этой ОС явно unsupported, MinIO остаётся доступным).
Тесты используют только собственные local temp directories через неэкспортируемый
fixture constructor; runtime не может отключить NFS requirement переменной env.
Проверяются атомарные metadata/data replacements, interrupted/size-mismatch uploads,
Range, кириллица/reserved names, file-prefix collisions, directories/moves, symlink/
forged-name escape, mount/marker/lock replacement, checksum corruption, foreign-volume
adoption, default config/CLI modes, anonymous readiness и bounded blocked probe.

Форматирование только изменённых Go/Markdown-файлов: gofmt/goimports/Prettier.
Реальные S3 credentials и production объекты не используются. Обычный локальный
запуск пропускает opt-in NAS/runtime tests; scratch NAS writes выполнялись только в
отдельно разрешённых попытках выше. Production перенос, private-share security,
NAS reboot/reclaim/outage ещё не проверены. Default amd64 CI build против
mirrored runtime прошёл. Native scoped runtime/locking/migration tests прошли также на новом
private export; scoped static review выполнено, но оставшиеся production gates обязательны.
