package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrUnmanaged is returned when a caller asks Generator to write a file
// whose manifest entry was previously taken over with KeepUnmanaged — the
// third drift resolution (doc 01 §2) has to stick until a caller
// explicitly reverses it, not just until the next apply.
var ErrUnmanaged = errors.New("config: file is unmanaged")

// manifestDir is the directory manifest.json lives under (drift.go), and
// reserved: a caller-supplied path can never target anything inside it,
// or KeepUnmanaged/Write could corrupt Generator's own bookkeeping.
const manifestDir = ".hoserva"

// resolvePath turns a caller-supplied, Root-relative path into the
// absolute path Generator may read or write and the manifest key to
// record it under — the one normalized, confined resolver every exported
// method (Write, Check, Diff, KeepUnmanaged) routes through, so equivalent
// spellings of the same path always collide on the same manifest entry
// and no path can resolve outside Root.
func (g *Generator) resolvePath(path string) (full string, key string, err error) {
	if filepath.IsAbs(path) {
		return "", "", fmt.Errorf("config: %s must be relative to Root", path)
	}
	clean := filepath.Clean(path)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("config: %s escapes Root", path)
	}
	if clean == manifestDir || strings.HasPrefix(clean, manifestDir+string(filepath.Separator)) {
		return "", "", fmt.Errorf("config: %s is reserved for Generator's own manifest", path)
	}
	return filepath.Join(g.Root, clean), clean, nil
}

// defaultFileMode is the permission Write uses when File.Mode is left
// zero: readable by any local account, for a generated file that carries
// nothing secret (the mode `hoserva doctor` and similar read-only tooling
// already expects).
const defaultFileMode = 0o644

// secretFileMode is the permission a File whose rendered body embeds a
// credential sets Mode to, so it never lands world-readable the way
// defaultFileMode would leave it — owner-read-write only, since a
// production hoservad runs as root (Q44) and a dev run owns Root itself.
const secretFileMode = 0o600

// File is one file Generator can write: a path relative to Root, the
// `hoserva <command>` line its header names, the rendered body a caller
// wants written below that header, and the permission it lands with. Mode
// left zero writes defaultFileMode; a render path whose body embeds a
// credential sets Mode to secretFileMode instead (nut.go's upsmon.conf),
// or a group-readable mode with Group set (nut.go's upsd.users). Group, when set, names the group the file
// is owned by — for a credential file a service reads after dropping to
// its own group (upsd.users, read by upsd as nut).
type File struct {
	Path    string
	Command string
	Body    []byte
	Mode    os.FileMode
	Group   string
}

// Generator writes managed files under Root and records each one's hash
// so a later Check can compare against it (doc 01 §2). Root is a
// caller-supplied directory — a temp directory in tests and dev runs — so
// nothing in this package ever writes /etc directly.
type Generator struct {
	Root string
	// LookupGroup resolves File.Group to a gid; nil uses the host's
	// group database.
	LookupGroup func(name string) (int, error)
}

func (g *Generator) groupID(name string) (int, error) {
	lookup := g.LookupGroup
	if lookup == nil {
		lookup = lookupHostGroup
	}
	gid, err := lookup(name)
	if err != nil {
		return 0, fmt.Errorf("config: resolving group %q: %w", name, err)
	}
	return gid, nil
}

func lookupHostGroup(name string) (int, error) {
	grp, err := user.LookupGroup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(grp.Gid)
}

// NewGenerator returns a Generator that writes under root.
func NewGenerator(root string) *Generator {
	return &Generator{Root: root}
}

// CanWrite reports whether Write would refuse path without writing it.
// It returns ErrUnmanaged or ErrExistingHostFile for the same Q76 cases
// Write does, so a multi-file apply can preflight every target before
// replacing any of them.
func (g *Generator) CanWrite(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	full, key, err := g.resolvePath(path)
	if err != nil {
		return err
	}
	manifest, err := g.loadManifest()
	if err != nil {
		return err
	}
	return wouldRefuse(manifest, full, key)
}

