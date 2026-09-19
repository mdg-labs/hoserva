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
VMs                           /vms
  ├ VM list                   /vms
  ├ Create VM                 /vms/create
  ├ VM detail (incl. console) /vms/[name]
  └ Passthrough               /vms/passthrough
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

Nine sidebar items (onboarding and login are not in the sidebar) — VMs is the one addition since Phase 3.5 (doc 14), kept as its own top-level item rather than folded into Apps because its lifecycle (console access, passthrough) is different enough to confuse users if merged, the same reasoning Unraid itself applies with separate Docker and VM Manager sections. Anything more and the sidebar becomes a menu nobody reads.

Routes with a `[param]` segment are resolved client-side; the SPA is served for every non-API path (doc 01 §1, Q8).

---

## Global elements

### Top bar

- **Array status pill** — `Healthy` / `Degraded` / `Parity stale` / `Action required`. Always visible, every page, clickable to `/storage`.
- **Parity freshness chip** — "Synced 6h ago" or "≈412 files unprotected" (change-journal count, doc 02 §2). Colour-coded per doc 02 §2.
- **Active jobs indicator** — count plus a dropdown showing running jobs with inline progress bars, linking to `/jobs`.
- **Notification bell** — unread alerts, grouped, with a mark-all-read action.
- **User menu** — profile, theme toggle, logout.

**Components:** `top-bar`. The array pill and parity chip are `status-badge` links; the jobs dropdown lists `job-progress` rows; on mobile, the bell and user menu open as `row-actions` drawers.

### Persistent banners

Shown above page content, dismissible only when the underlying condition clears:

- Array degraded — disk failed, with a direct link to the replace flow
- Sync blocked by threshold guard — with the diff summary and both actions inline
- Config drift detected — which file, with the three resolution options
- Prerequisite missing — Docker not found / wrong version
- Update available for Hoserva itself

**Components:** `banner` for each; the drift banner's *View diff* opens a Dialog with long content (`p-dialog-5`) holding the Q59 diff view; the Docker banner carries `copy-value` for the install commands.

### Cross-cutting rules

- **Every destructive action** requires typed or explicit confirmation and states the consequence in plain language. Not "Are you sure?" but "This will erase 4 TB on `WD80EFZX · S/N WCC4N…`. Data on this disk cannot be recovered."
- **Every long operation** becomes a job with progress; nothing blocks the UI.
- **Every technical term** gets a plain-language label with the underlying term available. `epmfs` appears as "Keep folders together" with `epmfs` shown in smaller text next to it — so the label is learnable and searchable.
- **Empty states teach.** A blank Shares page explains what a share is and links to creating one, rather than showing an empty table.
- **Dark and light mode**, following system preference by default.
- **Every UI string through the i18n message catalog** from the first component; English only for 1.0 (Q48).
- **Every action and every piece of data goes through a documented API operation** (D18), via the generated TypeScript client. The UI has no private endpoints — anything it can do, a script with an API token can do.
- **Nothing on a page load or poll walks a data disk.** Per-disk and per-share breakdowns come from data recorded at the last sync; anything that must stat live files (browse, run diff) is an explicit user action and says it will wake the disks.
- **Mobile**: dashboard, job progress, notifications, container start/stop and logs must be fully usable on a phone. Setup wizards and the compose editor may be desktop-first.

---

## Component system

**Every screen is built from coss ui (D15)**: Base UI primitives styled with Tailwind CSS v4, vendored into `web/src/components/ui/` with the shadcn CLI from the `@coss` registry. Particles (`p-*`, catalogued at coss.com/ui/particles) are starting points that get adapted into this codebase, never loaded at runtime. The **Components** line under each page below is the decision for that page; changing a choice means changing that line in the same commit.

### Rules

