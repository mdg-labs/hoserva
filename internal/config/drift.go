package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// record is one managed file's manifest entry: the hash Generator recorded
// when it last wrote the file, and whether a caller has since taken
// ownership of it with KeepUnmanaged.
type record struct {
	Hash        string    `json:"hash"`
	Revision    int       `json:"revision"`
	GeneratedAt time.Time `json:"generated_at"`
	Unmanaged   bool      `json:"unmanaged"`
}

// Status is a managed file's drift state, as of the last Check.
type Status int

const (
	// StatusUnknown means Generator has no record of the path at all — it
	// has never been written.
	StatusUnknown Status = iota
	// StatusManaged means the file on disk hashes to what Generator last
	// wrote.
	StatusManaged
	// StatusDrifted means the file on disk no longer hashes to what
	// Generator last wrote — hand-edited, or removed.
	StatusDrifted
	// StatusUnmanaged means a caller took ownership of the file with
	// KeepUnmanaged; Generator no longer writes or drift-checks it.
	StatusUnmanaged
)

func (s Status) String() string {
	switch s {
	case StatusManaged:
		return "managed"
	case StatusDrifted:
		return "drifted"
	case StatusUnmanaged:
		return "unmanaged"
	default:
		return "unknown"
	}
}

func (g *Generator) manifestPath() string {
	return filepath.Join(g.Root, ".hoserva", "manifest.json")
}

func (g *Generator) loadManifest() (map[string]record, error) {
	raw, err := os.ReadFile(g.manifestPath())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]record{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config: reading manifest: %w", err)
	}
	m := map[string]record{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("config: parsing manifest: %w", err)
	}
	return m, nil
}

func (g *Generator) saveManifest(m map[string]record) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("config: encoding manifest: %w", err)
	}
	return atomicWrite(g.manifestPath(), raw, 0o644)
}

// Check reports path's current drift Status against Generator's manifest
// (doc 01 §2) — the check an apply runs before overwriting a file, and
// that a periodic timer runs without ever touching a data disk: every path
// Generator has a record for lives under Root, never under /mnt.
func (g *Generator) Check(ctx context.Context, path string) (Status, error) {
	if err := ctx.Err(); err != nil {
		return StatusUnknown, err
	}
	manifest, err := g.loadManifest()
	if err != nil {
		return StatusUnknown, err
	}
	return g.check(manifest, path)
}

func (g *Generator) check(manifest map[string]record, path string) (Status, error) {
	rec, ok := manifest[path]
	if !ok {
		return StatusUnknown, nil
	}
	if rec.Unmanaged {
		return StatusUnmanaged, nil
	}

	current, err := os.ReadFile(filepath.Join(g.Root, path))
	if errors.Is(err, os.ErrNotExist) {
		return StatusDrifted, nil
	}
	if err != nil {
		return StatusUnknown, fmt.Errorf("config: reading %s: %w", path, err)
	}
	if hashContent(current) != rec.Hash {
		return StatusDrifted, nil
	}
	return StatusManaged, nil
}

// CheckAll checks every file Generator has ever written, by path. It is
// what an apply and the periodic drift timer both call (doc 01 §2) — and,
// like Check, it only ever reads files under Root.
func (g *Generator) CheckAll(ctx context.Context) (map[string]Status, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	manifest, err := g.loadManifest()
	if err != nil {
		return nil, err
	}

	result := make(map[string]Status, len(manifest))
	for path := range manifest {
		status, err := g.check(manifest, path)
		if err != nil {
			return nil, err
		}
		result[path] = status
	}
	return result, nil
}

// Diff renders what Write(ctx, file, revision, now) would produce and
// returns a unified diff against the file's current content on disk — the
// "view diff" drift resolution (doc 01 §2). It writes nothing.
func (g *Generator) Diff(ctx context.Context, file File, revision int, now time.Time) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	current, err := os.ReadFile(filepath.Join(g.Root, file.Path))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("config: reading %s: %w", file.Path, err)
	}

	fresh := Header(file.Command, revision, now) + string(file.Body)
	return unifiedDiff(current, []byte(fresh)), nil
}

// KeepUnmanaged is the "keep the file and stop managing it" drift
// resolution (doc 01 §2): path stays exactly as it is on disk, Generator
// stops writing or drift-checking it, and that stays true across every
// later Write and Check until a caller reverses it.
func (g *Generator) KeepUnmanaged(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	manifest, err := g.loadManifest()
	if err != nil {
		return err
	}
	rec, ok := manifest[path]
	if !ok {
		return fmt.Errorf("config: %s was never generated", path)
	}
	rec.Unmanaged = true
	manifest[path] = rec
	return g.saveManifest(manifest)
}

// unifiedDiff is a minimal line-oriented diff, enough for the drift view
// to show what a hand edit changed without a diff library.
func unifiedDiff(want, got []byte) string {
	wantLines := strings.Split(string(want), "\n")
	gotLines := strings.Split(string(got), "\n")

	max := len(wantLines)
	if len(gotLines) > max {
		max = len(gotLines)
	}

	var b strings.Builder
	for i := 0; i < max; i++ {
		var w, g string
		haveW, haveG := i < len(wantLines), i < len(gotLines)
		if haveW {
			w = wantLines[i]
		}
		if haveG {
			g = gotLines[i]
		}
		if haveW && haveG && w == g {
			continue
		}
		if haveW {
			fmt.Fprintf(&b, "-%s\n", w)
		}
		if haveG {
			fmt.Fprintf(&b, "+%s\n", g)
		}
	}
	return b.String()
}
