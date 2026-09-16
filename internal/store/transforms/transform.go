// Package transforms holds the data transforms D16 allows as the single
// exception to "everything is generated" (doc 01 §4): moving or reshaping
// existing data in a way a schema diff cannot express on its own — knowing
// that size_mb becomes size_bytes needs a factor of 1024*1024, which no
// diff between two CREATE TABLE statements can infer. Each Transform is
// bound to the version of the migration it must run alongside, in the
// same transaction, so a rollback of that migration also rolls back its
// transform. It is Go, not generated, and it is tested against every
// fixture database in testdata/db (doc 06 §2).
package transforms

import (
	"context"
	"database/sql"
)

// Transform is one data transform bound to Version — the migration version
// whose transaction it runs inside. Name exists only for error messages
// and test names.
type Transform struct {
	Version int
	Name    string
	Fn      func(ctx context.Context, tx *sql.Tx) error
}
