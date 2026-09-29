package backup

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
)

// ErrArchiveOtherInstallation is returned by CheckRestorable for an archive
// whose database was written by a different installation. Restored in
// place it would replace machine_key_check and backup_recipient, and the
// next start would fail the machine key check (Q28).
var ErrArchiveOtherInstallation = errors.New("the archive is from a different installation (its machine key check value differs or is missing); a different installation's archive is restored only onto a fresh install, by the bare-metal restore")

// ArrayMismatchError is returned by CheckRestorable for an archive whose
// view of the array differs from the live one. Differences names each.
type ArrayMismatchError struct {
	Differences []string
}

func (e *ArrayMismatchError) Error() string {
	return "the archive's array differs from the live array: " + strings.Join(e.Differences, "; ") +
		"; only a disk lifecycle job changes the array, so an in-place restore does not"
}

// CheckRestorable reports whether the database at archiveDB, the staged
// state.db of a verified archive, may replace live's content in place: it
// must come from this installation, and describe the same array (the same
// disks, in the same removal state, and the same relocation in flight).
// It returns ErrArchiveOtherInstallation or an *ArrayMismatchError for an
// archive that may not, and any other error when either database cannot be
// read: a check that cannot conclude refuses. Neither database is written.
//
// The comparison covers what a restore would put out of step with the disks:
// array_disks (role, role index, filesystem UUID, WWN, serial and
// removal_state, not the device name, which changes across a reboot),
// relocation_manifest and relocation_removing_disks (doc 09 §4, Q14).
func CheckRestorable(ctx context.Context, live *sql.DB, archiveDB string) error {
	arc, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: archiveDB, RawQuery: "mode=ro"}).String())
	if err != nil {
		return fmt.Errorf("opening the archive's database: %w", err)
	}
	defer func() { _ = arc.Close() }()

	if err := checkSameInstallation(ctx, live, arc); err != nil {
		return err
	}
	return checkSameArray(ctx, live, arc)
}

func checkSameInstallation(ctx context.Context, live, arc *sql.DB) error {
	liveCheck, found, err := machineKeyCheck(ctx, live)
	if err != nil {
		return fmt.Errorf("reading the live machine key check value: %w", err)
	}
	if !found {
		return errors.New("the live database has no machine key check value to compare the archive with")
	}
	arcCheck, found, err := machineKeyCheck(ctx, arc)
	if err != nil {
		return fmt.Errorf("reading the archive's machine key check value: %w", err)
	}
	if !found || !bytes.Equal(liveCheck, arcCheck) {
		return ErrArchiveOtherInstallation
	}
	return nil
}

func machineKeyCheck(ctx context.Context, db *sql.DB) ([]byte, bool, error) {
	var v []byte
	err := db.QueryRowContext(ctx, `SELECT check_value FROM machine_key_check WHERE id = 1`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return v, true, nil
}

func checkSameArray(ctx context.Context, live, arc *sql.DB) error {
	var diffs []string

	liveDisks, err := loadArrayDisks(ctx, live)
	if err != nil {
		return fmt.Errorf("reading the live array_disks: %w", err)
	}
	arcDisks, err := loadArrayDisks(ctx, arc)
	if err != nil {
		return fmt.Errorf("reading the archive's array_disks: %w", err)
	}
	diffs = append(diffs, diffArrayDisks(liveDisks, arcDisks)...)

	liveRemoving, err := loadRemovingDisks(ctx, live)
	if err != nil {
		return fmt.Errorf("reading the live relocation_removing_disks: %w", err)
	}
	arcRemoving, err := loadRemovingDisks(ctx, arc)
	if err != nil {
		return fmt.Errorf("reading the archive's relocation_removing_disks: %w", err)
	}
	for _, m := range slices.Sorted(maps.Keys(liveRemoving)) {
		if _, ok := arcRemoving[m]; !ok {
			diffs = append(diffs, fmt.Sprintf("the live array is evacuating %s, which the archive is not", m))
		}
	}
	for _, m := range slices.Sorted(maps.Keys(arcRemoving)) {
		if _, ok := liveRemoving[m]; !ok {
			diffs = append(diffs, fmt.Sprintf("the archive is evacuating %s, which the live array is not", m))
		}
	}

	manifestDiffs, err := diffRelocationManifests(ctx, live, arc)
	if err != nil {
		return err
	}
	diffs = append(diffs, manifestDiffs...)

	if len(diffs) > 0 {
		return &ArrayMismatchError{Differences: diffs}
	}
	return nil
}

type arrayDisk struct {
	fsUUID  string
	wwn     string
	serial  string
	removal string
}

func loadArrayDisks(ctx context.Context, db *sql.DB) (map[string]arrayDisk, error) {
	rows, err := db.QueryContext(ctx, `SELECT role, role_index, fs_uuid, wwn, serial, removal_state FROM array_disks`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]arrayDisk{}
	for rows.Next() {
		var (
			role                  string
			index                 int64
			d                     arrayDisk
			wwn, serial, removing sql.NullString
		)
		if err := rows.Scan(&role, &index, &d.fsUUID, &wwn, &serial, &removing); err != nil {
			return nil, err
		}
		d.wwn, d.serial, d.removal = wwn.String, serial.String, removing.String
		out[fmt.Sprintf("%s disk %d", role, index)] = d
	}
	return out, rows.Err()
}

func diffArrayDisks(live, arc map[string]arrayDisk) []string {
	keys := map[string]struct{}{}
	for k := range live {
		keys[k] = struct{}{}
	}
	for k := range arc {
		keys[k] = struct{}{}
	}
	var diffs []string
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		l, inLive := live[k]
		a, inArc := arc[k]
		switch {
		case !inArc:
			diffs = append(diffs, fmt.Sprintf("the live array has %s (filesystem UUID %s), which the archive does not", k, l.fsUUID))
		case !inLive:
			diffs = append(diffs, fmt.Sprintf("the archive has %s (filesystem UUID %s), which the live array does not", k, a.fsUUID))
		default:
			for _, f := range []struct{ name, live, arc string }{
				{"filesystem UUID", l.fsUUID, a.fsUUID},
				{"WWN", l.wwn, a.wwn},
				{"serial", l.serial, a.serial},
				{"removal_state", l.removal, a.removal},
			} {
				if f.live != f.arc {
					diffs = append(diffs, fmt.Sprintf("%s differs in %s: live %q, archive %q", k, f.name, f.live, f.arc))
				}
			}
		}
	}
	return diffs
}

