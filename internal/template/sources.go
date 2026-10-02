package template

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mdg-labs/hoserva/internal/store"
)

var (
	// ErrSourceNotFound is returned for a catalog source id no source has.
	ErrSourceNotFound = errors.New("template: no catalog source with that id")
	// ErrSourceExists is returned when a source with that URL exists.
	ErrSourceExists = errors.New("template: a catalog source with that URL already exists")
	// ErrSourceCurated is returned when the curated catalog is asked to be
	// removed.
	ErrSourceCurated = errors.New("template: the curated catalog cannot be removed")
	// ErrInvalidSource is returned for a source URL or public key that is
	// refused before anything is fetched.
	ErrInvalidSource = errors.New("template: invalid catalog source")
	// ErrSourceUnreachable is returned when a source's archive could not be
	// fetched.
	ErrSourceUnreachable = errors.New("template: the catalog source could not be fetched")
	// ErrSourceRejected is returned when a source served an archive Hoserva
	// refuses to trust: a bad signature, a malformed archive or an archive
	// that is not newer.
	ErrSourceRejected = errors.New("template: the catalog source's archive was refused")
)

// SourceCheckError is the check that made adding a source fail.
type SourceCheckError struct {
	Result CheckResult
}

func (e *SourceCheckError) Error() string {
	return fmt.Sprintf("the catalog source check failed (%s): %s", e.Result.Reason, e.Result.Message)
}

// Unwrap is ErrSourceUnreachable for a fetch failure, ErrSourceRejected for
// a verification failure, and nothing for a local install failure.
func (e *SourceCheckError) Unwrap() error {
	switch {
	case e.Result.Reason == ReasonFetchFailed:
		return ErrSourceUnreachable
	case e.Result.Reason.verification():
		return ErrSourceRejected
	}
	return nil
}

// SourceStore is the catalog_sources table; *store.CatalogSourceStore is the
// production implementation.
type SourceStore interface {
	Insert(ctx context.Context, src store.CatalogSource) error
	EnsureCurated(ctx context.Context, id, url string, at time.Time) error
	Get(ctx context.Context, id string) (store.CatalogSource, error)
	List(ctx context.Context) ([]store.CatalogSource, error)
	RecordRefresh(ctx context.Context, id string, verified bool, at time.Time) error
	DeleteUserAdded(ctx context.Context, id string) error
}

// CuratedRefresher runs the curated catalog's check; *Refresher is the
// production implementation.
type CuratedRefresher interface {
	Refresh(ctx context.Context) (CheckResult, error)
}

// SourceStatus is one source as the sources list reports it. Signed is true
// only for a source whose installed archive passed a signature check. Serial
// is zero when the source's installed catalog cannot be read, and Signed is
// then false for a user-added source.
type SourceStatus struct {
	ID              string
	URL             string
	Kind            store.CatalogSourceKind
	Signed          bool
	Serial          int64
	LastRefreshedAt time.Time
}

// AddRequest is a new user-added source: its URL, and optionally the public
// key its archive is signed with, as PEM or the base64 of the raw 32-byte
// Ed25519 key. With no key the source is unsigned.
type AddRequest struct {
	URL       string
	PublicKey string
}

var sourceIDPattern = regexp.MustCompile(`^src-[0-9a-f]{10}$`)

// ParseSourceURL validates a user-added source URL and returns its canonical
// form: an https address with a host, no credentials, query or fragment, and
// no trailing slash. The archive and signature are fetched from beneath it.
func ParseSourceURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return "", fmt.Errorf("%w: %q is not an https address", ErrInvalidSource, raw)
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("%w: the address must have no credentials, query or fragment", ErrInvalidSource)
	}
	if strings.EqualFold(u.Hostname(), "api.github.com") {
		return "", fmt.Errorf("%w: a catalog is never fetched from the GitHub API", ErrInvalidSource)
	}
	u.Host = strings.ToLower(u.Host)
	p := path.Clean("/" + u.Path)
	if p == "/" {
		p = ""
	}
	u.Path, u.RawPath = p, ""
	return u.String(), nil
}

