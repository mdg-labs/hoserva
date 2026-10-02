package template

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/mdg-labs/hoserva/internal/store"
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
	// ErrCatalogUnavailable is returned when the catalog cannot be listed:
	// none is installed, or its index.json cannot be read or is not a
	// catalog index.
	ErrCatalogUnavailable = errors.New("template: the catalog is not available")
	// ErrIconNotFound is returned when a template has no icon that can be
	// served: no such file, not a plain file inside the template's
	// directory, an extension outside the allow-list, or too large.
	ErrIconNotFound = errors.New("template: the template has no icon that can be served")
	// ErrScreenshotNotFound is returned when a template has no screenshot
	// with that number, or the file cannot be served: no such file, not a
	// plain file inside the template's directory, or too large.
	ErrScreenshotNotFound = errors.New("template: the template has no screenshot that can be served")
	// ErrInvalidInput is returned for an install value the template does
	// not accept.
	ErrInvalidInput = errors.New("template: invalid install input")
	// ErrGPUUnavailable is returned when a GPU was chosen but the host
	// cannot give it to a container.
	ErrGPUUnavailable = errors.New("template: the GPU cannot be given to a container")
	// ErrNoFreePort is returned when no port above the requested one is
	// free.
	ErrNoFreePort = errors.New("template: no free port is left above the requested one")
	// ErrPortTaken is returned when a stack's port input is changed to a
	// port that a container, another stack or the host already uses.
	ErrPortTaken = errors.New("template: the port is already in use")
	// ErrNetworkMissing is returned by an install that names a Docker network
	// that does not exist. Hoserva never creates networks (Q37).
	ErrNetworkMissing = errors.New("template: the network does not exist")
	// ErrStackHasNoTemplate is returned for a stack whose Compose file has
	// no x-hoserva block, so it has no inputs to show or change.
	ErrStackHasNoTemplate = errors.New("template: the stack has no x-hoserva block")
)

// Entry is one template as a catalog holds it: the compose.yaml bytes and
// the source they came from, with that source's badge: its Kind and whether
// its archive was Signed (verified against a key). A catalog that does not
// set them reports nothing about its source's trust, which readers treat as
// user-added and unsigned.
type Entry struct {
	Source string
	Kind   store.CatalogSourceKind
	Signed bool
	Data   []byte
}

// IndexEntry is one template as the catalog's index lists it.
type IndexEntry struct {
	ID         string
	Revision   int
	Title      string
	Categories []string
	Docs       string
	// Maintainer is empty when the template names none.
	Maintainer string
	// Description is empty when the template names none.
	Description string
	// Source, Kind and Signed are the entry's source and its badge, as on
	// Entry.
	Source string
	Kind   store.CatalogSourceKind
	Signed bool
}

// Index is what a catalog lists: its serial, when it was built when it says
// so, and each template in the catalog's own order.
type Index struct {
	Serial      int64
	GeneratedAt time.Time
	Templates   []IndexEntry
}

// Icon is a template's icon file with the content type its extension is
// allowed to be served as.
type Icon struct {
	ContentType string
	Data        []byte
}

// Screenshot is one of a template's screenshots with the content type its
// extension is allowed to be served as.
type Screenshot struct {
	ContentType string
	Data        []byte
}

// Catalog is a source of templates (doc 04 §4): the one interface the
// curated catalog sits behind, so another source changes what is registered,
// not the code that lists, shows and installs templates.
type Catalog interface {
	// Name is the source's name, recorded on every template it supplies.
	Name() string
	// Index lists the templates, or fails with ErrCatalogUnavailable.
	Index(ctx context.Context) (Index, error)
	// Entry returns one template's compose.yaml, or ErrTemplateNotFound.
	Entry(ctx context.Context, id string) (Entry, error)
	// Icon returns one template's icon, ErrTemplateNotFound for an unknown
	// template or ErrIconNotFound for one with no servable icon.
	Icon(ctx context.Context, id string) (Icon, error)
	// Screenshot returns the template's screenshot at that position in its
	// x-hoserva screenshots list, ErrTemplateNotFound for an unknown
	// template or ErrScreenshotNotFound for a position the template has no
	// servable screenshot at.
	Screenshot(ctx context.Context, id string, index int) (Screenshot, error)
}

