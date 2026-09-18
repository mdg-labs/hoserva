package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

// TimeFormat matches schema.sql's existing convention for every TEXT
// timestamp column: UTC, RFC3339, computed in Go rather than a SQL
// DEFAULT (Q60 flags `DEFAULT (datetime('now'))` as a spelling the
// schema migration generator's parser rejects outright). internal/job
// shares this one definition rather than redeclaring it — internal/job
// already imports this package (its own tests apply this package's real
// embedded migrations rather than a hand-built CREATE TABLE), so this is
// the cycle-free direction to share it in.
const TimeFormat = time.RFC3339

// HistoryRetention is Q74's default for spin-state events and the audit
// log: unlike metrics.db's downsampled time series (package
// internal/store/metrics) or a job's captured stdout/stderr
// (internal/job's LogStore, kept 90 days and capped at 1 GiB), both stay
// in the central database, in full, and are simply deleted once older
// than two years — no rollup step.
const HistoryRetention = 2 * 365 * 24 * time.Hour

// History persists spin-state events (Q32) and the audit log (doc 01 §7)
// in the central database, and prunes both per Q74. It has no business
// logic of its own — deciding when a transition or an audit-worthy
// action happened is the caller's job; History only writes and prunes
// rows, the same division internal/job's Store keeps for jobs.
type History struct {
	q *storedb.Queries
}

// NewHistory wraps db (typically *sql.DB, or a *sql.Tx via WithTx) for
// spin-event and audit-log persistence.
func NewHistory(db storedb.DBTX) *History {
	return &History{q: storedb.New(db)}
}

// RecordSpinEvent persists one observed spin-state transition (Q32) —
// the durable counterpart to internal/disk's in-process SpinEventLog,
// which does not itself survive a daemon restart. from and to are
// "active"/"standby" (disk.SpinState's own String method spells them
// exactly this way); store deliberately doesn't import internal/disk
// itself — the schema's own CHECK constraint is the authority on what's
// valid here, the same way CreateJob trusts its caller for "type"/class.
func (h *History) RecordSpinEvent(ctx context.Context, device string, from, to string, at time.Time) error {
	if err := h.q.InsertSpinEvent(ctx, storedb.InsertSpinEventParams{
		Device:    device,
		FromState: from,
		ToState:   to,
		At:        at.UTC().Format(TimeFormat),
	}); err != nil {
		return fmt.Errorf("store: recording spin event for %s: %w", device, err)
	}
	return nil
}

// CountSpinEvents reports how many spin events are currently persisted —
// a test seam for PruneHistory. A real listing view (doc 03 §3.3a)
// belongs to whichever issue builds it.
func (h *History) CountSpinEvents(ctx context.Context) (int64, error) {
	return h.q.CountSpinEvents(ctx)
}

// RecordAuditEntry persists one audit-log entry (doc 01 §7): actor,
// action and an optional free-text detail, at the time it happened.
func (h *History) RecordAuditEntry(ctx context.Context, actor, action, detail string, at time.Time) error {
	var detailArg sql.NullString
	if detail != "" {
		detailArg = sql.NullString{String: detail, Valid: true}
	}
	if err := h.q.InsertAuditLogEntry(ctx, storedb.InsertAuditLogEntryParams{
		Actor:  actor,
		Action: action,
		Detail: detailArg,
		At:     at.UTC().Format(TimeFormat),
	}); err != nil {
		return fmt.Errorf("store: recording audit entry %q by %q: %w", action, actor, err)
	}
	return nil
}

// CountAuditLog reports how many audit-log entries are currently
// persisted — a test seam for PruneHistory.
func (h *History) CountAuditLog(ctx context.Context) (int64, error) {
	return h.q.CountAuditLog(ctx)
}

// PruneHistory deletes every spin event and audit-log entry older than
// HistoryRetention, as of now (Q74: "Spin-state events and the audit log
// ... pruned after two years"). Like internal/job's LogStore.Prune, this
// is a maintenance step called directly — never a job of its own (doc 01
// §4) — and it touches only the central database on the boot SSD, never
// a data disk.
func (h *History) PruneHistory(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-HistoryRetention).UTC().Format(TimeFormat)
	if err := h.q.PruneSpinEvents(ctx, cutoff); err != nil {
		return fmt.Errorf("store: pruning spin events: %w", err)
	}
	if err := h.q.PruneAuditLog(ctx, cutoff); err != nil {
		return fmt.Errorf("store: pruning audit log: %w", err)
	}
	return nil
}