// ParseSourceKey reads a user-supplied Ed25519 public key, PEM (PKIX) or the
// base64 of the raw 32 bytes, and returns nil for an empty string.
func ParseSourceKey(raw string) (ed25519.PublicKey, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if strings.Contains(raw, "-----BEGIN") {
		block, _ := pem.Decode([]byte(raw))
		if block == nil || block.Type != "PUBLIC KEY" {
			return nil, fmt.Errorf("%w: the public key is not a PEM public key", ErrInvalidSource)
		}
		pub, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: the public key is not readable: %v", ErrInvalidSource, err)
		}
		key, ok := pub.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("%w: the public key is not an Ed25519 key", ErrInvalidSource)
		}
		return key, nil
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: the public key is neither PEM nor the base64 of a 32-byte Ed25519 key", ErrInvalidSource)
	}
	return ed25519.PublicKey(b), nil
}

// Sources is the catalog as the curated catalog and the user's own sources
// make it (doc 04 §4). It is a Catalog that lists and resolves a template
// from the curated catalog first and then from each user-added source in the
// order they were added; a template id one source already holds is never
// supplied by a later one, so a user-added entry can never shadow a curated
// entry. It also adds, refreshes, lists and removes the user-added sources.
type Sources struct {
	Store SourceStore
	// Dir holds one catalog directory per user-added source, <Dir>/<id>.
	Dir string
	// Curated is the curated catalog's own on-disk copy; its entries carry
	// the curated, signed badge.
	Curated Catalog
	// CuratedRefresh runs the curated catalog's check for refreshSource of
	// the curated id. Nil leaves that id unrefreshable.
	CuratedRefresh CuratedRefresher
	// Client makes the requests; nil means a plain http.Client. A redirect
	// must stay on the host asked.
	Client *http.Client
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu         sync.Mutex
	refreshers map[string]*Refresher
	locks      sync.Map
}

