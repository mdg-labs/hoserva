package api_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/store"
)

// seedShare inserts a minimal shares row directly, for tests that only
// need a share to exist as a foreign key target — not a working mergerfs
// mount or generated config, which is internal/share.Service's own
// concern, already covered by share_handler_test.go.
func seedShare(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	err := store.NewShareStore(db).Insert(context.Background(), store.Share{
		Name:         name,
		CacheMode:    "array-only",
		CreatePolicy: "mspmfs",
		NFSSquash:    "root_squash",
		CreatedAt:    now,
		UpdatedAt:    now,
	})
	if err != nil {
		t.Fatalf("seeding share %s: %v", name, err)
	}
}
