package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
)

// ReasonPreRestore marks the snapshot a restore takes of the appdata it is
// about to replace. Retention keeps the newest preChangeKeepCount pre-change
// (pre-restore and pre-update) archives, together, per container in addition
// to the ordinary tiers.
const ReasonPreRestore Reason = "pre-restore"

// appdataNamePattern matches appdata archive names:
// hoserva-appdata-<installation>-<container>-<timestamp>[-<n>][.pre-restore|.pre-update].tar.zst,
// with a trailing .age when it was encrypted. The container is whatever
// precedes the last timestamp; Docker names never end in one.
var appdataNamePattern = regexp.MustCompile(`^hoserva-appdata-([0-9a-f]{12})-(.+)-(\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2})(?:-\d+)?(?:\.(pre-restore|pre-update))?\.tar\.zst(\.age)?$`)

func appdataArchiveName(installation, container string, now time.Time, reason Reason, suffix int) string {
	ts := now.UTC().Format("2006-01-02T15-04-05")
	if suffix > 0 {
		ts = fmt.Sprintf("%s-%d", ts, suffix)
	}
	if reason != ReasonNone {
		return fmt.Sprintf("hoserva-appdata-%s-%s-%s.%s.tar.zst", installation, container, ts, reason)
	}
	return fmt.Sprintf("hoserva-appdata-%s-%s-%s.tar.zst", installation, container, ts)
}

// resolveAppdataName picks the first archive name no local destination
// already holds.
func resolveAppdataName(installation, container string, now time.Time, reason Reason, dests []Destination) string {
	name := appdataArchiveName(installation, container, now, reason, 0)
	for suffix := 2; archiveNameTaken(name, dests); suffix++ {
		name = appdataArchiveName(installation, container, now, reason, suffix)
	}
	return name
}

// AppdataArchive is one appdata archive on a destination.
type AppdataArchive struct {
	Name            string
	Container       string
	DestinationID   string
	DestinationName string
	ModTime         time.Time
	Size            int64
	Encrypted       bool
	Reason          Reason
}

func parseAppdataName(name string) (installation, container string, reason Reason, encrypted, ok bool) {
	m := appdataNamePattern.FindStringSubmatch(name)
	if m == nil {
		return "", "", "", false, false
	}
	return m[1], m[2], Reason(m[4]), m[5] != "", true
}

// AppdataUnavailable is a destination that could not be listed.
type AppdataUnavailable struct {
	DestinationID string
	Message       string
}

// destinations are the enabled destinations appdata archives go to. The
// boot device's default destination is left out: appdata archives are far
// larger than config archives, and the boot device is small.
func (a *AppdataService) destinations(ctx context.Context) ([]Destination, error) {
	all, err := a.Backup.loadDestinations(ctx)
	if err != nil {
		return nil, err
	}
	var out []Destination
	for _, d := range all {
		if d.Enabled && (d.ID != DefaultBootID || d.isRemote()) {
			out = append(out, d)
		}
	}
	return out, nil
}

func (a *AppdataService) passphrase(ctx context.Context) (string, error) {
	if a.Backup.Secrets == nil {
		return "", nil
	}
	p, ok, err := a.Backup.Secrets.BackupPassphrase(ctx)
	if err != nil {
		return "", fmt.Errorf("reading backup passphrase: %w", err)
	}
	if !ok {
		return "", nil
	}
	return p, nil
}

// requireEncryption refuses, before anything is stopped, a run that would
// have to encrypt and cannot.
func (a *AppdataService) requireEncryption(ctx context.Context, dests []Destination) (passphrase string, err error) {
	passphrase, err = a.passphrase(ctx)
	if err != nil {
		return "", err
	}
	for _, d := range dests {
		if !d.Encrypt && !d.isRemote() {
			continue
		}
		if a.Backup.Recipient == nil || a.Backup.Recipient.Public == "" || a.Backup.Recipient.Identity == "" {
			return "", fmt.Errorf("destination %q encrypts, but no onboarding recipient is available", d.Name)
		}
		if passphrase == "" {
			return "", fmt.Errorf("destination %q encrypts, but no backup passphrase is set", d.Name)
		}
	}
	return passphrase, nil
}