const (
	maxIconBytes       = 1 << 20
	maxScreenshotBytes = 4 << 20
)

var iconTypes = map[string]string{
	".svg":  "image/svg+xml",
	".png":  "image/png",
	".webp": "image/webp",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
}

// iconFile returns the icon's file name and content type from the template
// the entry holds.
func iconFile(id string, entry Entry) (name, contentType string, err error) {
	t, issues := Parse(entry.Data)
	if t == nil {
		return "", "", invalidTemplate(id, issues)
	}
	ct, ok := iconTypes[strings.ToLower(filepath.Ext(t.Block.Icon))]
	if !ok {
		return "", "", fmt.Errorf("%w: %q has the extension of no allowed image type", ErrIconNotFound, t.Block.Icon)
	}
	return t.Block.Icon, ct, nil
}

// screenshotFile returns the file name and content type of the screenshot at
// index in the template the entry holds.
func screenshotFile(id string, entry Entry, index int) (name, contentType string, err error) {
	t, issues := Parse(entry.Data)
	if t == nil {
		return "", "", invalidTemplate(id, issues)
	}
	if index < 0 || index >= len(t.Block.Screenshots) {
		return "", "", fmt.Errorf("%w: template %q has %d screenshots", ErrScreenshotNotFound, id, len(t.Block.Screenshots))
	}
	name = t.Block.Screenshots[index]
	ct, ok := iconTypes[strings.ToLower(filepath.Ext(name))]
	if !ok || ct == "image/svg+xml" {
		return "", "", fmt.Errorf("%w: %q has the extension of no allowed image type", ErrScreenshotNotFound, name)
	}
	return name, ct, nil
}

var idPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// DirCatalog is a Catalog over a directory of index.json and <id>/, the
// layout of the catalog archive's contents (doc 04 §7).
type DirCatalog struct {
	Root   string
	Source string
	// Kind and Signed are the badge every entry of this directory carries.
	Kind   store.CatalogSourceKind
	Signed bool
}

var errNotPlain = errors.New("not a plain file")

// openPlain opens a regular file for reading and refuses a symlink, however
// it points, and anything that is not a regular file; opening never blocks
// on a pipe.
func openPlain(file string) (*os.File, error) {
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ELOOP) {
		return nil, fmt.Errorf("%w: %s is a symlink", errNotPlain, file)
	}
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s", errNotPlain, file)
	}
	return f, nil
}

// plainFileUnder checks that rel names a regular file under root without any
// symlink on the way: every directory component and the file itself are
// looked at without following links. It returns the file's info.
func plainFileUnder(root, rel string) (os.FileInfo, error) {
	parts := strings.Split(rel, "/")
	p := root
	var info os.FileInfo
	for i, part := range parts {
		p = filepath.Join(p, part)
		var err error
		info, err = os.Lstat(p)
		if err != nil {
			return nil, err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: %s is a symlink", errNotPlain, p)
		}
		if last := i == len(parts)-1; last != info.Mode().IsRegular() || (!last && !info.IsDir()) {
			return nil, fmt.Errorf("%w: %s", errNotPlain, p)
		}
	}
	return info, nil
}

func (d DirCatalog) Name() string { return d.Source }

type indexDoc struct {
	Serial      int64     `json:"serial"`
	GeneratedAt time.Time `json:"generatedAt"`
	Templates   []struct {
		ID          string   `json:"id"`
		Revision    int      `json:"revision"`
		Title       string   `json:"title"`
		Categories  []string `json:"categories"`
		Docs        string   `json:"docs"`
		Maintainer  string   `json:"maintainer"`
		Description string   `json:"description"`
	} `json:"templates"`
}

