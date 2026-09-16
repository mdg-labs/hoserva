package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// assertingExecer wraps a real *sql.DB so a test can inspect every
// statement Snapshot runs through it before delegating to the real
// connection, so a test can assert VACUUM INTO actually targets the
// ".tmp" name rather than the final one.
type assertingExecer struct {
	db    *sql.DB
	check func(query string)
}

func (a *assertingExecer) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	a.check(query)
	return a.db.ExecContext(ctx, query, args...)
}

// Snapshot must write VACUUM INTO's own output to a temporary name and
// rename it into place only once it's complete — never
// straight to the final snapshot name, which would let a crash mid-VACUUM
// leave a corrupt file under the name PruneSnapshots and everything else
// in this package treats as a real, complete snapshot.
func TestSnapshot_WritesToTempNameBeforeRenaming(t *testing.T) {
	ctx := context.Background()
	real := openTestDB(t)
	dir := t.TempDir()

	execer := &assertingExecer{db: real, check: func(query string) {
		if !strings.Contains(query, "VACUUM INTO") {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), snapshotPrefix) && !strings.HasSuffix(e.Name(), ".tmp") {
				t.Fatalf("a final-named snapshot file %q already existed while VACUUM INTO was still running — it must be written to a temp name first", e.Name())
			}
		}
	}}

	if _, err := Snapshot(ctx, execer, dir, 1); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
}

// listSnapshots (and therefore PruneSnapshots) must never treat a leftover
// ".tmp" file as a real snapshot, whatever state it was left in by an
// interrupted VACUUM INTO.
func TestListSnapshots_NeverListsATempFile(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, fmt.Sprintf("%sv%010d-%010d.db.tmp", snapshotPrefix, 1, 1))
	if err := os.WriteFile(tmp, []byte("partial VACUUM INTO output"), 0o600); err != nil {
		t.Fatal(err)
	}

	snaps, err := listSnapshots(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 0 {
		t.Fatalf("listSnapshots returned %d entries for a directory containing only a .tmp file: %v", len(snaps), snaps)
	}
}

func TestSnapshot_WritesConsistentCopy(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := db.ExecContext(ctx, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT); INSERT INTO t VALUES (1, 'x');"); err != nil {
		t.Fatalf("seeding test database: %v", err)
	}

	dir := t.TempDir()
	path, err := Snapshot(ctx, db, dir, 1)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot file missing: %v", err)
	}

	snap, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("opening snapshot: %v", err)
	}
	defer closeQuietly(snap)
	var v string
	if err := snap.QueryRowContext(ctx, "SELECT v FROM t WHERE id = 1").Scan(&v); err != nil {
		t.Fatalf("reading snapshot content: %v", err)
	}
	if v != "x" {
		t.Fatalf("snapshot content = %q, want %q", v, "x")
	}
}

// A snapshot is a full copy of the configuration database — users,
// sessions, encrypted secrets — and has no business being readable by
// anyone but its owner.
func TestSnapshot_RestrictsPermissions(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	dir := filepath.Join(t.TempDir(), "snapshots")
	path, err := Snapshot(ctx, db, dir, 1)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("snapshot directory permissions = %o, want 0700", perm)
	}

	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("snapshot file permissions = %o, want 0600", perm)
	}
}

// MkdirAll doesn't tighten a directory that already existed under some
// more permissive mode — Snapshot must chmod it every call, not just at
// creation time.
func TestSnapshot_TightensPreExistingLoosePermissions(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	dir := filepath.Join(t.TempDir(), "snapshots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := Snapshot(ctx, db, dir, 1); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("snapshot directory permissions = %o, want 0700 (a pre-existing, more permissive directory must be tightened)", perm)
	}
}

