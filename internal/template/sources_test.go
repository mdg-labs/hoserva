package template

import (
	"archive/tar"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

var revisionLine = regexp.MustCompile(`revision: \d+`)

// sourceCompose is a fixture template's compose.yaml at the given revision.
func sourceCompose(t *testing.T, id string, revision int) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir, id, ComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	return revisionLine.ReplaceAllString(string(data), fmt.Sprintf("revision: %d", revision))
}

// sourceEntries is a catalog archive of fixture templates, id to revision.
func sourceEntries(t *testing.T, serial int64, templates map[string]int) []tarEntry {
	t.Helper()
	ids := make([]string, 0, len(templates))
	for id := range templates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var listed []string
	entries := []tarEntry{{Name: "index.json"}}
	for _, id := range ids {
		listed = append(listed, fmt.Sprintf(`{"id":%q,"revision":%d,"title":%q,"categories":[],"docs":"https://example.com"}`, id, templates[id], id))
		entries = append(entries,
			tarEntry{Name: id + "/", Type: tar.TypeDir},
			reg(id+"/compose.yaml", sourceCompose(t, id, templates[id])))
	}
	entries[0] = reg("index.json", fmt.Sprintf(`{"schema":1,"serial":%d,"templates":[%s]}`, serial, strings.Join(listed, ",")))
	return entries
}

