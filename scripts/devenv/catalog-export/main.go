// Command catalog-export turns the signed catalog archive into the plain
// directory the documentation site builds /apps from: index.json plus each
// template's icon and screenshots under static/apps/<id>/. The archive is
// checked with exactly the code hoservad checks it with
// (internal/template.VerifyArchive and CatalogStore.Install) and the compiled-in
// catalog key, so the site never renders an archive that does not verify.
//
// The pinned snapshot (scripts/devenv/catalog.pin, fetched by
// scripts/devenv/catalog-snapshot.sh) is the source unless -live names the
// catalog's host. A live archive is used only if it verifies and its serial
// is not lower than the pin's; otherwise the export warns and falls back to
// the pinned snapshot, which must itself verify or the export fails. The
// output directory is replaced only by a complete export.
//
// Usage: catalog-export -pin <file> -snapshot <dir> -out <dir> [-live <url>].
// -live defaults to $CATALOG_LIVE_URL.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mdg-labs/hoserva/internal/template"
)

const (
	archiveName = "catalog.tar.zst"
	sigName     = "catalog.tar.zst.sig"

	maxLiveArchiveBytes = 64 << 20
	maxLiveSigBytes     = 4 << 10
	liveTimeout         = 2 * time.Minute
)

func main() {
	if err := run(context.Background(), os.Args[1:], template.CatalogPublicKey, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "catalog-export:", err)
		os.Exit(1)
	}
}

type options struct {
	pin      string
	snapshot string
	out      string
	live     string
	key      ed25519.PublicKey
	client   *http.Client
	log      io.Writer
}

func run(ctx context.Context, args []string, key ed25519.PublicKey, log io.Writer) error {
	fs := flag.NewFlagSet("catalog-export", flag.ContinueOnError)
	fs.SetOutput(log)
	o := options{key: key, log: log}
	fs.StringVar(&o.pin, "pin", "", "the catalog pin file; its serial is the lowest serial accepted")
	fs.StringVar(&o.snapshot, "snapshot", "", "the directory holding the pinned catalog.tar.zst and its .sig")
	fs.StringVar(&o.out, "out", "", "the directory to write")
	fs.StringVar(&o.live, "live", os.Getenv("CATALOG_LIVE_URL"), "the catalog's host to try before the pinned snapshot")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || o.pin == "" || o.snapshot == "" || o.out == "" {
		return errors.New("usage: catalog-export -pin <file> -snapshot <dir> -out <dir> [-live <url>]")
	}
	return export(ctx, o)
}

func export(ctx context.Context, o options) error {
	minSerial, err := pinSerial(o.pin)
	if err != nil {
		return err
	}
	if o.live != "" {
		stage, err := o.fromLive(ctx, minSerial)
		if err == nil {
			return publish(stage, o.out)
		}
		o.warn("the live catalog at %s was not used (%v); using the pinned snapshot", o.live, err)
	}
	archive, err := os.ReadFile(filepath.Join(o.snapshot, archiveName))
	if err != nil {
		return fmt.Errorf("reading the pinned snapshot: %w", err)
	}
	sig, err := os.ReadFile(filepath.Join(o.snapshot, sigName))
	if err != nil {
		return fmt.Errorf("reading the pinned snapshot's signature: %w", err)
	}
	stage, err := o.build(ctx, archive, sig, minSerial)
	if err != nil {
		return fmt.Errorf("the pinned snapshot: %w", err)
	}
	return publish(stage, o.out)
}

func (o options) warn(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(o.log, "catalog-export: warning: %s\n", msg)
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		_, _ = fmt.Fprintf(o.log, "::warning title=Live catalog not used::%s\n", strings.ReplaceAll(msg, "\n", " "))
	}
}

func (o options) fromLive(ctx context.Context, minSerial int64) (stage string, err error) {
	ctx, cancel := context.WithTimeout(ctx, liveTimeout)
	defer cancel()
	archive, err := o.fetch(ctx, o.live, archiveName, maxLiveArchiveBytes)
	if err != nil {
		return "", err
	}
	sig, err := o.fetch(ctx, o.live, sigName, maxLiveSigBytes)
	if err != nil {
		return "", err
	}
	return o.build(ctx, archive, sig, minSerial)
}

func (o options) fetch(ctx context.Context, base, name string, limit int64) ([]byte, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return nil, fmt.Errorf("%q is not an http or https address", base)
	}
	u.Path = path.Join("/", u.Path, name)
	u.RawQuery, u.Fragment = "", ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	// The signature covers the archive's own bytes, so no content coding may
	// transform them in transit.
	req.Header.Set("Accept-Encoding", "identity")
	client := o.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching %s: the server answered %s", name, resp.Status)
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		return nil, fmt.Errorf("fetching %s: the server applied a %q content coding", name, enc)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("fetching %s: the response is larger than %d bytes", name, limit)
	}
	return body, nil
}