func (d DirCatalog) Index(_ context.Context) (Index, error) {
	file := filepath.Join(d.Root, indexFile)
	f, err := openPlain(file)
	if err != nil {
		return Index{}, fmt.Errorf("%w: %v", ErrCatalogUnavailable, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxIndexBytes+1))
	if err != nil {
		return Index{}, fmt.Errorf("%w: reading %s: %v", ErrCatalogUnavailable, file, err)
	}
	if len(data) > maxIndexBytes {
		return Index{}, fmt.Errorf("%w: %s is larger than %d bytes", ErrCatalogUnavailable, file, maxIndexBytes)
	}
	out, err := parseIndex(data)
	if err != nil {
		return Index{}, fmt.Errorf("%w: %s %v", ErrCatalogUnavailable, file, err)
	}
	for i := range out.Templates {
		out.Templates[i].Source, out.Templates[i].Kind, out.Templates[i].Signed = d.Source, d.Kind, d.Signed
	}
	return out, nil
}

// parseIndex reads an index.json: the serial, the build time and each
// template, refusing a missing serial and an invalid or repeated id.
func parseIndex(data []byte) (Index, error) {
	var doc indexDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return Index{}, fmt.Errorf("is not valid: %v", err)
	}
	if doc.Serial <= 0 {
		return Index{}, errors.New("carries no serial")
	}
	out := Index{Serial: doc.Serial, GeneratedAt: doc.GeneratedAt, Templates: make([]IndexEntry, len(doc.Templates))}
	seen := make(map[string]bool, len(doc.Templates))
	for i, t := range doc.Templates {
		if !idPattern.MatchString(t.ID) || seen[t.ID] {
			return Index{}, fmt.Errorf("lists the template id %q twice or invalidly", t.ID)
		}
		seen[t.ID] = true
		cats := t.Categories
		if cats == nil {
			cats = []string{}
		}
		if err := validateMaintainer(t.Maintainer); err != nil {
			return Index{}, fmt.Errorf("lists template %q with a maintainer that %v", t.ID, err)
		}
		if err := validateDescription(t.Description); err != nil {
			return Index{}, fmt.Errorf("lists template %q with a description that %v", t.ID, err)
		}
		out.Templates[i] = IndexEntry{ID: t.ID, Revision: t.Revision, Title: t.Title, Categories: cats, Docs: t.Docs, Maintainer: t.Maintainer, Description: t.Description}
	}
	return out, nil
}

func (d DirCatalog) Icon(ctx context.Context, id string) (Icon, error) {
	entry, err := d.Entry(ctx, id)
	if err != nil {
		return Icon{}, err
	}
	name, contentType, err := iconFile(id, entry)
	if err != nil {
		return Icon{}, err
	}
	file := filepath.Join(d.Root, id, name)
	f, err := openPlain(file)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errNotPlain) {
		return Icon{}, fmt.Errorf("%w: %q is not a plain file of template %q", ErrIconNotFound, name, id)
	}
	if err != nil {
		return Icon{}, fmt.Errorf("reading the icon of template %q: %w", id, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxIconBytes+1))
	if err != nil {
		return Icon{}, fmt.Errorf("reading the icon of template %q: %w", id, err)
	}
	if len(data) > maxIconBytes {
		return Icon{}, fmt.Errorf("%w: %q is larger than %d bytes", ErrIconNotFound, name, maxIconBytes)
	}
	return Icon{ContentType: contentType, Data: data}, nil
}