// curatedJellyfin is a curated catalog holding only jellyfin at revision 1,
// badged as the curated source is in production.
func curatedJellyfin(t *testing.T) DirCatalog {
	t.Helper()
	root := t.TempDir()
	copyFile(t, filepath.Join(fixtureDir, "jellyfin", ComposeFile), filepath.Join(root, "jellyfin", ComposeFile))
	index := `{"schema":1,"serial":7,"templates":[{"id":"jellyfin","revision":1,"title":"Jellyfin","categories":["media"],"docs":"https://example.com"}]}`
	if err := os.WriteFile(filepath.Join(root, indexFile), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	return DirCatalog{Root: root, Source: SourceCurated, Kind: store.CatalogSourceCurated, Signed: true}
}

func newSourceStore(t *testing.T) *store.CatalogSourceStore {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "sources.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store.NewCatalogSourceStore(db)
}

type sourceRig struct {
	sources *Sources
	store   *store.CatalogSourceStore
}

func newSourceRig(t *testing.T) *sourceRig {
	t.Helper()
	st := newSourceStore(t)
	rig := &sourceRig{store: st}
	rig.sources = &Sources{Store: st, Dir: filepath.Join(t.TempDir(), "catalog-sources"), Curated: curatedJellyfin(t)}
	if err := rig.sources.EnsureCurated(context.Background()); err != nil {
		t.Fatal(err)
	}
	return rig
}

// newHost starts an https catalog host and makes the rig's client trust it.
func (g *sourceRig) newHost(t *testing.T) (*fakeCatalogHost, string) {
	t.Helper()
	host := &fakeCatalogHost{}
	srv := httptest.NewTLSServer(host)
	t.Cleanup(srv.Close)
	g.sources.Client = srv.Client()
	return host, srv.URL
}

func (g *sourceRig) dirs(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(g.sources.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func (g *sourceRig) rows(t *testing.T) []store.CatalogSource {
	t.Helper()
	rows, err := g.store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestParseSourceURL(t *testing.T) {
	good := map[string]string{
		"https://example.com":                  "https://example.com",
		"https://example.com/":                 "https://example.com",
		"  https://Example.COM/cat/  ":         "https://example.com/cat",
		"https://example.com:8443/a/./b/":      "https://example.com:8443/a/b",
		"https://example.com/a/../cat":         "https://example.com/cat",
		"https://api.github.com.example.org/c": "https://api.github.com.example.org/c",
	}
	for in, want := range good {
		got, err := ParseSourceURL(in)
		if err != nil || got != want {
			t.Errorf("ParseSourceURL(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "example.com", "http://example.com", "ftp://example.com", "file:///etc/passwd", "https://",
		"https://user:pass@example.com", "https://user@example.com", "https://example.com/?token=x",
		"https://example.com/#frag", "https://example.com?", "https://api.github.com/repos/x", "https://API.GITHUB.COM/",
	} {
		if got, err := ParseSourceURL(in); !errors.Is(err, ErrInvalidSource) {
			t.Errorf("ParseSourceURL(%q) = %q, %v, want ErrInvalidSource", in, got, err)
		}
	}
}

func TestParseSourceKey(t *testing.T) {
	pub, _ := newKey(t)
	if k, err := ParseSourceKey(" "); k != nil || err != nil {
		t.Fatalf("empty key = %v, %v, want nil, nil", k, err)
	}
	if k, err := ParseSourceKey(base64.StdEncoding.EncodeToString(pub)); err != nil || !k.Equal(pub) {
		t.Fatalf("base64 key = %v, %v", k, err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	if k, err := ParseSourceKey(pemKey); err != nil || !k.Equal(pub) {
		t.Fatalf("PEM key = %v, %v", k, err)
	}
	for _, in := range []string{"not base64!", base64.StdEncoding.EncodeToString(pub[:16]), "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----"} {
		if _, err := ParseSourceKey(in); !errors.Is(err, ErrInvalidSource) {
			t.Errorf("ParseSourceKey(%q) = %v, want ErrInvalidSource", in, err)
		}
	}
}

// The security-review point of #283: an unsigned source's archive is
// accepted, no signature is even requested, and everything it supplies is
// badged user-added and unsigned. An unsigned template can ask for any
// privilege, and the privilege summary is computed from its Compose content
// exactly as for a curated one.
func TestSources_AnUnsignedSourceIsAcceptedAndEveryEntryIsBadgedUnsigned(t *testing.T) {
	ctx := context.Background()
	g := newSourceRig(t)
	host, url := g.newHost(t)
	archive := buildArchive(t, sourceEntries(t, 3, map[string]int{"risky-agent": 1}))
	host.serve(archive, []byte("not a signature"), "")

	st, err := g.sources.Add(ctx, AddRequest{URL: url})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if st.Kind != store.CatalogSourceUserAdded || st.Signed || st.Serial != 3 || st.LastRefreshedAt.IsZero() {
		t.Fatalf("status = %+v, want user-added, unsigned, serial 3, refreshed", st)
	}
	if reqs, _ := host.log(); len(reqs) != 1 || !strings.HasPrefix(reqs[0], "/catalog.tar.zst ") {
		t.Fatalf("requests = %v, want the archive only: an unsigned source's signature is never fetched", reqs)
	}

	idx, err := g.sources.Index(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var risky *IndexEntry
	for i := range idx.Templates {
		if idx.Templates[i].ID == "risky-agent" {
			risky = &idx.Templates[i]
		}
	}
	if risky == nil || risky.Source != st.ID || risky.Kind != store.CatalogSourceUserAdded || risky.Signed {
		t.Fatalf("index entry = %+v, want source %s, user-added, unsigned", risky, st.ID)
	}
	entry, err := g.sources.Entry(ctx, "risky-agent")
	if err != nil || entry.Source != st.ID || entry.Kind != store.CatalogSourceUserAdded || entry.Signed {
		t.Fatalf("entry = %+v, %v", entry, err)
	}
	detail, err := Show(ctx, g.sources, "risky-agent")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Signed || detail.Kind != store.CatalogSourceUserAdded || len(detail.Privileges) == 0 {
		t.Fatalf("detail = kind %q signed %v with %d privileges, want user-added, unsigned and the full privilege summary", detail.Kind, detail.Signed, len(detail.Privileges))
	}
	rows := g.rows(t)
	if len(rows) != 2 || rows[1].SignatureVerified || rows[1].PublicKey != "" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestSources_ASignedSourceVerifiesAgainstItsOwnKey(t *testing.T) {
	ctx := context.Background()
	g := newSourceRig(t)
	host, url := g.newHost(t)
	pub, priv := newKey(t)
	archive, sig := signed(t, priv, sourceEntries(t, 4, map[string]int{"risky-agent": 1}))
	host.serve(archive, sig, "")

	st, err := g.sources.Add(ctx, AddRequest{URL: url, PublicKey: base64.StdEncoding.EncodeToString(pub)})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !st.Signed || st.Kind != store.CatalogSourceUserAdded {
		t.Fatalf("status = %+v, want a signed user-added source", st)
	}
	entry, err := g.sources.Entry(ctx, "risky-agent")
	if err != nil || !entry.Signed || entry.Kind != store.CatalogSourceUserAdded || entry.Source != st.ID {
		t.Fatalf("entry = %+v, %v, want signed and user-added (never curated)", entry, err)
	}
	if rows := g.rows(t); !rows[1].SignatureVerified || rows[1].PublicKey == "" {
		t.Fatalf("row = %+v", rows[1])
	}
}

func TestSources_AKeyedSourceWhoseSignatureFailsIsRefusedAndLeavesNothing(t *testing.T) {
	ctx := context.Background()
	for name, mismatch := range map[string]func(t *testing.T, pub ed25519.PublicKey, priv ed25519.PrivateKey) ([]byte, []byte){
		"signed by another key": func(t *testing.T, _ ed25519.PublicKey, _ ed25519.PrivateKey) ([]byte, []byte) {
			_, other := newKey(t)
			return signed(t, other, sourceEntries(t, 1, map[string]int{"jellyfin": 1}))
		},
		"the signature is missing": func(t *testing.T, _ ed25519.PublicKey, _ ed25519.PrivateKey) ([]byte, []byte) {
			return buildArchive(t, sourceEntries(t, 1, map[string]int{"jellyfin": 1})), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newSourceRig(t)
			host, url := g.newHost(t)
			pub, priv := newKey(t)
			archive, sig := mismatch(t, pub, priv)
			host.serve(archive, sig, "")

			_, err := g.sources.Add(ctx, AddRequest{URL: url, PublicKey: base64.StdEncoding.EncodeToString(pub)})
			if !errors.Is(err, ErrSourceRejected) {
				t.Fatalf("Add = %v, want ErrSourceRejected", err)
			}
			if rows := g.rows(t); len(rows) != 1 {
				t.Fatalf("a refused source left rows: %+v", rows)
			}
			if d := g.dirs(t); len(d) != 0 {
				t.Fatalf("a refused source left files: %v", d)
			}
		})
	}
}

func TestSources_AddFailuresLeaveNoRowAndNoFiles(t *testing.T) {
	ctx := context.Background()
	g := newSourceRig(t)
	host, url := g.newHost(t)

	host.mu.Lock()
	host.status = http.StatusInternalServerError
	host.mu.Unlock()
	if _, err := g.sources.Add(ctx, AddRequest{URL: url}); !errors.Is(err, ErrSourceUnreachable) {
		t.Fatalf("Add against a failing host = %v, want ErrSourceUnreachable", err)
	}

	host.serve(buildArchive(t, []tarEntry{reg("index.json", `{"serial":1,"templates":[]}`), reg("evil.txt", "x")}), nil, "")
	if _, err := g.sources.Add(ctx, AddRequest{URL: url}); !errors.Is(err, ErrSourceRejected) {
		t.Fatalf("Add of a malformed archive = %v, want ErrSourceRejected", err)
	}

	if rows := g.rows(t); len(rows) != 1 {
		t.Fatalf("failed adds left rows: %+v", rows)
	}
	if d := g.dirs(t); len(d) != 0 {
		t.Fatalf("failed adds left files: %v", d)
	}
	for _, bad := range []AddRequest{{URL: "http://example.com"}, {URL: url, PublicKey: "junk"}, {URL: ""}} {
		if _, err := g.sources.Add(ctx, bad); !errors.Is(err, ErrInvalidSource) {
			t.Errorf("Add(%+v) = %v, want ErrInvalidSource", bad, err)
		}
	}
	if reqs, _ := host.log(); len(reqs) != 2 {
		t.Fatalf("requests = %v: an invalid URL or key must be refused before anything is fetched", reqs)
	}
}

func TestSources_ARedirectToAnotherHostIsNotFollowed(t *testing.T) {
	ctx := context.Background()
	g := newSourceRig(t)
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the redirect target was contacted: %s", r.URL)
	}))
	t.Cleanup(other.Close)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(other.URL, "127.0.0.1", "localhost", 1)+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	g.sources.Client = srv.Client()

	if _, err := g.sources.Add(ctx, AddRequest{URL: srv.URL}); !errors.Is(err, ErrSourceUnreachable) {
		t.Fatalf("Add = %v, want ErrSourceUnreachable", err)
	}
	if len(g.rows(t)) != 1 || len(g.dirs(t)) != 0 {
		t.Fatal("a refused redirect left a source behind")
	}
}

func TestSources_ADuplicateURLIsRefusedWhateverItsSpelling(t *testing.T) {
	ctx := context.Background()
	g := newSourceRig(t)
	host, url := g.newHost(t)
	host.serve(buildArchive(t, sourceEntries(t, 1, map[string]int{"risky-agent": 1})), nil, "")
	if _, err := g.sources.Add(ctx, AddRequest{URL: url}); err != nil {
		t.Fatal(err)
	}
	for _, again := range []string{url, url + "/", strings.ToUpper(url[:8]) + url[8:] + "/"} {
		if _, err := g.sources.Add(ctx, AddRequest{URL: again}); !errors.Is(err, ErrSourceExists) {
			t.Errorf("Add(%q) = %v, want ErrSourceExists", again, err)
		}
	}
	if _, err := g.sources.Add(ctx, AddRequest{URL: DefaultCatalogURL}); !errors.Is(err, ErrSourceExists) {
		t.Errorf("Add of the curated catalog's own URL = %v, want ErrSourceExists", err)
	}
	if len(g.rows(t)) != 2 || len(g.dirs(t)) != 1 {
		t.Fatalf("rows %d dirs %v", len(g.rows(t)), g.dirs(t))
	}
}

// A curated template id is never supplied by a user-added source, and of two
// user-added sources the one added first keeps an id both list.
func TestSources_NoUserAddedEntryShadowsAnEarlierSourcesEntry(t *testing.T) {
	ctx := context.Background()
	g := newSourceRig(t)
	hostA, urlA := g.newHost(t)
	hostA.serve(buildArchive(t, sourceEntries(t, 1, map[string]int{"jellyfin": 9, "risky-agent": 5})), nil, "")
	a, err := g.sources.Add(ctx, AddRequest{URL: urlA})
	if err != nil {
		t.Fatal(err)
	}
	hostB, urlB := g.newHost(t)
	hostB.serve(buildArchive(t, sourceEntries(t, 1, map[string]int{"risky-agent": 6, "aio-notes": 1})), nil, "")
	if _, err := g.sources.Add(ctx, AddRequest{URL: urlB}); err != nil {
		t.Fatal(err)
	}

	idx, err := g.sources.Index(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]IndexEntry{}
	for _, e := range idx.Templates {
		if _, dup := got[e.ID]; dup {
			t.Fatalf("template %q is listed twice", e.ID)
		}
		got[e.ID] = e
	}
	if j := got["jellyfin"]; j.Source != SourceCurated || j.Revision != 1 || j.Kind != store.CatalogSourceCurated || !j.Signed {
		t.Fatalf("jellyfin = %+v, want the curated entry at revision 1", j)
	}
	if r := got["risky-agent"]; r.Source != a.ID || r.Revision != 5 {
		t.Fatalf("risky-agent = %+v, want the first-added source's revision 5", r)
	}
	if _, ok := got["aio-notes"]; !ok || len(got) != 3 {
		t.Fatalf("index = %+v", got)
	}
	if e, err := g.sources.Entry(ctx, "jellyfin"); err != nil || e.Source != SourceCurated || e.Kind != store.CatalogSourceCurated || !strings.Contains(string(e.Data), "revision: 1") {
		t.Fatalf("Entry(jellyfin) = %+v, %v, want the curated one", e, err)
	}
	if e, err := g.sources.Entry(ctx, "risky-agent"); err != nil || e.Source != a.ID {
		t.Fatalf("Entry(risky-agent) = %+v, %v", e, err)
	}
	if _, err := g.sources.Entry(ctx, "nope"); !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("Entry(nope) = %v", err)
	}
	// Resolving through the one source an entry names reaches the shadowed
	// entry too, which is how a stack installed from it is still compared.
	cat, err := g.sources.From(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if e, err := cat.Entry(ctx, "jellyfin"); err != nil || e.Source != a.ID {
		t.Fatalf("From(a).Entry(jellyfin) = %+v, %v", e, err)
	}
}

func TestSources_RefreshInstallsANewerCatalogAndRecordsIt(t *testing.T) {
	ctx := context.Background()
	g := newSourceRig(t)
	host, url := g.newHost(t)
	clock := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	g.sources.Now = func() time.Time { clock = clock.Add(time.Hour); return clock }
	host.serve(buildArchive(t, sourceEntries(t, 1, map[string]int{"risky-agent": 1})), nil, `"v1"`)
	added, err := g.sources.Add(ctx, AddRequest{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	firstAt := added.LastRefreshedAt

	res, err := g.sources.Refresh(ctx, added.ID)
	if err != nil || res.Outcome != OutcomeUnchanged {
		t.Fatalf("Refresh of an unchanged source = %+v, %v", res, err)
	}

	host.serve(buildArchive(t, sourceEntries(t, 2, map[string]int{"risky-agent": 2, "aio-notes": 1})), nil, `"v2"`)
	res, err = g.sources.Refresh(ctx, added.ID)
	if err != nil || res.Outcome != OutcomeUpdated || res.New != 1 || res.Updated != 1 {
		t.Fatalf("Refresh = %+v, %v, want updated with 1 new and 1 updated", res, err)
	}
	entry, err := g.sources.Entry(ctx, "risky-agent")
	if err != nil || !strings.Contains(string(entry.Data), "revision: 2") {
		t.Fatalf("the refreshed copy is not in use: %v", err)
	}
	row, err := g.store.Get(ctx, added.ID)
	if err != nil || !row.LastRefreshedAt.After(firstAt) {
		t.Fatalf("last refreshed = %v after %v, %v", row.LastRefreshedAt, firstAt, err)
	}
	if row.SignatureVerified {
		t.Fatalf("a refresh marked an unsigned source verified: %+v", row)
	}
}

func TestSources_AFailedRefreshKeepsTheInstalledCopyAndTheRecordedState(t *testing.T) {
	ctx := context.Background()
	g := newSourceRig(t)
	host, url := g.newHost(t)
	pub, priv := newKey(t)
	archive, sig := signed(t, priv, sourceEntries(t, 5, map[string]int{"risky-agent": 1}))
	host.serve(archive, sig, `"v5"`)
	added, err := g.sources.Add(ctx, AddRequest{URL: url, PublicKey: base64.StdEncoding.EncodeToString(pub)})
	if err != nil {
		t.Fatal(err)
	}
	before, err := g.store.Get(ctx, added.ID)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(g.sources.Dir, added.ID)
	treeBefore := tree(t, dir)

	_, other := newKey(t)
	bad, badSig := signed(t, other, sourceEntries(t, 6, map[string]int{"risky-agent": 2}))
	host.serve(bad, badSig, `"v6"`)
	res, err := g.sources.Refresh(ctx, added.ID)
	if err != nil || res.Outcome != OutcomeFailed || res.Reason != ReasonBadSignature {
		t.Fatalf("Refresh = %+v, %v, want failed bad_signature", res, err)
	}
	equalTrees(t, tree(t, dir), treeBefore)
	after, err := g.store.Get(ctx, added.ID)
	if err != nil || !after.LastRefreshedAt.Equal(before.LastRefreshedAt) || !after.SignatureVerified {
		t.Fatalf("a failed refresh changed the row: %+v -> %+v, %v", before, after, err)
	}
}

// A key that is recorded but unreadable must never be treated as "no key":
// the source would then be installed unsigned and could be badged as if a
// check had passed.
func TestSources_AnUnreadableRecordedKeyFailsClosedAndFetchesNothing(t *testing.T) {
	ctx := context.Background()
	g := newSourceRig(t)
	host, url := g.newHost(t)
	host.serve(buildArchive(t, sourceEntries(t, 1, map[string]int{"risky-agent": 1})), nil, "")
	err := g.store.Insert(ctx, store.CatalogSource{ID: "src-0123456789", URL: url, Kind: store.CatalogSourceUserAdded, PublicKey: "garbage", SignatureVerified: true, AddedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.sources.Refresh(ctx, "src-0123456789"); err == nil {
		t.Fatal("Refresh with an unreadable recorded key succeeded")
	}
	if reqs, _ := host.log(); len(reqs) != 0 {
		t.Fatalf("requests = %v: nothing may be fetched without a usable key", reqs)
	}
	if _, err := os.Stat(filepath.Join(g.sources.Dir, "src-0123456789")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an archive was installed for a source whose key cannot be read: %v", err)
	}
}

func TestSources_RefreshOfTheCuratedIDRunsTheCuratedCheck(t *testing.T) {
	ctx := context.Background()
	g := newSourceRig(t)
	want := CheckResult{Outcome: OutcomeUnchanged, CheckedAt: time.Now().UTC()}
	g.sources.CuratedRefresh = fakeCuratedRefresher{res: want}
	got, err := g.sources.Refresh(ctx, SourceCurated)
	if err != nil || got != want {
		t.Fatalf("Refresh(curated) = %+v, %v", got, err)
	}
	if _, err := g.sources.Refresh(ctx, "src-ffffffffff"); !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("Refresh(unknown) = %v", err)
	}
	if _, err := g.sources.Refresh(ctx, "../etc"); !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("Refresh(path) = %v", err)
	}
}

type fakeCuratedRefresher struct{ res CheckResult }

func (f fakeCuratedRefresher) Refresh(context.Context) (CheckResult, error) { return f.res, nil }

func TestSources_CuratedCheckedRecordsOnlyASuccessfulCheck(t *testing.T) {
	ctx := context.Background()
	g := newSourceRig(t)
	at := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	g.sources.CuratedChecked(CheckResult{Outcome: OutcomeFailed, CheckedAt: at})
	if row, _ := g.store.Get(ctx, SourceCurated); !row.LastRefreshedAt.IsZero() {
		t.Fatalf("a failed check was recorded as a refresh: %v", row.LastRefreshedAt)
	}
	g.sources.CuratedChecked(CheckResult{Outcome: OutcomeUnchanged, CheckedAt: at})
	if row, _ := g.store.Get(ctx, SourceCurated); !row.LastRefreshedAt.Equal(at) || !row.SignatureVerified {
		t.Fatalf("row = %+v", row)
	}
}

func TestSources_RemoveRemovesOnlyThatSource(t *testing.T) {
	ctx := context.Background()
	g := newSourceRig(t)
	hostA, urlA := g.newHost(t)
	hostA.serve(buildArchive(t, sourceEntries(t, 1, map[string]int{"risky-agent": 1})), nil, "")
	a, err := g.sources.Add(ctx, AddRequest{URL: urlA})
	if err != nil {
		t.Fatal(err)
	}
	hostB, urlB := g.newHost(t)
	hostB.serve(buildArchive(t, sourceEntries(t, 1, map[string]int{"aio-notes": 1})), nil, "")
	b, err := g.sources.Add(ctx, AddRequest{URL: urlB})
	if err != nil {
		t.Fatal(err)
	}
	curatedTree := tree(t, g.sources.Curated.(DirCatalog).Root)
	bTree := tree(t, filepath.Join(g.sources.Dir, b.ID))

	if err := g.sources.Remove(ctx, a.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(g.sources.Dir, a.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the removed source's files remain: %v", err)
	}
	equalTrees(t, tree(t, filepath.Join(g.sources.Dir, b.ID)), bTree)
	equalTrees(t, tree(t, g.sources.Curated.(DirCatalog).Root), curatedTree)
	var ids []string
	for _, r := range g.rows(t) {
		ids = append(ids, r.ID)
	}
	if len(ids) != 2 || ids[0] != SourceCurated || ids[1] != b.ID {
		t.Fatalf("rows after removal = %v", ids)
	}
	if _, err := g.sources.Entry(ctx, "risky-agent"); !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("a removed source still supplies its template: %v", err)
	}
	if _, err := g.sources.Entry(ctx, "aio-notes"); err != nil {
		t.Fatalf("the other source lost its template: %v", err)
	}
	if err := g.sources.Remove(ctx, a.ID); !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("second Remove = %v, want ErrSourceNotFound", err)
	}
	if _, err := g.sources.From(ctx, a.ID); !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("From(removed) = %v, want ErrSourceNotFound", err)
	}
}

func TestSources_TheCuratedCatalogCannotBeRemoved(t *testing.T) {
	ctx := context.Background()
	g := newSourceRig(t)
	curatedTree := tree(t, g.sources.Curated.(DirCatalog).Root)
	if err := g.sources.Remove(ctx, SourceCurated); !errors.Is(err, ErrSourceCurated) {
		t.Fatalf("Remove(curated) = %v, want ErrSourceCurated", err)
	}
	equalTrees(t, tree(t, g.sources.Curated.(DirCatalog).Root), curatedTree)
	if _, err := g.store.Get(ctx, SourceCurated); err != nil {
		t.Fatalf("the curated row is gone: %v", err)
	}
	if _, err := g.sources.Index(ctx); err != nil {
		t.Fatalf("the curated catalog stopped listing: %v", err)
	}
	for _, id := range []string{"../catalog", "src-../../x", "", "src-zzzzzzzzzz"} {
		if err := g.sources.Remove(ctx, id); !errors.Is(err, ErrSourceNotFound) {
			t.Errorf("Remove(%q) = %v, want ErrSourceNotFound", id, err)
		}
	}
}

func TestSources_RemoveFinishesASourceWhoseFilesAreAlreadyGone(t *testing.T) {
	ctx := context.Background()
	g := newSourceRig(t)
	host, url := g.newHost(t)
	host.serve(buildArchive(t, sourceEntries(t, 1, map[string]int{"risky-agent": 1})), nil, "")
	a, err := g.sources.Add(ctx, AddRequest{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(g.sources.Dir, a.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := g.sources.Index(ctx); err != nil {
		t.Fatalf("a source with no files broke the catalog list: %v", err)
	}
	if err := g.sources.Remove(ctx, a.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(g.rows(t)) != 1 {
		t.Fatalf("rows = %+v", g.rows(t))
	}
}

func TestSources_ListReportsTheCuratedCatalogFirstThenEachSource(t *testing.T) {
	ctx := context.Background()
	g := newSourceRig(t)
	host, url := g.newHost(t)
	host.serve(buildArchive(t, sourceEntries(t, 11, map[string]int{"risky-agent": 1})), nil, "")
	a, err := g.sources.Add(ctx, AddRequest{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	list, err := g.sources.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %+v, %v", list, err)
	}
	c := list[0]
	if c.ID != SourceCurated || c.Kind != store.CatalogSourceCurated || !c.Signed || c.Serial != 7 || c.URL != "https://catalog.hoserva.dev" {
		t.Fatalf("curated = %+v", c)
	}
	if u := list[1]; u.ID != a.ID || u.Kind != store.CatalogSourceUserAdded || u.Signed || u.Serial != 11 || u.URL != url {
		t.Fatalf("user-added = %+v", u)
	}
}

// The curated catalog's store is the zero-value store: skipping the signature
// is something only a source with no key opts into, and a curated check
// against a host that serves an archive with a bad or missing signature is
// still refused.
func TestCatalogStore_AStoreThatIsNotMarkedUnsignedStillRefusesAnUnsignedArchive(t *testing.T) {
	pub, _ := newKey(t)
	archive := buildArchive(t, sourceEntries(t, 1, map[string]int{"jellyfin": 1}))
	for name, s := range map[string]CatalogStore{
		"explicit key": {Dir: filepath.Join(t.TempDir(), "c"), Key: pub},
		"default key":  {Dir: filepath.Join(t.TempDir(), "c")},
	} {
		if err := s.Install(archive, nil); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: Install of an unsigned archive = %v, want ErrBadSignature", name, err)
		}
		if _, err := s.InstallFetched(archive, []byte("junk"), Validators{}); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: InstallFetched of an unsigned archive = %v, want ErrBadSignature", name, err)
		}
		if _, ok, _ := s.Serial(); ok {
			t.Errorf("%s: an archive that failed the signature check was installed", name)
		}
	}
	unsigned := CatalogStore{Dir: filepath.Join(t.TempDir(), "c"), Unsigned: true}
	if err := unsigned.Install(archive, nil); err != nil {
		t.Fatalf("an unsigned store refused a well-formed archive: %v", err)
	}
	if err := (CatalogStore{Dir: filepath.Join(t.TempDir(), "c2"), Unsigned: true}).Install(buildArchive(t, []tarEntry{reg("x", "y")}), nil); !errors.Is(err, ErrBadArchive) {
		t.Fatalf("an unsigned store accepted a malformed archive: %v", err)
	}
}

func TestSources_WithNoDirectoryNothingIsFetchedOrWrittenRelativeToTheWorkingDirectory(t *testing.T) {
	ctx := context.Background()
	g := newSourceRig(t)
	host, url := g.newHost(t)
	host.serve(buildArchive(t, sourceEntries(t, 1, map[string]int{"risky-agent": 1})), nil, "")
	g.sources.Dir = ""
	if _, err := g.sources.Add(ctx, AddRequest{URL: url}); err == nil {
		t.Fatal("Add succeeded with no directory for the copies")
	}
	if reqs, _ := host.log(); len(reqs) != 0 {
		t.Fatalf("requests = %v, want none", reqs)
	}
	if len(g.rows(t)) != 1 {
		t.Fatalf("rows = %+v", g.rows(t))
	}
}

// A restored database brings the source rows back without their directories:
// until the source is refreshed it lists no serial, claims no signature check,
// supplies no entries and breaks nothing, and a refresh installs it again.
func TestSources_ARestoredSourceWithNoCopyIsHonestUntilRefreshed(t *testing.T) {
	ctx := context.Background()
	g := newSourceRig(t)
	host, url := g.newHost(t)
	pub, priv := newKey(t)
	archive, sig := signed(t, priv, sourceEntries(t, 4, map[string]int{"risky-agent": 1}))
	host.serve(archive, sig, `"v4"`)
	a, err := g.sources.Add(ctx, AddRequest{URL: url, PublicKey: base64.StdEncoding.EncodeToString(pub)})
	if err != nil || !a.Signed {
		t.Fatalf("Add = %+v, %v", a, err)
	}
	if err := os.RemoveAll(g.sources.Dir); err != nil {
		t.Fatal(err)
	}
	g.sources.refreshers = nil

	list, err := g.sources.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if u := list[1]; u.Serial != 0 || u.Signed {
		t.Fatalf("restored source = %+v, want no serial and no signed claim", u)
	}
	idx, err := g.sources.Index(ctx)
	if err != nil || len(idx.Templates) != 1 || idx.Templates[0].ID != "jellyfin" {
		t.Fatalf("index = %+v, %v, want only the curated entry", idx, err)
	}
	if _, err := g.sources.Entry(ctx, "risky-agent"); !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("Entry = %v, want ErrTemplateNotFound", err)
	}

	res, err := g.sources.Refresh(ctx, a.ID)
	if err != nil || res.Outcome != OutcomeUpdated {
		t.Fatalf("Refresh = %+v, %v, want the archive installed again", res, err)
	}
	list, _ = g.sources.List(ctx)
	if u := list[1]; u.Serial != 4 || !u.Signed {
		t.Fatalf("after the refresh = %+v", u)
	}
	if e, err := g.sources.Entry(ctx, "risky-agent"); err != nil || !e.Signed {
		t.Fatalf("Entry after the refresh = %+v, %v", e, err)
	}
}

func TestSources_ScreenshotsAndMaintainersComeFromTheSourceThatSuppliesTheTemplate(t *testing.T) {
	ctx := context.Background()
	g := newSourceRig(t)
	host, url := g.newHost(t)
	compose := strings.Replace(sourceCompose(t, "risky-agent", 2), "  docs:", "  maintainer: Some Team\n  screenshots: [shots/one.png]\n  docs:", 1)
	host.serve(buildArchive(t, []tarEntry{
		reg("index.json", `{"schema":1,"serial":1,"templates":[{"id":"risky-agent","revision":2,"title":"Risky agent","categories":[],"docs":"https://example.com","maintainer":"Some Team"}]}`),
		{Name: "risky-agent/", Type: tar.TypeDir},
		reg("risky-agent/compose.yaml", compose),
		{Name: "risky-agent/shots/", Type: tar.TypeDir},
		reg("risky-agent/shots/one.png", "PNG"),
	}), nil, "")
	if _, err := g.sources.Add(ctx, AddRequest{URL: url}); err != nil {
		t.Fatal(err)
	}

	idx, err := g.sources.Index(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var maintainer string
	for _, e := range idx.Templates {
		if e.ID == "risky-agent" {
			maintainer = e.Maintainer
		}
	}
	if maintainer != "Some Team" {
		t.Errorf("maintainer of risky-agent = %q", maintainer)
	}
	shot, err := g.sources.Screenshot(ctx, "risky-agent", 0)
	if err != nil || shot.ContentType != "image/png" || string(shot.Data) != "PNG" {
		t.Fatalf("Screenshot = %+v, %v", shot, err)
	}
	if _, err := g.sources.Screenshot(ctx, "risky-agent", 1); !errors.Is(err, ErrScreenshotNotFound) {
		t.Errorf("a position past the list: err = %v", err)
	}
	if _, err := g.sources.Screenshot(ctx, "jellyfin", 0); !errors.Is(err, ErrScreenshotNotFound) {
		t.Errorf("a curated template with none: err = %v", err)
	}
	if _, err := g.sources.Screenshot(ctx, "nope", 0); !errors.Is(err, ErrTemplateNotFound) {
		t.Errorf("an unknown template: err = %v", err)
	}
}
