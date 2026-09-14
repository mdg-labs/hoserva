# Hoserva — Web UI Specification

Complete page inventory. Every route, what it shows, what it does.

---

## Navigation structure

```
Dashboard                     /
Storage                       /storage
  ├ Pool overview             /storage
  ├ Disks                     /storage/disks
  ├ Wake events               /storage/disks/wake-events
  ├ Disk detail               /storage/disks/[id]
  ├ Parity                    /storage/parity
  ├ Cache & mover             /storage/cache
  └ Setup wizard              /storage/setup
Shares                        /shares
  ├ Share list                /shares
  └ Share detail              /shares/[name]
Apps                          /apps
  ├ Installed                 /apps
  ├ Catalog                   /apps/catalog
  ├ App detail (catalog)      /apps/catalog/[id]
  ├ Install wizard            /apps/install/[id]
  ├ Container detail          /apps/[name]
  └ Compose editor            /apps/[name]/compose
Jobs                          /jobs
  └ Job detail                /jobs/[id]
Users                         /users
Settings                      /settings
  ├ General                   /settings
  ├ Network                   /settings/network
  ├ Notifications             /settings/notifications
  ├ Schedules                 /settings/schedules
  ├ Backup & restore          /settings/backup
  ├ Updates                   /settings/updates
  └ Advanced                  /settings/advanced
Tools                         /tools
  ├ Logs                      /tools/logs
  ├ Terminal                  /tools/terminal
  ├ Migration                 /tools/migrate
  ├ Diagnostics               /tools/diagnostics
  └ Assistant (post-1.0)      /tools/assistant
Onboarding (first run)        /welcome
Login                         /login
```

Eight sidebar items (onboarding and login are not in the sidebar). Anything more and the sidebar becomes a menu nobody reads.

Routes with a `[param]` segment are resolved client-side; the SPA is served for every non-API path (doc 01 §1, Q8).

---

## Global elements

### Top bar

- **Array status pill** — `Healthy` / `Degraded` / `Parity stale` / `Action required`. Always visible, every page, clickable to `/storage`.
- **Parity freshness chip** — "Synced 6h ago" or "≈412 files unprotected" (change-journal count, doc 02 §2). Colour-coded per doc 02 §2.
- **Active jobs indicator** — count plus a dropdown showing running jobs with inline progress bars, linking to `/jobs`.
- **Notification bell** — unread alerts, grouped, with a mark-all-read action.
- **User menu** — profile, theme toggle, logout.

### Persistent banners

Shown above page content, dismissible only when the underlying condition clears:

- Array degraded — disk failed, with a direct link to the replace flow
- Sync blocked by threshold guard — with the diff summary and both actions inline
- Config drift detected — which file, with the three resolution options
- Prerequisite missing — Docker not found / wrong version
- Update available for Hoserva itself

### Cross-cutting rules

- **Every destructive action** requires typed or explicit confirmation and states the consequence in plain language. Not "Are you sure?" but "This will erase 4 TB on `WD80EFZX · S/N WCC4N…`. Data on this disk cannot be recovered."
- **Every long operation** becomes a job with progress; nothing blocks the UI.
- **Every technical term** gets a plain-language label with the underlying term available. `epmfs` appears as "Keep folders together" with `epmfs` shown in smaller text next to it — so the label is learnable and searchable.
- **Empty states teach.** A blank Shares page explains what a share is and links to creating one, rather than showing an empty table.
- **Dark and light mode**, following system preference by default.
- **Every UI string through the i18n message catalog** from the first component; English only for 1.0 (Q48).
- **Nothing on a page load or poll walks a data disk.** Per-disk and per-share breakdowns come from data recorded at the last sync; anything that must stat live files (browse, run diff) is an explicit user action and says it will wake the disks.
- **Mobile**: dashboard, job progress, notifications, container start/stop and logs must be fully usable on a phone. Setup wizards and the compose editor may be desktop-first.

---

## 1. `/welcome` — First-run onboarding

Blocks all other routes until complete. Cannot be skipped.

**Step 1 — Create admin account.** Username, password with strength meter, optional TOTP enrolment. No default credentials exist at any point.

**Step 2 — System check.** Runs `hoserva doctor`. Reports: Docker Engine present and API version, Compose plugin, mergerfs, SnapRAID, smartmontools, Samba, free space on the boot device, whether the boot device is removable (warn). Each item pass/fail with a remediation command for failures. Severity is explicit: a missing storage dependency blocks (it should be impossible, they are `.deb` dependencies); missing Docker is a **warning** — onboarding continues and Apps shows the prerequisite banner (doc 04 §3).

