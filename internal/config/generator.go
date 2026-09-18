package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrUnmanaged is returned when a caller asks Generator to write a file
// whose manifest entry was previously taken over with KeepUnmanaged — the
// third drift resolution (doc 01 §2) has to stick until a caller
// explicitly reverses it, not just until the next apply.
var ErrUnmanaged = errors.New("config: file is unmanaged")

// File is one file Generator can write: a path relative to Root, the
// `hoserva <command>` line its header names, and the rendered body a
// caller wants written below that header.
type File struct {
	Path    string
	Command string
	Body    []byte
}

// Generator writes managed files under Root and records each one's hash
// so a later Check can compare against it (doc 01 §2). Root is a
// caller-supplied directory — a temp directory in tests and dev runs — so
// nothing in this package ever writes /etc directly.
type Generator struct {
	Root string
}

// NewGenerator returns a Generator that writes under root.
func NewGenerator(root string) *Generator {
	return &Generator{Root: root}
}

// Write renders file's doc 01 §2 header plus its body, writes the result
// atomically (temp file plus rename) under Root, and records its hash for
// a later Check. It refuses a file KeepUnmanaged took over (ErrUnmanaged)
// — an unmanaged file stays unmanaged until a caller re-manages it.
func (g *Generator) Write(ctx context.Context, file File, revision int, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	manifest, err := g.loadManifest()
	if err != nil {
		return err
	}
	if rec, ok := manifest[file.Path]; ok && rec.Unmanaged {
		return fmt.Errorf("%w: %s", ErrUnmanaged, file.Path)
	}

	content := Header(file.Command, revision, now) + string(file.Body)
	full := filepath.Join(g.Root, file.Path)
	if err := atomicWrite(full, []byte(content), 0o644); err != nil {
		return err
	}

	manifest[file.Path] = record{
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
// leaves the previous file (or none) rather than a truncated one.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("config: creating directory %s: %w", dir, err)
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
	if err := os.Chmod(tmpPath, perm); err != nil {
		return fmt.Errorf("config: setting permissions on %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("config: renaming %s to %s: %w", tmpPath, path, err)
	}
	return nil
}
