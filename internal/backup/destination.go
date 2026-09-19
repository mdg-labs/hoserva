package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// archiveNamePattern matches doc 10 §1's archive filenames:
// hoserva-config-2026-09-14T03-00.tar.zst
var archiveNamePattern = regexp.MustCompile(`^hoserva-config-(\d{4}-\d{2}-\d{2}T\d{2}-\d{2})\.tar\.zst$`)

// Destination is one local backup target (doc 10 §1, Q40). Remote and
// rclone-backed destinations are issue #60's scope — this issue is local
// paths only.
type Destination struct {
	ID        string
	Path      string
	Enabled   bool
	Retention Retention
}

// Retention is the grandfather-father-son policy doc 10 §1 describes.
type Retention struct {
	Daily   int
	Weekly  int
	Monthly int
}

type archiveEntry struct {
	name    string
	path    string
	modTime time.Time
}

// writeArchive copies archivePath into dest.Path under its basename, after
// ensuring the destination directory exists with restrictive permissions.
func writeArchive(dest Destination, archivePath string) error {
	if err := os.MkdirAll(dest.Path, 0o700); err != nil {
		return fmt.Errorf("creating destination %q: %w", dest.Path, err)
	}
	if err := os.Chmod(dest.Path, 0o700); err != nil {
		return fmt.Errorf("restricting destination %q: %w", dest.Path, err)
	}

	name := filepath.Base(archivePath)
	final := filepath.Join(dest.Path, name)

	in, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("opening archive for copy: %w", err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.CreateTemp(dest.Path, name+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp archive in %q: %w", dest.Path, err)
	}
	tmp := out.Name()
	if err := out.Chmod(0o600); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("restricting temp archive %q: %w", tmp, err)
	}
	if _, err := copyFile(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("copying archive to %q: %w", tmp, err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("syncing temp archive %q: %w", tmp, err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("closing temp archive %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("finalizing archive at %q: %w", final, err)
	}
	if err := fsyncDir(dest.Path); err != nil {
		return err
	}
	return nil
}

// pruneDestination enforces dest's retention on archives it owns — only
// filenames matching archiveNamePattern are candidates.
func pruneDestination(dest Destination, now time.Time, justWritten string) error {
	entries, err := listArchives(dest.Path)
	if err != nil {
		return err
	}
	keep := retentionKeepers(entries, dest.Retention, now, justWritten)
	for _, e := range entries {
		if keep[e.name] {
			continue
		}
		if err := os.Remove(e.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("pruning archive %q: %w", e.path, err)
		}
	}
	return nil
}

func listArchives(dir string) ([]archiveEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing destination %q: %w", dir, err)
	}
	var out []archiveEntry
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if archiveNamePattern.FindStringSubmatch(e.Name()) == nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, archiveEntry{
			name:    e.Name(),
			path:    filepath.Join(dir, e.Name()),
			modTime: info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].modTime.After(out[j].modTime)
	})
	return out, nil
}

func retentionKeepers(entries []archiveEntry, ret Retention, now time.Time, justWritten string) map[string]bool {
	keep := map[string]bool{justWritten: true}
	if len(entries) == 0 {
		return keep
	}

	byDay := map[string]archiveEntry{}
	byWeek := map[string]archiveEntry{}
	byMonth := map[string]archiveEntry{}

	for _, e := range entries {
		day := e.modTime.Format("2006-01-02")
		if cur, ok := byDay[day]; !ok || e.modTime.After(cur.modTime) {
			byDay[day] = e
		}
		year, week := e.modTime.ISOWeek()
		weekKey := fmt.Sprintf("%04d-W%02d", year, week)
		if cur, ok := byWeek[weekKey]; !ok || e.modTime.After(cur.modTime) {
			byWeek[weekKey] = e
		}
		monthKey := e.modTime.Format("2006-01")
		if cur, ok := byMonth[monthKey]; !ok || e.modTime.After(cur.modTime) {
			byMonth[monthKey] = e
		}
	}

	days := sortedKeys(byDay)
	for i := 0; i < ret.Daily && i < len(days); i++ {
		keep[byDay[days[i]].name] = true
	}

	weeks := sortedKeys(byWeek)
	for i := 0; i < ret.Weekly && i < len(weeks); i++ {
		keep[byWeek[weeks[i]].name] = true
	}

	months := sortedKeys(byMonth)
	for i := 0; i < ret.Monthly && i < len(months); i++ {
		keep[byMonth[months[i]].name] = true
	}

	return keep
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	return keys
}