**Step 3 — Basics.** Hostname, timezone, one notification channel (strongly encouraged, with a "you will not be told about disk failures without this" warning if skipped), and the **backup passphrase** that protects secrets inside config backups (Q28) — skippable, with the consequence stated: a backup restored without it restores everything except secrets.

**Step 4 — Path taken.** Onboarding is complete at this point; the choice only decides where the user lands.
- *Fresh install* → the storage setup wizard
- *Migrating from Unraid* → `/tools/migrate` (doc 05)

---

## 2. `/` — Dashboard

Single screen, no scrolling required on a desktop for the critical information.

### Row 1 — Array health

- Large capacity visualisation: used / free / total across the pool
- Status: `Healthy` / `Degraded` / `Rebuilding` / `No array configured`
- Parity freshness: last sync timestamp, files changed since, next scheduled sync
- Disk count summary: `6 data · 1 parity · 1 cache`

### Row 2 — Disks strip

One compact tile per disk: identifier, role, fill bar, temperature, spin state (spinning / standby), SMART indicator dot. Click through to disk detail. At a glance, the user sees which disks are awake and whether any is unhealthy.

### Row 3 — Activity

- Read/write throughput graph, last hour
- Cache fill level with last mover run
- Active jobs, inline with progress

### Row 4 — Services and system

- Container summary: running / stopped / unhealthy / updates available. Unhealthy containers listed by name, not just counted.
- CPU, RAM, load, uptime
- Network throughput

### Row 5 — Attention

Only rendered if non-empty: SMART warnings, failed jobs, blocked syncs, config drift, expiring certificates, containers in crash loops.

**Design rule:** if everything is fine, this row is absent and the dashboard is calm. Persistent yellow badges for non-issues train users to ignore the UI.

---

## 3. Storage

### 3.1 `/storage/setup` — Array setup wizard

Runs on a system with no array. Steps:

1. **Disk discovery** — table of every block device: path, model, serial, size, current filesystem and label, SMART status, whether it appears to contain data, whether it looks like an Unraid disk. Devices that are the boot disk are excluded and marked as such.
2. **Role assignment** — drag or select each disk into Parity / Data / Cache / Ignore. Live validation: one or two parity disks (Q19), each ≥ largest data disk; at least one data disk; cache optional; warnings on any disk with existing data, SMART issues, or weak identity (a USB enclosure hiding the serial — refused as parity, Q21).
3. **Filesystem** — per data disk, default XFS. Disks with an adoptable existing filesystem (XFS, ext4, single-device btrfs) offer "keep existing data" instead of format. Parity disks are always formatted XFS (Q20).
4. **Pool options** — default create policy for new shares in plain language (doc 02 §1), minimum free space per disk.
5. **Review** — plain-language summary ("6 disks, 24 TB usable, single-disk fault tolerance, one 8 TB parity"), plus a collapsible preview of the exact generated `snapraid.conf` and mergerfs mount units. Power users will look; everyone else won't.
6. **Confirm and create** — explicit list of which disks will be erased, requiring typed confirmation. Then a job with progress.

### 3.2 `/storage` — Pool overview

- Capacity: total, used, free; per-disk breakdown as a stacked bar
- Pool mount point and health
- Create policy, editable
- Per-disk fill levels, highlighting imbalance
- Actions: **Add disk**, **Remove disk**, **Replace disk**, **Rebalance** (move files between disks to even out fill — a background job, not a rebuild)
- Explanatory note that adding a disk requires no rebuild, stated on the page rather than buried in docs

### 3.3 `/storage/disks` — Disk list

Table: device, serial, model, role, size, used, free, temperature, power-on hours, spin state, SMART summary, filesystem, mount point.

Sortable and filterable. Bulk actions: spin down all, run short self-test on all.

Row actions: detail, spin down, run self-test, identify (blink the drive LED if the controller supports it — small feature, disproportionately useful when physically finding a failed disk in a case with eight drives).

### 3.3a `/storage/disks/wake-events` — Wake events

Per-disk timeline of spin-state transitions: when each disk woke, how long it stayed awake, and a wake count per day (Phase 1). From Phase 4, each wake carries its most likely cause — process and, where possible, container — per doc 08 §1 (Q32). This is the "why won't my disks sleep" answer, and the page an assistant or a support thread links to.

### 3.4 `/storage/disks/[id]` — Disk detail