func (s *Sources) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Sources) lockSource(id string) func() {
	m, _ := s.locks.LoadOrStore(id, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (s *Sources) curatedID() string { return s.Curated.Name() }

func (s *Sources) curatedURL() string {
	u, _ := ParseSourceURL(DefaultCatalogURL)
	return u
}

// EnsureCurated records the curated catalog's row, which a fresh database
// does not have, and leaves an existing one alone.
func (s *Sources) EnsureCurated(ctx context.Context) error {
	return s.Store.EnsureCurated(ctx, s.curatedID(), s.curatedURL(), s.now())
}

// CuratedChecked records a finished curated check: a check that installed or
// confirmed the catalog is a refresh, any other outcome changes nothing. It
// is what Refresher.Finished calls, so it has no context and reports a
// failure to record in the log.
func (s *Sources) CuratedChecked(res CheckResult) {
	if res.Outcome == OutcomeFailed {
		return
	}
	if err := s.Store.RecordRefresh(context.Background(), s.curatedID(), true, res.CheckedAt); err != nil {
		log.Printf("hoservad: recording the curated catalog check: %v", err)
	}
}

func (s *Sources) Name() string { return s.Curated.Name() }

// userDirs is the user-added sources, in the order they were added, as
// directory catalogs badged with what each row says. A row with an id that
// could not have been generated is left out rather than joined into a path.
func (s *Sources) userDirs(ctx context.Context) ([]DirCatalog, error) {
	rows, err := s.Store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the catalog sources: %w", err)
	}
	var out []DirCatalog
	for _, row := range rows {
		if row.Kind == store.CatalogSourceUserAdded && sourceIDPattern.MatchString(row.ID) {
			out = append(out, s.dirFor(row))
		}
	}
	return out, nil
}

func (s *Sources) dirFor(row store.CatalogSource) DirCatalog {
	return DirCatalog{
		Root:   filepath.Join(s.Dir, row.ID),
		Source: row.ID,
		Kind:   store.CatalogSourceUserAdded,
		Signed: row.SignatureVerified && row.PublicKey != "",
	}
}

// Index lists the curated catalog's templates, then those of each user-added
// source that no earlier source lists. The serial and build time are the
// curated catalog's. A user-added source whose catalog cannot be read adds
// nothing; the curated catalog being unavailable fails the call.
func (s *Sources) Index(ctx context.Context) (Index, error) {
	out, err := s.Curated.Index(ctx)
	if err != nil {
		return Index{}, err
	}
	dirs, err := s.userDirs(ctx)
	if err != nil {
		return Index{}, err
	}
	seen := make(map[string]bool, len(out.Templates))
	for _, t := range out.Templates {
		seen[t.ID] = true
	}
	for _, d := range dirs {
		idx, err := d.Index(ctx)
		if err != nil {
			continue
		}
		for _, t := range idx.Templates {
			if !seen[t.ID] {
				seen[t.ID] = true
				out.Templates = append(out.Templates, t)
			}
		}
	}
	return out, nil
}

// Entry returns the template from the first source that has it.
func (s *Sources) Entry(ctx context.Context, id string) (Entry, error) {
	e, err := s.Curated.Entry(ctx, id)
	if !errors.Is(err, ErrTemplateNotFound) {
		return e, err
	}
	dirs, err := s.userDirs(ctx)
	if err != nil {
		return Entry{}, err
	}
	for _, d := range dirs {
		e, err := d.Entry(ctx, id)
		if !errors.Is(err, ErrTemplateNotFound) {
			return e, err
		}
	}
	return Entry{}, fmt.Errorf("%w: %q", ErrTemplateNotFound, id)
}

// Icon returns the icon of the template from the first source that has it.
func (s *Sources) Icon(ctx context.Context, id string) (Icon, error) {
	icon, err := s.Curated.Icon(ctx, id)
	if !errors.Is(err, ErrTemplateNotFound) {
		return icon, err
	}
	dirs, err := s.userDirs(ctx)
	if err != nil {
		return Icon{}, err
	}
	for _, d := range dirs {
		icon, err := d.Icon(ctx, id)
		if !errors.Is(err, ErrTemplateNotFound) {
			return icon, err
		}
	}
	return Icon{}, fmt.Errorf("%w: %q", ErrTemplateNotFound, id)
}

// From is the catalog of one source alone, by the id its entries carry as
// their Source, or ErrSourceNotFound.
func (s *Sources) From(ctx context.Context, source string) (Catalog, error) {
	if source == s.curatedID() {
		return s.Curated, nil
	}
	if !sourceIDPattern.MatchString(source) {
		return nil, fmt.Errorf("%w: %q", ErrSourceNotFound, source)
	}
	row, err := s.Store.Get(ctx, source)
	if errors.Is(err, store.ErrCatalogSourceNotFound) || (err == nil && row.Kind != store.CatalogSourceUserAdded) {
		return nil, fmt.Errorf("%w: %q", ErrSourceNotFound, source)
	}
	if err != nil {
		return nil, err
	}
	return s.dirFor(row), nil
}

// List reports every source, the curated catalog first.
func (s *Sources) List(ctx context.Context) ([]SourceStatus, error) {
	rows, err := s.Store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the catalog sources: %w", err)
	}
	out := make([]SourceStatus, 0, len(rows))
	for _, row := range rows {
		st := SourceStatus{
			ID:              row.ID,
			URL:             row.URL,
			Kind:            row.Kind,
			Signed:          row.SignatureVerified && (row.PublicKey != "" || row.Kind == store.CatalogSourceCurated),
			LastRefreshedAt: row.LastRefreshedAt,
		}
		var cat Catalog
		switch {
		case row.Kind == store.CatalogSourceCurated && row.ID == s.curatedID():
			cat = s.Curated
		case row.Kind == store.CatalogSourceUserAdded && sourceIDPattern.MatchString(row.ID):
			cat = s.dirFor(row)
		}
		if cat != nil {
			if idx, err := cat.Index(ctx); err == nil {
				st.Serial = idx.Serial
			} else if row.Kind == store.CatalogSourceUserAdded {
				// With no readable copy on disk (a restored database whose
				// source has not been refreshed yet) nothing has been verified
				// on this machine, whatever the row recorded.
				st.Signed = false
			}
		}
		out = append(out, st)
	}
	return out, nil
}

