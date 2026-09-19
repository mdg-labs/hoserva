package backup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// vacuumInto writes a consistent copy of db to dest using SQLite's VACUUM
// INTO (doc 10 §1: never copy a live database file). The pattern mirrors
// internal/store's Snapshot — temp file first, sync, rename, sync directory.
func vacuumInto(ctx context.Context, db *sql.DB, dest string) error {
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating snapshot directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("restricting snapshot directory: %w", err)
	}
	if err := removeStaleDBTemp(dir, filepath.Base(dest)); err != nil {
		return err
	}

	tmp := dest + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("creating temp database %q: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing temp database %q: %w", tmp, err)
	}

	quoted := quoteSQLiteLiteral(tmp)
	if _, err := db.ExecContext(ctx, fmt.Sprintf("VACUUM INTO %s", quoted)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("VACUUM INTO %q: %w", tmp, err)
	}
	if err := fsyncPath(tmp); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("syncing temp database %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("finalizing database %q: %w", dest, err)
	}
	return fsyncDir(dir)
}

func removeStaleDBTemp(dir, finalName string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("listing %q: %w", dir, err)
	}
	prefix := finalName + ".tmp"
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == prefix || strings.HasPrefix(name, prefix+".") {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				return fmt.Errorf("removing stale temp %q: %w", name, err)
			}
		}
	}
	return nil
}

func quoteSQLiteLiteral(s string) string {
	escaped := strings.ReplaceAll(s, "'", "''")
	return "'" + escaped + "'"
}
