package template

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// SourceCurated is the source name recorded for a template of Hoserva's own
// catalog (doc 04 §4).
const SourceCurated = "hoserva"

var (
	// ErrTemplateNotFound is returned when the catalog has no template of
	// that id.
	ErrTemplateNotFound = errors.New("template: no template with that id")
	// ErrInvalidTemplate is returned when a catalog entry fails the schema
	// or the template rules, so it is never installed.
	ErrInvalidTemplate = errors.New("template: the catalog entry is not a valid template")
	// ErrInvalidInput is returned for an install value the template does
	// not accept.
	ErrInvalidInput = errors.New("template: invalid install input")
	// ErrGPUUnavailable is returned when a GPU was chosen but the host
	// cannot give it to a container.
	ErrGPUUnavailable = errors.New("template: the GPU cannot be given to a container")
	// ErrNoFreePort is returned when no port above the requested one is
	// free.
	ErrNoFreePort = errors.New("template: no free port is left above the requested one")
)

// Entry is one template as a catalog holds it: the compose.yaml bytes and
// the name of the source they came from.
type Entry struct {
	Source string
	Data   []byte
}

// Catalog finds a template by id.
type Catalog interface {
	Entry(ctx context.Context, id string) (Entry, error)
}

var idPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// DirCatalog is a Catalog over a directory of <id>/compose.yaml, the layout
// of the catalog archive's contents (doc 04 §7).
type DirCatalog struct {
	Root   string
	Source string
}

func (d DirCatalog) Entry(_ context.Context, id string) (Entry, error) {
	if !idPattern.MatchString(id) {
		return Entry{}, fmt.Errorf("%w: %q", ErrTemplateNotFound, id)
	}
	dir := filepath.Join(d.Root, id)
	file := filepath.Join(dir, ComposeFile)
	for _, p := range []string{dir, file} {
		info, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return Entry{}, fmt.Errorf("%w: %q", ErrTemplateNotFound, id)
		}
		if err != nil {
			return Entry{}, fmt.Errorf("reading template %q: %w", id, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 || (p == dir && !info.IsDir()) || (p == file && !info.Mode().IsRegular()) {
			return Entry{}, fmt.Errorf("%w: %q is not a plain template directory", ErrTemplateNotFound, id)
		}
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return Entry{}, fmt.Errorf("reading template %q: %w", id, err)
	}
	return Entry{Source: d.Source, Data: data}, nil
}

// MapCatalog is a Catalog held in memory, id to compose.yaml text.
type MapCatalog struct {
	Source    string
	Templates map[string]string
}

func (m MapCatalog) Entry(_ context.Context, id string) (Entry, error) {
	data, ok := m.Templates[id]
	if !ok {
		return Entry{}, fmt.Errorf("%w: %q", ErrTemplateNotFound, id)
	}
	return Entry{Source: m.Source, Data: []byte(data)}, nil
}
