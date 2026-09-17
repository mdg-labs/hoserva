package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
)

// snapshotPrefix marks exactly the files this package created and is
// therefore allowed to prune. Retention pruning is a delete path
// (CLAUDE.md); it must never remove a file it doesn't recognize as its
// own, however old, and a plain glob on "*" would.
const snapshotPrefix = "hoserva-pre-migration-"

// snapshotPattern captures the schema version a snapshot was taken from
// (group 1) — sqlite-migrate's own sortable "<timestamp>_<slug>.sql"
// filename convention (Q60) gives every migration a 14-digit version, so
// this is fixed-width and lexicographically ordered the same as it is
// numerically, same as the previous zero-padded integer scheme. Retention
// is keyed on that version, never on a wall-clock timestamp: a real
// upgrade's fromVersion only ever increases, which stays true across a
// restart, a clock correction, or a home server with no working RTC before
// its first NTP sync — none of which a filename timestamp can promise.
// Group 2 only disambiguates two snapshots of the same version a single
// process takes (a retried migration re-snapshotting its unchanged
// starting database); it carries no ordering meaning of its own.
var snapshotPattern = regexp.MustCompile(`^hoserva-pre-migration-v(\d{14})-(\d{10})\.db$`)

var snapshotSeq atomic.Uint64

// KeepSnapshots is "the last three upgrades" (doc 01 §4).
const KeepSnapshots = 3

// Snapshot writes a consistent copy of db to dir with SQLite's own
// VACUUM INTO (doc 10 §1's consistency rule: never a plain file copy of a
// live database) before fromVersion's pending migrations run. VACUUM INTO
// fails inside a transaction, so this must run, and does run, before
// Runner.Apply opens its migration transaction.
//
// It writes to a temporary name first and renames into place only once the
// copy is complete and synced — both the file's own content (fsyncPath)
// and, after the rename, dir itself, so the rename that makes this
// snapshot visible under its real name is not left only in the
// filesystem's in-memory directory cache: without that second fsync, a
// power loss right after this call returns (and after the migration it
// precedes has already committed) could still lose the rename on some
// filesystems, leaving a migrated database with no pre-migration snapshot
// at all. VACUUM INTO has no atomicity of its own, so a crash partway
// through must never leave a partial file that still matches
// snapshotPattern and could count as one of the kept snapshots, or push
// out a good one. A crash here leaves only a ".tmp" file that
// snapshotPattern never matches, so it is never mistaken for a snapshot by
// anything in this package — and any such file left over from an earlier,
// interrupted attempt is removed before this one starts, rather than
// merely being made unlikely to collide with (see the removeStaleTemp
// call below: the sequence counter restarts at 1 in every process, so a
// name colliding with a previous process's leftover ".tmp" is the normal
// case after a restart, not a rare one — it works only because the rename
// below replaces whatever was there).
//
// The directory and the snapshot file are both restricted to their owner:
// a snapshot is a full copy of the configuration database — users,
// sessions, encrypted secrets — and has no business being group- or
// world-readable. dir is chmod'd every call, not just created
// with a restrictive mode, so a pre-existing, more permissive directory
// (however it got that way) is tightened too; the temp file is created
// with the final restrictive mode from the moment it exists, rather than
// relying on a Chmod after VACUUM INTO has already written it under the
// process umask.
//
// Pruning is a separate step (PruneSnapshots) that callers run only once
// they know the migration this snapshot precedes actually committed —
// never from here, and never before that's known.
func Snapshot(ctx context.Context, db sqlExecer, dir string, fromVersion string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating snapshot directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("restricting snapshot directory permissions: %w", err)
	}
	if err := removeStaleTemp(dir); err != nil {
		return "", err
	}

	name := fmt.Sprintf("%sv%s-%010d.db", snapshotPrefix, fromVersion, snapshotSeq.Add(1))
	finalPath := filepath.Join(dir, name)
	tmpPath := finalPath + ".tmp"

	// Pre-create the temp file, empty, with the final restrictive mode —
	// VACUUM INTO accepts an already-existing, empty destination file, and
	// this closes the window a Chmod after VACUUM INTO leaves open between
	// the file being created (under the process umask, possibly readable
	// by more than its owner) and being restricted.
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("creating snapshot temp file %q: %w", tmpPath, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("closing snapshot temp file %q: %w", tmpPath, err)
	}

	quoted := quoteSQLiteLiteral(tmpPath)
	if _, err := db.ExecContext(ctx, fmt.Sprintf("VACUUM INTO %s", quoted)); err != nil {
		return "", fmt.Errorf("VACUUM INTO %q: %w", tmpPath, err)
	}
	if err := fsyncPath(tmpPath); err != nil {
		return "", fmt.Errorf("syncing snapshot %q: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return "", fmt.Errorf("finalizing snapshot %q: %w", finalPath, err)
	}
	if err := fsyncPath(dir); err != nil {
		return "", fmt.Errorf("syncing snapshot directory %q: %w", dir, err)
	}
	return finalPath, nil
}

