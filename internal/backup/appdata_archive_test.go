package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
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

func TestAppdataArchive_VerifyRejectsATruncatedOrAlteredArchive(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "f"), bytes.Repeat([]byte("data"), 5000), 0o644); err != nil {
		t.Fatal(err)
	}
	archive, _, _ := packTestTree(t, src)
	full := readBytes(t, archive)

	truncated := filepath.Join(t.TempDir(), "t.tar.zst")
	if err := os.WriteFile(truncated, full[:len(full)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := verifyAppdata(truncated); err == nil {
		t.Fatal("a truncated archive verified")
	}
	flipped := append([]byte(nil), full...)
	flipped[len(flipped)/2] ^= 0xff
	altered := filepath.Join(t.TempDir(), "f.tar.zst")
	if err := os.WriteFile(altered, flipped, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := verifyAppdata(altered); err == nil {
		t.Fatal("an altered archive verified")
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
