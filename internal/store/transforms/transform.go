// Package transforms holds the data transforms D16 allows as the single
// exception to "everything is generated" (doc 01 §4): moving or reshaping
// existing data in a way a schema diff cannot express on its own — knowing
// that size_mb becomes size_bytes needs a factor of 1024*1024, which no
// diff between two CREATE TABLE statements can infer. Each Transform is
// bound to the checksum of the migration it must run alongside, in the
// same transaction, so a rollback of that migration also rolls back its
// transform. It is Go, not generated, and it is tested against every
// fixture database in testdata/db (doc 06 §2).
package transforms

import (
	"context"
	"database/sql"
)

// Transform is one data transform bound to Checksum — the SHA-256 checksum
// (sqlite-migrate's own Checksum, Q60) of the migration whose transaction
// it runs inside. Binding is by Checksum, not Version: a migration file is
// immutable once applied (D16), but nothing stops a later commit from
// reusing the same timestamp for a different, unrelated migration before
// this one ever ships, and Version alone can't tell those apart. Version
// is kept only to label which migration a transform is nominally paired
// with in error messages and test names; the Runner ignores it when
// deciding which transform to run.
type Transform struct {
	Version  string
	Checksum string
	Name     string
	Fn       func(ctx context.Context, tx *sql.Tx) error
}