// uploadAppdata writes the archive at archivePath (and its encrypted form
// and identity sidecar, where a destination encrypts) to every destination
// and prunes each by its retention. It reports how many destinations hold
// the archive afterwards, and every destination it could not write. The
// encrypted copies are removed before it returns. restoring names the
// archive a restore is reading, which no destination's prune removes ("" when
// nothing is being restored).
func (a *AppdataService) uploadAppdata(ctx context.Context, dests []Destination, archivePath, name, container, passphrase, restoring string, now time.Time) (written int, failures []error) {
	var artifacts *encryptedArtifacts
	defer func() {
		if artifacts != nil {
			_ = os.Remove(artifacts.ArchivePath)
			_ = os.Remove(artifacts.SidecarPath)
		}
	}()
	for _, dest := range dests {
		release, why := a.Backup.admitDestination(ctx, dest)
		if why != "" {
			a.Backup.log("skipping destination %q for %s: %s", dest.ID, name, why)
			failures = append(failures, fmt.Errorf("destination %q skipped: %s", dest.ID, why))
			continue
		}
		err := a.writeAppdataDestination(ctx, dest, archivePath, name, container, passphrase, restoring, now, &artifacts)
		release()
		if err != nil {
			failures = append(failures, err)
			var pruneErr *appdataPruneError
			if errors.As(err, &pruneErr) {
				written++
			}
			continue
		}
		written++
	}
	return written, failures
}

// appdataPruneError is a destination that holds the new archive but could
// not prune its old ones.
type appdataPruneError struct{ err error }

func (e *appdataPruneError) Error() string { return e.err.Error() }
func (e *appdataPruneError) Unwrap() error { return e.err }

func (a *AppdataService) writeAppdataDestination(ctx context.Context, dest Destination, archivePath, name, container, passphrase, restoring string, now time.Time, artifacts **encryptedArtifacts) error {
	if dest.isRemote() && !dest.Encrypt {
		return fmt.Errorf("writing destination %q: a remote destination is never written unencrypted (Q80)", dest.ID)
	}
	writePath, writeName, sidecarPath := archivePath, name, ""
	if dest.Encrypt {
		if *artifacts == nil {
			built, err := a.Backup.buildArtifacts(archivePath, passphrase)
			if err != nil {
				return fmt.Errorf("encrypting archive for destination %q: %w", dest.ID, err)
			}
			*artifacts = built
		}
		writePath, writeName, sidecarPath = (*artifacts).ArchivePath, filepath.Base((*artifacts).ArchivePath), (*artifacts).SidecarPath
	}
	target, err := a.Backup.targetFor(ctx, dest)
	if err != nil {
		return fmt.Errorf("preparing destination %q: %w", dest.ID, err)
	}
	if sidecarPath != "" {
		if err := target.write(ctx, sidecarPath); err != nil {
			return fmt.Errorf("writing destination %q: %w", dest.ID, err)
		}
	}
	if err := target.write(ctx, writePath); err != nil {
		return fmt.Errorf("writing destination %q: %w", dest.ID, err)
	}
	if err := pruneAppdata(ctx, target, a.Backup.installationID(), dest.Retention, now, container, writeName, restoring); err != nil {
		return &appdataPruneError{fmt.Errorf("pruning destination %q: %w", dest.ID, err)}
	}
	return nil
}

type appdataEntry struct {
	archiveEntry
	container string
	size      int64
	encrypted bool
}

func listAppdata(ctx context.Context, t archiveTarget, installation string) ([]appdataEntry, error) {
	files, err := t.files(ctx)
	if err != nil {
		return nil, err
	}
	var out []appdataEntry
	for _, f := range files {
		inst, container, reason, encrypted, ok := parseAppdataName(f.name)
		if !ok || inst != installation {
			continue
		}
		out = append(out, appdataEntry{
			archiveEntry: archiveEntry{name: f.name, modTime: f.modTime, reason: reason, installation: inst},
			container:    container,
			size:         f.size,
			encrypted:    encrypted,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].modTime.After(out[j].modTime) })
	return out, nil
}