func newSourceID() (string, error) {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating a catalog source id: %w", err)
	}
	return "src-" + hex.EncodeToString(b), nil
}

// catalogStoreFor is the on-disk store of a user-added source. A source with
// a key verifies against it; a source with no key is unsigned. A key that is
// recorded but cannot be read is an error, never an unsigned source.
func (s *Sources) catalogStoreFor(id, publicKey string) (CatalogStore, error) {
	if s.Dir == "" {
		return CatalogStore{}, errors.New("the catalog sources have no directory to keep their copies in")
	}
	dir := filepath.Join(s.Dir, id)
	if publicKey == "" {
		return CatalogStore{Dir: dir, Unsigned: true}, nil
	}
	key, err := ParseSourceKey(publicKey)
	if err != nil || key == nil {
		return CatalogStore{}, fmt.Errorf("the public key recorded for catalog source %s is not readable", id)
	}
	return CatalogStore{Dir: dir, Key: key}, nil
}

func (s *Sources) refresherFor(row store.CatalogSource) (*Refresher, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.refreshers[row.ID]; r != nil {
		return r, nil
	}
	cs, err := s.catalogStoreFor(row.ID, row.PublicKey)
	if err != nil {
		return nil, err
	}
	r := &Refresher{Store: cs, URL: row.URL, Client: s.Client, Now: s.Now}
	if s.refreshers == nil {
		s.refreshers = map[string]*Refresher{}
	}
	s.refreshers[row.ID] = r
	return r, nil
}

func (s *Sources) removeDir(id string) error {
	dir := filepath.Join(s.Dir, id)
	for _, p := range []string{dir, dir + stagingSuffix, dir + backupSuffix} {
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("removing %s: %w", p, err)
		}
	}
	return nil
}

// Add fetches a new source's archive and, only once it is installed on disk,
// records the source. A source with a key is verified against it and refused
// when the signature does not verify; a source with no key is accepted
// unsigned and is badged so. A source that cannot be fetched or verified
// leaves no row and no files.
func (s *Sources) Add(ctx context.Context, req AddRequest) (SourceStatus, error) {
	canonical, err := ParseSourceURL(req.URL)
	if err != nil {
		return SourceStatus{}, err
	}
	key, err := ParseSourceKey(req.PublicKey)
	if err != nil {
		return SourceStatus{}, err
	}
	rows, err := s.Store.List(ctx)
	if err != nil {
		return SourceStatus{}, fmt.Errorf("listing the catalog sources: %w", err)
	}
	for _, row := range rows {
		if u, err := ParseSourceURL(row.URL); err == nil && u == canonical {
			return SourceStatus{}, fmt.Errorf("%w: %s", ErrSourceExists, canonical)
		}
	}
	id, err := newSourceID()
	if err != nil {
		return SourceStatus{}, err
	}
	var encoded string
	if key != nil {
		encoded = base64.StdEncoding.EncodeToString(key)
	}
	cs, err := s.catalogStoreFor(id, encoded)
	if err != nil {
		return SourceStatus{}, err
	}
	// The directory of a source that was never recorded is removed on every
	// failure below, whether or not the caller's context is still alive.
	recorded := false
	defer func() {
		if !recorded {
			if err := s.removeDir(id); err != nil {
				log.Printf("hoservad: cleaning up the catalog source that was not added: %v", err)
			}
		}
	}()

	r := &Refresher{Store: cs, URL: canonical, Client: s.Client, Now: s.Now}
	res, err := r.Refresh(ctx)
	if err != nil {
		return SourceStatus{}, fmt.Errorf("waiting for the catalog source check: %w", err)
	}
	if res.Outcome != OutcomeUpdated {
		return SourceStatus{}, &SourceCheckError{Result: res}
	}
	row := store.CatalogSource{
		ID:                id,
		URL:               canonical,
		Kind:              store.CatalogSourceUserAdded,
		PublicKey:         encoded,
		SignatureVerified: key != nil,
		LastRefreshedAt:   res.CheckedAt,
		AddedAt:           s.now().UTC(),
	}
	if err := s.Store.Insert(context.WithoutCancel(ctx), row); err != nil {
		if errors.Is(err, store.ErrCatalogSourceExists) {
			return SourceStatus{}, fmt.Errorf("%w: %s", ErrSourceExists, canonical)
		}
		return SourceStatus{}, err
	}
	recorded = true
	s.mu.Lock()
	if s.refreshers == nil {
		s.refreshers = map[string]*Refresher{}
	}
	s.refreshers[id] = r
	s.mu.Unlock()
	st := SourceStatus{ID: id, URL: canonical, Kind: row.Kind, Signed: row.SignatureVerified, LastRefreshedAt: res.CheckedAt}
	if serial, ok, err := cs.Serial(); err == nil && ok {
		st.Serial = serial
	}
	return st, nil
}

