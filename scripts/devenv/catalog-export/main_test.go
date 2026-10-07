package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

const fixtures = "../../../internal/template/testdata/catalog"

var screenshotBytes = []byte("\x89PNG not really a picture")

type served struct {
	archive, sig []byte
	status       int
}

type env struct {
	t        *testing.T
	pub      ed25519.PublicKey
	priv     ed25519.PrivateKey
	dir      string
	pin      string
	snapshot string
	out      string
}

func newEnv(t *testing.T, pinned int64) *env {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	e := &env{t: t, pub: pub, priv: priv, dir: dir, pin: filepath.Join(dir, "catalog.pin"), snapshot: filepath.Join(dir, "snapshot"), out: filepath.Join(dir, "site", ".catalog")}
	if err := os.WriteFile(e.pin, []byte(fmt.Sprintf("# comment\nserial=%d\nsha256=00\n", pinned)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.snapshot, 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) read(rel string) string {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtures, rel))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(data)
}

func (e *env) archive(serial int64, ids ...string) []byte {
	e.t.Helper()
	type entry struct {
		Name string
		Body []byte
		Dir  bool
	}
	var entries []entry
	var index []map[string]any
	for _, id := range ids {
		compose := e.read(id + "/compose.yaml")
		if id == "jellyfin" {
			compose = strings.Replace(compose, "icon: icon.svg\n", "icon: icon.svg\n  screenshots: [shot.png]\n  description: A media server.\n", 1)
			entries = append(entries, entry{Name: id + "/shot.png", Body: screenshotBytes})
		}
		entries = append(entries,
			entry{Name: id + "/", Dir: true},
			entry{Name: id + "/compose.yaml", Body: []byte(compose)},
			entry{Name: id + "/icon.svg", Body: []byte(e.read(id + "/icon.svg"))},
		)
		index = append(index, map[string]any{
			"id": id, "revision": 1, "title": strings.ToUpper(id[:1]) + id[1:], "categories": []string{"media"},
			"docs": "https://example.com/" + id, "description": map[bool]string{true: "A media server."}[id == "jellyfin"],
		})
	}
	doc, err := json.Marshal(map[string]any{"schema": 1, "serial": serial, "generatedAt": "2026-10-01T11:14:10Z", "templates": index})
	if err != nil {
		e.t.Fatal(err)
	}
	entries = append([]entry{{Name: "index.json", Body: doc}}, entries...)

	var tarball bytes.Buffer
	tw := tar.NewWriter(&tarball)
	for _, en := range entries {
		hdr := &tar.Header{Name: en.Name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(en.Body))}
		if en.Dir {
			hdr.Typeflag, hdr.Mode = tar.TypeDir, 0o755
		}
		if err := tw.WriteHeader(hdr); err != nil {
			e.t.Fatal(err)
		}
		if _, err := tw.Write(en.Body); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		e.t.Fatal(err)
	}
	var out bytes.Buffer
	zw, err := zstd.NewWriter(&out)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := zw.Write(tarball.Bytes()); err != nil {
		e.t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		e.t.Fatal(err)
	}
	return out.Bytes()
}

func (e *env) signed(serial int64, ids ...string) (archive, sig []byte) {
	a := e.archive(serial, ids...)
	return a, ed25519.Sign(e.priv, a)
}

func (e *env) pinSnapshot(archive, sig []byte) {
	e.t.Helper()
	for name, data := range map[string][]byte{archiveName: archive, sigName: sig} {
		if err := os.WriteFile(filepath.Join(e.snapshot, name), data, 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
}

func tampered(archive []byte) []byte {
	out := bytes.Clone(archive)
	out[len(out)/2] ^= 0xff
	return out
}

func (e *env) run(extra ...string) (string, error) {
	e.t.Helper()
	var log bytes.Buffer
	args := append([]string{"-pin", e.pin, "-snapshot", e.snapshot, "-out", e.out}, extra...)
	err := run(context.Background(), args, e.pub, &log)
	return log.String(), err
}

func (e *env) live(s *served) string {
	e.t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.status != 0 {
			w.WriteHeader(s.status)
			return
		}
		switch r.URL.Path {
		case "/catalog/" + archiveName:
			_, _ = w.Write(s.archive)
		case "/catalog/" + sigName:
			_, _ = w.Write(s.sig)
		default:
			http.NotFound(w, r)
		}
	}))
	e.t.Cleanup(srv.Close)
	return srv.URL + "/catalog/"
}