- Identity: model, serial, firmware, size, interface, mount point, role
- Full SMART attribute table with raw and normalised values, **with per-attribute history graphs** — trend is the signal, not the current number
- Self-test history and controls (short, long, conveyance)
- Temperature history
- Spin state, spindown timer, manual spindown
- Filesystem usage and top-level directory breakdown as of the last sync (what is actually on this disk — genuinely useful when deciding what you lost if it dies); not recomputed live, so viewing it never wakes the disk
- Actions: remove from pool (with evacuation), mark as failed, replace

### 3.5 `/storage/parity` — Parity management

The most important page in the product after the dashboard.

**Status block**
- Parity disk identity and size
- Last successful sync, duration, files processed
- Files changed since last sync, live, from the change journal (approximate — labelled as such)
- Last scrub, percentage covered, errors found
- Parity age warning if beyond threshold

**Diff panel**
- `Run diff` button (states that it spins up every data disk), plus automatic diff before every sync
- Results grouped: added / updated / removed / moved / copied / moved-by-Hoserva (doc 02 §2), with counts and expandable file lists
- Removed files listed first and prominently — that is the dangerous category
- Threshold guard status: whether the next sync would be blocked and why

**Actions**
- Sync now (always shows the diff first and requires confirmation if thresholds are exceeded)
- Scrub now, with percentage selector
- Check (verify without repair)
- Fix (guided recovery flow — see below)

**Schedule** — links to `/settings/schedules`, but shows the current schedule inline so it is visible where it matters.

**History** — table of past sync and scrub runs: timestamp, duration, result, files processed, errors, link to the full job log.

**Guided fix flow** — a separate multi-step modal, because this runs when something has gone wrong and the user is stressed:
1. What are you recovering? (whole disk / specific files)
2. Preconditions check: parity present, content files readable, enough healthy disks
3. **Explicit statement of what cannot be recovered** — files written after the last sync, listed by count and, where known, by name
4. Dry-run showing what would be restored
5. Execute as a job with progress
6. Verification report

### 3.6 `/storage/cache` — Cache and mover

- Cache device, capacity, fill level
- Breakdown of what occupies it: appdata, pending moves, other
- Per-share cache mode overview (cache-then-move / cache-only / array-only), editable inline
- Mover schedule and threshold
- Run mover now
- Last mover run: files moved, bytes, duration, skipped-because-in-use list
- **Warning block**: cache-only data is not covered by parity, with the current appdata backup status and a link to configure it if absent

---

## 4. Shares

### 4.1 `/shares` — Share list

Table: name, path, size used, cache mode, SMB on/off, NFS on/off, access summary.

Actions: create share, edit, delete (with explicit warning about data), browse.

### 4.2 `/shares/[name]` — Share detail