// pinSerial is the serial a pin file names, which the pinned snapshot and any
// live archive must reach.
func pinSerial(file string) (int64, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return 0, fmt.Errorf("reading the pin: %w", err)
	}
	var values []string
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "serial="); ok {
			values = append(values, v)
		}
	}
	if len(values) != 1 {
		return 0, fmt.Errorf("%s must name exactly one serial=", file)
	}
	serial, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || serial <= 0 {
		return 0, fmt.Errorf("%s: the pinned serial %q is not a positive number", file, values[0])
	}
	return serial, nil
}

type siteIndex struct {
	Serial      int64          `json:"serial"`
	GeneratedAt string         `json:"generatedAt,omitempty"`
	Templates   []siteTemplate `json:"templates"`
}

// siteTemplate is a template's public fields. Icon and Screenshots are file
// names under static/apps/<id>/.
type siteTemplate struct {
	ID          string   `json:"id"`
	Revision    int      `json:"revision"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Categories  []string `json:"categories"`
	Docs        string   `json:"docs"`
	Maintainer  string   `json:"maintainer,omitempty"`
	Icon        string   `json:"icon,omitempty"`
	Screenshots []string `json:"screenshots"`
}

var imageExt = map[string]string{
	"image/svg+xml": ".svg",
	"image/png":     ".png",
	"image/webp":    ".webp",
	"image/jpeg":    ".jpg",
}

// build verifies the archive and writes the export into a new directory next
// to the output, which it returns. A failure leaves nothing behind.
func (o options) build(ctx context.Context, archive, sig []byte, minSerial int64) (stage string, err error) {
	serial, err := template.VerifyArchive(o.key, archive, sig)
	if err != nil {
		return "", err
	}
	if serial < minSerial {
		return "", fmt.Errorf("the archive's serial %d is lower than the pinned serial %d", serial, minSerial)
	}
	parent := filepath.Dir(filepath.Clean(o.out))
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(parent, ".catalog-export-")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(tmp)
		}
	}()

	root := filepath.Join(tmp, "catalog")
	if err := (template.CatalogStore{Dir: root, Key: o.key}).Install(archive, sig); err != nil {
		return "", err
	}
	cat := template.DirCatalog{Root: root, Source: template.SourceCurated, Signed: true}
	index, err := cat.Index(ctx)
	if err != nil {
		return "", err
	}

	stage = filepath.Join(tmp, "site")
	if err := os.Mkdir(stage, 0o755); err != nil {
		return "", err
	}
	doc := siteIndex{Serial: index.Serial, Templates: make([]siteTemplate, 0, len(index.Templates))}
	if !index.GeneratedAt.IsZero() {
		doc.GeneratedAt = index.GeneratedAt.UTC().Format(time.RFC3339)
	}
	for _, e := range index.Templates {
		t, err := exportTemplate(ctx, cat, e, filepath.Join(stage, "static", "apps", e.ID))
		if err != nil {
			return "", err
		}
		doc.Templates = append(doc.Templates, t)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(stage, "index.json"), append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	return stage, nil
}

func exportTemplate(ctx context.Context, cat template.DirCatalog, e template.IndexEntry, dir string) (siteTemplate, error) {
	detail, err := template.Show(ctx, cat, e.ID)
	if err != nil {
		return siteTemplate{}, err
	}
	t := siteTemplate{
		ID:          e.ID,
		Revision:    e.Revision,
		Title:       e.Title,
		Description: e.Description,
		Categories:  e.Categories,
		Docs:        e.Docs,
		Maintainer:  e.Maintainer,
		Screenshots: []string{},
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return siteTemplate{}, err
	}
	icon, err := cat.Icon(ctx, e.ID)
	switch {
	case err == nil:
		t.Icon = "icon" + imageExt[icon.ContentType]
		if err := os.WriteFile(filepath.Join(dir, t.Icon), icon.Data, 0o644); err != nil {
			return siteTemplate{}, err
		}
	case errors.Is(err, template.ErrIconNotFound):
	default:
		return siteTemplate{}, err
	}
	for i := 0; i < detail.Screenshots; i++ {
		shot, err := cat.Screenshot(ctx, e.ID, i)
		if errors.Is(err, template.ErrScreenshotNotFound) {
			continue
		}
		if err != nil {
			return siteTemplate{}, err
		}
		name := "screenshot-" + strconv.Itoa(i+1) + imageExt[shot.ContentType]
		if err := os.WriteFile(filepath.Join(dir, name), shot.Data, 0o644); err != nil {
			return siteTemplate{}, err
		}
		t.Screenshots = append(t.Screenshots, name)
	}
	return t, nil
}

// publish moves a finished export into place and removes what its staging
// directory held. out is removed first, so a stale export is never left
// beside a new one.
func publish(stage, out string) (err error) {
	tmp := filepath.Dir(stage)
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := os.RemoveAll(out); err != nil {
		return fmt.Errorf("removing the previous export: %w", err)
	}
	if err := os.Rename(stage, out); err != nil {
		return fmt.Errorf("moving the export into place: %w", err)
	}
	return nil
}
