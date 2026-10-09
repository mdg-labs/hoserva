package template

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	releasekey "github.com/mdg-labs/hoserva/internal/update"
)

type tarEntry struct {
	Name     string
	Type     byte
	Body     string
	Linkname string
}

func reg(name, body string) tarEntry { return tarEntry{Name: name, Type: tar.TypeReg, Body: body} }

func fixtureEntries(serial int64, extra ...tarEntry) []tarEntry {
	entries := []tarEntry{
		reg("index.json", fmt.Sprintf(`{"schema":1,"serial":%d,"templates":[]}`, serial)),
		{Name: "jellyfin/", Type: tar.TypeDir},
		reg("jellyfin/compose.yaml", fmt.Sprintf("name: jellyfin # serial %d\n", serial)),
	}
	return append(entries, extra...)
}

func buildArchive(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var tarball bytes.Buffer
	tw := tar.NewWriter(&tarball)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.Name, Typeflag: e.Type, Mode: 0o644, Linkname: e.Linkname, Size: int64(len(e.Body))}
		if e.Type == tar.TypeDir {
			hdr.Mode = 0o755
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.Body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	zw, err := zstd.NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(tarball.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func signed(t *testing.T, priv ed25519.PrivateKey, entries []tarEntry) (archive, sig []byte) {
	t.Helper()
	archive = buildArchive(t, entries)
	return archive, ed25519.Sign(priv, archive)
}

// tree is every path under root with its type, mode and content — what
// "byte-for-byte unchanged" compares.
func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			out[rel] = fmt.Sprintf("dir %v", info.Mode())
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = fmt.Sprintf("%v %q", info.Mode(), data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func listing(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func equalTrees(t *testing.T, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("tree has %d entries, want %d\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%q changed: got %s, want %s", k, got[k], v)
		}
	}
}

func installedStore(t *testing.T, serial int64) (CatalogStore, ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv := newKey(t)
	parent := t.TempDir()
	store := CatalogStore{Dir: filepath.Join(parent, "catalog"), Key: pub}
	archive, sig := signed(t, priv, fixtureEntries(serial))
	if err := store.Install(archive, sig); err != nil {
		t.Fatalf("installing the starting catalog: %v", err)
	}
	return store, priv, parent
}

func TestCatalogPublicKey_IsPinned(t *testing.T) {
	const want = "ddfb9ad6a1f0db76bd50a0b234b90ceb0679bcf82b5562b5c9598af3ab870f4f"
	if len(CatalogPublicKey) != ed25519.PublicKeySize {
		t.Fatalf("catalog public key is %d bytes, want %d", len(CatalogPublicKey), ed25519.PublicKeySize)
	}
	if got := hex.EncodeToString(CatalogPublicKey); got != want {
		t.Fatalf("catalog public key = %s, want %s (signing-key.pub.pem of mdg-labs/hoserva-catalog)", got, want)
	}
}

func TestCatalogStore_ChecksOnlyTheCatalogKey(t *testing.T) {
	if bytes.Equal(CatalogPublicKey, releasekey.EmbeddedPublicKey) {
		t.Fatal("the catalog key must be separate from the update key")
	}
	pub, priv := newKey(t)
	archive, sig := signed(t, priv, fixtureEntries(5))

	byDefault := CatalogStore{Dir: filepath.Join(t.TempDir(), "catalog")}
	if err := byDefault.Install(archive, sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("an archive signed with another key installed under the default key: %v", err)
	}
	if _, err := os.Stat(byDefault.Dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a refused archive left %s: %v", byDefault.Dir, err)
	}
	asUpdate := CatalogStore{Dir: filepath.Join(t.TempDir(), "catalog"), Key: releasekey.EmbeddedPublicKey}
	if err := asUpdate.Install(archive, sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("an archive installed against the update key: %v", err)
	}
	withOwnKey := CatalogStore{Dir: filepath.Join(t.TempDir(), "catalog"), Key: pub}
	if err := withOwnKey.Install(archive, sig); err != nil {
		t.Fatalf("the matching key refused its own archive: %v", err)
	}
}

func TestCatalogStore_InstallsIntoAMissingDirectoryAndServesIt(t *testing.T) {
	pub, priv := newKey(t)
	parent := t.TempDir()
	store := CatalogStore{Dir: filepath.Join(parent, "catalog"), Key: pub}
	archive, sig := signed(t, priv, fixtureEntries(7))
	if err := store.Install(archive, sig); err != nil {
		t.Fatal(err)
	}
	entry, err := DirCatalog{Root: store.Dir, Source: SourceCurated}.Entry(context.Background(), "jellyfin")
	if err != nil || !strings.Contains(string(entry.Data), "serial 7") {
		t.Fatalf("Entry = %q, %v", entry.Data, err)
	}
	serial, ok, err := store.Serial()
	if err != nil || !ok || serial != 7 {
		t.Fatalf("Serial = %d, %v, %v", serial, ok, err)
	}
	if got := listing(t, parent); len(got) != 1 || got[0] != "catalog" {
		t.Fatalf("the state directory holds %v, want only catalog", got)
	}
}

func TestCatalogStore_RefusesAReplayWithoutChangingTheOnDiskCatalog(t *testing.T) {
	store, priv, parent := installedStore(t, 100)
	before := tree(t, store.Dir)
	beforeParent := listing(t, parent)

	for _, serial := range []int64{100, 99, 1} {
		archive, sig := signed(t, priv, fixtureEntries(serial, reg("extra/compose.yaml", "name: extra\n")))
		err := store.Install(archive, sig)
		if !errors.Is(err, ErrNotNewer) {
			t.Fatalf("serial %d over 100: err = %v, want ErrNotNewer", serial, err)
		}
		equalTrees(t, tree(t, store.Dir), before)
		if got := listing(t, parent); strings.Join(got, ",") != strings.Join(beforeParent, ",") {
			t.Fatalf("serial %d left %v beside the catalog, want %v", serial, got, beforeParent)
		}
	}

	archive, sig := signed(t, priv, fixtureEntries(101, reg("extra/compose.yaml", "name: extra\n")))
	if err := store.Install(archive, sig); err != nil {
		t.Fatal(err)
	}
	if got := tree(t, store.Dir); got["extra/compose.yaml"] == "" || !strings.Contains(got["jellyfin/compose.yaml"], "serial 101") {
		t.Fatalf("a higher serial did not replace the catalog: %v", got)
	}
	if got := listing(t, parent); len(got) != 1 {
		t.Fatalf("a completed replacement left %v beside the catalog", got)
	}
}

func TestCatalogStore_RefusesABadSignatureWithoutChangingTheOnDiskCatalog(t *testing.T) {
	store, priv, parent := installedStore(t, 100)
	before := tree(t, store.Dir)

	good, goodSig := signed(t, priv, fixtureEntries(200))
	flipped := append([]byte(nil), goodSig...)
	flipped[0] ^= 0xff
	flippedArchive := append([]byte(nil), good...)
	flippedArchive[len(flippedArchive)-1] ^= 0xff
	_, otherPriv := newKey(t)
	otherSig := ed25519.Sign(otherPriv, good)

	for name, c := range map[string]struct{ archive, sig []byte }{
		"flipped signature":  {good, flipped},
		"tampered archive":   {flippedArchive, goodSig},
		"another key":        {good, otherSig},
		"truncated":          {good, goodSig[:32]},
		"empty signature":    {good, nil},
		"signature of nil":   {good, ed25519.Sign(priv, nil)},
		"archive not zstd":   {[]byte("not an archive"), ed25519.Sign(priv, []byte("not an archive"))},
		"empty signed bytes": {nil, ed25519.Sign(priv, nil)},
	} {
		err := store.Install(c.archive, c.sig)
		if err == nil {
			t.Fatalf("%s: installed", name)
		}
		equalTrees(t, tree(t, store.Dir), before)
		if got := listing(t, parent); len(got) != 1 {
			t.Fatalf("%s: left %v beside the catalog", name, got)
		}
	}
}

func TestCatalogStore_RefusesAMalformedArchiveWithoutChangingTheOnDiskCatalog(t *testing.T) {
	store, priv, parent := installedStore(t, 100)
	before := tree(t, store.Dir)

	cases := map[string][]tarEntry{
		"parent directory":        fixtureEntries(200, reg("../escape", "x")),
		"nested parent":           fixtureEntries(200, reg("jellyfin/../../escape", "x")),
		"absolute path":           fixtureEntries(200, reg("/etc/escape", "x")),
		"dot slash prefix":        fixtureEntries(200, reg("./jellyfin/extra", "x")),
		"backslash":               fixtureEntries(200, reg(`jellyfin\..\escape`, "x")),
		"empty component":         fixtureEntries(200, reg("jellyfin//extra", "x")),
		"symlink":                 fixtureEntries(200, tarEntry{Name: "jellyfin/link", Type: tar.TypeSymlink, Linkname: "/etc/passwd"}),
		"hard link":               fixtureEntries(200, tarEntry{Name: "jellyfin/link", Type: tar.TypeLink, Linkname: "index.json"}),
		"character device":        fixtureEntries(200, tarEntry{Name: "jellyfin/dev", Type: tar.TypeChar}),
		"block device":            fixtureEntries(200, tarEntry{Name: "jellyfin/dev", Type: tar.TypeBlock}),
		"fifo":                    fixtureEntries(200, tarEntry{Name: "jellyfin/fifo", Type: tar.TypeFifo}),
		"entry outside an id":     fixtureEntries(200, reg("README.md", "x")),
		"directory not an id":     fixtureEntries(200, reg("Not_An_Id/compose.yaml", "x")),
		"file under index.json":   fixtureEntries(200, reg("index.json/x", "x")),
		"second index.json":       fixtureEntries(200, reg("index.json", `{"serial":999}`)),
		"duplicate file":          fixtureEntries(200, reg("jellyfin/compose.yaml", "name: other\n")),
		"directory over a file":   fixtureEntries(200, tarEntry{Name: "jellyfin/compose.yaml/", Type: tar.TypeDir}),
		"id as a plain file":      fixtureEntries(200, reg("plexless", "x")),
		"no index.json":           {{Name: "jellyfin/", Type: tar.TypeDir}, reg("jellyfin/compose.yaml", "x")},
		"index.json not json":     {reg("index.json", "nope"), reg("jellyfin/compose.yaml", "x")},
		"index.json without time": {reg("index.json", `{"schema":1,"templates":[]}`), reg("jellyfin/compose.yaml", "x")},
	}
	for name, entries := range cases {
		archive, sig := signed(t, priv, entries)
		err := store.Install(archive, sig)
		if !errors.Is(err, ErrBadArchive) {
			t.Errorf("%s: err = %v, want ErrBadArchive", name, err)
			continue
		}
		equalTrees(t, tree(t, store.Dir), before)
		if got := listing(t, parent); len(got) != 1 {
			t.Errorf("%s: left %v beside the catalog", name, got)
		}
		if _, err := os.Stat(filepath.Join(parent, "escape")); err == nil {
			t.Errorf("%s: wrote outside the catalog", name)
		}
	}
}

func TestCatalogStore_RefusesAnArchiveBeyondTheSizeCap(t *testing.T) {
	store, priv, parent := installedStore(t, 100)
	before := tree(t, store.Dir)
	old := maxCatalogBytes
	maxCatalogBytes = 4096
	t.Cleanup(func() { maxCatalogBytes = old })
	big := strings.Repeat("a", maxCatalogBytes+1)
	archive, sig := signed(t, priv, fixtureEntries(200, reg("jellyfin/big.bin", big)))
	if err := store.Install(archive, sig); !errors.Is(err, ErrBadArchive) {
		t.Fatalf("err = %v, want ErrBadArchive", err)
	}
	equalTrees(t, tree(t, store.Dir), before)
	if got := listing(t, parent); len(got) != 1 {
		t.Fatalf("left %v beside the catalog", got)
	}
}

func lowerEntryCaps(t *testing.T, entries, bytes int) {
	t.Helper()
	oldEntries, oldBytes := maxCatalogEntries, maxCatalogBytes
	maxCatalogEntries, maxCatalogBytes = entries, bytes
	t.Cleanup(func() { maxCatalogEntries, maxCatalogBytes = oldEntries, oldBytes })
}

func manyFiles(n int) []tarEntry {
	var out []tarEntry
	for i := 0; i < n; i++ {
		out = append(out, reg(fmt.Sprintf("jellyfin/f%04d", i), ""))
	}
	return out
}

func manyDirs(n int) []tarEntry {
	var out []tarEntry
	for i := 0; i < n; i++ {
		out = append(out, tarEntry{Name: fmt.Sprintf("jellyfin/d%04d/", i), Type: tar.TypeDir})
	}
	return out
}

func TestCatalogStore_RefusesAnArchiveWithTooManyEntriesOrAnOverlongOrDeepName(t *testing.T) {
	store, priv, parent := installedStore(t, 100)
	before := tree(t, store.Dir)
	lowerEntryCaps(t, 20, 256<<20)

	cases := map[string][]tarEntry{
		"too many files":       fixtureEntries(200, manyFiles(maxCatalogEntries)...),
		"too many directories": fixtureEntries(200, manyDirs(maxCatalogEntries)...),
		"one over-long name":   fixtureEntries(200, reg("jellyfin/"+strings.Repeat("a", maxEntryNameBytes), "x")),
		"one over-long directory name": fixtureEntries(200,
			tarEntry{Name: "jellyfin/" + strings.Repeat("a", maxEntryNameBytes) + "/", Type: tar.TypeDir}),
		"too deep": fixtureEntries(200, reg("jellyfin/"+strings.Repeat("d/", maxEntryDepth)+"f", "x")),
	}
	for name, entries := range cases {
		archive, sig := signed(t, priv, entries)
		if _, _, err := checkArchive(context.Background(), archive); !errors.Is(err, ErrBadArchive) {
			t.Errorf("%s: checkArchive err = %v, want ErrBadArchive", name, err)
		}
		if err := store.Install(archive, sig); !errors.Is(err, ErrBadArchive) {
			t.Errorf("%s: Install err = %v, want ErrBadArchive", name, err)
			continue
		}
		equalTrees(t, tree(t, store.Dir), before)
		if got := listing(t, parent); len(got) != 1 {
			t.Errorf("%s: left %v beside the catalog", name, got)
		}
	}
}

func TestCatalogStore_AcceptsAnArchiveAtTheEntryAndNameCaps(t *testing.T) {
	store, priv, _ := installedStore(t, 100)
	lowerEntryCaps(t, 20, 256<<20)
	entries := fixtureEntries(200, manyFiles(maxCatalogEntries-4)...)
	entries = append(entries, reg("jellyfin/"+strings.Repeat("d/", maxEntryDepth-2)+strings.Repeat("a", 100), "x"))
	archive, sig := signed(t, priv, entries)
	if err := store.Install(archive, sig); err != nil {
		t.Fatalf("Install at the caps: %v", err)
	}
}

func TestCatalogStore_ChargesEveryEntryAgainstTheByteBudget(t *testing.T) {
	store, priv, parent := installedStore(t, 100)
	before := tree(t, store.Dir)
	lowerEntryCaps(t, 1000, 10*entryCostBytes)
	archive, sig := signed(t, priv, fixtureEntries(200, manyFiles(20)...))
	if err := store.Install(archive, sig); !errors.Is(err, ErrBadArchive) {
		t.Fatalf("err = %v, want ErrBadArchive", err)
	}
	equalTrees(t, tree(t, store.Dir), before)
	if got := listing(t, parent); len(got) != 1 {
		t.Fatalf("left %v beside the catalog", got)
	}
}

func TestCatalogStore_InstallFetchedStopsWhenItsContextEnds(t *testing.T) {
	store, priv, parent := installedStore(t, 100)
	before := tree(t, store.Dir)
	archive, sig := signed(t, priv, fixtureEntries(200))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.InstallFetched(ctx, archive, sig, Validators{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	equalTrees(t, tree(t, store.Dir), before)
	if got := listing(t, parent); len(got) != 1 {
		t.Fatalf("left %v beside the catalog", got)
	}
}

func TestCatalogStore_AFailedSwapKeepsThePreviousCatalog(t *testing.T) {
	store, priv, parent := installedStore(t, 100)
	before := tree(t, store.Dir)
	archive, sig := signed(t, priv, fixtureEntries(200))

	renames := 0
	store.rename = func(oldpath, newpath string) error {
		renames++
		if renames == 2 {
			return errors.New("injected: the replacement cannot be moved into place")
		}
		return os.Rename(oldpath, newpath)
	}
	if err := store.Install(archive, sig); err == nil {
		t.Fatal("a failed swap reported success")
	}
	equalTrees(t, tree(t, store.Dir), before)
	if got := listing(t, parent); len(got) != 1 {
		t.Fatalf("left %v beside the catalog", got)
	}
}

func TestCatalogStore_ASwapInterruptedBetweenItsRenamesIsRecovered(t *testing.T) {
	store, priv, parent := installedStore(t, 100)
	before := tree(t, store.Dir)
	archive, sig := signed(t, priv, fixtureEntries(200))

	// The process dies after the previous copy moved aside and before the
	// replacement moved in: every later rename fails too.
	renames := 0
	store.rename = func(oldpath, newpath string) error {
		renames++
		if renames >= 2 {
			return errors.New("injected: process died")
		}
		return os.Rename(oldpath, newpath)
	}
	if err := store.Install(archive, sig); err == nil {
		t.Fatal("an interrupted swap reported success")
	}
	if _, err := os.Stat(store.Dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected the interrupted state (no catalog directory), got %v", err)
	}

	restarted := CatalogStore{Dir: store.Dir, Key: store.Key}
	if err := restarted.Recover(); err != nil {
		t.Fatal(err)
	}
	equalTrees(t, tree(t, store.Dir), before)
	if got := listing(t, parent); len(got) != 1 {
		t.Fatalf("recovery left %v beside the catalog", got)
	}
}

func TestCatalogStore_RecoverCleansLeftoversAndNeverTouchesALiveCatalog(t *testing.T) {
	store, _, parent := installedStore(t, 100)
	before := tree(t, store.Dir)

	for _, leftover := range []string{store.Dir + stagingSuffix, store.Dir + backupSuffix} {
		if err := os.MkdirAll(filepath.Join(leftover, "x"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(leftover, "x", "compose.yaml"), []byte("name: x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Recover(); err != nil {
		t.Fatal(err)
	}
	equalTrees(t, tree(t, store.Dir), before)
	if got := listing(t, parent); len(got) != 1 {
		t.Fatalf("left %v beside the catalog", got)
	}

	if err := store.Recover(); err != nil {
		t.Fatalf("recovering a clean state: %v", err)
	}
	empty := CatalogStore{Dir: filepath.Join(t.TempDir(), "catalog")}
	if err := empty.Recover(); err != nil {
		t.Fatalf("recovering a state directory with no catalog: %v", err)
	}
}

func TestCatalogStore_SeedInstallsWhenMissingOrOlderAndLeavesAnEqualOrNewerCopyAlone(t *testing.T) {
	pub, priv := newKey(t)
	dir := filepath.Join(t.TempDir(), "catalog")
	store := CatalogStore{Dir: dir, Key: pub}

	archive, sig := signed(t, priv, fixtureEntries(100))
	installed, err := store.Seed(archive, sig)
	if err != nil || !installed {
		t.Fatalf("seeding an empty state directory: %v, %v", installed, err)
	}
	seeded := tree(t, dir)

	installed, err = store.Seed(archive, sig)
	if err != nil || installed {
		t.Fatalf("seeding the same serial again: %v, %v", installed, err)
	}
	equalTrees(t, tree(t, dir), seeded)

	older, olderSig := signed(t, priv, fixtureEntries(50))
	installed, err = store.Seed(older, olderSig)
	if err != nil || installed {
		t.Fatalf("seeding an older serial: %v, %v", installed, err)
	}
	equalTrees(t, tree(t, dir), seeded)

	newer, newerSig := signed(t, priv, fixtureEntries(150))
	installed, err = store.Seed(newer, newerSig)
	if err != nil || !installed {
		t.Fatalf("seeding a newer serial: %v, %v", installed, err)
	}
	if got := tree(t, dir); !strings.Contains(got["jellyfin/compose.yaml"], "serial 150") {
		t.Fatalf("the newer snapshot was not installed: %v", got)
	}
}

func TestCatalogStore_SeedVerifiesTheSignatureEvenWhenItWouldNotInstall(t *testing.T) {
	pub, priv := newKey(t)
	store := CatalogStore{Dir: filepath.Join(t.TempDir(), "catalog"), Key: pub}
	archive, sig := signed(t, priv, fixtureEntries(100))
	if _, err := store.Seed(archive, sig); err != nil {
		t.Fatal(err)
	}
	before := tree(t, store.Dir)

	older, _ := signed(t, priv, fixtureEntries(50))
	if _, err := store.Seed(older, make([]byte, ed25519.SignatureSize)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a badly signed snapshot was accepted: %v", err)
	}
	newer, _ := signed(t, priv, fixtureEntries(500))
	if installed, err := store.Seed(newer, make([]byte, ed25519.SignatureSize)); !errors.Is(err, ErrBadSignature) || installed {
		t.Fatalf("a badly signed newer snapshot was installed: %v, %v", installed, err)
	}
	equalTrees(t, tree(t, store.Dir), before)
}

func TestCatalogStore_ReplacesAnOnDiskCopyWithoutAnIndex(t *testing.T) {
	pub, priv := newKey(t)
	dir := filepath.Join(t.TempDir(), "catalog")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	store := CatalogStore{Dir: dir, Key: pub}
	archive, sig := signed(t, priv, fixtureEntries(100))
	installed, err := store.Seed(archive, sig)
	if err != nil || !installed {
		t.Fatalf("seeding over an empty directory: %v, %v", installed, err)
	}
	if serial, ok, err := store.Serial(); err != nil || !ok || serial != 100 {
		t.Fatalf("Serial = %d, %v, %v", serial, ok, err)
	}
}

func TestEmbeddedSnapshot_IsThePinnedArchiveAndVerifiesAgainstTheCatalogKey(t *testing.T) {
	archive, sig, err := EmbeddedSnapshot()
	if err != nil {
		t.Fatalf("%v — run `make catalog-snapshot` first", err)
	}
	serial, err := VerifyArchive(CatalogPublicKey, archive, sig)
	if err != nil {
		t.Fatalf("the embedded snapshot does not verify against the catalog key: %v", err)
	}
	pin, err := os.ReadFile(filepath.Join("..", "..", "scripts", "devenv", "catalog.pin"))
	if err != nil {
		t.Fatal(err)
	}
	var pinSerial, pinSum string
	for _, line := range strings.Split(string(pin), "\n") {
		if v, ok := strings.CutPrefix(line, "serial="); ok {
			pinSerial = v
		}
		if v, ok := strings.CutPrefix(line, "sha256="); ok {
			pinSum = v
		}
	}
	sum := sha256.Sum256(archive)
	if hex.EncodeToString(sum[:]) != pinSum {
		t.Fatalf("the embedded archive's SHA-256 is %x, the pin names %s", sum, pinSum)
	}
	if fmt.Sprint(serial) != pinSerial {
		t.Fatalf("the embedded archive's serial is %d, the pin names %s", serial, pinSerial)
	}
}