// pruneAppdata enforces the destination's retention on this installation's
// archives of one container. The pre-change (pre-restore and pre-update)
// archives are kept apart from the ordinary tiers: the newest
// preChangeKeepCount of them, together, survive, and they never take an
// ordinary backup's daily, weekly or monthly slot. The archive just written
// always survives, and so does the archive in restoring (the one being
// restored, matched with or without its ".age"), so a restore's own
// pre-restore snapshot never prunes what it restores from. An encrypted
// archive's identity sidecar goes with it.
func pruneAppdata(ctx context.Context, t archiveTarget, installation string, ret Retention, now time.Time, container, justWritten, restoring string) error {
	listed, err := listAppdata(ctx, t, installation)
	if err != nil {
		return err
	}
	var ordinary, snapshots []archiveEntry
	for _, e := range listed {
		if e.container != container {
			continue
		}
		if e.reason == ReasonNone {
			ordinary = append(ordinary, e.archiveEntry)
		} else {
			snapshots = append(snapshots, e.archiveEntry)
		}
	}
	keep := retentionKeepers(ordinary, ret, now, justWritten)
	for i, e := range snapshots {
		if i < preChangeKeepCount {
			keep[e.name] = true
		}
	}
	for _, e := range append(ordinary, snapshots...) {
		if keep[e.name] || (restoring != "" && strings.TrimSuffix(e.name, ".age") == strings.TrimSuffix(restoring, ".age")) {
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

// ListArchives lists this installation's appdata archives on every enabled
// destination that takes them, newest first, optionally for one container.
// A destination that cannot be listed is reported, not read as empty.
func (a *AppdataService) ListArchives(ctx context.Context, container string) ([]AppdataArchive, []AppdataUnavailable, error) {
	dests, err := a.destinations(ctx)
	if err != nil {
		return nil, nil, err
	}
	var archives []AppdataArchive
	var unavailable []AppdataUnavailable
	for _, dest := range dests {
		release, why := a.Backup.admitDestination(ctx, dest)
		if why != "" {
			unavailable = append(unavailable, AppdataUnavailable{DestinationID: dest.ID, Message: why})
			continue
		}
		entries, err := a.listDestination(ctx, dest)
		release()
		if err != nil {
			unavailable = append(unavailable, AppdataUnavailable{DestinationID: dest.ID, Message: err.Error()})
			continue
		}
		for _, e := range entries {
			if container != "" && e.container != container {
				continue
			}
			archives = append(archives, AppdataArchive{
				Name: e.name, Container: e.container, DestinationID: dest.ID, DestinationName: dest.Name,
				ModTime: e.modTime, Size: e.size, Encrypted: e.encrypted, Reason: e.reason,
			})
		}
	}
	sort.SliceStable(archives, func(i, j int) bool { return archives[i].ModTime.After(archives[j].ModTime) })
	return archives, unavailable, nil
}

func (a *AppdataService) listDestination(ctx context.Context, dest Destination) ([]appdataEntry, error) {
	target, err := a.Backup.targetFor(ctx, dest)
	if err != nil {
		return nil, fmt.Errorf("preparing destination %q: %w", dest.ID, err)
	}
	return listAppdata(ctx, target, a.Backup.installationID())
}

// fetchAppdata copies the named archive from dest into dir and, when it is
// encrypted, decrypts it with the onboarding identity. It returns the path
// of the plain tar.zst.
func (a *AppdataService) fetchAppdata(ctx context.Context, dest Destination, name, dir string) (string, error) {
	release, why := a.Backup.admitDestination(ctx, dest)
	if why != "" {
		return "", fmt.Errorf("reading destination %q: %s", dest.ID, why)
	}
	defer release()
	target, err := a.Backup.targetFor(ctx, dest)
	if err != nil {
		return "", fmt.Errorf("preparing destination %q: %w", dest.ID, err)
	}
	entries, err := listAppdata(ctx, target, a.Backup.installationID())
	if err != nil {
		return "", fmt.Errorf("listing destination %q: %w", dest.ID, err)
	}
	found := false
	for _, e := range entries {
		if e.name == name {
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("%w: %s on destination %q", ErrAppdataArchiveNotFound, name, dest.ID)
	}
	fetched := filepath.Join(dir, "fetched")
	if err := target.fetch(ctx, name, fetched); err != nil {
		return "", fmt.Errorf("fetching %s from destination %q: %w", name, dest.ID, err)
	}
	if _, _, _, encrypted, _ := parseAppdataName(name); !encrypted {
		return fetched, nil
	}
	if a.Backup.Recipient == nil || a.Backup.Recipient.Identity == "" {
		return "", errors.New("the archive is encrypted, but no onboarding identity is available to open it")
	}
	plain := filepath.Join(dir, "plain")
	if err := decryptArchiveFile(fetched, plain, a.Backup.Recipient.Identity); err != nil {
		return "", err
	}
	_ = os.Remove(fetched)
	return plain, nil
}

// decryptArchiveFile decrypts src with the age X25519 identity into dst,
// streaming, since an appdata archive does not fit in memory.
func decryptArchiveFile(src, dst, identity string) error {
	id, err := parseIdentity(identity)
	if err != nil {
		return fmt.Errorf("parsing onboarding identity: %w", err)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening encrypted archive: %w", err)
	}
	defer func() { _ = in.Close() }()
	r, err := age.Decrypt(in, id)
	if err != nil {
		return fmt.Errorf("decrypting archive: %w", err)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("creating %s: %w", dst, err)
	}
	if _, err := io.Copy(out, r); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("decrypting archive: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("decrypting archive: %w", err)
	}
	return nil
}
