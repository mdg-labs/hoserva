package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// archiveNamePattern matches doc 10 §1's archive filenames. The first group
// is the id of the installation that wrote the archive, absent on an
// archive written before archives carried one. The timestamp group accepts
// both the original minute resolution
// (hoserva-config-2026-09-14T03-00.tar.zst, still on disk from before
// #401) and the current second resolution
// (hoserva-config-<id>-2026-09-14T03-00-05.tar.zst) that fix added, an
// optional "-<n>" collision counter (#401), an optional ".<reason>"
// pre-change marker (#401), and the same name with a trailing ".age" when
// the archive written to a destination was age-encrypted (Q80).
var archiveNamePattern = regexp.MustCompile(`^hoserva-config-(?:([0-9a-f]{12})-)?(\d{4}-\d{2}-\d{2}T\d{2}-\d{2}(?:-\d{2})?)(?:-\d+)?(?:\.(pre-import|pre-update|pre-topology))?\.tar\.zst(\.age)?$`)

// DestinationType is how a destination is reached (doc 10 §1). A local
// path is written directly; every other type goes through rclone.
type DestinationType string

const (
	TypeLocal  DestinationType = "local"
	TypeSMB    DestinationType = "smb"
	TypeS3     DestinationType = "s3"
	TypeSFTP   DestinationType = "sftp"
	TypeWebDAV DestinationType = "webdav"
	TypeRclone DestinationType = "rclone"
)

// Destination is one backup target (doc 10 §1, Q40). The zero Type is a
// local path. Encrypt requests age encryption of the archive written here
// (Q80): local destinations may opt in, and every remote destination has
// it set, after ValidateRemoteDestination has already refused one with no
// backup passphrase configured.
type Destination struct {
	ID   string
	Name string
	Type DestinationType
	// Path is the directory for a local destination; for a remote one it
	// is the path within the remote (bucket and prefix, share and folder).
	Path      string
	Enabled   bool
	Encrypt   bool
	Retention Retention

	// Options are the non-secret rclone backend settings of a remote
	// destination, keyed by rclone's own option names.
	Options map[string]string
	// SealedSecrets is the machine-key ciphertext of the remote
	// destination's credentials, as stored. Secrets holds them in plain
	// text, and is only ever populated for the duration of one operation.
	SealedSecrets []byte
	Secrets       map[string]string

	LastSuccessfulBackupAt *time.Time
	StaleAlertedAt         *time.Time
	CreatedAt              time.Time
}

func (d Destination) isRemote() bool {
	return d.Type != "" && d.Type != TypeLocal
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
	reason  Reason
	// installation is the id of the installation that wrote the archive;
	// empty for a legacy archive whose name carries none.
	installation string
}

// archiveOwner says which archives on a destination one installation may
// prune. A destination can be shared with other installations (one storage
// box, one bucket prefix, one NFS mount), so retention only ever removes
// archives that carry this installation's id. A legacy archive, written
// before names carried an id, is only claimed on a destination this
// installation alone has written to: the default boot and pool
// destinations.
type archiveOwner struct {
	installation string
	legacy       bool
}

func (o archiveOwner) owns(e archiveEntry) bool {
	if e.installation == "" {
		return o.legacy
	}
	return e.installation == o.installation
}

// writeArchive copies archivePath into dest.Path under its basename. A
// destination directory it has to create is private (0700); one that
// already exists keeps its mode and owner, since it may be a share other
// users rely on. The archive itself is always written 0600.
func writeArchive(dest Destination, archivePath string) error {
	if err := os.MkdirAll(dest.Path, 0o700); err != nil {
		return fmt.Errorf("creating destination %q: %w", dest.Path, err)
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

// archiveTarget is where a destination's archives live: a directory, or
// an rclone remote.
type archiveTarget interface {
	write(ctx context.Context, srcPath string) error
	list(ctx context.Context) ([]archiveEntry, error)
	remove(ctx context.Context, name string) error
	readBack(ctx context.Context, name string) ([]byte, error)
}

type localTarget struct {
	dest Destination
}

func (t localTarget) write(_ context.Context, srcPath string) error {
	return writeArchive(t.dest, srcPath)
}

func (t localTarget) list(_ context.Context) ([]archiveEntry, error) {
	return listArchives(t.dest.Path)
}

func (t localTarget) readBack(_ context.Context, name string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(t.dest.Path, name))
	if err != nil {
		return nil, fmt.Errorf("reading %q back: %w", filepath.Join(t.dest.Path, name), err)
	}
	return b, nil
}

func (t localTarget) remove(_ context.Context, name string) error {
	if err := os.Remove(filepath.Join(t.dest.Path, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing %q: %w", filepath.Join(t.dest.Path, name), err)
	}
	return nil
}

// pruneDestination enforces dest's retention on a local destination.
func pruneDestination(dest Destination, owner archiveOwner, now time.Time, justWritten string) error {
	return pruneTarget(context.Background(), localTarget{dest: dest}, owner, dest.Retention, now, justWritten)
}

// pruneTarget enforces retention on the archives owner owns — only
// filenames matching archiveNamePattern that carry owner's installation id
// (or, for a legacy owner, no id) are candidates, and an archive another
// installation wrote to the same directory is neither counted towards nor
// removed by this retention. An encrypted archive's
// identity sidecar (identitySidecarSuffix) is never itself a candidate —
// it never matches archiveNamePattern — but is removed alongside its own
// archive so pruning an old encrypted archive never leaves its sidecar
// behind as an orphan.
func pruneTarget(ctx context.Context, t archiveTarget, owner archiveOwner, ret Retention, now time.Time, justWritten string) error {
	listed, err := t.list(ctx)
	if err != nil {
		return err
	}
	var entries []archiveEntry
	for _, e := range listed {
		if owner.owns(e) {
			entries = append(entries, e)
		}
	}
	keep := retentionKeepers(entries, ret, now, justWritten)
	for _, e := range entries {
		if keep[e.name] {
			continue
		}
		if err := t.remove(ctx, e.name+identitySidecarSuffix); err != nil {
			return fmt.Errorf("pruning identity sidecar for %q: %w", e.name, err)
		}
		if err := t.remove(ctx, e.name); err != nil {
			return fmt.Errorf("pruning archive %q: %w", e.name, err)
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
		m := archiveNamePattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, archiveEntry{
			name:         e.Name(),
			path:         filepath.Join(dir, e.Name()),
			modTime:      info.ModTime(),
			reason:       Reason(m[3]),
			installation: m[1],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].modTime.After(out[j].modTime)
	})
	return out, nil
}

// preChangeKeepCount bounds how many pre-change archives (#401) survive
// per destination on top of the daily/weekly/monthly tiers below — doc 10
// §1's recommended default, covering a burst of imports or updates on one
// day without growing retention unbounded.
const preChangeKeepCount = 5

func retentionKeepers(entries []archiveEntry, ret Retention, now time.Time, justWritten string) map[string]bool {
	keep := map[string]bool{justWritten: true}
	if len(entries) == 0 {
		return keep
	}

	// entries is sorted newest-first (listArchives), so the first
	// preChangeKeepCount pre-change archives encountered are the most
	// recent ones — exactly the ones #401 requires to survive an ordinary
	// same-day backup's daily-tier pruning. Any pre-change archive beyond
	// the bound falls back to the same daily/weekly/monthly tiers as an
	// ordinary archive.
	preChangeKept := 0
	for _, e := range entries {
		if e.reason == ReasonNone {
			continue
		}
		if preChangeKept >= preChangeKeepCount {
			break
		}
		keep[e.name] = true
		preChangeKept++
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
