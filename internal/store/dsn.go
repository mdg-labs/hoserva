package store

// DSN builds the modernc.org/sqlite connection string this project's
// own SQLite databases share (doc 01 §1): WAL mode plus a busy timeout,
// so two of the job system's own concurrent writers (doc 01 §4), or a
// concurrent SMART poll and a metrics Downsample run, don't see
// SQLITE_BUSY the instant they overlap. cmd/hoservad's central database
// and internal/store/metrics's separate metrics.db (D4: kept apart so
// losing it never loses configuration) both open with this same DSN
// shape — one definition instead of two copies drifting apart.
//
// _txlock=immediate (#387 finding 1) makes every BeginTx acquire
// SQLite's write lock at BEGIN, before its first statement, rather than
// the driver default of starting deferred and only locking on the first
// write. A deferred read-then-write transaction (store.ArrayStore
// .SetRemovalState's own read of the current removing disk, before its
// write) that loses a race to a second writer's single-statement write
// landing in between gets an immediate SQLITE_BUSY on the upgrade
// attempt — a stale-snapshot conflict busy_timeout does not retry —
// which turned a resumable job's own checkpoint into an outright
// failure the instant job.Scheduler.EnterMaintenance's persisted write
// (persistMaintenanceLocked) landed in that window. Immediate mode
// instead serializes the two transactions through the normal write-lock
// wait busy_timeout already covers.
func DSN(path string) string {
	return "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate"
}
