package store

// DSN builds the modernc.org/sqlite connection string this project's
// own SQLite databases share (doc 01 §1): WAL mode plus a busy timeout,
// so two of the job system's own concurrent writers (doc 01 §4), or a
// concurrent SMART poll and a metrics Downsample run, don't see
// SQLITE_BUSY the instant they overlap. cmd/hoservad's central database
// and internal/store/metrics's separate metrics.db (D4: kept apart so
// losing it never loses configuration) both open with this same DSN
// shape — one definition instead of two copies drifting apart.
func DSN(path string) string {
	return "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
}
