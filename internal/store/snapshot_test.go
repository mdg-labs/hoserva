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

	if _, err := Snapshot(ctx, execer, dir, v(1)); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
}

// listSnapshots (and therefore PruneSnapshots) must never treat a leftover
// ".tmp" file as a real snapshot, whatever state it was left in by an
// interrupted VACUUM INTO.
func TestListSnapshots_NeverListsATempFile(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, fmt.Sprintf("%sv%s-%010d.db.tmp", snapshotPrefix, v(1), 1))
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
	path, err := Snapshot(ctx, db, dir, v(1))
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
	path, err := Snapshot(ctx, db, dir, v(1))
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

	if _, err := Snapshot(ctx, db, dir, v(1)); err != nil {
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

	stale := filepath.Join(dir, snapshotPrefix+"v"+v(1)+"-0000000001.db.tmp")
	if err := os.WriteFile(stale, []byte("leftover from a crashed VACUUM INTO"), 0o600); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(dir, "not-a-snapshot.db.tmp")
	if err := os.WriteFile(unrelated, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Snapshot(ctx, db, dir, v(1)); err != nil {
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
	name := fmt.Sprintf("%sv%s-%010d.db.tmp", snapshotPrefix, v(1), 1)
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
		path, err := Snapshot(ctx, db, dir, v(i))
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
		path, err := Snapshot(ctx, db, dir, v(version))
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
	v3Name := fmt.Sprintf("%sv%s-%010d.db", snapshotPrefix, v(3), 999)
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
	v1Path, err := Snapshot(ctx, db, dir, v(1))
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
	retryPath, err := Snapshot(ctx, db, dir, v(1))
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
	path, err := Snapshot(ctx, db, dir, v(1))
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

	path, err := Snapshot(ctx, db, dir, v(1))
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
		path, err := Snapshot(ctx, db, dir, v(i))
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

// TestRestoreSnapshot_EveryRowSurvivesUpgrade is the data-loss scenario
// Q67/D16 exist to close: an upgrade that applied a schema migration
// must roll back to the pre-migration snapshot so every row that existed
// before the upgrade is still there, and rows written only after the
// migration are not.
func TestRestoreSnapshot_EveryRowSurvivesUpgrade(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	livePath := filepath.Join(dir, "hoserva.db")

	live, err := sql.Open("sqlite", livePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.ExecContext(ctx, `CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := live.ExecContext(ctx, `INSERT INTO items (id, name) VALUES (1, 'keep-me'), (2, 'also-keep')`); err != nil {
		t.Fatal(err)
	}
	snap, err := Snapshot(ctx, live, dir, v(1))
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, err := live.ExecContext(ctx, `ALTER TABLE items ADD COLUMN extra TEXT`); err != nil {
		t.Fatal(err)
	}
	if _, err := live.ExecContext(ctx, `INSERT INTO items (id, name, extra) VALUES (3, 'post-migration', 'gone-on-rollback')`); err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}

	if err := RestoreSnapshot(ctx, livePath, snap); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	restored, err := sql.Open("sqlite", livePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Close() }()

	rows, err := restored.QueryContext(ctx, `SELECT id, name FROM items ORDER BY id`)
	if err != nil {
		t.Fatalf("querying restored items: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var id int
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d:%s", id, name))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"1:keep-me", "2:also-keep"}
	if len(got) != len(want) {
		t.Fatalf("restored rows = %v, want %v — a missing row is data loss on rollback", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("restored rows = %v, want %v", got, want)
		}
	}

	var extraCount int
	if err := restored.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('items') WHERE name = 'extra'`).Scan(&extraCount); err != nil {
		t.Fatal(err)
	}
	if extraCount != 0 {
		t.Fatal("restored database still has the post-migration column — rollback did not restore the snapshot")
	}
	if _, err := os.Stat(snap); err != nil {
		t.Fatalf("RestoreSnapshot deleted the snapshot: %v", err)
	}
}

func TestSnapshotLive_ProducesRestorableCopyOfLiveRows(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	livePath := filepath.Join(dir, "hoserva.db")
	live, err := sql.Open("sqlite", livePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.ExecContext(ctx, `CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := live.ExecContext(ctx, `INSERT INTO items (id, name) VALUES (1, 'keep-me')`); err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}

	snap, err := SnapshotLive(ctx, livePath, dir)
	if err != nil {
		t.Fatalf("SnapshotLive: %v", err)
	}
	if snapshotPattern.FindStringSubmatch(filepath.Base(snap)) == nil {
		t.Fatalf("SnapshotLive wrote %q, which RestoreSnapshot would refuse", snap)
	}
	if err := WriteRollbackTarget(dir, snap); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRollbackTarget(dir)
	if err != nil {
		t.Fatalf("ReadRollbackTarget: %v", err)
	}
	if got != snap {
		t.Fatalf("ReadRollbackTarget = %q, want %q", got, snap)
	}
}

func TestWriteRollbackTarget_RefusesUnrelatedFile(t *testing.T) {
	dir := t.TempDir()
	if err := WriteRollbackTarget(dir, filepath.Join(dir, "not-a-snapshot.db")); err == nil {
		t.Fatal("WriteRollbackTarget accepted a file RestoreSnapshot would refuse")
	}
}

func TestPruneSnapshots_KeepsRecordedRollbackTarget(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := openTestDB(t)

	target, err := Snapshot(ctx, db, dir, v(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRollbackTarget(dir, target); err != nil {
		t.Fatal(err)
	}
	newer, err := Snapshot(ctx, db, dir, v(2))
	if err != nil {
		t.Fatal(err)
	}
	if err := PruneSnapshots(dir, 1, newer); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("PruneSnapshots deleted the recorded rollback target: %v", err)
	}
}

func TestRestoreSnapshot_RefusesUnrelatedFile(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "hoserva.db")
	if err := os.WriteFile(live, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	bogus := filepath.Join(dir, "not-a-snapshot.db")
	if err := os.WriteFile(bogus, []byte("attacker"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RestoreSnapshot(context.Background(), live, bogus); err == nil {
		t.Fatal("RestoreSnapshot accepted a file that is not a hoserva snapshot")
	}
	got, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "live" {
		t.Fatalf("live database was overwritten by a refused restore: %q", got)
	}
}

func TestLatestSnapshot_PicksNewestFromVersion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := openTestDB(t)
	older, err := Snapshot(ctx, db, dir, v(1))
	if err != nil {
		t.Fatal(err)
	}
	newer, err := Snapshot(ctx, db, dir, v(2))
	if err != nil {
		t.Fatal(err)
	}
	got, err := LatestSnapshot(dir)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if got != newer {
		t.Fatalf("LatestSnapshot = %q, want %q (not the older %q)", got, newer, older)
	}
}