func (d DirCatalog) Screenshot(ctx context.Context, id string, index int) (Screenshot, error) {
	entry, err := d.Entry(ctx, id)
	if err != nil {
		return Screenshot{}, err
	}
	name, contentType, err := screenshotFile(id, entry, index)
	if err != nil {
		return Screenshot{}, err
	}
	dir := filepath.Join(d.Root, id)
	if _, err := plainFileUnder(dir, name); errors.Is(err, fs.ErrNotExist) || errors.Is(err, errNotPlain) {
		return Screenshot{}, fmt.Errorf("%w: %q is not a plain file of template %q", ErrScreenshotNotFound, name, id)
	} else if err != nil {
		return Screenshot{}, fmt.Errorf("reading screenshot %d of template %q: %w", index, id, err)
	}
	f, err := openPlain(filepath.Join(dir, filepath.FromSlash(name)))
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errNotPlain) {
		return Screenshot{}, fmt.Errorf("%w: %q is not a plain file of template %q", ErrScreenshotNotFound, name, id)
	}
	if err != nil {
		return Screenshot{}, fmt.Errorf("reading screenshot %d of template %q: %w", index, id, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxScreenshotBytes+1))
	if err != nil {
		return Screenshot{}, fmt.Errorf("reading screenshot %d of template %q: %w", index, id, err)
	}
	if len(data) > maxScreenshotBytes {
		return Screenshot{}, fmt.Errorf("%w: %q is larger than %d bytes", ErrScreenshotNotFound, name, maxScreenshotBytes)
	}
	return Screenshot{ContentType: contentType, Data: data}, nil
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
	return Entry{Source: d.Source, Kind: d.Kind, Signed: d.Signed, Data: data}, nil
}

// MapCatalog is a Catalog held in memory, id to compose.yaml text, for tests
// and the mock. Icons holds each template's icon file by template id, under
// the file name its compose.yaml declares.
type MapCatalog struct {
	Source      string
	Kind        store.CatalogSourceKind
	Signed      bool
	Serial      int64
	GeneratedAt time.Time
	Templates   map[string]string
	Icons       map[string][]byte
	// Screenshots holds each template's screenshot files by template id, in
	// the order its compose.yaml lists them.
	Screenshots map[string][][]byte
}

func (m MapCatalog) Name() string { return m.Source }

func (m MapCatalog) Index(_ context.Context) (Index, error) {
	ids := sortedKeys(m.Templates)
	out := Index{Serial: m.Serial, GeneratedAt: m.GeneratedAt, Templates: make([]IndexEntry, len(ids))}
	for i, id := range ids {
		e := IndexEntry{ID: id, Title: id, Categories: []string{}}
		if t, _ := Parse([]byte(m.Templates[id])); t != nil {
			e = IndexEntry{ID: id, Revision: t.Block.Revision, Title: t.Block.Title, Categories: t.Block.Categories, Docs: t.Block.Docs, Maintainer: t.Block.Maintainer, Description: t.Block.Description}
		}
		e.Source, e.Kind, e.Signed = m.Source, m.Kind, m.Signed
		out.Templates[i] = e
	}
	return out, nil
}

func (m MapCatalog) Entry(_ context.Context, id string) (Entry, error) {
	data, ok := m.Templates[id]
	if !ok {
		return Entry{}, fmt.Errorf("%w: %q", ErrTemplateNotFound, id)
	}
	return Entry{Source: m.Source, Kind: m.Kind, Signed: m.Signed, Data: []byte(data)}, nil
}

func (m MapCatalog) Icon(ctx context.Context, id string) (Icon, error) {
	entry, err := m.Entry(ctx, id)
	if err != nil {
		return Icon{}, err
	}
	_, contentType, err := iconFile(id, entry)
	if err != nil {
		return Icon{}, err
	}
	data, ok := m.Icons[id]
	if !ok || len(data) > maxIconBytes {
		return Icon{}, fmt.Errorf("%w: template %q", ErrIconNotFound, id)
	}
	return Icon{ContentType: contentType, Data: data}, nil
}

func (m MapCatalog) Screenshot(ctx context.Context, id string, index int) (Screenshot, error) {
	entry, err := m.Entry(ctx, id)
	if err != nil {
		return Screenshot{}, err
	}
	_, contentType, err := screenshotFile(id, entry, index)
	if err != nil {
		return Screenshot{}, err
	}
	files := m.Screenshots[id]
	if index >= len(files) || len(files[index]) > maxScreenshotBytes {
		return Screenshot{}, fmt.Errorf("%w: template %q", ErrScreenshotNotFound, id)
	}
	return Screenshot{ContentType: contentType, Data: files[index]}, nil
}