- **coss first.** Use the primitive and particle this spec names. No second component library, and no hand-rolled version of something coss provides. What coss doesn't cover (charts, code editor, terminal, VNC viewer, diff, virtualised lists) uses the libraries in Q59, each wrapped once in `web/src/components/`, so pages never import those libraries directly.
- **Shared patterns are built once** in `web/src/components/patterns/` from the particles in the table below. Pages use the pattern rather than re-adapting the particle, so a status, a confirmation or a job never looks two different ways.
- **Behaviour picks the primitive, not appearance** (coss's segmented-control rule): choosing one value is a RadioGroup, navigating is links, a clearable filter is a ToggleGroup, and switching content panels is Tabs.
- **Navigation:** the sidebar holds the nine sections only. A section's sub-routes are `section-nav` links at the top of the page, and detail routes (`[id]`, `[name]`) get a Breadcrumb (`p-breadcrumb-5`).
- **Long operations are jobs** shown with `job-progress`, never a spinner in a toast. Toasts confirm short actions or link to the job that was started.
- **Tokens, not colours:** semantic tokens only (`bg-destructive`, `text-muted-foreground`), never raw palette classes. Icons come from `lucide-react`, which coss particles use, and decorative icons are `aria-hidden`.
- **Particle text is placeholder.** Every string goes through the i18n catalog (Q48).
- **Fonts are bundled, never fetched** (Q49, Q59). coss's automatic font setup targets a Next.js `layout.tsx`, so under Vite the fonts are wired to `--font-sans`, `--font-heading` and `--font-mono` by hand.
- **Mobile:** `form-overlay` becomes a drawer and `row-actions` becomes a bottom drawer below the desktop breakpoint, which covers the phone-usable surfaces the cross-cutting rules require.

### Shared patterns

| Pattern | Built from | Particle(s) | Used for |
|---|---|---|---|
| `app-shell` | Sidebar, SidebarInset, SidebarTrigger inside SidebarProvider | — (primitive docs) | Sidebar with the nine sections; collapse and mobile behaviour from SidebarProvider |
| `section-nav` | Links with segmented styling, in a horizontal ScrollArea on mobile | `p-navigation-1`, `p-navigation-2`, `p-scroll-area-2` | Sub-routes of Storage, Apps, VMs, Settings, Tools |
| `top-bar` | Badge, Popover, Menu, Avatar | `p-button-32` (jobs, bell), `p-popover-2`, `p-menu-6`, `p-menu-4` (theme), `p-avatar-6` | Array pill, parity chip, jobs dropdown, notifications, user menu |
| `status-badge` | Badge | `p-badge-6` success, `p-badge-7` warning, `p-badge-8` error, `p-badge-5` info, `p-badge-2` outline | Every state: array, disk, SMART, spin, job, container, VM, protocol |
| `plain-term` | Label with the technical term in muted text, Tooltip for the explanation | `p-tooltip-1` | "Keep folders together · `epmfs`" (cross-cutting rules) |
| `banner` | Alert with AlertAction | `p-alert-3`; `p-alert-6` warning, `p-alert-7` error | Persistent banners, the attention row, inline warning blocks |
| `inline-note` | Alert, info variant | `p-alert-4` | Explanations stated on the page rather than in docs |
| `confirm` | AlertDialog | `p-alert-dialog-1` | Consequential actions, with the consequence stated in the dialog |
| `typed-confirm` | AlertDialog + Field + Input; confirm enabled only on an exact match | `p-alert-dialog-1`, `p-field-4` | Erasing disks, deleting data, the migration point of no return |
| `data-table` | Table + TanStack Table in a CardFrame; card-style rows on mobile | `p-table-8` (sort, paginate), `p-table-6` (selection for bulk actions), `p-table-7` (small, static), `p-table-5` | Every list |
| `table-filters` | InputGroup search, Group with filter Combobox | `p-input-group-20`, `p-group-23` | Filter bar above a `data-table` |
| `row-actions` | Menu from an icon Button; Drawer on mobile | `p-menu-1`, `p-button-13`, `p-drawer-13` | Per-row and per-card actions |
| `empty-state` | Empty | `p-empty-1` | Every list with nothing in it — "empty states teach" |
| `loading` | Skeleton; Button `loading` prop | `p-skeleton-1`, `p-button-41` | Page loads; waits scoped to one button |
| `job-progress` | Progress (indeterminate when no percentage), cancel Button or an info Tooltip explaining why not | `p-progress-2`, `p-tooltip-1` | Jobs dropdown, jobs page, the last step of wizards |
| `feedback-toast` | toastManager / anchoredToastManager | `p-toast-2`, `p-toast-5` (promise), `p-toast-7` (anchored, after copy) | Short actions; "job started — view" |
| `form` | Form + Field with a zod schema | `p-form-2`, `p-field-18`, `p-field-4` | Every form |
| `form-overlay` | `form` in a Dialog on desktop, a Drawer on mobile | `p-dialog-1`, `p-drawer-12` | Create or edit without leaving the page |
| `side-panel` | Sheet | `p-sheet-1` | Editing or inspecting one row while the list stays visible |
| `unsaved-guard` | Dialog close confirmation | `p-dialog-4` | Guided fix flow, Compose editor, wizards |
| `wizard` | CardFrame with a step Progress and a Back/Next button pair | `p-card-6`, `p-progress-2`, `p-button-33` | Onboarding, array setup, fix flow, migration — coss has no stepper, so this is composed once |
| `choice-cards` | RadioGroup cards with descriptions | `p-radio-group-4`, `p-radio-group-3` | Choices whose consequence is stated inline: create policy, cache mode, role, network mode |
| `segmented-choice` | RadioGroup with segmented styling | `p-radio-group-7`, `p-radio-group-8` | Small exclusive values: access level per row, DHCP/static, update channel, card/table view |
| `detail-tabs` | Tabs, underline; vertical on long detail pages | `p-tabs-2`, `p-tabs-4`, `p-tabs-10` (count) | Disk, share, container and VM detail |
| `setting-switch` | Switch with description; switch card for consequential toggles | `p-switch-3`, `p-switch-4`, `p-field-15` | Enable/disable settings |
| `number-unit` | NumberField in an InputGroup with end text | `p-input-group-6`, `p-number-field-7` (range) | Sizes, percentages, ports, vCPUs |
| `path-picker` | Combobox with grouped items (shares grouped by cache and array) | `p-combobox-8` | Container paths, ISO and vdisk location, backup targets |
| `multi-pick` | Combobox, multiple, with chips | `p-combobox-19` | Groups, systemd units, categories |
| `list-input` | Group with input and add button, removable Badges | `p-group-16`, `p-badge-20` | NTP servers, NFS hosts |
| `timezone` | Timezone Combobox with search | `p-combobox-17` | Onboarding, general settings |
| `secret-input` | Password visibility toggle, strength indicator, generate button | `p-input-9`, `p-input-group-26`, `p-input-group-9` | Passwords, backup passphrase, secret environment variables |
| `totp` | OTPField with auto validation | `p-otp-field-7` | TOTP enrolment and login |
| `copy-value` | Read-only Input with a copy button | `p-input-17`, `p-button-35` | Remediation commands, API tokens, export lines |
| `code-view` | Collapsible around a read-only Q59 editor in a Frame | `p-collapsible-1`, `p-frame-1` | Generated config previews, raw templates, domain XML |
| `file-upload` | File Input in a Field | `p-input-5` | Flash Backup zip, config restore, Compose import |
| `log-view` | ScrollArea + Q59 virtualised list; search, level ToggleGroup, follow Switch | `p-scroll-area-4`, `p-input-group-20`, `p-toggle-group-8`, `p-switch-1` | Job, container and system logs |
| `grouped-results` | Accordion allowing several open, a count Badge per group | `p-accordion-3`, `p-badge-13` | Diff groups, scan report, IOMMU groups, redaction list |
| `stacked-checks` | Frame with stacked panels | `p-frame-4` | System check, privilege summary, nightly chain steps |
| `metric-tile` | Card + Meter with a formatted value | `p-card-1`, `p-meter-3` | Capacity, cache fill, CPU and RAM, disk tiles |
| `chart` | Q59 chart library inside a Card | — | Throughput, temperature, SMART history, wake timeline, stats, stacked capacity |
| `time-window` | Date picker with presets | `p-date-picker-4` | Log and history time ranges |
| `danger-zone` | Frame + destructive-outline Buttons leading to `confirm` or `typed-confirm` | `p-frame-1`, `p-button-5` | Remove disk, delete share, remove app or VM |

---

## 1. `/welcome` — First-run onboarding

Blocks all other routes until complete. Cannot be skipped.

**Step 1 — Create admin account.** Username, password with strength meter, optional TOTP enrolment. No default credentials exist at any point.

**Step 2 — System check.** Runs `hoserva doctor`. Reports: Docker Engine present and API version, Compose plugin, mergerfs, SnapRAID, smartmontools, Samba, free space on the boot device, whether the boot device is removable (warn), and configuration already on the host — Samba shares, NFS exports, fstab mounts, Docker containers and images — each offered for import or to be left unmanaged (Q76). Each item pass/fail with a remediation command for failures. Severity is explicit: a missing storage dependency blocks (it should be impossible, they are `.deb` dependencies); missing Docker is a **warning** — onboarding continues and Apps shows the prerequisite banner (doc 04 §3).

**Step 3 — Basics.** Hostname, timezone, one notification channel (strongly encouraged, with a "you will not be told about disk failures without this" warning if skipped), and the **backup passphrase** that protects secrets inside config backups (Q28) — skippable, with the consequence stated: a backup restored without it restores everything except secrets.

**Step 4 — Path taken.** Onboarding is complete at this point; the choice only decides where the user lands.
- *Fresh install* → the storage setup wizard
- *Migrating from Unraid* → `/tools/migrate` (doc 05)

**Components:** `wizard`. Step 1: `form`, `secret-input` with strength, `totp` with its QR code in a Card. Step 2: `stacked-checks`, each item a `status-badge` plus `copy-value` for its remediation. Step 3: `form`, `timezone`, channel type Select with icons (`p-select-9`), `secret-input` for the passphrase; skipping a step shows a warning `banner` stating the consequence. Step 4: `choice-cards`. `/login`: authentication card (`p-card-2`) with `totp`.

---

## 2. `/` — Dashboard

Single screen, no scrolling required on a desktop for the critical information.

### Row 1 — Array health

- Large capacity visualisation: used / free / total across the pool
- Status: `Healthy` / `Degraded` / `Rebuilding` / `No array configured`
- Parity freshness: last sync timestamp, files changed since, next scheduled sync
- Disk count summary: `6 data · 1 parity · 1 cache`

**Components:** a large `metric-tile` with the array `status-badge`; parity freshness as text with a `status-badge`.

### Row 2 — Disks strip

One compact tile per disk: identifier, role, fill bar, temperature, spin state (spinning / standby), SMART indicator dot. Click through to disk detail. At a glance, the user sees which disks are awake and whether any is unhealthy.

**Components:** one `metric-tile` per disk, the whole tile a link, with SMART and spin-state `status-badge`s, in a horizontal ScrollArea (`p-scroll-area-2`).

### Row 3 — Activity

- Read/write throughput graph, last hour
- Cache fill level with last mover run
- Active jobs, inline with progress

**Components:** throughput `chart`; cache `metric-tile`; `job-progress` rows.

### Row 4 — Services and system

- Container summary: running / stopped / unhealthy / updates available. Unhealthy containers listed by name, not just counted.
- CPU, RAM, load, uptime
- Network throughput

**Components:** a Card with `status-badge` counts and unhealthy containers named; CPU and RAM `metric-tile`s; network `chart`.

### Row 5 — Attention

Only rendered if non-empty: SMART warnings, failed jobs, blocked syncs, config drift, expiring certificates, containers in crash loops.

**Design rule:** if everything is fine, this row is absent and the dashboard is calm. Persistent yellow badges for non-issues train users to ignore the UI.

**Components:** a stack of `banner`s, not rendered at all when empty.

---

## 3. Storage

### 3.1 `/storage/setup` — Array setup wizard

Runs on a system with no array. Steps:

1. **Disk discovery** — table of every block device: path, model, serial, size, current filesystem and label, SMART status, whether it appears to contain data, whether it looks like an Unraid disk. Devices that are the boot disk are excluded and marked as such.
2. **Role assignment** — select each disk's role: Parity / Data / Cache / Ignore. Live validation: one or two parity disks (Q19), each ≥ largest data disk; at least one data disk; cache optional; warnings on any disk with existing data, SMART issues, or weak identity (a USB enclosure hiding the serial — refused as parity, Q21).
3. **Filesystem** — per data disk, default XFS. Disks with an adoptable existing filesystem (XFS, ext4, single-device btrfs) offer "keep existing data" instead of format. Parity disks are always formatted XFS (Q20).
4. **Pool options** — default create policy for new shares in plain language (doc 02 §1), minimum free space per disk.
5. **Review** — plain-language summary ("6 disks, 24 TB usable, single-disk fault tolerance, one 8 TB parity"), plus a collapsible preview of the exact generated `snapraid.conf` and mergerfs mount units. Power users will look; everyone else won't.
6. **Confirm and create** — explicit list of which disks will be erased, requiring typed confirmation. Then a job with progress.

**Components:** `wizard`. Discovery: `data-table` (`p-table-7`) with `status-badge`s; the boot disk's row is disabled with the reason shown. Role assignment: a Select per row with role icons (`p-select-9`) — select only, there is no drag and drop; validation as a `banner` above the table plus Field errors (`p-field-4`). Filesystem: `choice-cards` per data disk (format vs. keep existing data). Pool options: `choice-cards` with `plain-term`, `number-unit` for minimum free space. Review: a summary Card plus `code-view`. Confirm: `typed-confirm`, then `job-progress`.

### 3.2 `/storage` — Pool overview

- Capacity: total, used, free; per-disk breakdown as a stacked bar
- Pool mount point and health
- Create policy, editable
- Per-disk fill levels, highlighting imbalance
- Actions: **Add disk**, **Remove disk**, **Replace disk**, **Upgrade disk** (a larger disk for a healthy one, Q71), **Rebalance** (move files between disks to even out fill — a background job, not a rebuild), **Stop array** / **Start array** (maintenance mode, Q70)
- Explanatory note that adding a disk requires no rebuild, stated on the page rather than buried in docs

**Components:** `section-nav` for the Storage routes; capacity `metric-tile` plus a stacked per-disk `chart`; the create policy shown with `plain-term` and edited through `form-overlay` with `choice-cards`; actions as a button Group (`p-group-1`), with Remove, Replace and Upgrade going through `typed-confirm` and Stop array through `confirm` listing what will stop; the no-rebuild explanation is an `inline-note`.

### 3.3 `/storage/disks` — Disk list

Table: device, serial, model, role, size, used, free, temperature, power-on hours, spin state, SMART summary, filesystem, mount point.

Sortable and filterable. Bulk actions: spin down all, run short self-test on all.

Row actions: detail, spin down, run self-test, identify (blink the drive LED if the controller supports it — small feature, disproportionately useful when physically finding a failed disk in a case with eight drives).

**External disks** (Q72) are listed as their own group — mount, eject, format, and whether one is a backup destination. They are never in the pool or parity.

**Components:** `data-table` with selection (`p-table-6`) and sorting (`p-table-8`); `table-filters`; bulk actions in a Group above the table; `row-actions`; identify and spin down confirm with `feedback-toast`; self-tests are jobs; external disks as a second `data-table` with mount and eject in `row-actions` and format through `typed-confirm`.

### 3.3a `/storage/disks/wake-events` — Wake events

Per-disk timeline of spin-state transitions: when each disk woke, how long it stayed awake, and a wake count per day (Phase 1). From Phase 4, each wake carries its most likely cause — process and, where possible, container — per doc 08 §1 (Q32). This is the "why won't my disks sleep" answer, and the page an assistant or a support thread links to.

**Components:** timeline `chart` plus `data-table`; `time-window`.

### 3.4 `/storage/disks/[id]` — Disk detail

- Identity: model, serial, firmware, size, interface, mount point, role
- Full SMART attribute table with raw and normalised values, **with per-attribute history graphs** — trend is the signal, not the current number
- Self-test history and controls (short, long, conveyance)
- Temperature history
- Spin state, spindown timer, manual spindown
- Filesystem usage and top-level directory breakdown as of the last sync (what is actually on this disk — genuinely useful when deciding what you lost if it dies); not recomputed live, so viewing it never wakes the disk
- Actions: remove from pool (with evacuation), mark as failed, replace

**Components:** vertical `detail-tabs` (`p-tabs-4`): Overview (identity Card; spin state; spindown timer Select, `p-select-23`), SMART (`data-table`, `p-table-7`; a row opens a `side-panel` with that attribute's `chart`), Self-tests (`data-table` with a start button Group), Temperature (`chart`), Contents (`data-table` as of the last sync). `danger-zone` for remove, mark failed and replace.

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

**Components:** Status: a Card with `status-badge`s; the change count marked approximate with `plain-term`. Diff: *Run diff* Button with its wake warning as description text; results as `grouped-results`, removed first and open by default; guard state as a `banner`, error variant when the next sync would be blocked. Actions: *Sync now* opens a Dialog (`p-dialog-5`) with the diff summary, becoming `typed-confirm` when thresholds are exceeded; scrub percentage via a slider with input (`p-slider-12`) in a `form-overlay`. Schedule: a read-only Card with a link Button (`p-button-17`). History: `data-table`. Guided fix flow: `wizard` inside a Dialog (`p-dialog-5`) with `unsaved-guard` — step 1 `choice-cards`, step 3 a warning `banner` with a `data-table` of unrecoverable files, step 4 `data-table`, step 5 `job-progress`, step 6 a Card of `status-badge`s.

### 3.6 `/storage/cache` — Cache and mover

- Cache device, capacity, fill level
- Breakdown of what occupies it: appdata, pending moves, other
- Per-share cache mode overview (cache-then-move / cache-only / array-only), editable inline
- Mover schedule and threshold
- Run mover now
- Last mover run: files moved, bytes, duration, skipped-because-in-use list
- **Warning block**: cache-only data is not covered by parity, with the current appdata backup status and a link to configure it if absent

**Components:** cache `metric-tile` plus a breakdown `chart`; per-share cache modes as a `data-table` with a small Select per row (`p-select-2`), where a change opens the relocation `form-overlay`; the mover schedule in a Card; *Run mover* Button (`loading`) followed by a `feedback-toast` linking the job; the skipped-because-in-use list in a Collapsible (`p-collapsible-1`); the warning block as a warning `banner` with an action.

---

## 4. Shares

### 4.1 `/shares` — Share list

Table: name, path, size used, cache mode, SMB on/off, NFS on/off, access summary.

Actions: create share, edit, delete (with explicit warning about data), browse.

**Components:** `data-table` (`p-table-8`) with SMB/NFS `status-badge`s; create share via `form-overlay`; `row-actions`; delete through `typed-confirm`; `empty-state`.

### 4.2 `/shares/[name]` — Share detail

**General** — name, real path, size used, per-disk distribution as of the last sync (which disks actually hold this share's files), included in parity yes/no.

**Allocation** — this share's create policy in plain language (doc 02 §1, Q11), with the tradeoff stated inline.

**Cache** — mode selector (three options per doc 02 §3), with the consequence of each stated inline. Changing the mode offers a share relocation job for existing files (doc 09 §2); nothing moves silently.

**SMB** — enable, guest access, read-only, browseable, recycle bin, Time Machine support with a maximum size (Q73), per-user and per-group access (no access / read-only / read-write), export path preview.

**NFS** — enable, allowed hosts and subnets, squash options, export line preview.

**Browse** — a simple file browser: navigate, see file sizes, see which underlying disk holds each file (mergerfs `user.mergerfs.basepath` xattr), delete. Not a full file manager; enough to answer "where did that go". Browsing is an explicit action and may wake disks.

**Danger zone** — delete share, with a clear statement of whether data is deleted or only the share definition removed. These must be two distinct actions with different confirmations.

**Components:** vertical `detail-tabs` (`p-tabs-4`), one tab per section above. Allocation and Cache: `choice-cards` with `plain-term`; changing the cache mode opens a `form-overlay` offering the relocation job. SMB: `setting-switch`es, the Time Machine size as `number-unit`; per-user and per-group access as a `data-table` with a `segmented-choice` per row (No access / Read-only / Read-write); export preview as `code-view`. NFS: `list-input` for hosts, squash Select (`p-select-23`), export line as `copy-value`. Browse: Breadcrumb with folder menu (`p-breadcrumb-4`) over a `data-table`; the wake warning is an `inline-note` shown before the first listing; delete through `confirm`. Danger zone: `danger-zone` with two separate actions — remove the definition (`confirm`) and delete the data (`typed-confirm`).

---

## 5. Apps

Scope per doc 04.

### 5.1 `/apps` — Installed

Card or table view of every container, including **unmanaged** containers created outside Hoserva (Portainer, manual `docker run`), clearly badged as such.

Per container: name, image and tag, state, uptime, CPU and RAM usage, ports with clickable links to the web UI where a port maps to one, update-available badge.

Actions: start, stop, restart, update, logs, detail, remove.

Bulk: update all, stop all, start all.

**Components:** `section-nav` (Installed / Catalog); card/table view as `segmented-choice` (`p-radio-group-7`); cards are CardFrames with a header action (`p-card-11`), the table is a `data-table`; *unmanaged* and *update available* `status-badge`s (outline, info); port links as Badges with links (`p-badge-12`); `row-actions`; bulk actions as a button Group; start/stop/restart with a promise `feedback-toast`; updates are jobs; missing Docker shows a `banner` with `copy-value`; `empty-state`.

### 5.2 `/apps/catalog` — App catalog

Searchable, category-filtered grid of templates. Per entry: icon, name, maintainer, short description, source repository, install count if available.

Filters: category, maintainer, installed / not installed, verified / community.

**Source indicator** on every entry — Hoserva curated or user-added repository. Users should know where a template came from.

**Components:** search InputGroup (`p-input-group-20`); category `multi-pick`; installed and verified filters as a ToggleGroup (`p-toggle-group-4`), since they are clearable filters; a grid of Cards (`p-card-1`) with the app icon as an Avatar with fallback (`p-avatar-1`) and a source `status-badge`; Pagination with page size (`p-pagination-3`); `empty-state` for no results.

### 5.3 `/apps/catalog/[id]` — Catalog app detail

Full description, screenshots, icon, maintainer, project and support links, the raw template, and a **privilege summary** computed from the template: does it request privileged mode, host networking, the Docker socket, or host path mounts outside the pool. Each flagged in plain language with an explanation of the risk.

**Components:** header Card with Avatar and source `status-badge`; screenshots in a horizontal ScrollArea (`p-scroll-area-2`); project, support and donate links as link Buttons (`p-button-17`); raw template as `code-view`; privilege summary as `stacked-checks`, each flag a warning Alert (`p-alert-6`); primary *Install* Button.

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

**Components:** one `form`; Basic/Advanced as segmented Tabs (`p-tabs-1`), since they switch panels; ports as `number-unit` (`p-number-field-7`) with a conflict shown as a Field error (`p-field-4`) plus a *use next free port* action; paths via `path-picker`; environment variables as Field + Input, secrets via `secret-input` with generate; network mode as `choice-cards` with a warning `banner` for host; a missing network shows `copy-value`; extra parameters in a Textarea (`p-textarea-5`); restart policy Select; resource limits as `number-unit`. Preview: `code-view`, expanded, with converter warnings as `banner`s. Run: `job-progress` plus `log-view`.

### 5.5 `/apps/[name]` — Container detail

Tabs:
- **Overview** — state, image, created, uptime, restart count, health check status, ports, mounted paths with the underlying disk shown
- **Logs** — live tail, search, download, adjustable line count
- **Stats** — CPU, RAM, network, disk IO graphs
- **Config** — the settings form again, editable; applying restarts the container
- **Update** — current tag vs. available, changelog link if the template provides one

Actions: start, stop, restart, recreate, remove (with a separate, clearly distinct option for whether to also delete appdata).

**Components:** `detail-tabs` (`p-tabs-2`): Overview Card with `status-badge`s, Logs `log-view`, Stats `chart`s, Config `form`, Update Card with version `status-badge`s. Actions as a button Group; remove goes through `confirm`, with a separate *also delete appdata* Checkbox with description (`p-checkbox-3`) that turns it into `typed-confirm`.

### 5.6 `/apps/[name]/compose` — Compose editor

Raw YAML editor with syntax highlighting and validation. Applying regenerates and recreates the stack. This is the power-user escape hatch; editing here marks the stack as "manually edited" so the template form doesn't silently overwrite the changes.

Also the entry point for **importing** an existing `docker-compose.yml`, either pasted or uploaded.

**Components:** the Q59 code editor in a Frame (`p-frame-1`) under a Toolbar (`p-toolbar-1`: validate, apply, import); validation errors as a `banner`; leaving with unapplied edits triggers `unsaved-guard`; a warning `status-badge` for *manually edited*; import via `file-upload` or pasting into the editor; apply goes through `confirm`, since it recreates the stack.

---

## 6. `/jobs` — Jobs

List of all jobs: type, target, status, started, duration, progress. Filterable by status and type.

Running jobs show live progress and a cancel action where cancellation is supported. Where it isn't, the button is absent and a tooltip explains why rather than showing a disabled control with no reason.

**Components:** `data-table` (`p-table-8`, card-style rows on mobile) with `table-filters` for status and type; running rows show `job-progress`; a cancel Button where cancellation is supported, otherwise an info icon with a Tooltip (`p-tooltip-1`) in its place.

### `/jobs/[id]` — Job detail

Full metadata, progress, complete captured stdout/stderr with search, downloadable log, and — on failure — the exit code plus a plain-language interpretation of common failure modes where one is known.

**Components:** metadata Card with `status-badge`; `job-progress`; `log-view` with a download Button (`p-button-16`); on failure, an error `banner` carrying the plain-language interpretation.

---

## 7. `/users` — Users and access

- User list: name, role, TOTP enabled, last login, share access summary
- **Roles** (Q27): *Admin* (full UI), *Viewer* (read-only UI), *Share-only* (SMB/NFS, no UI login — the default for new users, so a family member's share login never reaches a page that formats disks)
- Create, edit, delete users; setting a password updates the UI credential and the Samba passdb entry together
- Group management; share data is group `users` (GID 100), identical to Unraid (Q26)
- Per-user share permissions, editable from either side (here or on the share)
- Session list with revoke
- **A locked-out admin** is not recovered here: `hoserva user reset-password` over the root socket does it (Q78); every other user is reset by an admin on this page
- Personal API tokens, role-scoped, with revoke (Q43)
- **Note:** a user account can carry both UI login and SMB access. The UI must be explicit about which a given account has, because the dual purpose surprises people.
- OIDC configuration — roadmap, shown as a disabled section with a note rather than hidden

**Components:** `data-table` with role `status-badge`s; create and edit in a `side-panel` holding the `form`: role as `choice-cards`, and *UI login* and *SMB access* as two switch cards (`p-switch-4`) so which access an account has is always visible; password via `secret-input`; groups via `multi-pick`; share permissions as a `segmented-choice` per share. Sessions and API tokens as `data-table`s with revoke through `confirm`; a new token is shown once in a Dialog with `copy-value` and an anchored `feedback-toast`. OIDC: a disabled Fieldset (`p-fieldset-1`) with an `inline-note`.

---

## 8. Settings

**Components (all settings pages):** `section-nav` across the seven routes; each page is a `form` split into framed Cards with header and footer (`p-card-6`), with the save/cancel pair (`p-button-33`) in the footer.

### 8.1 `/settings` — General
Hostname, timezone, NTP servers, UI language, theme default, session timeout. **UPS** (Q77): connection (USB, or a network NUT server), on-battery behaviour, and the battery level or runtime that triggers the clean shutdown.

**Components:** hostname Input; `timezone`; NTP servers via `list-input`; default theme as a RadioGroup of image cards (`p-radio-group-6`); session timeout Select (`p-select-23`); UPS as a Card with the connection type as `choice-cards` and thresholds as `number-unit`. UI language stays hidden (Q48).

### 8.2 `/settings/network`
Interfaces, IP configuration (DHCP / static), DNS, gateway — editable when the host uses ifupdown and read-only with the reason otherwise; every change applies with a 60-second confirm-or-revert (Q75). HTTPS settings: self-signed cert regeneration, Let's Encrypt via DNS-01, certificate expiry. Access scope for the UI — LAN-only source filter by default (doc 01 §7, Q10) — with an explicit warning when allowing all sources. Port configuration (default `:8008`, TLS-only).

**Components:** interfaces as a `data-table` (`p-table-7`); DHCP/static as `segmented-choice`; certificate expiry as a `status-badge`; Let's Encrypt setup via `form-overlay`; access scope as a switch card (`p-switch-4`), where allowing all sources goes through `confirm`; port as `number-unit`; applying a network change shows a countdown `banner` with a confirm Button, and the previous configuration returns when it runs out.

### 8.3 `/settings/notifications`
Channels: email (SMTP), Gotify, ntfy, Discord webhook, generic webhook. Each with a **Send test notification** button — untested notification config is the same as no notification config.

Per-event routing matrix: which events go to which channels, at which severity.

Events to cover: SMART warning, SMART failure, disk offline, array degraded, sync succeeded (opt-in, off by default), sync failed, **sync blocked by threshold**, scrub found errors, pool above threshold, **any single disk near `minfreespace`** (doc 09 §5), cache above threshold, mover repeatedly skipping files, config drift detected, container unhealthy or crash-looping, container update available, Hoserva update available, reboot required, UPS on battery, UPS battery low, login failure burst, admin password or TOTP reset, certificate expiring, config backup failed, appdata backup failed, backup destination stale, restore drill failed.

Quiet hours, with a "critical alerts always deliver" override that cannot be disabled.

**Components:** each channel a CardFrame with header action (`p-card-11`) holding an enable `setting-switch` and a *Send test notification* Button (`loading`) that reports through `feedback-toast`; add channel via `form-overlay` with a type Select with icons (`p-select-9`); the routing matrix as a `data-table` (`p-table-7`) with Checkbox cells and a severity Select per row; quiet hours as labelled From/To time groups (`p-switch-9`); the critical-alerts override as a checked, disabled Switch with description (`p-switch-2`, `p-switch-3`).

### 8.4 `/settings/schedules`
All recurring jobs in one place: the nightly maintenance chain, SMART self-tests, appdata backup, restore drill, container update checks.

**The nightly maintenance chain** (Q30) is one schedule entry with ordered steps: mover → diff + threshold guard → sync → config backup, plus scrub on the weekly day. Each step starts when the previous one finishes, so the mover-before-sync order cannot be broken by a slow run. Individual steps can be disabled, not reordered.

Per job: enabled, cron-like schedule with a human-readable preview ("every day at 02:00"), and the next run time. Conflict detection covers jobs scheduled outside the chain — per doc 01 §4's mutually exclusive classes, when two scheduled windows overlap and the scheduler would refuse to run them together (e.g. two Parity jobs at once, or two Array-write jobs on the same disks).

**Components:** the nightly chain as `stacked-checks` with one `setting-switch` per step and no reordering control; every other job as a Card with an enable Switch, frequency Select, time Input, the human-readable preview and the next run; conflicts as a warning `banner`.

### 8.5 `/settings/backup`
Full design in doc 10.

**Destinations** — list of backup destinations (local path, SMB/NFS, S3-compatible, SFTP, WebDAV, rclone remote), each with enabled, retention, encryption, last successful backup, and a **Test connection** button that writes and reads back a file. A fresh install has two local destinations: the boot device and a pool path (Q40).

**Config backup** — the archive described in doc 10 §1; manual run and download; schedule is part of the nightly chain. Backup passphrase set/change (Q28).

**Restore** — upload an archive, preview what it contains and what will change, apply.

**Appdata backup** — scope, per-container stop policy (with known database images flagged), destinations, schedule, retention.

**Restore drill** — last result and next run.

**Components:** destinations as a `data-table` with a *Test connection* Button (`loading`) reporting through `feedback-toast`; add destination via `form-overlay`; config backup Card with run and download Buttons (`p-button-16`); passphrase via `form-overlay` with `secret-input`; restore as `file-upload`, then a `grouped-results` preview, then `typed-confirm`; appdata backup as a `form` with a per-container stop-policy `data-table` and warning `status-badge`s on database images; restore drill as a Card with a `status-badge`.

### 8.6 `/settings/updates`
Current Hoserva version, available version, changelog, update channel (stable / beta), update action, **rollback to the previous version** (Q67), update check on/off (read from Hoserva's own release index, never a system-wide `apt update` — Q49, Q67). Pending Debian updates and whether a reboot is required: security updates install unattended, and **Reboot** is always the user's action (Q68). Also: available updates for mergerfs and SnapRAID, and whether they fall inside the version range the installed Hoserva release was tested against (Q7); installed versions are read from package metadata.

Updating runs `apt` in a transient systemd unit so the daemon can restart itself. It is refused while a Parity, Array-write or Topology job is running (doc 01 §4), and a config backup runs automatically first (doc 10 §1). Reboot waits for the same jobs and runs the clean shutdown sequence (Q70).

**Components:** version Card with `status-badge`; changelog in a ScrollArea with fading edges (`p-scroll-area-4`); channel as `segmented-choice`; update check as `setting-switch`; dependency versions as a `data-table` (`p-table-7`) with in-range `status-badge`s; *Update*, *Rollback* and *Reboot* go through `confirm`, and when refused, a `banner` names the job that blocks it.

### 8.7 `/settings/advanced`
Raw mergerfs options, raw SnapRAID extra options, custom Samba include file, API bind settings, debug logging level, and the config-drift management view (which files are managed, which are unmanaged, hashes, and the option to take or release ownership of each).

**Components:** raw option fields as Q59 editors in expanded `code-view`s; drift management as a `data-table` with managed/unmanaged `status-badge`s and `row-actions` (take or release ownership, through `confirm`); a file's diff opens in a Dialog (`p-dialog-5`).

---

## 9. Tools

**Components (all tools pages):** `section-nav` across the tools routes.

### 9.1 `/tools/logs`
System log viewer (journald) with level filter, unit filter, search, time range, and download. Plus Hoserva's own application log as a separate stream.

**Components:** `log-view`; journald vs. application log as Tabs (`p-tabs-1`); unit filter via `multi-pick`; `time-window`; download Button (`p-button-16`).

### 9.2 `/tools/terminal`
Browser terminal (ttyd or equivalent). **Off by default.** Enabling requires a confirmation that states plainly this is root shell access to the host.

**Components:** the Q59 terminal in a Frame (`p-frame-1`); enabling it is a switch card (`p-switch-4`) followed by a `confirm` stating that this is root shell access.

### 9.3 `/tools/migrate`
The Unraid migration workspace. Detailed in doc 05. Four phases: scan, review, import, verify.

**Components:** `wizard` over the four phases. Scan: source as `choice-cards` (Flash Backup via `file-upload`, or the stick attached read-only); the report as `grouped-results` with a download Button. Review: disk mapping as a `data-table` with a role Select per row; share and template previews as `data-table`s with warning-count `status-badge`s; the unprotected-window warning as a non-dismissible error `banner`; VMs found (Phase 3.5) as a `data-table`. Import: `job-progress`. Verify: a comparison `data-table` (`p-table-7`) with `status-badge`s; *Initialise parity* is `typed-confirm` — the point of no return.

### 9.4 `/tools/diagnostics`
One-click diagnostic bundle: `hoserva doctor` output, version info, config (secrets redacted), recent logs, job history, disk inventory, SMART reports. Downloadable as a single archive for bug reports.

Redaction must be verifiable — the UI shows what was removed, and the archive is inspectable before sending. Users paste these into public forums. One redaction engine serves this bundle and the assistant's tool boundary (doc 11 §3).

**Components:** a generate Button that starts a job (`job-progress`); removed items as `grouped-results`; archive contents in a ScrollArea (`p-scroll-area-1`); download Button (`p-button-16`).

### 9.5 `/tools/assistant` — post-1.0

Free-form assistant chat and provider configuration, per doc 11. Not built before 1.0 (Q47); listed so the route is reserved.

**Components:** decided when doc 11 work starts; candidates are the command palette with assistant (`p-command-2`) and the chat input (`p-input-group-29`).

---

## 10. Page priority for implementation

Not everything ships at once. Order by what makes the product usable:

| Tier | Pages |
|---|---|
| **1 — Minimum viable** (Phase 1) | `/welcome`, `/login`, `/storage/setup`, `/storage`, `/storage/disks`, `/storage/disks/wake-events` (event log), `/storage/parity`, `/`, `/jobs`, `/settings/notifications`, `/settings/schedules` (nightly chain), `/settings`, `/settings/updates` |
| **2 — Actually a NAS** (Phase 2) | `/shares`, `/shares/[name]`, `/storage/cache`, `/users`, `/settings/backup`, `/settings/network` |
| **3 — Complete home server** (Phase 3) | `/apps` and all sub-pages, `/tools/migrate` |
| **3.5 — Virtual machines** (Phase 3.5, doc 14) | `/vms` and all sub-pages, including `/tools/migrate`'s VM-import phase |
| **4 — Polish** (Phase 4) | `/storage/disks/[id]` history graphs, wake attribution, `/tools/logs`, `/tools/diagnostics`, `/settings/advanced`, `/tools/terminal` |
| **Post-1.0** | `/tools/assistant` |

Notifications and a sync/scrub schedule are Tier 1 because Phase 1's definition of done is a clean soak test (doc 07 §1) — an array that cannot report a dead disk or a blocked sync is not one to trust with real data. Config backup exists from Phase 1 via the CLI (`hoserva config export`); its UI arrives in Tier 2.

Tier 1 plus Tier 2 is a working, honest NAS. Tier 3 makes it a complete home server, and is what users migrating an existing setup need.

---

## 11. VMs

Scope per doc 14. Numbered out of sequence because it was added after the rest of this spec (Phase 3.5) — see the nav structure above for where it actually sits in the sidebar, between Apps and Jobs.

### 11.1 `/vms` — VM list

Card view of every defined VM: name, OS icon (user-selected at creation, not auto-detected), state (running / stopped / paused), vCPU and RAM allocation, assigned passthrough devices if any, autostart toggle.

Actions: start, stop (graceful and force), restart, console, detail, remove.

**Components:** `section-nav` (VMs / Passthrough); each VM a CardFrame with header action (`p-card-11`) holding a state `status-badge`, an autostart `setting-switch` and passthrough devices as Badges; `row-actions`; *Console* Button; `empty-state`.

### 11.2 `/vms/create` — Create VM

- **Basics** — name, OS type (for sane defaults only, e.g. firmware choice), vCPUs, RAM, autostart
- **Storage** — new qcow2 disk (size picker, share-aware placement per Q12/Q51) or an existing image path; optional ISO for installation, picked from a share
- **Network** — bridged (`vmbr0`, default) or isolated (Q54); no network-creation UI here either, same discipline as doc 04 §5's Q37
- **Passthrough** — optional, only shown when at least one device passed `hoserva vm passthrough check` (§11.4); each candidate device shows its compatibility verdict inline, not just its name
- **Preview** — the generated domain XML, visible before creation, mirroring doc 04's "always reviewable" rule for Compose

**Components:** one `form` in Fieldsets (`p-fieldset-1`) rather than a `wizard`; vCPUs and disk size as `number-unit`; RAM as a slider with input (`p-slider-12`); ISO and disk location via `path-picker`; network as `choice-cards`; passthrough as a CheckboxGroup with disabled items (`p-checkbox-group-2`) and a verdict `status-badge` per device; preview as `code-view`.

### 11.3 `/vms/[name]` — VM detail

Tabs:
- **Console** — embedded noVNC viewer over the existing authenticated connection (doc 14 §4); no separate login
- **Overview** — state, uptime, resource allocation, assigned passthrough devices, disk paths and their share/disk
- **Config** — the creation form again, editable while stopped; most fields are disabled while running, with an explanation of why
- **Snapshots** — where supported by the storage backend; not a substitute for backup, and the UI says so

Actions mirror `/vms`, plus clone and export.

**Components:** `detail-tabs` (`p-tabs-2`). Console: noVNC (Q59) in a Frame under a Toolbar (`p-toolbar-1`: Ctrl+Alt+Del, fullscreen). Config: `form` with fields that are disabled while running explained by a Tooltip (`p-tooltip-1`). Snapshots: `data-table` plus an `inline-note` that snapshots are not backups. Delete via `danger-zone` with a *keep disks* Checkbox with description (`p-checkbox-3`).

### 11.4 `/vms/passthrough` — Passthrough

Read-only IOMMU group listing at all times (doc 14 §3): every group, its member devices, and an ACS isolation verdict per group. Devices the host itself needs (boot controller, sole console GPU) are shown but marked **not assignable**, with the reason stated rather than just disabled.

Assigning a device to a VM queues a **VFIO binding change**, clearly flagged as requiring a reboot to take effect — never applied live. GPU passthrough candidates get an extra single-GPU warning where the compatibility check detects that configuration (doc 14 §3).

**Components:** IOMMU groups as `grouped-results`, each holding a device `data-table` (`p-table-7`) with an ACS `status-badge`; not-assignable devices get an outline `status-badge` with the reason inline; assigning opens a `form-overlay` with a VM Select (`p-select-23`); pending VFIO changes show a warning `banner` that stays until reboot; the single-GPU warning is a `banner`.