// removeStaleTemp removes every leftover "*.db.tmp" this package's own
// naming produces (snapshotPrefix-anchored, so this can never touch a file
// it didn't create the name pattern for) — a crash between creating one and
// renaming it into place otherwise leaves it on disk forever, since nothing
// else in this package ever looks at a ".tmp" name again once a fresh
// sequence number moves on.
func removeStaleTemp(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("listing snapshot directory: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, snapshotPrefix) || !strings.HasSuffix(name, ".db.tmp") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("removing stale snapshot temp file %q: %w", name, err)
		}
	}
	return nil
}

func fsyncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer closeQuietly(f)
	return f.Sync()
}

// quoteSQLiteLiteral produces a SQLite string literal for path. VACUUM INTO
// takes a filename as a SQL string literal, not a bound parameter (SQLite
// does not support parameter binding in this statement) — this is the one
// place this package builds SQL text from a value that isn't a fixed
// migration file, so it stays narrowly scoped to escaping a single quote
// character, never string concatenation of a command.
func quoteSQLiteLiteral(s string) string {
	escaped := ""
	for _, r := range s {
		if r == '\'' {
			escaped += "''"
		} else {
			escaped += string(r)
		}
	}
	return "'" + escaped + "'"
}

// snapshotFile is one on-disk snapshot this package recognizes.
type snapshotFile struct {
	name        string
	fromVersion string
}

func listSnapshots(dir string) ([]snapshotFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("listing snapshot directory: %w", err)
	}
	var out []snapshotFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := snapshotPattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		out = append(out, snapshotFile{name: e.Name(), fromVersion: m[1]})
	}
	return out, nil
}

// PruneSnapshots keeps only the snapshots of the last keep distinct
// upgrades (doc 01 §4), by fromVersion — never by wall-clock order — plus
// justWritten itself, unconditionally.
//
// Callers run this only after the migration justWritten precedes is known
// to have committed: pruning before that's known is how a failing
// migration in a systemd restart loop could re-snapshot the same
// fromVersion over and over and, each time being "the newest snapshot" by
// wall clock, push older, unrelated upgrades' snapshots out within
// seconds — replacing three genuinely different upgrades' history with
// copies of the same never-migrated database. Any other file already
// on disk for justWritten's own fromVersion is a stale retry of the same
// upgrade attempt and is always dropped in favor of justWritten, regardless
// of whether justWritten's version is among the newest keep — the snapshot
// just taken for the upgrade about to run is never a pruning candidate.
func PruneSnapshots(dir string, keep int, justWritten string) error {
	all, err := listSnapshots(dir)
	if err != nil {
		return err
	}
	justWrittenName := filepath.Base(justWritten)

	byVersion := map[string][]snapshotFile{}
	for _, s := range all {
		byVersion[s.fromVersion] = append(byVersion[s.fromVersion], s)
	}

	// One representative per fromVersion: justWritten's own file if it's
	// among that version's snapshots (a retry always supersedes whatever
	// stale attempt(s) preceded it for the same fromVersion), otherwise the
	// lexicographically last name — deterministic, and never a correctness
	// question beyond hygiene, since every duplicate for one fromVersion is
	// a snapshot of the same unmodified starting database.
	representative := map[string]string{}
	for version, snaps := range byVersion {
		rep := snaps[0].name
		for _, s := range snaps {
			if s.name > rep {
				rep = s.name
			}
			if s.name == justWrittenName {
				rep = s.name
			}
		}
		representative[version] = rep
	}

	// fromVersion is sqlite-migrate's own fixed-width, 14-digit timestamp
	// (Q60), so lexicographic and numeric order agree — a plain string sort
	// is enough, the same guarantee the previous zero-padded integer scheme
	// gave.
	versions := make([]string, 0, len(representative))
	for v := range representative {
		versions = append(versions, v)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(versions)))

	keepNames := map[string]bool{justWrittenName: true}
	for i, v := range versions {
		if i >= keep {
			break
		}
		keepNames[representative[v]] = true
	}

	for _, s := range all {
		if keepNames[s.name] {
			continue
		}
		if err := os.Remove(filepath.Join(dir, s.name)); err != nil {
			return fmt.Errorf("removing old snapshot %q: %w", s.name, err)
		}
	}
	return nil
}
