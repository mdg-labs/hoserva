package backup

import (
	"context"
	"database/sql"
	"fmt"

	sqlite "modernc.org/sqlite"
)

// backupRestorer is the modernc.org/sqlite driver connection's own online
// backup API (NewRestore, backed by sqlite3_backup_init/_step/_finish) —
// declared narrowly here, rather than importing the driver's unexported
// conn type, so RestoreDatabase can reach it through database/sql.Conn.Raw
// using nothing but this one exported method's signature.
type backupRestorer interface {
	NewRestore(srcURI string) (*sqlite.Backup, error)
}

// RestoreDatabase replaces db's content with the database at srcPath,
// through SQLite's own online backup API rather than a file-level copy or
// rename (doc 10 §1, #269). The old config-import path
// (copyFileAtomic, now deleted) renamed a freshly unpacked state.db over
// the live database's path while db's pooled connections stayed open in
// WAL mode: their *-wal/*-shm sidecars are addressed by that same path,
// not by the file's inode, so a rename leaves them paired with whatever
// new file now sits there. A pooled connection already attached to that
// wal-index group keeps reading and writing through it — and the file
// the rename installed is never actually consulted — so the import
// appeared to succeed while every live connection, and the file itself
// once a later checkpoint ran, kept the pre-import data. Reproduced in
// TestImportConfig_ReplacesRunningDatabaseWithoutCorruption (config_
// import_test.go): the same request sequence through the real handler
// left the reopened database holding the interim mutation, not the
// import.
//
// sqlite3_backup_step instead copies srcPath page by page into db
// through the ordinary pager and its own WAL machinery, as one write
// transaction on a connection every other pooled connection already
// shares — so every one of them, including one that opened before this
// call, observes either the database exactly as it was or exactly as
// srcPath is, with no rename and no sidecar left stale.
//
// srcPath is opened read-only and is never modified. Callers are
// responsible for verifying srcPath (VerifyArchiveForImport) before
// calling this — RestoreDatabase itself trusts it completely.
func RestoreDatabase(ctx context.Context, db *sql.DB, srcPath string) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring a dedicated connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	return conn.Raw(func(driverConn any) error {
		r, ok := driverConn.(backupRestorer)
		if !ok {
			return fmt.Errorf("restoring database: driver connection does not support the online backup API")
		}
		b, err := r.NewRestore("file:" + srcPath + "?mode=ro")
		if err != nil {
			return fmt.Errorf("starting restore: %w", err)
		}
		if _, err := b.Step(-1); err != nil {
			_ = b.Finish()
			return fmt.Errorf("restoring pages: %w", err)
		}
		if err := b.Finish(); err != nil {
			return fmt.Errorf("finishing restore: %w", err)
		}
		return nil
	})
}
