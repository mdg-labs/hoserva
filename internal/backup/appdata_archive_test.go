package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"

	"github.com/mdg-labs/hoserva/internal/beneath"
)

// testAppdataKey authenticates the archives the archive-level tests pack.
var testAppdataKey = func() []byte {
	key, err := deriveAppdataKey("AGE-SECRET-KEY-TEST-IDENTITY")
	if err != nil {
		panic(err)
	}
	return key
}()

// extractAppdata unpacks into targets whose parents already exist, holding
// the parents only for the call.
func extractAppdata(ctx context.Context, archivePath string, hdr appdataHeader, targets []string) error {
	dirs := &heldDirs{}
	defer dirs.close()
	return dirs.extract(ctx, archivePath, hdr, targets, make([]liveIdentity, len(targets)), nil)
}

func packTestTree(t *testing.T, dir string) (string, appdataHeader, appdataTrailer) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "a.tar.zst")
	hdr := appdataHeader{Container: "alpha", CreatedAt: time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC), Dirs: []string{dir}}
	trailer, err := packAppdata(context.Background(), dest, hdr, testAppdataKey)
	if err != nil {
		t.Fatalf("packAppdata: %v", err)
	}
	return dest, hdr, trailer
}

func TestAppdataArchive_RoundTripKeepsContentModesLinksAndTimes(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(src, "nested", "deeper"), 0o750))
	must(os.WriteFile(filepath.Join(src, "a.txt"), []byte("alpha"), 0o640))
	must(os.WriteFile(filepath.Join(src, "nested", "deeper", "b.bin"), bytes.Repeat([]byte{7}, 100000), 0o600))
	must(os.WriteFile(filepath.Join(src, "empty"), nil, 0o644))
	must(os.Symlink("a.txt", filepath.Join(src, "link")))
	must(os.Symlink("/etc/hosts", filepath.Join(src, "absolute-link")))
	mtime := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	must(os.Chtimes(filepath.Join(src, "a.txt"), mtime, mtime))
	must(os.Chmod(filepath.Join(src, "nested"), 0o750))
	sock, err := net.Listen("unix", filepath.Join(src, "s.sock"))
	if err != nil {
		t.Skipf("cannot create a unix socket here: %v", err)
	}
	defer func() { _ = sock.Close() }()

	archive, hdr, trailer := packTestTree(t, src)
	if trailer.Files != 3 || trailer.Bytes != int64(len("alpha")+100000) || trailer.Skipped != 1 || trailer.Changed != 0 {
		t.Fatalf("trailer = %+v, want 3 files, the socket skipped and nothing changed", trailer)
	}
	gotHdr, gotTrailer, err := verifyAppdata(archive)
	if err != nil {
		t.Fatalf("verifyAppdata: %v", err)
	}
	if gotHdr.Container != "alpha" || gotTrailer != trailer {
		t.Fatalf("verified %+v %+v", gotHdr, gotTrailer)
	}

	dst := filepath.Join(t.TempDir(), "dst")
	if err := extractAppdata(context.Background(), archive, hdr, []string{dst}); err != nil {
		t.Fatalf("extractAppdata: %v", err)
	}
	if got := readFile(t, filepath.Join(dst, "a.txt")); got != "alpha" {
		t.Fatalf("a.txt = %q", got)
	}
	if got := readBytes(t, filepath.Join(dst, "nested", "deeper", "b.bin")); len(got) != 100000 || got[99999] != 7 {
		t.Fatalf("b.bin has %d bytes", len(got))
	}
	info, err := os.Lstat(filepath.Join(dst, "a.txt"))
	if err != nil || info.Mode().Perm() != 0o640 || !info.ModTime().Equal(mtime) {
		t.Fatalf("a.txt info = %v, %v; want mode 0640 and the original mtime", info, err)
	}
	if info, err := os.Stat(filepath.Join(dst, "nested")); err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("nested dir = %v, %v", info, err)
	}
	for link, want := range map[string]string{"link": "a.txt", "absolute-link": "/etc/hosts"} {
		got, err := os.Readlink(filepath.Join(dst, link))
		if err != nil || got != want {
			t.Fatalf("readlink %s = %q, %v; want %q", link, got, err, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(dst, "s.sock")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the socket was archived: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "empty")); err != nil {
		t.Fatalf("the empty file is missing: %v", err)
	}
}

