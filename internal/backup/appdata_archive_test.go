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
)

func packTestTree(t *testing.T, dir string) (string, appdataHeader, appdataTrailer) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "a.tar.zst")
	hdr := appdataHeader{Container: "alpha", CreatedAt: time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC), Dirs: []string{dir}}
	trailer, err := packAppdata(context.Background(), dest, hdr)
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
	if _, err := packAppdata(ctx, dest, appdataHeader{Container: "alpha", CreatedAt: time.Now(), Dirs: []string{src}}); !errors.Is(err, context.Canceled) {
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
