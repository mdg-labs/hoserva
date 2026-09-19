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
    created_at TEXT NOT NULL
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
-- for a resumable type. There is no log-path column: job stdout/stderr
-- lives in compressed files under a log directory the daemon passes in
-- (Q74), named by job id, so nothing here duplicates that path.
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
    finished_at TEXT
) STRICT;

CREATE INDEX jobs_status_idx ON jobs ("status");
CREATE INDEX jobs_class_idx ON jobs (class);

-- Users and sessions (#22, doc 01 §7, Q27, Q28, Q44). No default
-- credential ever exists (doc 01 §7): the first row is written by the
-- first-run setup flow, atomically, and only while this table is empty.
-- Only admin/viewer accounts are represented — share-only users (Q27) have
-- no API access and never get a row here. totp_secret and
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
CREATE TABLE users (
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('admin', 'viewer')),
    totp_secret BLOB,
    totp_confirmed_at TEXT,
    totp_last_step INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    totp_pending_secret BLOB
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
