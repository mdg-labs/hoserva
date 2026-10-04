package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// preflights are read-only checks Apply runs inside the migration transaction,
// just before the migration whose checksum they are bound to, for a change the
// data already in the database can make fail. A check that finds the data
// unfit refuses the whole upgrade with a message about the data, in place of
// the bare constraint error the migration would raise; nothing is written
// either way, so the transaction rolls back and the database is as it was.
var preflights = map[string]func(ctx context.Context, tx *sql.Tx) error{
	// add_users_username_lower_unique
	"7a5dafb3e8f919aa965a09ae4e5ded96f61ad8c0825852af7bdf0506d59339aa": usernamesUniqueIgnoringCase,
}

// usernamesUniqueIgnoringCase refuses an upgrade over accounts whose names
// differ only in case, which the new unique index cannot hold. Every write
// path stores a lower-cased name, so only a database edited by hand, or filled
// by an older build, can hold such a pair. Which of two accounts keeps its name
// is the owner's decision, since renaming one changes the identity a client
// logs in as, so the check names the accounts and changes none.
func usernamesUniqueIgnoringCase(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
SELECT group_concat('"' || username || '" (id ' || id || ')', ', ')
FROM users GROUP BY lower(username) HAVING count(*) > 1 ORDER BY lower(username)`)
	if err != nil {
		return fmt.Errorf("checking usernames for case-only duplicates: %w", err)
	}
	defer closeQuietly(rows)
	var sets []string
	for rows.Next() {
		var set string
		if err := rows.Scan(&set); err != nil {
			return fmt.Errorf("reading a case-only duplicate: %w", err)
		}
		sets = append(sets, set)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("checking usernames for case-only duplicates: %w", err)
	}
	if len(sets) == 0 {
		return nil
	}
	return fmt.Errorf("usernames are unique without regard to case, but these accounts differ only in case: %s. "+
		"Nothing was changed. Rename all but one of each set, for example "+
		"sqlite3 <database> \"UPDATE users SET username = '<new name>' WHERE id = '<id>'\", and start again",
		strings.Join(sets, "; "))
}
