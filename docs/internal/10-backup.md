# Hoserva — Backup Strategy

## The scope question

*Is everything except the internal SQLite DB the user's problem, to be solved with third-party tools of their choice?*

**No — but the line is not where it first appears.** The right split is by **who is uniquely positioned to back it up correctly**, not by what is convenient.

| Data | Owner | Reason |
|---|---|---|
| **Hoserva config** (SQLite, generated configs, templates) | **Hoserva** | Only Hoserva knows its own schema and what a consistent snapshot looks like |
| **Container appdata** (on cache, outside parity) | **Hoserva** | Requires stopping containers in the right order; Hoserva is the only thing that knows which containers exist and which are running |
| **VM vdisks and domain definitions** (Phase 3.5, doc 14) | **Hoserva**, off by default | Only Hoserva knows which VMs exist, whether they're running, and that a vdisk must not be copied while its QEMU process holds it open — but a full vdisk copy is large enough that opting in should be a deliberate choice, not a silent default |
| **Bulk user data** (media, documents, the pool itself) | **The user** | Terabytes, wildly varying requirements, mature tools already exist |
| **Off-site replication** | **The user**, with Hoserva making it easy | rclone, restic, Borg, Duplicati all do this better than a NAS UI would |

The reasoning: **back up what only you can back up correctly, and get out of the way for everything else.**

A user can point restic at `/mnt/user/documents` without Hoserva's help. A user cannot easily produce a consistent appdata backup without knowing which of their 23 containers hold a SQLite database that must not be copied while running — and that is exactly the knowledge Hoserva has.

Building a general-purpose backup product inside a NAS is also the road to becoming a worse Duplicati. The three shipped backup jobs below are narrow, well-defined, and each exists because leaving it out creates a specific, known data-loss scenario.

---

## 1. Config backup

### What it contains

```
hoserva-config-<id>-2026-09-14T03-00-00.tar.zst
├── manifest.json            version, timestamp, host, checksums
├── state.db                 SQLite, consistent snapshot via VACUUM INTO
├── secrets.age              secret columns and stack .env files, under the backup passphrase (Q28, Q80)
├── identity.age             the onboarding recipient's own private identity, under the backup passphrase (Q80)
├── generated/               snapraid.conf, smb.conf, exports, mount units
├── stacks/                  every docker-compose.yml (their .env files are in secrets.age)
├── templates/               installed app templates
├── custom/                  user-owned config (smb.custom.conf etc.)
└── snapraid-content/        SnapRAID content files (optional, large)
```

For a destination that encrypts (below), what actually leaves the box is two files, not one:
`hoserva-config-<id>-2026-09-14T03-00-00.tar.zst.age` — the tree above, age-encrypted whole to the
onboarding recipient's public key — and a small sidecar,
`hoserva-config-<id>-2026-09-14T03-00-00.tar.zst.age.identity.age`, carrying the same private identity
as `identity.age` above, age-scrypt-encrypted under the backup passphrase on its own. They are
two files because age's own format refuses to combine a scrypt recipient with any other
recipient in one encrypted file — a restore recovers the identity from the sidecar with the
passphrase, then uses it to open the main archive; the box itself can also open the main archive
directly with its own copy of the identity, without the sidecar or the passphrase at all.

Not included: `metrics.db` and job logs (Q74) — history, not configuration.

### Consistency

**Never copy a live SQLite file.** Use `VACUUM INTO '<path>'` (or the backup API) to produce a consistent snapshot without stopping the daemon. A `cp` of a WAL-mode database mid-write yields a file that restores as corrupt, and does so intermittently, which is the worst kind of bug.

