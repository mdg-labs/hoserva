# Hoserva — Backup Strategy

## The scope question

*Is everything except the internal SQLite DB the user's problem, to be solved with third-party tools of their choice?*

**No — but the line is not where it first appears.** The right split is by **who is uniquely positioned to back it up correctly**, not by what is convenient.

| Data | Owner | Reason |
|---|---|---|
| **Hoserva config** (SQLite, generated configs, templates) | **Hoserva** | Only Hoserva knows its own schema and what a consistent snapshot looks like |
| **Container appdata** (on cache, outside parity) | **Hoserva** | Requires stopping containers in the right order; Hoserva is the only thing that knows which containers exist and which are running |
| **Bulk user data** (media, documents, the pool itself) | **The user** | Terabytes, wildly varying requirements, mature tools already exist |
| **Off-site replication** | **The user**, with Hoserva making it easy | rclone, restic, Borg, Duplicati all do this better than a NAS UI would |

The reasoning: **back up what only you can back up correctly, and get out of the way for everything else.**

A user can point restic at `/mnt/user/documents` without Hoserva's help. A user cannot easily produce a consistent appdata backup without knowing which of their 23 containers hold a SQLite database that must not be copied while running — and that is exactly the knowledge Hoserva has.

Building a general-purpose backup product inside a NAS is also the road to becoming a worse Duplicati. The three shipped backup jobs below are narrow, well-defined, and each exists because leaving it out creates a specific, known data-loss scenario.

---

## 1. Config backup

### What it contains

```
hoserva-config-2026-09-14T03-00.tar.zst
├── manifest.json            version, timestamp, host, checksums
├── state.db                 SQLite, consistent snapshot via VACUUM INTO
├── generated/               snapraid.conf, smb.conf, exports, mount units
├── stacks/                  every docker-compose.yml + .env
├── templates/               installed app templates
├── custom/                  user-owned config (smb.custom.conf etc.)
└── snapraid-content/        SnapRAID content files (optional, large)
```

### Consistency

**Never copy a live SQLite file.** Use `VACUUM INTO '<path>'` (or the backup API) to produce a consistent snapshot without stopping the daemon. A `cp` of a WAL-mode database mid-write yields a file that restores as corrupt, and does so intermittently, which is the worst kind of bug.

**Secrets** (SMTP passwords, notification tokens, API keys, Let's Encrypt keys) are handled in two layers (Q28), because the daemon must use them unattended — a 03:00 disk-failure alert cannot wait for someone to type a passphrase:

- **At runtime**, secret columns are encrypted with a machine key in `/etc/hoserva/secret.key` (root, `0600`, generated at install). This protects against the database file leaking — in a diagnostics bundle or a copied backup.
- **In backups**, the secrets section is re-encrypted under a **backup passphrase** the user sets during onboarding (doc 03 §1). The machine key itself is never included. A backup restored without the passphrase restores everything except secrets, and says so clearly.

### Schedule and retention

- Default: nightly, as the last step of the maintenance chain (Q30), plus automatically before every self-update and every array-topology change
- Retention: keep 7 daily, 4 weekly, 6 monthly, pruned per destination
- Each backup is small (single-digit MB without content files), so retention is generous by default

### Multi-destination — yes, from v1

This is the right call. A config backup stored only on the array it describes is not a backup, and a user whose boot device and array both fail is exactly who needs it most.

Destination types:

| Type | Notes |
|---|---|
| **Local path** | Anywhere on the pool or an unassigned disk. Always available. |
| **SMB / NFS share** | Another NAS, a router USB disk |
| **S3-compatible** | Backblaze B2, Wasabi, MinIO, Hetzner Storage Box |
| **SFTP / SSH** | A VPS, another homelab box |
| **WebDAV** | Nextcloud, pCloud, Koofr |
| **rclone remote** | If rclone is present, any of its 70+ backends via an existing remote name |

Implementation: **rclone as an optional dependency (`Recommends:`, Q41) covers everything except local paths.** Rather than writing six protocol clients, Hoserva writes the archive locally and shells out to `rclone copy` for remote destinations. This is the same "orchestrate, don't reinvent" principle as D1, and it means every backend rclone gains, Hoserva gains.

Per-destination configuration: enabled, schedule, retention, encryption, and a **Test connection** button that actually writes and reads back a file. An untested backup destination is decoration.

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

**Same destination system** as config backup — multi-destination, rclone-backed, verified.

**Restore is per-container**, with a preview of what will be overwritten and an automatic pre-restore snapshot of the current state.

### Schedule

Weekly by default, plus **automatically before any container update** (doc 04 §6) — the single most valuable moment to have one, and the thing most often missing when someone needs it.

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

Defaults that protect people who never open the settings page are worth more than options for people who do.