func (e *env) exported() siteIndex {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.out, "index.json"))
	if err != nil {
		e.t.Fatal(err)
	}
	var doc siteIndex
	if err := json.Unmarshal(data, &doc); err != nil {
		e.t.Fatal(err)
	}
	return doc
}

func (e *env) noLeftovers() {
	e.t.Helper()
	entries, err := os.ReadDir(filepath.Dir(e.out))
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		e.t.Fatal(err)
	}
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), ".catalog-export-") {
			e.t.Errorf("a staging directory %s was left behind", en.Name())
		}
	}
}

func TestExportsThePinnedSnapshot(t *testing.T) {
	e := newEnv(t, 10)
	e.pinSnapshot(e.signed(10, "jellyfin", "aio-notes"))
	if out, err := e.run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	doc := e.exported()
	if doc.Serial != 10 || len(doc.Templates) != 2 || doc.GeneratedAt != "2026-10-01T11:14:10Z" {
		t.Fatalf("index = %+v", doc)
	}
	jf := doc.Templates[0]
	if jf.ID != "jellyfin" || jf.Icon != "icon.svg" || len(jf.Screenshots) != 1 || jf.Screenshots[0] != "screenshot-1.png" || jf.Description != "A media server." {
		t.Fatalf("jellyfin = %+v", jf)
	}
	dir := filepath.Join(e.out, "static", "apps", "jellyfin")
	if got, err := os.ReadFile(filepath.Join(dir, "screenshot-1.png")); err != nil || !bytes.Equal(got, screenshotBytes) {
		t.Fatalf("screenshot = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "icon.svg")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "compose.yaml")); err == nil {
		t.Error("the compose file was exported; the site needs only the public fields")
	}
	e.noLeftovers()
}

func TestExportsAnEmptyCatalog(t *testing.T) {
	e := newEnv(t, 10)
	e.pinSnapshot(e.signed(10))
	if out, err := e.run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if doc := e.exported(); doc.Serial != 10 || len(doc.Templates) != 0 {
		t.Fatalf("index = %+v", doc)
	}
	data, _ := os.ReadFile(filepath.Join(e.out, "index.json"))
	if !strings.Contains(string(data), `"templates": []`) {
		t.Fatalf("an empty catalog must list templates as [], got %s", data)
	}
}

func TestRefusesATamperedSnapshot(t *testing.T) {
	e := newEnv(t, 10)
	archive, sig := e.signed(10, "jellyfin")
	e.pinSnapshot(tampered(archive), sig)
	out, err := e.run()
	if err == nil || !strings.Contains(err.Error(), "signature does not verify") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if _, statErr := os.Stat(e.out); statErr == nil {
		t.Error("a refused archive left an export behind")
	}
	e.noLeftovers()
}

func TestRefusesASnapshotSignedByAnotherKey(t *testing.T) {
	e := newEnv(t, 10)
	archive, _ := e.signed(10, "jellyfin")
	_, other, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	e.pinSnapshot(archive, ed25519.Sign(other, archive))
	if out, err := e.run(); err == nil || !strings.Contains(err.Error(), "signature does not verify") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if _, statErr := os.Stat(e.out); statErr == nil {
		t.Error("a refused archive left an export behind")
	}
}

func TestRefusesASnapshotBelowThePin(t *testing.T) {
	e := newEnv(t, 10)
	e.pinSnapshot(e.signed(9, "jellyfin"))
	out, err := e.run()
	if err == nil || !strings.Contains(err.Error(), "lower than the pinned serial 10") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if _, statErr := os.Stat(e.out); statErr == nil {
		t.Error("a refused archive left an export behind")
	}
	e.noLeftovers()
}

func TestRefusedExportKeepsAnExistingOne(t *testing.T) {
	e := newEnv(t, 10)
	e.pinSnapshot(e.signed(10, "jellyfin"))
	if out, err := e.run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	e.pinSnapshot(e.signed(9, "jellyfin", "aio-notes"))
	if _, err := e.run(); err == nil {
		t.Fatal("a low-serial snapshot was exported")
	}
	if doc := e.exported(); doc.Serial != 10 || len(doc.Templates) != 1 {
		t.Fatalf("the earlier export was changed: %+v", doc)
	}
}

func TestLiveArchiveIsUsedWhenItVerifies(t *testing.T) {
	e := newEnv(t, 10)
	e.pinSnapshot(e.signed(10, "jellyfin"))
	archive, sig := e.signed(12, "jellyfin", "aio-notes")
	if out, err := e.run("-live", e.live(&served{archive: archive, sig: sig})); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if doc := e.exported(); doc.Serial != 12 || len(doc.Templates) != 2 {
		t.Fatalf("index = %+v", doc)
	}
}

func TestLiveArchiveAtThePinnedSerialIsUsed(t *testing.T) {
	e := newEnv(t, 10)
	e.pinSnapshot(e.signed(10, "jellyfin"))
	archive, sig := e.signed(10, "jellyfin", "aio-notes")
	if out, err := e.run("-live", e.live(&served{archive: archive, sig: sig})); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if doc := e.exported(); len(doc.Templates) != 2 {
		t.Fatalf("index = %+v", doc)
	}
}

func TestLiveFallsBackToThePinnedSnapshot(t *testing.T) {
	cases := map[string]func(e *env) *served{
		"tampered archive": func(e *env) *served {
			a, s := e.signed(12, "jellyfin", "aio-notes")
			return &served{archive: tampered(a), sig: s}
		},
		"low serial": func(e *env) *served {
			a, s := e.signed(9, "jellyfin", "aio-notes")
			return &served{archive: a, sig: s}
		},
		"server error": func(*env) *served { return &served{status: http.StatusInternalServerError} },
		"missing signature": func(e *env) *served {
			a, _ := e.signed(12, "jellyfin", "aio-notes")
			return &served{archive: a}
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, 10)
			e.pinSnapshot(e.signed(10, "jellyfin"))
			out, err := e.run("-live", e.live(mk(e)))
			if err != nil {
				t.Fatalf("the pinned snapshot was not used: %v\n%s", err, out)
			}
			if !strings.Contains(out, "warning: the live catalog at") || !strings.Contains(out, "using the pinned snapshot") {
				t.Errorf("no warning in the log:\n%s", out)
			}
			if doc := e.exported(); doc.Serial != 10 || len(doc.Templates) != 1 {
				t.Fatalf("the export is not the pinned snapshot: %+v", doc)
			}
			e.noLeftovers()
		})
	}
}