// A crash between creating a ".tmp" file and renaming it into place
// leaves that file on disk forever — nothing else
// in this package ever revisits a ".tmp" name once the sequence counter
// moves on. Snapshot must remove one it finds at the start of its own next
// call, and must never touch anything that isn't its own naming pattern.
func TestSnapshot_RemovesStaleTempFileButNothingElse(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	dir := t.TempDir()

	stale := filepath.Join(dir, snapshotPrefix+"v0000000001-0000000001.db.tmp")
	if err := os.WriteFile(stale, []byte("leftover from a crashed VACUUM INTO"), 0o600); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(dir, "not-a-snapshot.db.tmp")
	if err := os.WriteFile(unrelated, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Snapshot(ctx, db, dir, 1); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale snapshot temp file still exists after Snapshot: err=%v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("Snapshot removed a file that didn't match its own naming pattern: %v", err)
	}
}

// A crash mid-VACUUM-INTO must never leave a file that PruneSnapshots or
// anything else mistakes for a real snapshot. Snapshot writes to a ".tmp"
// name first — this proves that name never matches snapshotPattern, so a
// leftover one from an interrupted attempt is silently ignored rather
// than counted as one of the kept snapshots.
func TestSnapshotPattern_NeverMatchesTempFile(t *testing.T) {
	name := fmt.Sprintf("%sv%010d-%010d.db.tmp", snapshotPrefix, 1, 1)
	if snapshotPattern.MatchString(name) {
		t.Fatalf("snapshotPattern matched a .tmp file %q — a crash mid-snapshot could count it as a real snapshot", name)
	}
}

// Every other test in this file drives PruneSnapshots through the
// KeepSnapshots constant rather than a literal count, so a changed
// constant value would otherwise pass every one of them unchanged. This
// pins doc 01 §4's own retention count directly.
func TestKeepSnapshots_IsThree(t *testing.T) {
	if KeepSnapshots != 3 {
		t.Fatalf("KeepSnapshots = %d, want 3 (doc 01 §4: the last three upgrades)", KeepSnapshots)
	}
}

func TestSnapshot_KeepsOnlyNewestByVersion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	var last string
	for i := 0; i < KeepSnapshots+2; i++ {
		db := openTestDB(t)
		path, err := Snapshot(ctx, db, dir, i)
		if err != nil {
			t.Fatalf("Snapshot #%d: %v", i, err)
		}
		if err := PruneSnapshots(dir, KeepSnapshots, path); err != nil {
			t.Fatalf("PruneSnapshots #%d: %v", i, err)
		}
		last = path
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != KeepSnapshots {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("dir has %d entries, want %d: %v", len(entries), KeepSnapshots, names)
	}
	if _, err := os.Stat(last); err != nil {
		t.Fatalf("pruning deleted the snapshot just taken: %v", err)
	}
}