// Refresh runs one check of a source now and reports how it ended; a failed
// check keeps the installed copy and changes nothing recorded. The curated
// id runs the curated catalog's own check.
func (s *Sources) Refresh(ctx context.Context, id string) (CheckResult, error) {
	if id == s.curatedID() {
		if s.CuratedRefresh == nil {
			return CheckResult{}, fmt.Errorf("%w: the curated catalog has no refresher", ErrSourceNotFound)
		}
		return s.CuratedRefresh.Refresh(ctx)
	}
	if !sourceIDPattern.MatchString(id) {
		return CheckResult{}, fmt.Errorf("%w: %q", ErrSourceNotFound, id)
	}
	defer s.lockSource(id)()
	row, err := s.Store.Get(ctx, id)
	if errors.Is(err, store.ErrCatalogSourceNotFound) || (err == nil && row.Kind != store.CatalogSourceUserAdded) {
		return CheckResult{}, fmt.Errorf("%w: %q", ErrSourceNotFound, id)
	}
	if err != nil {
		return CheckResult{}, err
	}
	r, err := s.refresherFor(row)
	if err != nil {
		return CheckResult{}, err
	}
	res, err := r.Refresh(ctx)
	if err != nil {
		return CheckResult{}, fmt.Errorf("waiting for the catalog source check: %w", err)
	}
	if res.Outcome != OutcomeFailed {
		if err := s.Store.RecordRefresh(context.WithoutCancel(ctx), id, row.PublicKey != "", res.CheckedAt); err != nil {
			log.Printf("hoservad: recording the refresh of catalog source %s: %v", id, err)
		}
	}
	return res, nil
}

// Remove deletes one user-added source: its files, then its row, so a
// failure part way leaves a source the next Remove finishes removing. It
// touches nothing of any other source, and nothing of a stack that was
// installed from it. The curated catalog is refused.
func (s *Sources) Remove(ctx context.Context, id string) error {
	if id == s.curatedID() {
		return ErrSourceCurated
	}
	if !sourceIDPattern.MatchString(id) {
		return fmt.Errorf("%w: %q", ErrSourceNotFound, id)
	}
	defer s.lockSource(id)()
	row, err := s.Store.Get(ctx, id)
	if errors.Is(err, store.ErrCatalogSourceNotFound) {
		return fmt.Errorf("%w: %q", ErrSourceNotFound, id)
	}
	if err != nil {
		return err
	}
	if row.Kind != store.CatalogSourceUserAdded {
		return ErrSourceCurated
	}
	if err := s.removeDir(id); err != nil {
		return err
	}
	if err := s.Store.DeleteUserAdded(ctx, id); err != nil {
		if errors.Is(err, store.ErrCatalogSourceNotFound) {
			return fmt.Errorf("%w: %q", ErrSourceNotFound, id)
		}
		return err
	}
	s.mu.Lock()
	delete(s.refreshers, id)
	s.mu.Unlock()
	return nil
}