**Secrets** (SMTP passwords, notification tokens, API keys, Let's Encrypt keys) are handled in two layers (Q28), because the daemon must use them unattended — a 03:00 disk-failure alert cannot wait for someone to type a passphrase:

- **At runtime**, secret columns are encrypted with a machine key in `/etc/hoserva/secret.key` (root, `0600`, generated at install). This protects against the database file leaking — in a diagnostics bundle or a copied backup.
- **In backups**, the secrets section is re-encrypted under a **backup passphrase** the user sets during onboarding (doc 03 §1). The machine key itself is never included. A backup restored without the passphrase restores everything except secrets, and says so clearly.
- **Off-box, the whole archive is encrypted** with age before it leaves the box (Q80). An onboarding age recipient (X25519) is generated exactly once, at first start; the box keeps only its public half in the clear, and the matching private identity is kept at rest only wrapped under the machine key — never in the clear — so the daemon can use it unattended, the same reasoning as the machine key itself. Every destination other than a local path always encrypts; a local path may opt in.

  Encrypting to the recipient alone would strand every off-box archive the moment the box holding the private identity is lost, so the private identity also travels with every encrypted archive, itself wrapped under the backup passphrase (age's scrypt mode, `identity.age` above): a restore recovers it with only the passphrase, then uses it to open the archive, and can go on protecting future archives with the same recipient rather than starting a fresh one. **A restore of an *encrypted* archive needs the passphrase — there is no path around it, because the passphrase is what unlocks the identity that unlocks the archive.** This is stricter than the secrets-only case above: losing the passphrase for an off-box archive, with the box itself gone too, loses the whole archive, not only its secrets section. A *local, unencrypted* archive is unaffected — state.db and every plain file restore exactly as they do today, and only its embedded secrets.age is passphrase-gated. A remote destination can't be added until a backup passphrase is set.

### Schedule and retention

- Default: nightly, as the last step of the maintenance chain (Q30), plus automatically before every self-update and every array-topology change actually starts, including one that waited behind another topology job in the queue first
- Retention: keep 7 daily, 4 weekly, 6 monthly, pruned per destination
- Each backup is small (single-digit MB without content files), so retention is generous by default
- **Pre-change exemption (#401):** the pre-import safety backup (`POST /config/import`), the pre-update backup (the self-update path) and the pre-topology backup (below) mark their archive with that reason in its filename (`hoserva-config-<id>-<timestamp>.pre-import.tar.zst`, `.pre-update.`, `.pre-topology.`). Retention keeps the **5 most recent pre-change archives per destination** in addition to the daily/weekly/monthly tiers above, so a second same-day change never prunes the one copy of the state from before the first. A pre-change archive beyond that bound falls back to the ordinary tiers like any other archive. Archive filenames carry second resolution, with a numeric suffix on collision, so two backups in the same minute — or the same second — never overwrite each other; an archive written before this fix, with the original minute-only name and no reason marker, still parses as an archive (and is pruned only where retention below claims legacy archives).
- **Pre-topology backup (#406, #408):** every job in doc 01 §4's Topology class — disk format, disk add, disk remove, disk replace, data or parity disk upgrade, and pool remount — runs this backup at the moment it actually starts, not at the moment it is submitted: the two are the same instant for a job that starts immediately, but not for one that has to wait behind another Topology, Parity or Array-write job first (doc 01 §4's own mutual exclusion), since a snapshot taken at submit time would describe the configuration from before whatever is currently running finishes, not from just before this job's own change. `job.Scheduler.Submit` runs it itself for a job starting immediately, once the job's admission checks pass and before the job row is created — a submission admission refuses (maintenance mode, a database restore, an array not stopped for a data-disk upgrade, an on-battery hold) writes no archive, so refused retries never take the pre-change retention slots a real pre-import or pre-update archive relies on, and the admission checks run once more after the backup, in the same step that creates the job; `dispatch()` runs it for a queued job at the moment its turn comes, once whatever it was waiting behind has finished, and before its own run function is ever called — so a future Topology job type is covered by construction rather than needing its own call site either way. The behaviour matches the pre-update backup: an unconfigured backup (no `job.Scheduler.SetTopologyBackup` wired at all) skips it rather than failing. A backup that fails once wired refuses the change outright: for a job starting immediately, no job row is created at all and the caller sees why; for a queued job whose type has no cleanup to run (`job.AbortFunc`, doc 02 §4), it ends failed with the backup's error recorded, its run function is never called, and the rest of the queue keeps moving. A queued job cancelled while this backup is dispatching, or whose type does have one — a data-disk upgrade's Unwind, doc 02 §4 E3 — runs that cleanup first, before either outcome is recorded (doc 02 §4 invariant 3: `cancelled` and `failed` both mean the type's own cleanup already succeeded): it ends cancelled, or failed with the backup's error, only once the cleanup succeeds, and interrupted with the cleanup's own error instead if it does not, leaving the job resumable exactly as a plainly queued or interrupted job's own failed Cancel already does. A data-disk upgrade resumed at its releasing checkpoint is the one exception: past that release decision it is no longer cancellable and doc 02 §4 invariant 4 makes `succeeded` the only outcome left, so there is nothing for Unwind to undo — Cancel already refuses it outright, and a failed start-time backup for it skips Unwind entirely and leaves the job interrupted at its unchanged checkpoint with the backup's own error, resumable once the backup destination is fixed, never `failed`. This is orchestrated the same way as the pre-import and pre-update paths: `internal/backup` implements the archive itself, `internal/job` only calls it through the small `Run(ctx) error` interface it already used for the nightly chain's own last step.
- **The pool destination while the array is stopped (#409):** a destination path under the pool's own catch-all mount root (doc 02 §1, Q12) is never written to unless that mount root is confirmed mounted first, using the same stat-based check the pool's own mount lifecycle already relies on. With the array stopped — every data-disk upgrade's own pre-topology backup runs in exactly this state (doc 02 §4 E8, UR3) — the catch-all path is a bare, empty directory on the boot device's own root filesystem; without this check a backup would create the destination there and write into it, reporting success, and that archive would be silently hidden the moment the pool mounted back over it. A `backup.PoolWriteGate` closes that check's own remaining race: `job.ArraySequence.Stop` closes it, refusing every new pool-destination write and waiting for any already admitted to finish, **before** it unmounts anything — every stop path that unmounts, including the rollback of a failed `array start`, shares this one call — and `Start` re-opens it only once the pool is actually mounted again. A write refused this way is **skipped and recorded as such**, exactly like an unmounted pool, never an error on its own: the boot destination (a separate failure domain, unaffected by the array's own mount state) still captures the change. A run left with nothing actually written anywhere — every enabled destination skipped or itself failing — is a failed run: a pre-change backup (pre-import, pre-update, pre-topology) still fails closed and refuses the change it guards rather than reporting a snapshot that was never taken. This gate is not specific to the pre-topology backup: every `backup.Service.RunReason` caller — the pre-import safety backup, the pre-update backup, the nightly chain, and every pre-topology backup alike — shares the one instance `cmd/hoservad` wires into both `backup.Service` and `job.ArraySequence`, so none of them can race the unmount of a stop in progress within this daemon's lifetime: the gate refuses a pool write from the point `Stop` reaches its unmount steps — after running jobs and share mutations have drained, services have stopped and the storage-target gate has closed, immediately before the first unmount — until `array start` has mounted the pool again, and `Close` waits for any pool write admitted before that point. It does not survive a restart — `poolWriteGate := &backup.PoolWriteGate{}` starts open, and `RestorePersistedMaintenance` restores only the persisted array-stopped state, not the gate — so after a reboot with a persisted `array stop` (the usual stop → power off → swap disk → boot → upgrade sequence), the gate is open and the mount check above is the only thing keeping a write off the still-unmounted pool; the mount check is required for this case, not redundant with the gate. A data-disk upgrade's own pre-topology backup runs at `Submit` only once the job's UR3 admission (doc 02 §4) has passed — the stop sequence has completed and the pool is unmounted, so its pool destination is skipped (by the closed gate, or after a reboot by the mount check above); `ArraySequence.Stop`'s own `Close` call still waits for any pool write admitted before that point to finish before its unmount runs.

### Multi-destination — yes, from v1

This is the right call. A config backup stored only on the array it describes is not a backup, and a user whose boot device and array both fail is exactly who needs it most.

Destination types:

| Type | Notes |
|---|---|
| **Local path** | Anywhere on the pool or an external disk (Q72). Always available. |
| **SMB share** | Another NAS, a router USB disk. rclone has no NFS client, so an NFS share is a **local path** at wherever the host has mounted it. |
| **S3-compatible** | Backblaze B2, Wasabi, MinIO, Hetzner Storage Box |
| **SFTP / SSH** | A VPS, another homelab box |
| **WebDAV** | Nextcloud, pCloud, Koofr |
| **rclone remote** | If rclone is present, any of its 70+ backends via an existing remote name |

Implementation: **rclone as an optional dependency (`Recommends:`, Q41) covers everything except local paths.** Rather than writing six protocol clients, Hoserva writes the archive locally and shells out to `rclone copy` for remote destinations. This is the same "orchestrate, don't reinvent" principle as D1, and it means every backend rclone gains, Hoserva gains.

Per-destination configuration: enabled, schedule, retention, encryption (always on for remote destinations, optional for local paths — Q80), and a **Test connection** button that actually writes and reads back a file. An untested backup destination is decoration.

**How destinations work (#60).** Destinations are rows in SQLite (`backup_destinations`, D4) and are read on every backup run, so one added through `POST /backup/destinations` receives the next backup without a restart. A fresh install (or one upgraded from before destinations were stored) is seeded with Q40's two local destinations, boot and pool, only while no destination exists at all, both in one transaction so an interrupted start leaves none and the next start seeds both. `GET /backup/destinations`, `POST /backup/destinations`, `DELETE /backup/destinations/{id}` and `POST /backup/destinations/{id}/test` are the whole surface; `backup.PrepareDestination` is the one admission check the daemon and `cmd/mockapi` both run.

- **Types and options.** `local`, `smb`, `s3`, `sftp`, `webdav` and `rclone`. A remote type takes only rclone's own option names from a fixed list per type (for example `s3`: `access_key_id`, `provider`, `endpoint`, `region`, plus the `secret_access_key` credential; `sftp` verifies the host key only when `known_hosts_file` is given); anything else is refused, so a request cannot inject an arbitrary rclone option. `rclone` names a remote already configured in rclone's own config.
- **How rclone is invoked.** `rclone copy --immutable <local file> <remote>:<path>` with an argv — never `sh -c`. The remote is configured through `RCLONE_CONFIG_HOSERVADEST_*` environment variables, not argv, so a credential is not visible in the process list; a password is passed in rclone's obscured form (`rclone obscure -`, from stdin). After each upload a listing confirms the file is there at its written size. Every rclone call has its own deadline. Credentials are sealed under the machine key (Q28), never returned by the API (`hasSecrets` only) and included in `secrets.age` like the other database secrets.
- **rclone missing.** Adding a remote destination without rclone installed fails with `424 rclone_missing`, whose message carries the install command (`sudo apt install rclone`, Q41) — nothing is stored. Local destinations never need rclone.
- **Encryption.** A remote destination is always encrypted (Q80): `POST /backup/destinations` refuses one with `400 backup_passphrase_required` while no backup passphrase is set (`backup.ValidateRemoteDestination`), an explicit `encrypt: false` is refused, and `Service.Run` never writes an unencrypted archive to a remote even if a stored row says otherwise. The archive and its identity sidecar are what `rclone copy` uploads. The connection test's file is random bytes with no archive content, so it is not encrypted.
- **Test connection** writes a small file through the same write path a backup uses, reads it back, compares it and deletes it. A destination that cannot be reached is a `200` with `success: false` and the reason; a pool destination is not written to while the pool is unmounted (#409), so its test reports that instead of creating the directory on the boot device.
- **Retention** is applied per destination after each write — on a remote by listing it with `rclone lsjson` and removing with `rclone deletefile` (an archive's identity sidecar goes with it). It only ever removes archives this installation wrote. Every archive name carries a 12-hex installation id (`hoserva-config-<id>-<timestamp>…`), a hash of the onboarding recipient's public key, which is generated once per installation and never rotated (Q80); a destination shared with another installation (one storage box, one bucket prefix, one NFS mount) therefore keeps each installation's archives under that installation's own retention, and neither counts the other's towards its tiers. An archive without an id was written by an earlier release; it is only claimed on the boot and pool default destinations, local directories on this installation's own boot device and pool that earlier releases wrote to and nothing else does, and there it ages out under the ordinary tiers — on any other destination it is left alone, since it cannot be shown to be this installation's. Anything else in the directory is left alone as well.
- **Local directories.** A destination directory that does not exist is created private (`0700`); one that already exists keeps its mode and owner, since it may be a share other users rely on. Archives themselves are `0600`. `POST /backup/destinations` refuses a local path in a system location (`/etc`, `/usr`, `/boot`, `/proc`, `/sys`, `/dev`, `/run`, `/tmp` and `/var` themselves, and anything under the first seven), a symlink into one, and the daemon's own state and configuration directories (except the state directory's `backups` subdirectory, the boot default).
- **Partial failure.** Every enabled destination is attempted in turn. If at least one archive was written, a destination that failed is logged and the run succeeds; it shows up through its own last-successful-backup time. A run that wrote nowhere fails, as it always has.
- **Stale destinations.** Each destination stores `lastSuccessfulBackupAt`. An enabled destination that has gone 48 hours without a success — counted from its creation until its first — is stale, and the schedule tick that already runs every minute publishes `backup_destination_stale` once for it, again only after a success has ended the staleness and a later one begins. The check reads stored timestamps only; it never lists or reads a destination, so nothing on a timer walks a disk (Q13).

### Verification

A backup that has never been restored is a hypothesis. Hoserva therefore:

- Verifies the archive after every write (checksum, and confirm the SQLite snapshot opens and passes `PRAGMA integrity_check`)
- Runs a **monthly automated restore drill**: extract the newest backup into a temp directory, validate schema and checksums, discard. Failure raises a high-priority alert.
- Shows **last successful backup per destination** on the dashboard's attention row when any destination is stale

### Restore

Two paths:

**In-place restore** — from the UI, for rolling back a bad config change. Preview what will change, then apply.

**Bare-metal restore** — the important one. `hoserva config import <archive>` on a freshly installed system (the same command as in-place restore; it detects a fresh install and runs this flow):

1. Read the manifest and check compatibility: an archive from an older Hoserva is upgraded by the same schema-migration runner as a normal upgrade (doc 01 §4, D16); an archive from a newer Hoserva is refused
2. Scan attached disks and match against the recorded serials
3. **Show the mapping and require confirmation** — disks may have moved, been replaced, or be absent
4. Restore the DB, regenerate configs, remount the pool
5. Restore stacks and offer to start containers
6. Report anything that could not be restored, explicitly

This is the "OS is disposable" claim from doc 01 §6 made real, and doc 06 §4 makes it a routine CI test rather than an assumption.

---

## 2. Appdata backup

### Why it must ship

Cache-only data sits outside parity by design. Without this job, the cache is a single point of failure for every service on the box — and the failure is silent until the SSD dies.

Unraid's community solved this with a third-party plugin that became near-universal. That is strong evidence it belongs in the core rather than left to the user.

### How it works

```
for each container in scope:
  if stop_before_backup:  stop it, record order
archive /mnt/cache/appdata → destination (tar.zst, or per-container archives)
restart stopped containers in reverse order
verify archive
prune per retention
```

### Requirements

**Per-container stop policy.** Databases (Postgres, MariaDB, anything with a SQLite file) must be stopped or quiesced. Stateless containers need not be. Default to stopping, allow per-container opt-out, and **flag known database images automatically** so a user who opts out of stopping Postgres gets told why that is a bad idea.

**Minimise downtime.** Stop → archive → start, with the archive written locally first and uploaded afterwards, so containers are down for the copy and not for a slow upload.

**Per-container archives**, not one monolith. Restoring one broken service should not require unpacking 80 GB.

**Same destination system** as config backup — multi-destination, rclone-backed, verified, and encrypted off-box (Q80).

**Restore is per-container**, with a preview of what will be overwritten and an automatic pre-restore snapshot of the current state.

### Schedule

Weekly by default, plus **automatically before any container update** (doc 04 §6) — the single most valuable moment to have one, and the thing most often missing when someone needs it.

### How it works (#61)

`backup.AppdataService` runs the backup and the restore over the same destinations, encryption and pool-write gate the config backup uses (§1), and over `container.Lifecycle` for the Docker Engine: it never talks to Docker itself, and every stop and start goes through the lifecycle actions.

- **Scope.** A container is in scope when one of its bind mounts, symbolic links resolved, is a directory strictly inside the appdata location (the cache disk's `appdata` directory, the same root container removal deletes under); a mount of the location itself, or of a file, is not appdata. A cache-less array has no appdata location, and the run says so and does nothing. `GET /appdata/backup` lists the containers in scope.
- **Policy.** Each container is stopped and included unless the operator says otherwise, stored per container name in `appdata_backup_containers` (D4) through `PUT /appdata/backup/containers/{name}`. Known database images — the image's last path segment, so `lscr.io/linuxserver/mariadb` and `bitnami/postgresql:16` both match, against a fixed list (Postgres, MariaDB, MySQL, MongoDB, Redis and similar) in `backup.IsDatabaseImage` — carry `databaseImage`, and one that is included but not stopped carries `warning` in the listing and in the `PUT` response, is named in the job output, and is recorded in its archive header. Opting out is allowed; the flag is the mechanism, not a refusal. A file-based database in an unrecognised image is not detected: stopping stays the default for everything.
- **Run.** `POST /appdata/backup` queues an `appdata_backup` job (service class, scoped to the containers it covers so a recreate of one queues behind it), refused with `array_stopped` while the array is stopped or its storage is not ready — with the cache disk unmounted the appdata directories are empty, and an archive of them would report success. In order: every running, to-be-stopped container is stopped (by name, in name order); every included container is archived to a staging directory on the cache disk, next to the appdata location, one archive each; the stopped containers are started again in reverse order; only then is each archive verified and written to the destinations, so a container is down for the copy and not for a slow upload. A container that would not stop is not archived, since its copy would not be consistent; the run continues with the others and fails at the end naming it. Each start has its own deadline that a cancelled job does not cut short. Every container the run attempted to stop is started again on every path, including a cancel and a failed archive.
- **Interruption.** Before the first stop the run adds the names it is about to stop to `appdata-backup-stopped.json` in the state directory, keeping any name an earlier run left there, and removes each name it started; one whose start failed, or that an earlier run left stopped, stays in it until the schedule's tick starts it. The file is removed when no name is left. The schedule's minute tick reads that one small file and, while no backup or restore is running in this process and the array is running, starts what it names (`AppdataService.RecoverStopped`) — after a daemon restart mid-backup, an array stop that cut a run short, or an engine that refused a start. It never reads a data disk.
- **Archive.** `hoserva-appdata-<installation>-<container>-<timestamp>[.pre-restore].tar.zst`, with the same age encryption and identity sidecar as a config archive where a destination encrypts (a remote always does). A tar.zst holds a header (container, image, whether it was stopped, the directories archived), one `data/<n>/` tree per directory with modes, owners, times and symbolic links, and a trailer with the file count, byte count and a SHA-256 over every file's content. Verification is one sequential read against the trailer; a truncated or altered archive has none or fails it. Sockets, devices and pipes are skipped and counted; a file that shrank while its copy was in progress is padded, one that grew is cut at the size it had when its copy began, and both are counted (as is one that vanished), so an archive of a container that was not stopped says how many files changed size. Growth is noticed when the copy of that file ends, and a file rewritten in place at the same size is not detected.
- **Destinations.** Every enabled destination except the boot device's default one (Q40): appdata archives are far larger than config archives and that disk is small. A run fails when any enabled destination could not be written, naming it; the archives already written stay. Unlike a config backup it does not record a success on the destination, so an appdata backup never hides a destination whose config backups have stopped from the stale-destination alert.
- **Retention.** Each destination's daily/weekly/monthly retention, applied per container and only to this installation's archives of that container. The pre-restore snapshots (below) are kept apart: the 5 newest per container survive, and they never take an ordinary backup's tier slot.
- **Restore.** `POST /appdata/backup/restore` (`confirm: true` required) queues an `appdata_restore` job for one container, one archive on one destination; `GET /appdata/backup/archives` lists what can be restored, and a destination that could not be listed is reported, not read as empty. The job fetches the archive, decrypts it with the onboarding identity and verifies it end to end, and checks that every directory it names is exactly where the backup would have put it — absolute, already resolved, strictly inside the appdata location — before anything is changed. Then, if the container is running, it is stopped; **a snapshot of the appdata about to be replaced is written to the destinations and the restore refuses, changing nothing, unless at least one of them holds it**; the archive is unpacked next to each live directory, flushed to disk, and swapped in by renames, so a failure while unpacking or swapping leaves the live appdata as it was (a swap that fails part way puts back what it already swapped); and the container is started again on every path. Extraction creates only directories, files and symbolic links, and refuses an entry that would be written outside its tree or through a link the archive itself created. The restore job is not cancellable: a cancel between its steps is the way to leave the appdata half replaced.
- **Restore preview.** `POST /appdata/backup/restore/preview` takes the same container, archive and destination as the restore (no `confirm`, since it changes nothing) and queues an `appdata_restore_preview` job, in the service class scoped to the container like the restore, so the scheduler runs it behind a backup or restore of the same container and beside those of others; it never takes the lock those two share, so a scheduled backup is queued or runs, never refused, while a preview is reading a large remote archive. `GET /appdata/backup/restore/preview/{jobId}` returns the result once the job has succeeded, and says why there is none while it is queued or running (409 `appdata_preview_not_ready`), when it failed, was cancelled or was interrupted (409 `appdata_preview_failed`), or when the result is no longer held (404 `appdata_preview_gone`). The daemon keeps the sixteen newest results in memory only, so a result is gone after a restart and after sixteen newer previews; a preview is cheap to run again and nothing else depends on it. The request is checked like the restore's before the job is queued (array running, the archive's name, the destination, the archive being on it), and the job fetches, decrypts and verifies the archive with the code the restore uses (`fetchVerifiedAppdata`), so it fails with the restore's own error for a corrupt archive, another container's, or a directory outside the appdata location. Per archived directory (named relative to the appdata location, never by its mount path) it compares the archive with the live directory and returns, for each of three groups, a file count, a byte total and a sample of at most 20 paths in path order, relative to the directory: `replaced` (in both; the bytes are the live files that would be lost), `added` (only in the archive; the archive's bytes) and `removed` (only live, which the restore deletes because it swaps the directory as a whole; the live bytes). Directories are not counted, symbolic links and other entries are, and a file is `replaced` whether or not its content differs, since telling would mean reading every live file. It also refuses the entry layouts extraction refuses, by building the tree extraction would build in memory and checking each entry against it: a path leaving its tree, a link type other than a symbolic one, an entry whose place an earlier one already took (a path twice, a directory listed twice, a file where a directory is), and an entry written below a file, below a directory the archive has not listed yet, or through a link that leaves the tree. Files below a link that stays in the tree are listed where extraction puts them. A restore that would fail at unpacking because of its layout is therefore refused before anything is stopped; a disk that fails while unpacking is not predicted. It stops no container and writes nothing under the appdata location; the fetched archive is staged in a directory of its own next to the location, apart from the backup and restore staging so neither clears the other's, and removed when the job ends, on success and failure (one a dead daemon left is removed by the next preview). It walks the live appdata only when asked (Q13), never on a timer. It is cancellable: a cancel stops the fetch, the reading of the archive and the walk of the live directories, but not the decryption or the verification read that follow the fetch, which take no context and run to their end first.
- **Schedule.** The `appdata_backup` schedule is on and weekly (Sunday 04:00) by default, and runs through the daemon's one loop for separately scheduled jobs (`tickOtherJobs`), which claims a window before starting the job, so a restart in the same window cannot start it twice. A schedule saved or seeded mid-week waits for its next window; a window missed while the daemon was down still runs once. With nothing in scope the job succeeds having done nothing. A scheduled or manual backup that fails, or that cannot even be started (the array is stopped), publishes `appdata_backup_failed`.
- **Not built yet.** The automatic pre-update snapshot waits for the container update path (doc 04 §6); there is no per-destination choice of what it takes (`/settings/backup`'s appdata section, #64).

---

## 3. Pool data backup — what Hoserva does and does not do

**Does not:** implement deduplication, incremental block-level backup, versioning, or cloud sync for bulk data. Those are solved problems with better tools.

**Does:**

- **Be honest.** The docs and the UI state plainly that parity is not a backup — it protects against disk failure, not against deletion, ransomware, corruption, fire, or theft. A surprising number of NAS users believe otherwise, and the `concepts/parity-is-not-backup` and `concepts/what-happens-when-a-disk-dies` pages (doc 05 §7) should say so directly.
- **Ship curated backup containers** in the catalog with pool-aware defaults: Duplicati, restic/Backrest, Kopia, Borgmatic, rclone. Pre-configured to see the pool, store their own config on cache, and not accidentally back up the appdata they live in.
- **Surface backup status** — if a known backup container is installed, show its last-run state on the dashboard. Shallow integration, high value: the common failure is a backup job that silently stopped working months ago.
- **Provide snapshot-friendly primitives.** `snapraid` content and parity give point-in-time recovery for accidental deletion (doc 02 §2) — **but only until the next sync**, which makes the deletion permanent. That is the nightly chain by default, and the threshold guard only holds a sync for *mass* deletions. The guided fix flow (doc 03 §3.5) is the recovery path, and it should be documented as "undelete" — with that time limit stated in the first sentence, not a footnote.

### The stated posture, for the docs

> Hoserva protects you from a disk dying. It does not protect you from you, from ransomware, or from your house burning down. Parity is redundancy, not backup. For anything you cannot replace, keep a copy somewhere else — the Apps catalog has good tools for that, and the Backup guide walks through setting one up.

Saying this clearly costs nothing and prevents the single most damaging category of user disappointment.

---

## 4. What gets backed up by default on a fresh install

The onboarding flow should end with backups configured, not leave it as an exercise:

| Job | Default | Destination default |
|---|---|---|
| Config backup | **On**, nightly | **Two local destinations** — the boot device (`/var/lib/hoserva/backups`) and a pool path, covering the two separate failure domains in doc 02 §6 — plus a prompt to add an off-box destination (Q40) |
| Appdata backup | **On** once the first container is installed, weekly | Local path on the pool |
| Pre-update appdata snapshot | **On** | Local |
| Restore drill | **On**, monthly | n/a |
| Pool data backup | Off | User's choice, with a pointer to the guide |
| VM backup (Phase 3.5) | Off | Local path on the pool, once enabled |

Defaults that protect people who never open the settings page are worth more than options for people who do.

---

## 5. VM backup (Phase 3.5, doc 14)

VM vdisks are large files living on cache or array (doc 14 §2), covered by neither config backup (too big, not what it's for) nor appdata backup (scoped to `/mnt/cache/appdata`, not `/mnt/user/domains`). Same reasoning as §2, applied to a different kind of file.

### What ships

- **Domain backup.** On schedule or on demand, `hoserva vm backup <name>` stops the VM, copies its domain XML and vdisk(s) to a destination through the same multi-destination, rclone-backed system as config and appdata backup (§1, §2), verifies, restarts the VM. Live, non-disruptive snapshotting (a libvirt external snapshot while the guest quiesces) is a post-1.0 refinement, not required for Phase 3.5.
- **Not continuous, not incremental.** A full vdisk copy on a schedule — the same deliberately narrow posture as appdata backup's per-container archives (§2), consistent with doc 00 §4's refusal to build general-purpose backup tooling.
- **Snapshots are not backup.** The libvirt/qcow2 internal snapshots doc 03 §11.3 offers are a convenience for undoing a recent change inside the guest — they live on the same disk as the vdisk they snapshot, and are lost with it. Stated plainly in the UI, the same posture as §3's "parity is not backup."

### Schedule

**Off by default** — a full VM image can be large, so enabling this is a deliberate choice, not an assumption. Weekly when enabled, plus automatically before a VM's domain definition changes materially (vCPU/RAM/passthrough edits), the same "back up before the risky moment" pattern as the appdata backup's pre-update snapshot (§2).