// Pruning must key retention on the schema version a
// snapshot was taken from, not on the wall-clock order it happened to be
// written in — a clock correction or a restart with no working RTC before
// NTP sync can make a new snapshot's timestamp sort *before* an existing
// one's, even though its fromVersion is higher.
func TestPruneSnapshots_OrdersByVersionNotWallClock(t *testing.T) {
	dir := t.TempDir()

	// v1 and v2's own snapshots, "taken" in order.
	write := func(version int) string {
		ctx := context.Background()
		db := openTestDB(t)
		path, err := Snapshot(ctx, db, dir, version)
		if err != nil {
			t.Fatalf("Snapshot v%d: %v", version, err)
		}
		return path
	}
	_ = write(1)
	v2Path := write(2)
	if err := PruneSnapshots(dir, 2, v2Path); err != nil {
		t.Fatal(err)
	}

	// v3's snapshot is taken by a process whose clock is behind the one
	// that wrote v1/v2's filenames — simulated directly, since forcing an
	// actual wall-clock regression from this test would be fragile. The
	// filename's timestamp-free format means there is nothing here for a
	// clock to get wrong: only fromVersion (3)
	// determines where this sorts.
	v3Name := fmt.Sprintf("%sv%010d-%010d.db", snapshotPrefix, 3, 999)
	v3Path := filepath.Join(dir, v3Name)
	if err := os.WriteFile(v3Path, []byte("not a real sqlite file, just a stand-in"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PruneSnapshots(dir, 2, v3Path); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	if !names[filepath.Base(v2Path)] || !names[v3Name] {
		t.Fatalf("expected v2 and v3's snapshots to survive (the two highest versions), got: %v", names)
	}
	if len(names) != 2 {
		t.Fatalf("dir has %d entries, want 2: %v", len(names), names)
	}
}

// A migration retried after a failure re-snapshots the same, unchanged
// starting database each time. Those retries must
// never count as distinct upgrades and push out a different, genuinely
// older upgrade's snapshot.
func TestPruneSnapshots_RetryOfSameVersionNeverEvictsOtherVersions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	db := openTestDB(t)
	v1Path, err := Snapshot(ctx, db, dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := PruneSnapshots(dir, 1, v1Path); err != nil {
		t.Fatal(err)
	}

	// A second attempt at fromVersion 1 (a retried, still-failing
	// migration re-snapshotting the same unmodified database) must
	// supersede the first attempt's snapshot, but keep=1 must still mean
	// "the newest one distinct upgrade", not "zero, because there are now
	// two files for the one version this test has".
	retryPath, err := Snapshot(ctx, db, dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := PruneSnapshots(dir, 1, retryPath); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("dir has %d entries after a same-version retry, want 1: %v", len(entries), names)
	}
	if entries[0].Name() != filepath.Base(retryPath) {
		t.Fatalf("survivor = %q, want the retry's own snapshot %q", entries[0].Name(), filepath.Base(retryPath))
	}
}

// VACUUM INTO takes its destination as a SQL string
// literal, not a bound parameter — a snapshot directory whose path
// contains a single quote (an unusual but legal path component) must
// still produce a snapshot at the exact path requested, not one truncated
// or corrupted by an unescaped quote.
func TestSnapshot_PathContainingSingleQuote(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	dir := filepath.Join(t.TempDir(), "it's a dir")
	path, err := Snapshot(ctx, db, dir, 1)
	if err != nil {
		t.Fatalf("Snapshot with a quote in its directory path: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot file missing at the expected path %q: %v", path, err)
	}
}

func TestQuoteSQLiteLiteral_EscapesSingleQuote(t *testing.T) {
	got := quoteSQLiteLiteral(`/tmp/it's a path.db`)
	want := `'/tmp/it''s a path.db'`
	if got != want {
		t.Fatalf("quoteSQLiteLiteral(%q) = %q, want %q", `/tmp/it's a path.db`, got, want)
	}
}

func TestPruneSnapshots_NeverDeletesTheOneJustWritten(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := openTestDB(t)

	path, err := Snapshot(ctx, db, dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	// keep=0 would, without the explicit exclusion, delete everything —
	// including the snapshot just taken for the upgrade about to run.
	if err := PruneSnapshots(dir, 0, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("PruneSnapshots deleted the snapshot just written: %v", err)
	}
}

func TestPruneSnapshots_NeverDeletesUnrelatedFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// A file that would sort before every real snapshot name, one that
	// would sort after, and a near-miss of the pattern (wrong digit
	// width) — so removing snapshotPattern's filter, and relying on sort
	// position instead, would still be caught by at least one of these.
	unrelatedFirst := filepath.Join(dir, "a-unrelated.db")
	unrelatedLast := filepath.Join(dir, "z-unrelated.db")
	nearMiss := filepath.Join(dir, "hoserva-pre-migration-v1-1.db")
	for _, p := range []string{unrelatedFirst, unrelatedLast, nearMiss} {
		if err := os.WriteFile(p, []byte("keep me"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for i := 0; i < KeepSnapshots+2; i++ {
		db := openTestDB(t)
		path, err := Snapshot(ctx, db, dir, i)
		if err != nil {
			t.Fatalf("Snapshot #%d: %v", i, err)
		}
		if err := PruneSnapshots(dir, KeepSnapshots, path); err != nil {
			t.Fatalf("PruneSnapshots #%d: %v", i, err)
		}
	}

	for _, p := range []string{unrelatedFirst, unrelatedLast, nearMiss} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("pruning removed a file it did not create: %s: %v", p, err)
		}
	}
}