func TestLiveFailureDoesNotExcuseABadSnapshot(t *testing.T) {
	e := newEnv(t, 10)
	archive, sig := e.signed(10, "jellyfin")
	e.pinSnapshot(tampered(archive), sig)
	liveArchive, liveSig := e.signed(12, "jellyfin")
	tamperedLive := e.live(&served{archive: tampered(liveArchive), sig: liveSig})
	out, err := e.run("-live", tamperedLive)
	if err == nil {
		t.Fatalf("a bad pinned snapshot was exported\n%s", out)
	}
	if _, statErr := os.Stat(e.out); statErr == nil {
		t.Error("an export was left behind")
	}
}

func TestLiveURLComesFromTheEnvironment(t *testing.T) {
	e := newEnv(t, 10)
	e.pinSnapshot(e.signed(10, "jellyfin"))
	archive, sig := e.signed(12, "jellyfin", "aio-notes")
	t.Setenv("CATALOG_LIVE_URL", e.live(&served{archive: archive, sig: sig}))
	if out, err := e.run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if doc := e.exported(); doc.Serial != 12 {
		t.Fatalf("the live catalog was not used: %+v", doc)
	}
}

func TestRefusesAMalformedPin(t *testing.T) {
	for name, pin := range map[string]string{
		"no serial":        "sha256=00\n",
		"two serials":      "serial=1\nserial=2\n",
		"not a number":     "serial=abc\n",
		"not positive":     "serial=0\n",
		"empty serial":     "serial=\n",
		"commented serial": "# serial=5\n",
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, 10)
			e.pinSnapshot(e.signed(10, "jellyfin"))
			if err := os.WriteFile(e.pin, []byte(pin), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := e.run(); err == nil {
				t.Fatal("a malformed pin was accepted")
			}
		})
	}
}

func TestUsageNeedsEveryPath(t *testing.T) {
	if err := run(context.Background(), []string{"-pin", "x"}, nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("err = %v", err)
	}
}