// smallArchive packs a tree with every kind of entry extraction consumes,
// small enough that every bit of the result can be flipped in a test.
func smallArchive(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	mtime := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(src, "d"), 0o750))
	must(os.WriteFile(filepath.Join(src, "f"), []byte("hello world"), 0o644))
	must(os.WriteFile(filepath.Join(src, "d", "g"), bytes.Repeat([]byte("ab"), 30), 0o600))
	must(os.Symlink("f", filepath.Join(src, "l")))
	for _, p := range []string{"", "d", "f", "d/g"} {
		must(os.Chtimes(filepath.Join(src, p), mtime, mtime))
	}
	archive, _, _ := packTestTree(t, src)
	return archive
}

func decodeZstd(b []byte) ([]byte, error) {
	zr, err := zstd.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// Whatever happens to the bytes of an archive at rest, one that still
// verifies must decompress, without error, to exactly the tar stream that
// was written: extraction reads that stream and nothing else, so it then
// writes exactly what the original would have. A change that lands in
// bytes the compressor treats as redundant (a reserved bit of the frame
// header, an unused table entry) decodes to the same stream and is not an
// alteration of anything a restore uses.
func TestAppdataArchive_VerifyAcceptsOnlyBytesThatDecodeToTheSameArchive(t *testing.T) {
	archive := smallArchive(t)
	full := readBytes(t, archive)
	plain, err := decodeZstd(full)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := verifyAppdata(archive); err != nil {
		t.Fatalf("the unaltered archive does not verify: %v", err)
	}

	probe := filepath.Join(t.TempDir(), "probe.tar.zst")
	verifies := func(variant []byte) bool {
		t.Helper()
		if err := os.WriteFile(probe, variant, 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := verifyAppdata(probe)
		return err == nil
	}
	check := func(what string, variant []byte) (same bool) {
		t.Helper()
		if !verifies(variant) {
			return false
		}
		got, err := decodeZstd(variant)
		if err != nil || !bytes.Equal(got, plain) {
			t.Errorf("%s verified although it no longer decodes to the archive that was written (decode error: %v)", what, err)
			return false
		}
		return true
	}

	var ignored int
	for i := range full {
		for bit := range 8 {
			variant := append([]byte(nil), full...)
			variant[i] ^= 1 << bit
			if check(fmt.Sprintf("flipping bit %d of byte %d of %d", bit, i, len(full)), variant) {
				ignored++
			}
		}
	}
	t.Logf("%d of %d single-bit alterations of a %d-byte archive change nothing a restore reads", ignored, len(full)*8, len(full))

	for n := range full {
		check(fmt.Sprintf("truncating to %d of %d bytes", n, len(full)), full[:n])
	}
	check("a byte appended", append(append([]byte(nil), full...), 0))
	check("a zero byte appended twice", append(append([]byte(nil), full...), 0, 0))
	check("the archive appended to itself", append(append([]byte(nil), full...), full...))
	check("garbage appended", append(append([]byte(nil), full...), []byte("not a zstd frame")...))
}

type archivedEntry struct {
	hdr  *tar.Header
	body []byte
}

func readArchivedEntries(t *testing.T, archive string) []archivedEntry {
	t.Helper()
	in, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	zr, err := zstd.NewReader(in)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	var entries []archivedEntry
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return entries
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, archivedEntry{h, body})
	}
}

