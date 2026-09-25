-- The central schema (D16, doc 01 §4). This is the only hand-edited
-- description of the database — every other schema artifact is generated
-- from it: `make db-migration` diffs this file against the schema the
-- existing migrations produce and writes the next file under
-- internal/store/migrations/, and `make gen` runs sqlc against this file
-- to produce internal/store/db/. Nothing else declares a table.
--
-- schema_info is deliberately the only table this issue adds. Issue #18 is
-- the schema/migration/query/runner pipeline itself, not a feature: the
-- tables real features need — jobs (#19), users and sessions (#22), disks,
-- shares — belong to the issues that design them, not to a foundation issue
-- guessing their shape. schema_info gives that pipeline one genuine,
-- permanent row to exercise end to end instead of an invented placeholder:
-- every install has exactly one row here, written once at first startup,
-- read by anything that needs to identify "this installation" (diagnostics
-- bundles, doc 03 §9.4; update/rollback, doc 01 §3).
CREATE TABLE schema_info (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    installation_id TEXT NOT NULL,
    created_at TEXT NOT NULL,
    hostname TEXT,
    timezone TEXT,
    backup_passphrase BLOB,
    update_channel TEXT NOT NULL DEFAULT 'stable' CHECK (update_channel IN ('stable', 'beta')),
    update_check_enabled INTEGER NOT NULL DEFAULT 1 CHECK (update_check_enabled IN (0, 1)),
    previous_version TEXT
) STRICT;

-- Jobs (#19, doc 01 §4): the persisted record behind every long-running
-- operation. "type" and "status" are quoted (Q60: sqldef's SQLite parser
-- rejects "status" unquoted, and rejects "type" unquoted too). class and
-- resumable/cancellable are derived, in Go, from type — never trusted from
-- a caller — so the CHECK constraints here are a second, database-level
-- guard against a row that names an exclusion class or resumability the
-- type doesn't actually have.
-- resource_ids is a JSON array of the disk/container/VM ids a scoped job's
-- mutual-exclusion check compares (doc 01 §4's "same disks" / "same
-- container" / "same VM" rows); NULL for job types the scheduler never
-- scopes. checkpoint is the opaque, resumable-job-type checkpoint blob
-- (Q29); NULL until the first checkpoint is saved, and only ever written
-- for a resumable type. params is the request payload for job types that
-- have one (JSON object: sync's dryRun/confirm, scrub's percent, fix's
-- confirm/disk); NULL for types that have none. It is never resource_ids
-- or checkpoint: those stay exclusion keys and resume state. Declared
-- last because SQLite's ALTER TABLE ADD COLUMN can only append (Q60).
-- There is no log-path column: job stdout/stderr lives in compressed
-- files under a log directory the daemon passes in (Q74), named by job
-- id, so nothing here duplicates that path.
CREATE TABLE jobs (
    id TEXT PRIMARY KEY,
    "type" TEXT NOT NULL,
    class TEXT NOT NULL CHECK (class IN ('parity', 'array_write', 'topology', 'service', 'vm')),
    "status" TEXT NOT NULL CHECK ("status" IN ('queued', 'running', 'interrupted', 'succeeded', 'failed', 'cancelled')),
    progress INTEGER,
    resumable INTEGER NOT NULL CHECK (resumable IN (0, 1)),
    cancellable INTEGER NOT NULL CHECK (cancellable IN (0, 1)),
    resource_ids TEXT,
    checkpoint BLOB,
    error_code TEXT,
    error_message TEXT,
    created_at TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT,
    params TEXT
) STRICT;

CREATE INDEX jobs_status_idx ON jobs ("status");
CREATE INDEX jobs_class_idx ON jobs (class);

-- Users and sessions (#22, #49, doc 01 §7, Q27, Q28, Q44). No default
-- credential ever exists (doc 01 §7): the first row is written by the
-- first-run setup flow, atomically, and only while this table is empty.
-- role's 'share-only' value (#49) is a real, admin-manageable row with no
-- UI login at all — Login refuses it outright, and every operation but
-- the public ones fails closed against it since it satisfies neither
-- RoleViewer nor RoleAdmin. totp_secret and
-- totp_pending_secret are both encrypted with the machine key (Q28)
-- before either reaches this table, so a leaked database file alone
-- never yields a usable TOTP secret. totp_secret is the *active*,
-- confirmed credential a login actually checks — NULL until the first
-- confirmTotp call ever succeeds, at which point totp_confirmed_at is
-- set and (barring a later re-enrolment) never cleared again.
-- totp_pending_secret is a separate, independent column for a secret
-- enrollTotp just generated but confirmTotp has not yet activated: it
-- exists so that starting or replacing a pending enrolment can never, by
-- itself, disturb an already-active credential — confirmTotp is the only
-- path that ever promotes a pending secret into totp_secret. totp_last_step
-- is the last TOTP step accepted against the *active* secret, enforcing
-- "replay protection within a step" (doc 01 §7) — 0 (the Unix epoch's own
-- step) before any code has ever been accepted.
-- totp_pending_secret is declared last, not next to totp_secret, because
-- it was added by 0004 after 0003 already created this table (Q60:
-- SQLite's ALTER TABLE ADD COLUMN can only append) — replaying every
-- migration has to reproduce this exact column order for db-check's own
-- schema-drift comparison to pass.
--
-- last_login_at and smb_credential_set_at (#225, doc 03 §7) are declared
-- last for the same append-only reason. last_login_at is set only by a
-- successful Login — never derived from session liveness, so it still
-- reads "never logged in" (NULL) once a session expires or is revoked.
-- smb_credential_set_at is set the first time setUserPassword's Samba
-- write succeeds for this account and, since there is no separate
-- "revoke SMB access" operation short of deleting the whole account
-- (Q27: one password sets both surfaces), never cleared again — it
-- answers "has a Samba credential ever been provisioned for this
-- account", which is exactly "is SMB access currently on" given that.
CREATE TABLE users (
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('admin', 'viewer', 'share-only')),
    totp_secret BLOB,
    totp_confirmed_at TEXT,
    totp_last_step INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    totp_pending_secret BLOB,
    last_login_at TEXT,
    smb_credential_set_at TEXT
) STRICT;

-- Enforces "creating the admin is atomic — a race between two setup
-- requests can't create two admins" (#22) at the database level, not just
-- in application code: a partial unique index limits the whole table to
-- at most one 'admin' row, so two concurrent createFirstAdmin inserts can
-- never both succeed, regardless of how their transactions interleave.
-- User management (adding further admins) is out of this issue's scope
-- (Q43's Phase 2), so this is not a limit on Hoserva ever having more
-- than one admin — only on how the very first one is created.
CREATE UNIQUE INDEX users_one_admin_idx ON users (role) WHERE role = 'admin';

-- token_hash is the SHA-256 of the random 256-bit token the session
-- cookie carries (doc 01 §7): the raw token is never stored, so a leaked
-- database file cannot be replayed as a live session, mirroring
-- password_hash's own reasoning above.
CREATE TABLE sessions (
    token_hash TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users (id),
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
) STRICT;

CREATE INDEX sessions_user_id_idx ON sessions (user_id);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

-- Personal API tokens (#50, Q43, doc 01 §5, §7): scoped to admin or
-- viewer, for scripting and the remote CLI over TCP. token_hash is the
-- SHA-256 of the random 256-bit raw token, exactly like sessions above —
-- the raw value is shown once, at creation, and never stored. role can
-- never exceed the owning account's own role at creation time
-- (AuthService.CreateAPIToken), and is capped to the account's *current*
-- role on every use (effectiveTokenRole) rather than only at creation, so
-- a token issued while its account was admin is never usable at more
-- than viewer the moment that account is demoted, with no separate
-- revocation step needed. name is a caller-chosen label so an account
-- with more than one token can tell them apart in the /users page's
-- data-table (doc 03 §7).
CREATE TABLE api_tokens (
    token_hash TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('admin', 'viewer')),
    created_at TEXT NOT NULL
) STRICT;

CREATE INDEX api_tokens_user_id_idx ON api_tokens (user_id);

-- Spin-state events (#110, Q32, Q74): every observed standby/active
-- transition, persisted so the wake-events view (doc 03 §3.3a) survives a
-- daemon restart — internal/disk's SpinEventLog is the in-process record a
-- single run builds while polling; this table is where that history
-- actually lives across restarts. Retained for two years, like the audit
-- log below, and unlike metrics.db's downsampled SMART/temperature time
-- series (a separate database, Q74) or a job's captured stdout/stderr
-- (compressed files, kept 90 days — internal/job's LogStore).
CREATE TABLE spin_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    device TEXT NOT NULL,
    from_state TEXT NOT NULL CHECK (from_state IN ('active', 'standby')),
    to_state TEXT NOT NULL CHECK (to_state IN ('active', 'standby')),
    at TEXT NOT NULL
) STRICT;

CREATE INDEX spin_events_at_idx ON spin_events (at);
CREATE INDEX spin_events_device_idx ON spin_events (device);

-- Audit log (#110, doc 01 §7, Q74): configuration changes and destructive
-- actions, with actor and timestamp. This table and its retention are
-- this issue's scope; the write path — calling INSERT from an actual
-- handler — belongs to whichever issue adds actor logging to internal/api
-- (doc 01 §7's "audit-logged" requirements on account recovery and
-- passthrough attach/detach, doc 14 §3).
CREATE TABLE audit_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    actor TEXT NOT NULL,
    action TEXT NOT NULL,
    detail TEXT,
    at TEXT NOT NULL
) STRICT;

CREATE INDEX audit_log_at_idx ON audit_log (at);

-- Notification channels (#35, Q28): each row is one configured alerting
-- destination (email, gotify, ntfy, discord webhook, generic webhook).
-- config holds every non-secret field the channel type needs, JSON-encoded
-- by internal/notify; secret is nullable and, when present, is ciphertext
-- from internal/auth.MachineKey.Encrypt (nonce||AES-256-GCM ciphertext) —
-- the SMTP password, Gotify app token, ntfy auth token, Discord webhook
-- URL or generic webhook auth header value, whichever the channel type
-- has. A leaked database file alone never yields a usable credential.
CREATE TABLE notify_channels (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    "type" TEXT NOT NULL CHECK ("type" IN ('email', 'gotify', 'ntfy', 'discord', 'webhook')),
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    config TEXT NOT NULL,
    secret BLOB,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

-- Per-event routing (doc 03 §8.3's routing matrix): presence of a row
-- means event_type is routed to channel_id. Deleting a channel drops
-- every route that named it. There is deliberately no "enabled" column
-- here — unrouting an event from a channel is removing the row, not
-- flagging one.
CREATE TABLE notify_routes (
    event_type TEXT NOT NULL,
    channel_id TEXT NOT NULL REFERENCES notify_channels (id) ON DELETE CASCADE,
    PRIMARY KEY (event_type, channel_id)
) STRICT;

CREATE INDEX notify_routes_channel_id_idx ON notify_routes (channel_id);

-- Per-event severity overrides (doc 03 §8.3's per-row severity Select).
-- internal/notify's fixed event catalog carries a compiled-in default
-- severity for every event type; a row here overrides it. No row means
-- "use the compiled-in default" — every event type is valid whether or
-- not it has ever been overridden, so there is nothing to seed here.
CREATE TABLE notify_event_severity (
    event_type TEXT PRIMARY KEY,
    severity TEXT NOT NULL CHECK (severity IN ('info', 'warning', 'error', 'critical'))
) STRICT;

-- Quiet hours (doc 03 §8.3): one row, id=1, the same singleton pattern as
-- schema_info above. The "critical alerts always deliver" override is not
-- a column here — doc 03 §8.3 and CLAUDE.md both require it to be
-- impossible to disable, so internal/notify hardcodes it rather than
-- reading a value that could ever be set to false.
CREATE TABLE notify_quiet_hours (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    start_time TEXT NOT NULL,
    end_time TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

-- Delivery queue (#35): every notification a channel should receive is one
-- row here, sent by internal/notify's own delivery worker and retried on
-- failure — "delivery failures retried and logged, never silently
-- dropped" survives a daemon restart because the row, not an in-memory
-- queue, is what durably says a delivery is still owed. `suppressed` is
-- not a failure: quiet hours can suppress every severity but critical,
-- and this is where a would-be delivery is still recorded, so "why
-- nothing was sent" is never silent either.
CREATE TABLE notify_deliveries (
    id TEXT PRIMARY KEY,
    channel_id TEXT NOT NULL REFERENCES notify_channels (id) ON DELETE CASCADE,
    event_type TEXT NOT NULL,
    severity TEXT NOT NULL CHECK (severity IN ('info', 'warning', 'error', 'critical')),
    title TEXT NOT NULL,
    message TEXT NOT NULL,
    "status" TEXT NOT NULL CHECK ("status" IN ('pending', 'delivered', 'failed', 'suppressed')),
    attempts INTEGER NOT NULL,
    last_error TEXT,
    created_at TEXT NOT NULL,
    next_attempt_at TEXT NOT NULL,
    delivered_at TEXT
) STRICT;

CREATE INDEX notify_deliveries_status_idx ON notify_deliveries ("status", next_attempt_at);
CREATE INDEX notify_deliveries_channel_id_idx ON notify_deliveries (channel_id);

-- In-app notification inbox (#188, doc 03 §2): every alert Service.Publish
-- records for the top-bar bell. read_at is NULL while unread; mark-all-read
-- and per-alert read set it. event_type groups the inbox (doc 03 §2); id is
-- the same key NotificationEvent carries over SSE (D18).
CREATE TABLE notify_alerts (
    id TEXT PRIMARY KEY,
    event_type TEXT NOT NULL,
    severity TEXT NOT NULL CHECK (severity IN ('info', 'warning', 'error', 'critical')),
    title TEXT NOT NULL,
    message TEXT NOT NULL,
    created_at TEXT NOT NULL,
    read_at TEXT
) STRICT;

CREATE INDEX notify_alerts_read_at_idx ON notify_alerts (read_at);
CREATE INDEX notify_alerts_created_at_idx ON notify_alerts (created_at);

-- The machine key's check value (#22, Q28). Written once, the moment
-- this installation's machine key is first generated, and read at every
-- later start (auth.LoadOrGenerateMachineKey) to prove a candidate key
-- file is really the one protecting every already-encrypted secret
-- column: a lost or swapped key file must be a fatal startup error, not
-- a silent regeneration that permanently strands every TOTP secret it
-- can no longer decrypt. check_value is an HMAC-SHA256 of a fixed
-- constant computed under the key — never the key itself, and never
-- reversible back to it.
CREATE TABLE machine_key_check (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    check_value BLOB NOT NULL,
    created_at TEXT NOT NULL
) STRICT;

-- Array topology (#180, D4, doc 01 §2, doc 02 §1): the wizard's plan after
-- a successful create-array job, which is the source of truth config
-- generators read. One array per install in v1 (singleton, same pattern as
-- schema_info). Job-params JSON is the queued request only — after the
-- Topology job succeeds, these tables are what Render and WritePoolMounts
-- consume, never the job row. Expand-only (D16): nothing here is dropped.
-- create_policy / min_free_space are the pool-wide mergerfs options the
-- wizard collected (doc 02 §1); omitted request fields persist as the
-- engine defaults (mspmfs / 50G).
CREATE TABLE array_settings (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    create_policy TEXT NOT NULL,
    min_free_space TEXT NOT NULL,
    created_at TEXT NOT NULL
) STRICT;

-- One assigned disk per row. role_index is 1-based for the documented
-- mountpoints (doc 01 §6): /mnt/diskN, /mnt/parityN, /mnt/cache (cache is
-- always role_index 1). fs_uuid is the filesystem UUID mounts bind to
-- (Q21), recorded after FormatPlan succeeds — a failed format never
-- inserts a row. size_bytes is capacity at join time (Q21 weak-identity
-- match); NULL for rows written before that column existed.
-- removal_state is doc 09 §4's own disk-removal state machine (#359,
-- #358): NULL for a disk not in removal; 'evacuating' from before an
-- evacuation's first copy until its post-check passes (step 2, every
-- pool mount marks this disk NC); 'evacuated' once that copy and
-- post-check finish but the disk is still in every mergerfs branch list,
-- SnapRAID layout and mount table (steps 7-9 have not run yet);
-- 'unpooled' and 'unlisted' are #358's own later steps of that same
-- removal. All four are declared now, in one column, because SQLite
-- cannot widen a CHECK constraint without rebuilding the table, which
-- D16 forbids outside an expand/contract change. removal_job_id is the
-- id of the evacuation job that set removal_state: cancelling that job
-- clears the state, cancelling any other job never does. It is NULL
-- whenever removal_state is.
-- UNIQUE(device) and UNIQUE(fs_uuid) are a second, database-level guard
-- against one physical disk (or one filesystem) holding two roles.
-- Identities (wwn/serial/by_id_name/weak_identity) are copied from the
-- Provider.List call that populated the wizard.
CREATE TABLE array_disks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    role TEXT NOT NULL CHECK (role IN ('parity', 'data', 'cache')),
    role_index INTEGER NOT NULL CHECK (role_index >= 1),
    device TEXT NOT NULL,
    filesystem TEXT NOT NULL,
    fs_uuid TEXT NOT NULL,
    size_bytes INTEGER,
    wwn TEXT,
    serial TEXT,
    by_id_name TEXT,
    weak_identity INTEGER NOT NULL DEFAULT 0 CHECK (weak_identity IN (0, 1)),
    mountpoint TEXT NOT NULL,
    removal_state TEXT CHECK (removal_state IS NULL OR removal_state IN ('evacuating', 'evacuated', 'unpooled', 'unlisted')),
    removal_job_id TEXT,
    UNIQUE (role, role_index),
    UNIQUE (device),
    UNIQUE (fs_uuid),
    UNIQUE (mountpoint)
) STRICT;

CREATE INDEX array_disks_role_idx ON array_disks (role, role_index);

-- Recurring schedules (#197, doc 03 §8.4, Q30): the nightly maintenance
-- chain and separately scheduled jobs. Next-run times and conflict
-- detection are computed by the daemon from these rows plus the
-- installation timezone in schema_info — never by the UI. last_run_at is
-- the RFC3339 UTC instant the daemon claimed the current window (#200);
-- NULL means the chain has never started. Settings upserts leave it
-- untouched so an overlapping restart cannot re-fire the same night.
CREATE TABLE schedule_chain (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    start_time TEXT NOT NULL,
    weekly_scrub_day INTEGER NOT NULL CHECK (weekly_scrub_day BETWEEN 0 AND 6),
    mover_enabled INTEGER NOT NULL CHECK (mover_enabled IN (0, 1)),
    diff_guard_enabled INTEGER NOT NULL CHECK (diff_guard_enabled IN (0, 1)),
    sync_enabled INTEGER NOT NULL CHECK (sync_enabled IN (0, 1)),
    scrub_enabled INTEGER NOT NULL CHECK (scrub_enabled IN (0, 1)),
    config_backup_enabled INTEGER NOT NULL CHECK (config_backup_enabled IN (0, 1)),
    last_run_at TEXT,
    updated_at TEXT NOT NULL
) STRICT;

CREATE TABLE schedule_jobs (
    job_id TEXT PRIMARY KEY CHECK (job_id IN ('smart_self_test', 'appdata_backup', 'restore_drill', 'container_update_check')),
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    frequency TEXT NOT NULL CHECK (frequency IN ('daily', 'weekly', 'monthly')),
    start_time TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

-- Q76 host-config onboarding: one row per detected category the user
-- chose to import or leave unmanaged. facts is JSON of the parsed Samba
-- shares, NFS exports, fstab mounts, or Docker inventory — enough for a
-- later generate to take ownership, not a Shares product. leave records
-- the choice so config.Generator never writes that host file (doc 01 §2).
CREATE TABLE host_config (
    kind TEXT PRIMARY KEY CHECK (kind IN ('samba', 'nfs', 'fstab', 'docker_containers', 'docker_images')),
    decision TEXT NOT NULL CHECK (decision IN ('import', 'leave')),
    facts TEXT NOT NULL,
    applied_at TEXT NOT NULL
) STRICT;

-- Shares (#46, D4, doc 02 §1, doc 03 §4): one row per user share. Name is
-- the primary key and the path segment under every branch and under
-- /mnt/user (pool.ValidateShareName). Cache mode and create policy drive
-- the existing per-share mergerfs mount (Q12, Q11); SMB columns drive
-- generated samba/smb.conf (Q73 for Time Machine max size); NFS columns
-- drive generated /etc/exports (doc 03 §4.2). Per-user ACLs belong to a
-- later issue. Expand-only (D16).
CREATE TABLE shares (
    name TEXT PRIMARY KEY,
    cache_mode TEXT NOT NULL CHECK (cache_mode IN ('cache-then-move', 'cache-only', 'array-only')),
    create_policy TEXT NOT NULL CHECK (create_policy IN ('mspmfs', 'mfs', 'lfs', 'ff')),
    smb_enabled INTEGER NOT NULL CHECK (smb_enabled IN (0, 1)),
    smb_guest INTEGER NOT NULL CHECK (smb_guest IN (0, 1)),
    smb_read_only INTEGER NOT NULL CHECK (smb_read_only IN (0, 1)),
    smb_browseable INTEGER NOT NULL CHECK (smb_browseable IN (0, 1)),
    smb_recycle INTEGER NOT NULL CHECK (smb_recycle IN (0, 1)),
    smb_time_machine INTEGER NOT NULL CHECK (smb_time_machine IN (0, 1)),
    smb_time_machine_max_size TEXT,
    nfs_enabled INTEGER NOT NULL DEFAULT 0 CHECK (nfs_enabled IN (0, 1)),
    nfs_hosts TEXT NOT NULL DEFAULT '[]',
    nfs_squash TEXT NOT NULL DEFAULT 'root_squash' CHECK (nfs_squash IN ('root_squash', 'no_root_squash', 'all_squash')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

-- Let's Encrypt DNS-01 (#211, Q9, Q28): one row, id=1, the same singleton
-- pattern as schema_info. dns_secret and account_key are ciphertext from
-- internal/auth.MachineKey.Encrypt (nonce||AES-256-GCM) — Cloudflare API
-- token or RFC 2136 TSIG secret, and the ACME account private key. A
-- leaked database file alone never yields a usable credential. HTTP-01
-- is not represented here: ports 80/443 are never claimed (Q9).
CREATE TABLE acme_config (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    domain TEXT NOT NULL,
    provider TEXT NOT NULL CHECK (provider IN ('cloudflare', 'rfc2136')),
    provider_config TEXT NOT NULL,
    dns_secret BLOB NOT NULL,
    account_key BLOB NOT NULL,
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    last_error TEXT,
    updated_at TEXT NOT NULL
) STRICT;

-- Disks outside the array (Q72, doc 02 §4): Ignore-role or a later USB
-- disk, mounted on request at /mnt/disks/<label>. Never written into
-- array_disks (that table's role CHECK is parity|data|cache only). The
-- backup_destination flag is whether this disk's mount is a local
-- config-backup path (doc 10 §1); the mountpoint is also the stable
-- bind-mount source for a container path.
CREATE TABLE external_disks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    label TEXT NOT NULL CHECK (length(label) >= 1),
    device TEXT NOT NULL,
    filesystem TEXT NOT NULL,
    fs_uuid TEXT NOT NULL,
    wwn TEXT,
    serial TEXT,
    by_id_name TEXT,
    weak_identity INTEGER NOT NULL CHECK (weak_identity IN (0, 1)),
    mountpoint TEXT NOT NULL CHECK (mountpoint GLOB '/mnt/disks/*'),
    backup_destination INTEGER NOT NULL CHECK (backup_destination IN (0, 1)),
    UNIQUE (label),
    UNIQUE (device),
    UNIQUE (fs_uuid),
    UNIQUE (mountpoint)
) STRICT;

CREATE INDEX external_disks_device_idx ON external_disks (device);

-- User groups (#49, Q27, doc 03 §7): a named collection of accounts, kept
-- purely for bulk share-permission assignment ("per-group share access,
-- editable from either side") — distinct from the fixed `users` system
-- group (GID 100, Q26) every share's files belong to on disk, which this
-- table has no bearing on.
CREATE TABLE user_groups (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
) STRICT;

-- Group membership (#49). Deletes are issued explicitly by application
-- code, not by this FK's ON DELETE CASCADE alone: store.DSN's runtime
-- connections don't run with "PRAGMA foreign_keys = ON" (see
-- internal/notify/store.go's DeleteChannel for the same reasoning), so the
-- cascade here documents intent and backs PRAGMA foreign_key_check during
-- a migration, but a user or group row is still cleaned up in application
-- code before it is deleted.
CREATE TABLE user_group_members (
    group_id TEXT NOT NULL REFERENCES user_groups (id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, user_id)
) STRICT;

CREATE INDEX user_group_members_user_id_idx ON user_group_members (user_id);

-- Per-user share access (#49, Q27, doc 03 §7): none/read-only/read-write,
-- editable from the user or the share. A user with no row for a share is
-- not represented — none of the three levels is assumed. Enforcing this
-- in generated Samba/mergerfs config, and the file ownership/mode a grant
-- implies, is a later issue; this table is the stored grant itself.
CREATE TABLE share_user_permissions (
    share_name TEXT NOT NULL REFERENCES shares (name) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    access TEXT NOT NULL CHECK (access IN ('none', 'read-only', 'read-write')),
    PRIMARY KEY (share_name, user_id)
) STRICT;

CREATE INDEX share_user_permissions_user_id_idx ON share_user_permissions (user_id);

-- Per-group share access (#49, Q27, doc 03 §7) — the group-side twin of
-- share_user_permissions above, same caveats.
CREATE TABLE share_group_permissions (
    share_name TEXT NOT NULL REFERENCES shares (name) ON DELETE CASCADE,
    group_id TEXT NOT NULL REFERENCES user_groups (id) ON DELETE CASCADE,
    access TEXT NOT NULL CHECK (access IN ('none', 'read-only', 'read-write')),
    PRIMARY KEY (share_name, group_id)
) STRICT;

CREATE INDEX share_group_permissions_group_id_idx ON share_group_permissions (group_id);

-- Per-share, per-disk bytes used (#223, doc 02 §1 line 78, doc 03
-- §4.1-4.2): computed once after every successful sync from SnapRAID's
-- own tracked state (`snapraid list`, which reads the content file the
-- sync just wrote), never a live directory walk (CLAUDE.md). Recomputed
-- from scratch each time — the whole table is cleared and rewritten in
-- one transaction (parity.UsageStore.Replace), so a disk a share no
-- longer occupies simply has no row after the next sync rather than a
-- stale one lingering. share_name is not a foreign key to shares.name: a
-- share can be deleted while its disks still hold files from before the
-- next sync runs, and the last-known distribution should keep reading
-- rather than vanish out from under a still-mounted branch.
CREATE TABLE share_usage (
    share_name TEXT NOT NULL,
    disk_mountpoint TEXT NOT NULL,
    bytes INTEGER NOT NULL CHECK (bytes >= 0),
    PRIMARY KEY (share_name, disk_mountpoint)
) STRICT;

CREATE INDEX share_usage_share_idx ON share_usage (share_name);

-- Singleton marking when share_usage was last (re)computed (#223, doc 03
-- §4.2's "as of the last sync"). Absent until the first sync computes it,
-- the same pattern schedule_chain's own singleton row uses — a share
-- created after this timestamp has honestly never been through that
-- computation yet, distinct from a share that has and genuinely holds
-- zero bytes.
CREATE TABLE share_usage_computed_at (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    computed_at TEXT NOT NULL
) STRICT;

-- Q15's own persisted relocation manifest (doc 09 §3-4): the file-level
-- record a mover/rebalance/evacuation/share-relocation job builds as it
-- copies and verifies each file onto its target, before requesting the
-- threshold-guarded sync that protects those copies. RunParityDiff and
-- RunSync (internal/job/parity_run.go) read this table at the wiring
-- boundary and pass it into Guard.Evaluate, which is the only place that
-- ever matches an entry against a diff (D1: this table never computes
-- anything, it only persists what a relocation job already decided).
-- Every row here is cleared, in the same transaction as
-- relocation_removing_disks below, once a relocation job's own trailing
-- sync (Q14's two-phase "sync, delete, sync again") has finished
-- accounting for it: both tables empty is "no relocation in progress",
-- the same nil-manifest behaviour Guard.Evaluate has always had.
CREATE TABLE relocation_manifest (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    rel_path TEXT NOT NULL,
    size INTEGER NOT NULL CHECK (size >= 0),
    mtime TEXT NOT NULL,
    source_disk TEXT NOT NULL,
    target_disk TEXT NOT NULL
) STRICT;

-- Disks currently in doc 09 §4's "removing" state (disk evacuation):
-- exempt from the guard's zero-files rule (Q15) while their branches are
-- set to no-create and their own files are being relocated off. Cleared
-- alongside relocation_manifest above once the evacuation's own final
-- `--force-empty` sync has completed.
CREATE TABLE relocation_removing_disks (
    mountpoint TEXT PRIMARY KEY
) STRICT;

-- Most recent finished mover run's structured result (#273, doc 09 §2,
-- doc 03 §3.6): files moved, bytes, duration, and every skipped entry
-- with its reason. Singleton row (id = 1); RunMover upserts it whenever
-- cache.Run produced a started report, including interrupted or failed
-- runs that still have partial results. Absent until the first mover
-- job has ever finished — GET /mover/last-run then returns null, the
-- same honest "never run" shape share_usage uses before the first sync.
CREATE TABLE mover_run_result (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    started_at TEXT NOT NULL,
    finished_at TEXT NOT NULL,
    duration_ms INTEGER NOT NULL CHECK (duration_ms >= 0),
    files_moved INTEGER NOT NULL CHECK (files_moved >= 0),
    bytes_moved INTEGER NOT NULL CHECK (bytes_moved >= 0),
    interrupted INTEGER NOT NULL CHECK (interrupted IN (0, 1)),
    skipped_json TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

-- Cache usage breakdown (#273, doc 03 §3.6, Q87): appdata / pending
-- moves / other, computed as a by-product of each mover run — never a
-- live directory walk on a timer (Q13). Singleton; absent until the
-- first mover run that could resolve a cache disk has finished.
CREATE TABLE cache_usage_breakdown (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    appdata_bytes INTEGER NOT NULL CHECK (appdata_bytes >= 0),
    pending_moves_bytes INTEGER NOT NULL CHECK (pending_moves_bytes >= 0),
    other_bytes INTEGER NOT NULL CHECK (other_bytes >= 0),
    computed_at TEXT NOT NULL
) STRICT;

-- UPS / NUT settings (#249, doc 03 §8.1, Q77, Q28): one row, id=1, the
-- same singleton pattern as acme_config. monitor_password and
-- network_password are ciphertext from internal/auth.MachineKey.Encrypt
-- (nonce||AES-256-GCM) — write-only on the API; GET only exposes the
-- *PasswordSet booleans. Connection-specific columns for the inactive
-- mode are empty strings / zero / empty blobs.
CREATE TABLE ups_config (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    connection TEXT NOT NULL CHECK (connection IN ('usb', 'network')),
    driver TEXT NOT NULL,
    port TEXT NOT NULL,
    monitor_password BLOB NOT NULL,
    network_host TEXT NOT NULL,
    network_port INTEGER NOT NULL CHECK (network_port >= 0 AND network_port <= 65535),
    network_ups_name TEXT NOT NULL,
    network_username TEXT NOT NULL,
    network_password BLOB NOT NULL,
    low_battery_percent INTEGER NOT NULL CHECK (low_battery_percent >= 0 AND low_battery_percent <= 100),
    runtime_seconds INTEGER NOT NULL CHECK (runtime_seconds >= 0),
    updated_at TEXT NOT NULL
) STRICT;
