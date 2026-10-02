package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

func TestUsePrivateTempDir_CreatesItPrivateEmptiesItAndPointsTMPDIRAtIt(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	state := t.TempDir()
	dir := filepath.Join(state, "tmp")
	if err := os.MkdirAll(filepath.Join(dir, "left-by-a-crash"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "left-by-a-crash", "spill"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "multipart-123"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := usePrivateTempDir(state); err != nil {
		t.Fatalf("usePrivateTempDir: %v", err)
	}

	if got := os.TempDir(); got != dir {
		t.Errorf("os.TempDir() = %q, want %q", got, dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("private temp directory mode = %o, want 700", info.Mode().Perm())
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("private temp directory holds %v (%v), want it emptied", entries, err)
	}
}

func TestUsePrivateTempDir_CreatesAMissingDirectoryAndResolvesARelativeStateDir(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Chdir(t.TempDir())
	if err := usePrivateTempDir("./state"); err != nil {
		t.Fatalf("usePrivateTempDir: %v", err)
	}
	if got := os.TempDir(); !filepath.IsAbs(got) || filepath.Base(got) != "tmp" || filepath.Base(filepath.Dir(got)) != "state" {
		t.Errorf("os.TempDir() = %q, want an absolute <state>/tmp", got)
	}
}

// A directory that cannot be prepared stops the daemon: the temp directory is
// left as it was, never repointed, so nothing falls back to the system /tmp
// silently.
func TestUsePrivateTempDir_RefusesWhenItCannotPrepareTheDirectory(t *testing.T) {
	system := t.TempDir()
	t.Setenv("TMPDIR", system)

	asFile := t.TempDir()
	if err := os.WriteFile(filepath.Join(asFile, "tmp"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	asLink := t.TempDir()
	if err := os.Symlink(system, filepath.Join(asLink, "tmp")); err != nil {
		t.Fatal(err)
	}
	stateIsFile := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(stateIsFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	for name, state := range map[string]string{"tmp is a file": asFile, "tmp is a symlink": asLink, "state dir is a file": stateIsFile} {
		if err := usePrivateTempDir(state); err == nil {
			t.Errorf("%s: usePrivateTempDir succeeded, want an error", name)
		}
		if got := os.TempDir(); got != system {
			t.Errorf("%s: os.TempDir() = %q after a failure, want it unchanged (%q)", name, got, system)
		}
	}
}

// run() makes the private directory before the database is opened and returns
// its error rather than going on.
func TestRun_PreparesThePrivateTempDirectoryBeforeAnythingElseAndStopsOnFailure(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var run *ast.FuncDecl
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "run" {
			run = fd
		}
	}
	if run == nil {
		t.Fatal("run not found")
	}
	var prepared, opened token.Pos
	stopsOnFailure := false
	ast.Inspect(run.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.IfStmt:
			init, ok := n.Init.(*ast.AssignStmt)
			if !ok || len(init.Rhs) != 1 {
				return true
			}
			if call, ok := init.Rhs[0].(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "usePrivateTempDir" {
					prepared = n.Pos()
					if len(n.Body.List) == 1 {
						if ret, ok := n.Body.List[0].(*ast.ReturnStmt); ok && len(ret.Results) == 1 {
							stopsOnFailure = true
						}
					}
				}
			}
		case *ast.CallExpr:
			if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "openDatabase" && opened == 0 {
				opened = n.Pos()
			}
		}
		return true
	})
	if prepared == 0 || opened == 0 || prepared > opened {
		t.Errorf("run must call usePrivateTempDir (at %d) before openDatabase (at %d)", prepared, opened)
	}
	if !stopsOnFailure {
		t.Error("run must return the error from usePrivateTempDir")
	}
}

// A multipart part past the generated server's memory limit is spilled by
// net/http while the handler has not run yet. Through the daemon's own unix
// server, that file is under the private temp directory and nothing reaches
// the system one.
func TestPrivateTempDir_ALargeMultipartUploadSpillsOnlyIntoThePrivateDirectory(t *testing.T) {
	system := t.TempDir()
	t.Setenv("TMPDIR", system)
	w := newContainersWiringHarness(t)
	if err := wireMigration(context.Background(), w.handler, w.registry, disk.NewFakeProvider(), disk.NewFakeReadOnlyMounter(), store.NewMigrationSessionStore(w.db), w.root); err != nil {
		t.Fatal(err)
	}
	if err := usePrivateTempDir(w.root); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(w.root, "tmp")

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	req, err := http.NewRequest(http.MethodPost, "http://unix"+apiPathPrefix+"/migrate/scan", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	done := make(chan error, 1)
	go func() {
		resp, err := w.client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()

	part, err := mw.CreateFormFile("file", "boot.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(make([]byte, 33<<20)); err != nil {
		t.Fatal(err)
	}

	var spilled []os.DirEntry
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if spilled, _ = os.ReadDir(private); len(spilled) > 0 {
			break
		}
	}
	if len(spilled) == 0 {
		t.Errorf("no spill file under %s while the upload was in flight", private)
	} else if !strings.HasPrefix(spilled[0].Name(), "multipart-") {
		t.Errorf("spill file %q, want a multipart-* file", spilled[0].Name())
	}
	if leaked, _ := os.ReadDir(system); len(leaked) != 0 {
		t.Errorf("the system temp directory holds %v, want nothing", leaked)
	}

	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("upload: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("upload did not finish")
	}
}