**General** — name, real path, size used, per-disk distribution as of the last sync (which disks actually hold this share's files), included in parity yes/no.

**Allocation** — this share's create policy in plain language (doc 02 §1, Q11), with the tradeoff stated inline.

**Cache** — mode selector (three options per doc 02 §3), with the consequence of each stated inline. Changing the mode offers a share relocation job for existing files (doc 09 §2); nothing moves silently.

**SMB** — enable, guest access, read-only, browseable, recycle bin, Time Machine support, per-user and per-group access (no access / read-only / read-write), export path preview.

**NFS** — enable, allowed hosts and subnets, squash options, export line preview.

**Browse** — a simple file browser: navigate, see file sizes, see which underlying disk holds each file (mergerfs `user.mergerfs.basepath` xattr), delete. Not a full file manager; enough to answer "where did that go". Browsing is an explicit action and may wake disks.

**Danger zone** — delete share, with a clear statement of whether data is deleted or only the share definition removed. These must be two distinct actions with different confirmations.

---

## 5. Apps

Scope per doc 04.

### 5.1 `/apps` — Installed

Card or table view of every container, including **unmanaged** containers created outside Hoserva (Portainer, manual `docker run`), clearly badged as such.

Per container: name, image and tag, state, uptime, CPU and RAM usage, ports with clickable links to the web UI where a port maps to one, update-available badge.

Actions: start, stop, restart, update, logs, detail, remove.

Bulk: update all, stop all, start all.

### 5.2 `/apps/catalog` — App catalog

Searchable, category-filtered grid of templates. Per entry: icon, name, maintainer, short description, source repository, install count if available.

Filters: category, maintainer, installed / not installed, verified / community.

**Source indicator** on every entry — Hoserva curated, Unraid Community Applications, or user-added repository. Users should know where a template came from.

### 5.3 `/apps/catalog/[id]` — Catalog app detail

Full description, screenshots, icon, maintainer, project and support links, the raw template, and a **privilege summary** computed from the template: does it request privileged mode, host networking, the Docker socket, or host path mounts outside the pool. Each flagged in plain language with an explanation of the risk.

### 5.4 `/apps/install/[id]` — Install wizard

Form generated from the template:

- **Ports** — with live conflict detection against running containers and host listeners; suggests the next free port on conflict
- **Paths** — picker aware of existing shares, defaulting to sensible pool paths; appdata defaults to `/mnt/cache/appdata/<app>`
- **Environment variables** — with the template's descriptions, required fields marked, password fields masked with a generate button
- **Network mode** — bridge / host / any existing custom network (macvlan/ipvlan included), with a warning on host. No network creation in v1; a template needing a missing network shows the exact `docker network create` command (Q37)
- **Advanced** — raw extra parameters, restart policy, resource limits

Basic and Advanced views, mirroring what Unraid users already expect.

**Preview** — the generated `docker-compose.yml`, fully visible before anything runs, with any converter warnings listed (doc 04).

Then: create, pull, start, with progress and logs.

### 5.5 `/apps/[name]` — Container detail

Tabs:
- **Overview** — state, image, created, uptime, restart count, health check status, ports, mounted paths with the underlying disk shown
- **Logs** — live tail, search, download, adjustable line count
- **Stats** — CPU, RAM, network, disk IO graphs
- **Config** — the settings form again, editable; applying restarts the container
- **Update** — current tag vs. available, changelog link if the template provides one

Actions: start, stop, restart, recreate, remove (with a separate, clearly distinct option for whether to also delete appdata).

### 5.6 `/apps/[name]/compose` — Compose editor

Raw YAML editor with syntax highlighting and validation. Applying regenerates and recreates the stack. This is the power-user escape hatch; editing here marks the stack as "manually edited" so the template form doesn't silently overwrite the changes.

Also the entry point for **importing** an existing `docker-compose.yml`, either pasted or uploaded.

---

## 6. `/jobs` — Jobs

List of all jobs: type, target, status, started, duration, progress. Filterable by status and type.

Running jobs show live progress and a cancel action where cancellation is supported. Where it isn't, the button is absent and a tooltip explains why rather than showing a disabled control with no reason.

### `/jobs/[id]` — Job detail

Full metadata, progress, complete captured stdout/stderr with search, downloadable log, and — on failure — the exit code plus a plain-language interpretation of common failure modes where one is known.

---

## 7. `/users` — Users and access

- User list: name, role, TOTP enabled, last login, share access summary
- **Roles** (Q27): *Admin* (full UI), *Viewer* (read-only UI), *Share-only* (SMB/NFS, no UI login — the default for new users, so a family member's share login never reaches a page that formats disks)
- Create, edit, delete users; setting a password updates the UI credential and the Samba passdb entry together
- Group management; share data is group `users` (GID 100), identical to Unraid (Q26)
- Per-user share permissions, editable from either side (here or on the share)
- Session list with revoke
- Personal API tokens, role-scoped, with revoke (Q43)
- **Note:** a user account can carry both UI login and SMB access. The UI must be explicit about which a given account has, because the dual purpose surprises people.
- OIDC configuration — roadmap, shown as a disabled section with a note rather than hidden

---

## 8. Settings

### 8.1 `/settings` — General
Hostname, timezone, NTP servers, UI language, theme default, session timeout.

### 8.2 `/settings/network`
Interfaces, IP configuration (DHCP / static), DNS, gateway. HTTPS settings: self-signed cert regeneration, Let's Encrypt via DNS-01, certificate expiry. Access scope for the UI — LAN-only source filter by default (doc 01 §7, Q10) — with an explicit warning when allowing all sources. Port configuration (default `:8008`, TLS-only).

### 8.3 `/settings/notifications`
Channels: email (SMTP), Gotify, ntfy, Discord webhook, generic webhook. Each with a **Send test notification** button — untested notification config is the same as no notification config.

Per-event routing matrix: which events go to which channels, at which severity.

Events to cover: SMART warning, SMART failure, disk offline, array degraded, sync succeeded (opt-in, off by default), sync failed, **sync blocked by threshold**, scrub found errors, pool above threshold, **any single disk near `minfreespace`** (doc 09 §5), cache above threshold, mover repeatedly skipping files, config drift detected, container unhealthy or crash-looping, container update available, Hoserva update available, login failure burst, certificate expiring, config backup failed, appdata backup failed, backup destination stale, restore drill failed.

Quiet hours, with a "critical alerts always deliver" override that cannot be disabled.

### 8.4 `/settings/schedules`
All recurring jobs in one place: the nightly maintenance chain, SMART self-tests, appdata backup, restore drill, container update checks.

**The nightly maintenance chain** (Q30) is one schedule entry with ordered steps: mover → diff + threshold guard → sync → config backup, plus scrub on the weekly day. Each step starts when the previous one finishes, so the mover-before-sync order cannot be broken by a slow run. Individual steps can be disabled, not reordered.

Per job: enabled, cron-like schedule with a human-readable preview ("every day at 02:00"), and the next run time. Conflict detection covers jobs scheduled outside the chain — e.g. appdata backup overlapping the chain, or two heavy jobs at once.

### 8.5 `/settings/backup`
Full design in doc 10.

**Destinations** — list of backup destinations (local path, SMB/NFS, S3-compatible, SFTP, WebDAV, rclone remote), each with enabled, retention, encryption, last successful backup, and a **Test connection** button that writes and reads back a file. A fresh install has two local destinations: the boot device and a pool path (Q40).

**Config backup** — the archive described in doc 10 §1; manual run and download; schedule is part of the nightly chain. Backup passphrase set/change (Q28).

**Restore** — upload an archive, preview what it contains and what will change, apply.

**Appdata backup** — scope, per-container stop policy (with known database images flagged), destinations, schedule, retention.

**Restore drill** — last result and next run.

### 8.6 `/settings/updates`
Current Hoserva version, available version, changelog, update channel (stable / beta), update action, update check on/off (the only outbound request Hoserva makes on its own, Q49). Also: available updates for mergerfs and SnapRAID, and whether they fall inside the version range the installed Hoserva release was tested against (Q7); installed versions are read from package metadata.

Updating runs `apt` in a transient systemd unit so the daemon can restart itself. It is refused while a Parity, Array-write or Topology job is running (doc 01 §4), and a config backup runs automatically first (doc 10 §1).

### 8.7 `/settings/advanced`
Raw mergerfs options, raw SnapRAID extra options, custom Samba include file, API bind settings, debug logging level, and the config-drift management view (which files are managed, which are unmanaged, hashes, and the option to take or release ownership of each).

---

## 9. Tools

### 9.1 `/tools/logs`
System log viewer (journald) with level filter, unit filter, search, time range, and download. Plus Hoserva's own application log as a separate stream.

### 9.2 `/tools/terminal`
Browser terminal (ttyd or equivalent). **Off by default.** Enabling requires a confirmation that states plainly this is root shell access to the host.

### 9.3 `/tools/migrate`
The Unraid migration workspace. Detailed in doc 05. Four phases: scan, review, import, verify.

### 9.4 `/tools/diagnostics`
One-click diagnostic bundle: `hoserva doctor` output, version info, config (secrets redacted), recent logs, job history, disk inventory, SMART reports. Downloadable as a single archive for bug reports.

Redaction must be verifiable — the UI shows what was removed, and the archive is inspectable before sending. Users paste these into public forums. One redaction engine serves this bundle and the assistant's tool boundary (doc 11 §3).

### 9.5 `/tools/assistant` — post-1.0

Free-form assistant chat and provider configuration, per doc 11. Not built before 1.0 (Q47); listed so the route is reserved.

---

## 10. Page priority for implementation

Not everything ships at once. Order by what makes the product usable:

| Tier | Pages |
|---|---|
| **1 — Minimum viable** (Phase 1) | `/welcome`, `/login`, `/storage/setup`, `/storage`, `/storage/disks`, `/storage/disks/wake-events` (event log), `/storage/parity`, `/`, `/jobs`, `/settings/notifications`, `/settings/schedules` (nightly chain) |
| **2 — Actually a NAS** (Phase 2) | `/shares`, `/shares/[name]`, `/storage/cache`, `/users`, `/settings/backup` |
| **3 — Complete home server** (Phase 3) | `/apps` and all sub-pages, `/tools/migrate` |
| **4 — Polish** (Phase 4) | `/storage/disks/[id]` history graphs, wake attribution, `/tools/logs`, `/tools/diagnostics`, `/settings/advanced`, `/tools/terminal` |
| **Post-1.0** | `/tools/assistant` |

Notifications and a sync/scrub schedule are Tier 1 because Phase 1's definition of done is running the author's own array for a month (doc 07 §1) — an array that cannot report a dead disk or a blocked sync is not one to trust with real data. Config backup exists from Phase 1 via the CLI (`hoserva config export`); its UI arrives in Tier 2.

Tier 1 plus Tier 2 is a working, honest NAS. Tier 3 makes it a complete home server, and is what users migrating an existing setup need.