// writeArchivedEntries recompresses entries into a valid tar.zst, so the
// zstd frame's own checksum is correct and only the trailer can notice.
func writeArchivedEntries(t *testing.T, entries []archivedEntry) string {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		e.hdr.Size = int64(len(e.body))
		if err := tw.WriteHeader(e.hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "forged.tar.zst")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Each case is a well-formed archive that differs from the packed one in
// one thing extraction writes. Only the trailer's checksum of the tar
// stream can tell.
func TestAppdataArchive_VerifyRejectsEveryAlterationOfWhatExtractionWrites(t *testing.T) {
	archive := smallArchive(t)
	find := func(entries []archivedEntry, name string) *archivedEntry {
		t.Helper()
		for i := range entries {
			if strings.HasSuffix(entries[i].hdr.Name, name) {
				return &entries[i]
			}
		}
		t.Fatalf("no entry named %s", name)
		return nil
	}
	rewriteMeta := func(e *archivedEntry, edit func(*appdataHeader)) {
		var h appdataHeader
		if err := json.Unmarshal(e.body, &h); err != nil {
			t.Fatal(err)
		}
		edit(&h)
		body, err := json.Marshal(h)
		if err != nil {
			t.Fatal(err)
		}
		e.body = body
	}
	cases := []struct {
		name   string
		mutate func(e []archivedEntry) []archivedEntry
	}{
		{"content of the same size", func(e []archivedEntry) []archivedEntry {
			find(e, "/f").body = []byte("HELLO WORLD")
			return e
		}},
		{"content of another size", func(e []archivedEntry) []archivedEntry {
			find(e, "/f").body = []byte("hello")
			return e
		}},
		{"a file's mode", func(e []archivedEntry) []archivedEntry {
			find(e, "/f").hdr.Mode = 0o777
			return e
		}},
		{"a directory's mode", func(e []archivedEntry) []archivedEntry {
			find(e, "/d/").hdr.Mode = 0o777
			return e
		}},
		{"a file's owner", func(e []archivedEntry) []archivedEntry {
			find(e, "/f").hdr.Uid += 1
			return e
		}},
		{"a file's group", func(e []archivedEntry) []archivedEntry {
			find(e, "/f").hdr.Gid += 1
			return e
		}},
		{"a file's modification time", func(e []archivedEntry) []archivedEntry {
			f := find(e, "/f")
			f.hdr.ModTime = f.hdr.ModTime.Add(time.Hour)
			return e
		}},
		{"a file's name", func(e []archivedEntry) []archivedEntry {
			find(e, "/f").hdr.Name = "data/0/other"
			return e
		}},
		{"a symbolic link's target", func(e []archivedEntry) []archivedEntry {
			find(e, "/l").hdr.Linkname = "/etc/shadow"
			return e
		}},
		{"a symbolic link made a file", func(e []archivedEntry) []archivedEntry {
			l := find(e, "/l")
			l.hdr.Typeflag, l.hdr.Linkname = tar.TypeReg, ""
			return e
		}},
		{"a symbolic link added", func(e []archivedEntry) []archivedEntry {
			extra := archivedEntry{hdr: &tar.Header{Name: "data/0/x", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777}}
			return append(e[:len(e)-1], extra, e[len(e)-1])
		}},
		{"a directory added", func(e []archivedEntry) []archivedEntry {
			extra := archivedEntry{hdr: &tar.Header{Name: "data/0/x/", Typeflag: tar.TypeDir, Mode: 0o755}}
			return append(e[:len(e)-1], extra, e[len(e)-1])
		}},
		{"an empty file added", func(e []archivedEntry) []archivedEntry {
			extra := archivedEntry{hdr: &tar.Header{Name: "data/0/x", Typeflag: tar.TypeReg, Mode: 0o644}}
			return append(e[:len(e)-1], extra, e[len(e)-1])
		}},
		{"a symbolic link removed", func(e []archivedEntry) []archivedEntry {
			l := find(e, "/l")
			return slices.DeleteFunc(e, func(x archivedEntry) bool { return x.hdr == l.hdr })
		}},
		{"a directory removed", func(e []archivedEntry) []archivedEntry {
			d := find(e, "/d/")
			return slices.DeleteFunc(e, func(x archivedEntry) bool { return x.hdr == d.hdr })
		}},
		{"two entries swapped", func(e []archivedEntry) []archivedEntry {
			e[2], e[3] = e[3], e[2]
			return e
		}},
		{"the directory an archived tree is restored to", func(e []archivedEntry) []archivedEntry {
			rewriteMeta(&e[0], func(h *appdataHeader) { h.Dirs = []string{"/srv/elsewhere"} })
			return e
		}},
		{"the container the archive is for", func(e []archivedEntry) []archivedEntry {
			rewriteMeta(&e[0], func(h *appdataHeader) { h.Container = "beta" })
			return e
		}},
	}

	if _, _, err := verifyAppdata(writeArchivedEntries(t, readArchivedEntries(t, archive))); err != nil {
		t.Fatalf("an archive rewritten without any change does not verify, so the cases below prove nothing: %v", err)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			forged := writeArchivedEntries(t, c.mutate(readArchivedEntries(t, archive)))
			_, _, err := verifyAppdata(forged)
			if err == nil || !strings.Contains(err.Error(), "does not match its trailer") {
				t.Fatalf("verifyAppdata = %v, want a trailer mismatch", err)
			}
		})
	}
}

func TestAppdataArchive_VerifyRejectsAContentChangeThatKeepsTheStructure(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	archive, hdr, _ := packTestTree(t, src)

	// Rewrite the archive with the same header and trailer but different
	// file content: zstd and tar are both valid, only the trailer's
	// checksum can catch it.
	in, _ := os.Open(archive)
	defer func() { _ = in.Close() }()
	zr, _ := zstd.NewReader(in)
	defer zr.Close()
	tr := tar.NewReader(zr)
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(tr)
		if strings.HasSuffix(h.Name, "/f") {
			body = bytes.NewBufferString("tampered")
		}
		h.Size = int64(body.Len())
		_ = tw.WriteHeader(h)
		_, _ = tw.Write(body.Bytes())
	}
	_ = tw.Close()
	_ = zw.Close()
	forged := filepath.Join(t.TempDir(), "forged.tar.zst")
	if err := os.WriteFile(forged, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = hdr
	_, _, err := verifyAppdata(forged)
	if err == nil || !strings.Contains(err.Error(), "does not match its trailer") {
		t.Fatalf("verifyAppdata = %v, want a trailer mismatch", err)
	}
}