func wouldRefuse(manifest map[string]record, full, key string) error {
	if rec, ok := manifest[key]; ok && rec.Unmanaged {
		return fmt.Errorf("%w: %s", ErrUnmanaged, key)
	}
	if _, ok := manifest[key]; !ok {
		if _, err := os.Stat(full); err == nil {
			return fmt.Errorf("%w: %s", ErrExistingHostFile, key)
		} else if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("config: checking %s: %w", key, err)
		}
	}
	return nil
}

// Write renders file's doc 01 §2 header plus its body, writes the result
// atomically (temp file plus rename) under Root, and records its hash for
// a later Check. It refuses a file KeepUnmanaged took over (ErrUnmanaged)
// — an unmanaged file stays unmanaged until a caller re-manages it.
func (g *Generator) Write(ctx context.Context, file File, revision int, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	full, key, err := g.resolvePath(file.Path)
	if err != nil {
		return err
	}

	manifest, err := g.loadManifest()
	if err != nil {
		return err
	}
	if err := wouldRefuse(manifest, full, key); err != nil {
		return err
	}
	_, tracked := manifest[key]
	untracked := !tracked

	mode := file.Mode
	if mode == 0 {
		mode = defaultFileMode
	}
	gid := -1
	if file.Group != "" {
		if gid, err = g.groupID(file.Group); err != nil {
			return err
		}
	}
	content := Header(file.Command, revision, now) + string(file.Body)
	if err := atomicWrite(full, []byte(content), mode, gid, untracked); err != nil {
		if untracked && errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %s", ErrExistingHostFile, key)
		}
		return err
	}

	manifest[key] = record{
		Hash:        hashContent([]byte(content)),
		Revision:    revision,
		GeneratedAt: now.UTC(),
	}
	return g.saveManifest(manifest)
}

func hashContent(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// atomicWrite writes data to path by creating a temp file in path's own
// directory, syncing and closing it, then renaming it over path — so a
// reader never observes a partially written file, and a crash mid-write
// leaves the previous file (or none) rather than a truncated one. exclusive
// uses renameat2(RENAME_NOREPLACE) so the first create of an untracked host
// file cannot replace a destination that appeared after Stat. It also
// fsyncs the destination directory after the rename: tmp.Sync() below
// only persists the temp file's own content, not the renamed directory
// entry — without this, a crash right after a successful Rename can still
// lose the new file (or leave the old one) despite Write having returned
// nil.
// gid -1 leaves the new file's group as created; any other value is
// applied to the temp file before the rename, so the file never appears
// at path with the wrong group.
func atomicWrite(path string, data []byte, perm os.FileMode, gid int, exclusive bool) error {
	dir := filepath.Dir(path)
	if err := ensureDirSynced(dir); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("config: creating temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: writing %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: syncing %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: closing %s: %w", tmpPath, err)
	}
	if gid >= 0 {
		if err := os.Chown(tmpPath, -1, gid); err != nil {
			return fmt.Errorf("config: setting group on %s: %w", tmpPath, err)
		}
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return fmt.Errorf("config: setting permissions on %s: %w", tmpPath, err)
	}
	if exclusive {
		if err := renameNoReplace(tmpPath, path); err != nil {
			if errors.Is(err, os.ErrExist) {
				return err
			}
			return fmt.Errorf("config: renaming %s to %s: %w", tmpPath, path, err)
		}
	} else if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("config: renaming %s to %s: %w", tmpPath, path, err)
	}
	return fsyncDir(dir)
}

// ensureDirSynced is os.MkdirAll, except every newly created directory's
// entry is fsynced into its parent as soon as it's created. MkdirAll alone
// leaves those entries unpersisted until something unrelated happens to
// sync the parent — a crash before that could lose the directory Write
// just reported creating.
func ensureDirSynced(dir string) error {
	info, err := os.Stat(dir)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("config: %s exists and is not a directory", dir)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("config: checking %s: %w", dir, err)
	}

	parent := filepath.Dir(dir)
	if parent != dir {
		if err := ensureDirSynced(parent); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, 0o755); err != nil && !os.IsExist(err) {
		return fmt.Errorf("config: creating directory %s: %w", dir, err)
	}
	return fsyncDir(parent)
}

// fsyncDir opens dir and fsyncs it directly — the documented way to
// persist a directory entry (a create, rename or remove within it) rather
// than just the file content involved.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("config: opening %s to sync: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("config: syncing %s: %w", dir, err)
	}
	return nil
}