func loadRemovingDisks(ctx context.Context, db *sql.DB) (map[string]struct{}, error) {
	rows, err := db.QueryContext(ctx, `SELECT mountpoint FROM relocation_removing_disks`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]struct{}{}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out[m] = struct{}{}
	}
	return out, rows.Err()
}

type manifestEntry struct {
	path, mtime, source, target string
	size                        int64
}

func (e manifestEntry) String() string {
	return fmt.Sprintf("%q (%d bytes, %s to %s)", e.path, e.size, e.source, e.target)
}

func (e manifestEntry) compare(o manifestEntry) int {
	for _, c := range []int{
		strings.Compare(e.path, o.path),
		strings.Compare(e.source, o.source),
		strings.Compare(e.target, o.target),
		compareInt64(e.size, o.size),
		strings.Compare(e.mtime, o.mtime),
	} {
		if c != 0 {
			return c
		}
	}
	return 0
}

func compareInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// manifestCursor reads relocation_manifest in one total order (BINARY
// collation, the same order strings.Compare gives), so two of them can be
// merged without holding a whole manifest in memory.
type manifestCursor struct {
	rows *sql.Rows
	cur  manifestEntry
	ok   bool
}

func openManifestCursor(ctx context.Context, db *sql.DB) (*manifestCursor, error) {
	rows, err := db.QueryContext(ctx, `SELECT rel_path, source_disk, target_disk, size, mtime FROM relocation_manifest ORDER BY rel_path, source_disk, target_disk, size, mtime`)
	if err != nil {
		return nil, err
	}
	c := &manifestCursor{rows: rows}
	if err := c.next(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	return c, nil
}

func (c *manifestCursor) next() error {
	c.ok = c.rows.Next()
	if !c.ok {
		return c.rows.Err()
	}
	return c.rows.Scan(&c.cur.path, &c.cur.source, &c.cur.target, &c.cur.size, &c.cur.mtime)
}

const manifestSampleSize = 3

func diffRelocationManifests(ctx context.Context, live, arc *sql.DB) ([]string, error) {
	l, err := openManifestCursor(ctx, live)
	if err != nil {
		return nil, fmt.Errorf("reading the live relocation_manifest: %w", err)
	}
	defer func() { _ = l.rows.Close() }()
	a, err := openManifestCursor(ctx, arc)
	if err != nil {
		return nil, fmt.Errorf("reading the archive's relocation_manifest: %w", err)
	}
	defer func() { _ = a.rows.Close() }()

	var liveOnly, arcOnly []manifestEntry
	var liveOnlyN, arcOnlyN int
	for l.ok || a.ok {
		var c int
		switch {
		case !a.ok:
			c = -1
		case !l.ok:
			c = 1
		default:
			c = l.cur.compare(a.cur)
		}
		if c <= 0 {
			if c < 0 {
				liveOnlyN++
				if len(liveOnly) < manifestSampleSize {
					liveOnly = append(liveOnly, l.cur)
				}
			}
			if err := l.next(); err != nil {
				return nil, fmt.Errorf("reading the live relocation_manifest: %w", err)
			}
		}
		if c >= 0 {
			if c > 0 {
				arcOnlyN++
				if len(arcOnly) < manifestSampleSize {
					arcOnly = append(arcOnly, a.cur)
				}
			}
			if err := a.next(); err != nil {
				return nil, fmt.Errorf("reading the archive's relocation_manifest: %w", err)
			}
		}
	}

	var diffs []string
	if liveOnlyN > 0 {
		diffs = append(diffs, fmt.Sprintf("the live relocation manifest has %d entries the archive's does not, e.g. %s", liveOnlyN, joinEntries(liveOnly)))
	}
	if arcOnlyN > 0 {
		diffs = append(diffs, fmt.Sprintf("the archive's relocation manifest has %d entries the live one does not, e.g. %s", arcOnlyN, joinEntries(arcOnly)))
	}
	return diffs, nil
}

func joinEntries(es []manifestEntry) string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.String()
	}
	return strings.Join(parts, ", ")
}