// craftedArchive builds a tar.zst with a valid header and trailer around
// the given raw entries, for the extraction-safety tests.
func craftedArchive(t *testing.T, entries []*tar.Header, bodies map[string]string) (string, appdataHeader) {
	t.Helper()
	hdr := appdataHeader{Version: appdataFormatVersion, Container: "alpha", Dirs: []string{"/x"}}
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := writeAppdataMeta(tw, appdataHeaderName, hdr, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		body := bodies[e.Name]
		e.Size = int64(len(body))
		if err := tw.WriteHeader(e); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	if err := writeAppdataMeta(tw, appdataTrailerName, appdataTrailer{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = zw.Close()
	path := filepath.Join(t.TempDir(), "crafted.tar.zst")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, hdr
}

func TestAppdataExtract_RefusesEntriesThatEscapeTheirDirectory(t *testing.T) {
	victim := t.TempDir()
	tests := []struct {
		name    string
		entries []*tar.Header
		bodies  map[string]string
	}{
		{
			name:    "dot-dot path",
			entries: []*tar.Header{{Name: "data/0/../../escape", Typeflag: tar.TypeReg, Mode: 0o644}},
			bodies:  map[string]string{"data/0/../../escape": "x"},
		},
		{
			name:    "absolute path",
			entries: []*tar.Header{{Name: "data/0//etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644}},
			bodies:  map[string]string{"data/0//etc/passwd": "x"},
		},
		{
			name: "file written through a symlink the archive created",
			entries: []*tar.Header{
				{Name: "data/0/link", Typeflag: tar.TypeSymlink, Linkname: victim, Mode: 0o777},
				{Name: "data/0/link/pwned", Typeflag: tar.TypeReg, Mode: 0o644},
			},
			bodies: map[string]string{"data/0/link/pwned": "x"},
		},
		{
			name:    "another tree's prefix",
			entries: []*tar.Header{{Name: "data/1/f", Typeflag: tar.TypeReg, Mode: 0o644}},
			bodies:  map[string]string{"data/1/f": "x"},
		},
		{
			name:    "a device node",
			entries: []*tar.Header{{Name: "data/0/dev", Typeflag: tar.TypeChar, Mode: 0o644}},
		},
		{
			name:    "a hard link",
			entries: []*tar.Header{{Name: "data/0/hl", Typeflag: tar.TypeLink, Linkname: "/etc/passwd", Mode: 0o644}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			archive, hdr := craftedArchive(t, tc.entries, tc.bodies)
			parent := t.TempDir()
			target := filepath.Join(parent, "tree")
			if err := extractAppdata(context.Background(), archive, hdr, []string{target}); err == nil {
				t.Fatal("extractAppdata accepted the entry")
			}
			if left, _ := os.ReadDir(victim); len(left) != 0 {
				t.Fatalf("a file was written outside the target: %v", left)
			}
			for _, escaped := range []string{filepath.Join(parent, "escape"), "/etc/passwd.hoserva"} {
				if _, err := os.Lstat(escaped); err == nil {
					t.Fatalf("%s was created", escaped)
				}
			}
		})
	}
}

func TestAppdataPack_CancelStopsTheCopyAndLeavesNoArchive(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dest := filepath.Join(t.TempDir(), "a.tar.zst")
	if _, err := packAppdata(ctx, dest, appdataHeader{Container: "alpha", CreatedAt: time.Now(), Dirs: []string{src}}, testAppdataKey); !errors.Is(err, context.Canceled) {
		t.Fatalf("packAppdata = %v, want context.Canceled", err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(dest)); len(entries) != 0 {
		t.Fatalf("a cancelled pack left %v behind", entries)
	}
}

func TestIsDatabaseImage(t *testing.T) {
	yes := []string{"postgres", "postgres:16", "docker.io/library/postgres", "lscr.io/linuxserver/mariadb", "bitnami/postgresql:16.2", "MySQL", "redis@sha256:abc", "ghcr.io/x/mongo", "registry.example.com:5000/team/influxdb:2"}
	no := []string{"nginx", "postgres-exporter", "linuxserver/sonarr", "myredis", "pgadmin4", "", "library/postgres-backup"}
	for _, image := range yes {
		if !IsDatabaseImage(image) {
			t.Errorf("IsDatabaseImage(%q) = false", image)
		}
	}
	for _, image := range no {
		if IsDatabaseImage(image) {
			t.Errorf("IsDatabaseImage(%q) = true", image)
		}
	}
}

func TestCopyExactly_ReportsAFileThatShrankOrGrew(t *testing.T) {
	var out bytes.Buffer
	short, err := copyExactly(&out, strings.NewReader("abcd"), 10)
	if err != nil || !short {
		t.Fatalf("copyExactly = %v, %v; want short and no error", short, err)
	}
	if got := out.String(); got != "abcd\x00\x00\x00\x00\x00\x00" {
		t.Fatalf("output = %q, want the 4 bytes read then zero padding to 10", got)
	}
	out.Reset()
	if changed, err := copyExactly(&out, strings.NewReader("abcdefgh"), 4); err != nil || !changed || out.String() != "abcd" {
		t.Fatalf("a file that grew: %q, changed=%v, err=%v; want its first 4 bytes and changed", out.String(), changed, err)
	}
	out.Reset()
	if changed, err := copyExactly(&out, strings.NewReader("abcd"), 4); err != nil || changed || out.String() != "abcd" {
		t.Fatalf("an unchanged file: %q, changed=%v, err=%v", out.String(), changed, err)
	}
}

func TestAppdataExtract_AppliesDirectoryMetadataBeneathTheTreeNotThroughASwappedDirectory(t *testing.T) {
	outside := t.TempDir()
	if err := os.Chmod(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(outside)
	if err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	archive, hdr := craftedArchive(t, []*tar.Header{
		{Name: "data/0/", Typeflag: tar.TypeDir, Mode: 0o700, ModTime: mtime},
		{Name: "data/0/sub/", Typeflag: tar.TypeDir, Mode: 0o500, ModTime: mtime},
	}, nil)
	target := filepath.Join(t.TempDir(), "tree")
	beforeAppdataMeta = func() {
		sub := filepath.Join(target, "sub")
		if err := os.Rename(sub, sub+".moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, sub); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeAppdataMeta = nil })

	if err := extractAppdata(context.Background(), archive, hdr, []string{target}); !errors.Is(err, beneath.ErrSymlink) {
		t.Fatalf("extractAppdata = %v, want it to refuse the directory replaced by a link", err)
	}
	after, err := os.Lstat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("the directory outside the tree was changed: mode %v -> %v, mtime %v -> %v", before.Mode(), after.Mode(), before.ModTime(), after.ModTime())
	}
}

func TestAppdataExtract_AppliesModesToADirectoryMadeThroughALinkInsideTheTree(t *testing.T) {
	mtime := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	archive, hdr := craftedArchive(t, []*tar.Header{
		{Name: "data/0/", Typeflag: tar.TypeDir, Mode: 0o750, ModTime: mtime},
		{Name: "data/0/d/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: mtime},
		{Name: "data/0/d/e/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: mtime},
		{Name: "data/0/d/e/up", Typeflag: tar.TypeSymlink, Linkname: "..", Mode: 0o777, ModTime: mtime},
		{Name: "data/0/d/e/up/sub/", Typeflag: tar.TypeDir, Mode: 0o500, ModTime: mtime},
		{Name: "data/0/d/e/up/sub/f", Typeflag: tar.TypeReg, Mode: 0o640, ModTime: mtime},
	}, map[string]string{"data/0/d/e/up/sub/f": "x"})
	target := filepath.Join(t.TempDir(), "tree")
	if err := extractAppdata(context.Background(), archive, hdr, []string{target}); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(target, "d", "sub")
	t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })
	info, err := os.Lstat(sub)
	if err != nil || info.Mode().Perm() != 0o500 || !info.ModTime().Equal(mtime) {
		t.Fatalf("d/sub = %v, %v, want a 0500 directory with the archived time", info, err)
	}
	if got := readFile(t, filepath.Join(sub, "f")); got != "x" {
		t.Fatalf("d/sub/f = %q", got)
	}
	if info, err := os.Lstat(target); err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("the tree's own mode = %v, %v, want 0750", info, err)
	}
}

func TestAppdataArchive_RecordsTheIdentityOfEveryEntryItArchives(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a.txt", "nested/b.txt"} {
		if err := os.WriteFile(filepath.Join(src, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("a.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(src, "a.txt"), filepath.Join(src, "hard")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(src, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}

	archived := archivedIDs{}
	dest := filepath.Join(t.TempDir(), "a.tar.zst")
	hdr := appdataHeader{Container: "alpha", CreatedAt: time.Now(), Dirs: []string{src}}
	if _, err := packAppdataRecording(context.Background(), dest, hdr, testAppdataKey, archived); err != nil {
		t.Fatal(err)
	}
	var want []devIno
	for _, rel := range []string{".", "a.txt", "hard", "nested", "nested/b.txt", "link", "pipe"} {
		var st unix.Stat_t
		if err := unix.Lstat(filepath.Join(src, rel), &st); err != nil {
			t.Fatal(err)
		}
		id := devIno{uint64(st.Dev), uint64(st.Ino)}
		if _, ok := archived[src][id]; !ok {
			t.Errorf("%s (%v) was archived and not recorded", rel, id)
		}
		want = append(want, id)
	}
	// a.txt and hard are one inode, so six identities cover the seven names.
	if got := len(archived[src]); got != len(want)-1 {
		t.Fatalf("recorded %d identities, want %d", got, len(want)-1)
	}
}

func TestAppdataArchive_RecordsNothingForAnEntryItDoesNotArchive(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	archived := archivedIDs{}
	dest := filepath.Join(t.TempDir(), "a.tar.zst")
	hdr := appdataHeader{Container: "alpha", CreatedAt: time.Now(), Dirs: []string{src}}
	if _, err := packAppdataRecording(context.Background(), dest, hdr, testAppdataKey, archived); err != nil {
		t.Fatal(err)
	}
	late := filepath.Join(src, "late")
	if err := os.WriteFile(late, []byte("added after packing"), 0o644); err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Lstat(late, &st); err != nil {
		t.Fatal(err)
	}
	if _, ok := archived[src][devIno{uint64(st.Dev), uint64(st.Ino)}]; ok {
		t.Fatal("a file created after packing is recorded")
	}
}

// swapOutside builds an appdata tree and a directory outside it that holds a
// file only root could read, and returns both.
func swapOutside(t *testing.T) (src, outside string) {
	t.Helper()
	base := t.TempDir()
	src = filepath.Join(base, "src")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(src, "sub"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for p, content := range map[string]string{
		filepath.Join(src, "sub", "inside"):    "inside content",
		filepath.Join(src, "file"):             "file content",
		filepath.Join(outside, "secret"):       "outside secret content",
		filepath.Join(outside, "other-secret"): "other outside secret content",
	} {
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return src, outside
}

// swapOnOpen makes the walk replace rel, once, with a link to target just
// before the packer opens it, which is the moment a container that can
// write the tree would do it.
func swapOnOpen(t *testing.T, src, rel, target string) {
	t.Helper()
	var swapped bool
	beforeOpen = func(got string) {
		if got != rel || swapped {
			return
		}
		swapped = true
		p := filepath.Join(src, rel)
		if err := os.RemoveAll(p); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeOpen = nil })
}

func requireNothingFromOutside(t *testing.T, archive string, archived archivedIDs, src, outside string) {
	t.Helper()
	for _, e := range readArchivedEntries(t, archive) {
		if strings.Contains(string(e.body), "secret") {
			t.Errorf("entry %q carries content from outside the tree: %q", e.hdr.Name, e.body)
		}
		if strings.Contains(e.hdr.Name, "secret") {
			t.Errorf("entry %q names a file from outside the tree", e.hdr.Name)
		}
	}
	for _, rel := range []string{".", "secret", "other-secret"} {
		var st unix.Stat_t
		if err := unix.Lstat(filepath.Join(outside, rel), &st); err != nil {
			t.Fatal(err)
		}
		if _, ok := archived[src][devIno{uint64(st.Dev), uint64(st.Ino)}]; ok {
			t.Errorf("the identity of outside/%s is recorded as archived", rel)
		}
	}
}

func TestAppdataArchive_ADirectorySwappedForALinkIsNotFollowed(t *testing.T) {
	src, outside := swapOutside(t)
	swapOnOpen(t, src, "sub", outside)

	archived := archivedIDs{}
	dest := filepath.Join(t.TempDir(), "a.tar.zst")
	hdr := appdataHeader{Container: "alpha", CreatedAt: time.Now(), Dirs: []string{src}}
	trailer, err := packAppdataRecording(context.Background(), dest, hdr, testAppdataKey, archived)
	if err != nil {
		t.Fatal(err)
	}
	requireNothingFromOutside(t, dest, archived, src, outside)
	if trailer.Changed == 0 {
		t.Errorf("trailer = %+v, want the swapped directory counted as changed", trailer)
	}
	for _, e := range readArchivedEntries(t, dest) {
		if e.hdr.Name == "data/0/sub/inside" {
			t.Errorf("the swapped directory's former content is archived under %q", e.hdr.Name)
		}
	}
}

func TestAppdataArchive_AFileSwappedForALinkIsNotFollowed(t *testing.T) {
	src, outside := swapOutside(t)
	swapOnOpen(t, src, "file", filepath.Join(outside, "secret"))

	archived := archivedIDs{}
	dest := filepath.Join(t.TempDir(), "a.tar.zst")
	hdr := appdataHeader{Container: "alpha", CreatedAt: time.Now(), Dirs: []string{src}}
	trailer, err := packAppdataRecording(context.Background(), dest, hdr, testAppdataKey, archived)
	if err != nil {
		t.Fatal(err)
	}
	requireNothingFromOutside(t, dest, archived, src, outside)
	if trailer.Changed == 0 {
		t.Errorf("trailer = %+v, want the swapped file counted as changed", trailer)
	}
	for _, e := range readArchivedEntries(t, dest) {
		if e.hdr.Name == "data/0/file" && e.hdr.Typeflag != tar.TypeSymlink {
			t.Errorf("data/0/file is archived as type %q with %q", e.hdr.Typeflag, e.body)
		}
	}
}

func TestAppdataArchive_AFileSwappedForADirectoryIsCountedAsChangedAndNotRecorded(t *testing.T) {
	src, _ := swapOutside(t)
	swapped := filepath.Join(src, "file")
	var swappedID devIno
	beforeOpen = func(got string) {
		if got != "file" || swappedID != (devIno{}) {
			return
		}
		if err := os.Remove(swapped); err != nil {
			t.Error(err)
		}
		if err := os.Mkdir(swapped, 0o755); err != nil {
			t.Error(err)
		}
		var st unix.Stat_t
		if err := unix.Lstat(swapped, &st); err != nil {
			t.Error(err)
		}
		swappedID = devIno{uint64(st.Dev), uint64(st.Ino)}
	}
	t.Cleanup(func() { beforeOpen = nil })

	archived := archivedIDs{}
	dest := filepath.Join(t.TempDir(), "a.tar.zst")
	hdr := appdataHeader{Container: "alpha", CreatedAt: time.Now(), Dirs: []string{src}}
	trailer, err := packAppdataRecording(context.Background(), dest, hdr, testAppdataKey, archived)
	if err != nil {
		t.Fatal(err)
	}
	if swappedID == (devIno{}) {
		t.Fatal("the file was never swapped")
	}
	if trailer.Changed == 0 || trailer.Skipped != 0 {
		t.Errorf("trailer = %+v, want the swapped-in directory counted as changed, not skipped", trailer)
	}
	if _, ok := archived[src][swappedID]; ok {
		t.Error("the swapped-in directory is recorded as archived")
	}
	for _, e := range readArchivedEntries(t, dest) {
		if strings.TrimSuffix(e.hdr.Name, "/") == "data/0/file" {
			t.Errorf("the swapped-in directory is archived as %q", e.hdr.Name)
		}
	}
}

func TestAppdataArchive_ADirectoryHoldingALinkBelowTheRootIsNotFollowed(t *testing.T) {
	src, outside := swapOutside(t)
	if err := os.MkdirAll(filepath.Join(src, "deep", "er"), 0o755); err != nil {
		t.Fatal(err)
	}
	swapOnOpen(t, src, "deep/er", outside)

	archived := archivedIDs{}
	dest := filepath.Join(t.TempDir(), "a.tar.zst")
	hdr := appdataHeader{Container: "alpha", CreatedAt: time.Now(), Dirs: []string{src}}
	if _, err := packAppdataRecording(context.Background(), dest, hdr, testAppdataKey, archived); err != nil {
		t.Fatal(err)
	}
	requireNothingFromOutside(t, dest, archived, src, outside)
}

func TestAppdataArchive_AnAppdataDirectoryThatIsALinkIsRefused(t *testing.T) {
	src, outside := swapOutside(t)
	link := filepath.Join(filepath.Dir(src), "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "a.tar.zst")
	hdr := appdataHeader{Container: "alpha", CreatedAt: time.Now(), Dirs: []string{link}}
	if _, err := packAppdataRecording(context.Background(), dest, hdr, testAppdataKey, nil); !errors.Is(err, beneath.ErrSymlink) {
		t.Fatalf("packing a link = %v, want an error wrapping ErrSymlink", err)
	}
}

func TestAppdataArchive_ADirectoryRemovedBeforeItIsListedDoesNotEndTheArchive(t *testing.T) {
	src, _ := swapOutside(t)
	if err := os.MkdirAll(filepath.Join(src, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a", "gone"), []byte("gone content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "z"), []byte("z content"), 0o600); err != nil {
		t.Fatal(err)
	}
	var removed bool
	beforeList = func(rel string) {
		if rel != "a" || removed {
			return
		}
		removed = true
		if err := os.RemoveAll(filepath.Join(src, "a")); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeList = nil })

	dest := filepath.Join(t.TempDir(), "a.tar.zst")
	hdr := appdataHeader{Container: "alpha", CreatedAt: time.Now(), Dirs: []string{src}}
	trailer, err := packAppdataRecording(context.Background(), dest, hdr, testAppdataKey, archivedIDs{})
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("the walk never listed the directory")
	}
	if trailer.Changed == 0 {
		t.Errorf("trailer = %+v, want the vanished directory counted as changed", trailer)
	}
	names := map[string]bool{}
	for _, e := range readArchivedEntries(t, dest) {
		names[e.hdr.Name] = true
	}
	for _, want := range []string{"data/0/file", "data/0/z"} {
		if !names[want] {
			t.Errorf("%s is missing from the archive: the walk stopped at the vanished directory (entries %v)", want, names)
		}
	}
}

func TestAppdataArchive_TrailerTagIsGivenByItsKeyAndNoOther(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	archive, _, _ := packTestTree(t, src)
	_, trailer, err := verifyAppdata(archive)
	if err != nil {
		t.Fatal(err)
	}
	if !trailer.authentic(testAppdataKey) {
		t.Fatal("the tag of a packed archive is not authentic under the key it was packed with")
	}
	other, err := deriveAppdataKey("AGE-SECRET-KEY-ANOTHER-IDENTITY")
	if err != nil {
		t.Fatal(err)
	}
	if trailer.authentic(other) || trailer.authentic(nil) {
		t.Fatal("the tag is authentic under another key, or under none")
	}
	for name, edit := range map[string]func(*appdataTrailer){
		"files":   func(x *appdataTrailer) { x.Files++ },
		"bytes":   func(x *appdataTrailer) { x.Bytes++ },
		"skipped": func(x *appdataTrailer) { x.Skipped++ },
		"changed": func(x *appdataTrailer) { x.Changed++ },
		"sha256":  func(x *appdataTrailer) { x.SHA256 = strings.Repeat("0", 64) },
		"removed": func(x *appdataTrailer) { x.MAC = "" },
		"garbled": func(x *appdataTrailer) { x.MAC = "zz" },
	} {
		altered := trailer
		edit(&altered)
		if altered.authentic(testAppdataKey) {
			t.Errorf("a trailer with its %s altered is still authentic", name)
		}
	}
}

func TestAppdataArchive_PackRefusesToWriteWithoutAKey(t *testing.T) {
	src := t.TempDir()
	dest := filepath.Join(t.TempDir(), "a.tar.zst")
	if _, err := packAppdata(context.Background(), dest, appdataHeader{Container: "alpha", CreatedAt: time.Now(), Dirs: []string{src}}, nil); err == nil {
		t.Fatal("packAppdata without a key succeeded")
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an archive was written without a key: %v", err)
	}
	if _, err := deriveAppdataKey(""); err == nil {
		t.Fatal("a key was derived from an empty identity")
	}
}

func TestAppdataArchive_RestoredModeLeavesOffSetuidAndSetgidOfRootOnly(t *testing.T) {
	tests := []struct {
		name     string
		mode     int64
		uid, gid int
		want     uint32
		stripped string
	}{
		{"root setuid", 0o4755, 0, 5, 0o755, "setuid"},
		{"root setgid", 0o2755, 5, 0, 0o755, "setgid"},
		{"root both", 0o6755, 0, 0, 0o755, "setuid and setgid"},
		{"user setuid", 0o4755, 5, 5, 0o4755, ""},
		{"user setgid", 0o2755, 5, 5, 0o2755, ""},
		{"root-owned but group not root keeps setgid", 0o6755, 0, 5, 0o2755, "setuid"},
		{"sticky kept", 0o1777, 0, 0, 0o1777, ""},
		{"root plain", 0o644, 0, 0, 0o644, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, stripped := restoredMode(&tar.Header{Typeflag: tar.TypeReg, Mode: tc.mode, Uid: tc.uid, Gid: tc.gid})
			if got != tc.want || stripped != tc.stripped {
				t.Fatalf("restoredMode = %#o, %q; want %#o, %q", got, stripped, tc.want, tc.stripped)
			}
		})
	}
}
