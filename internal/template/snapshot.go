package template

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
)

// snapshotFS holds the pinned catalog archive and its detached signature
// (Q65). snapshot/ is filled by `make catalog-snapshot`
// (scripts/devenv/catalog-snapshot.sh) from the pin in
// scripts/devenv/catalog.pin and is git-ignored; a committed
// snapshot/.gitkeep keeps //go:embed satisfied on a clean checkout that has
// never run it, so `go build ./...` and `go vet ./...` never fail for its
// absence. `make build` fails outright if the archive is missing, so a
// release never ships the placeholder.
//
//go:embed all:snapshot
var snapshotFS embed.FS

const (
	snapshotArchive   = "snapshot/catalog.tar.zst"
	snapshotSignature = "snapshot/catalog.tar.zst.sig"
)

// ErrNoSnapshot is returned when this build carries no catalog archive
// (only the placeholder is embedded).
var ErrNoSnapshot = errors.New("template: this build embeds no catalog snapshot")

// EmbeddedSnapshot returns the archive and detached signature embedded at
// build time. Neither is verified here.
func EmbeddedSnapshot() (archive, sig []byte, err error) {
	archive, err = snapshotFS.ReadFile(snapshotArchive)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, ErrNoSnapshot
	}
	if err != nil {
		return nil, nil, fmt.Errorf("reading the embedded catalog archive: %w", err)
	}
	sig, err = snapshotFS.ReadFile(snapshotSignature)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, fmt.Errorf("%w: the signature is missing", ErrNoSnapshot)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("reading the embedded catalog signature: %w", err)
	}
	return archive, sig, nil
}
