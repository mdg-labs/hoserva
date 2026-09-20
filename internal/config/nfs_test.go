package config

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/config/golden"
)

func TestRenderNFSExports(t *testing.T) {
	cases, err := os.ReadDir(testdataDir)
	if err != nil {
		t.Fatalf("reading %s: %v", testdataDir, err)
	}

	found := 0
	for _, c := range cases {
		if !c.IsDir() {
			continue
		}
		dir := filepath.Join(testdataDir, c.Name())
		goldenPath := filepath.Join(dir, "exports.golden")
		if _, err := os.Stat(goldenPath); err != nil {
			continue
		}
		found++
		name := c.Name()
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
			if err != nil {
				t.Fatalf("reading state.json: %v", err)
			}
			var state NFSState
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatalf("parsing state.json: %v", err)
			}
			got := RenderNFSExports(state.Shares)
			golden.Compare(t, goldenPath, []byte(got))
		})
	}
	if found == 0 {
		t.Fatal("no exports.golden files under testdata/configs")
	}
}

func TestRenderNFSExports_NoShares(t *testing.T) {
	if got := RenderNFSExports(nil); got != "" {
		t.Fatalf("empty shares: %q", got)
	}
}

func TestWriteNFS_RefusesUnmanaged(t *testing.T) {
	root := t.TempDir()
	g := NewGenerator(root)
	ctx := context.Background()
	file := File{Path: PathNFS, Command: "share", Body: []byte("/mnt/user/media 127.0.0.1(rw,sync,no_subtree_check,root_squash)\n")}
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	if err := g.Write(ctx, file, 1, now); err != nil {
		t.Fatalf("seed Write: %v", err)
	}
	if err := g.KeepUnmanaged(ctx, PathNFS); err != nil {
		t.Fatalf("KeepUnmanaged: %v", err)
	}
	if err := g.WriteNFS(ctx, []NFSShare{{Name: "media", Hosts: []string{"127.0.0.1"}, Squash: "root_squash"}}, "share", 2, now); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("WriteNFS on unmanaged = %v, want ErrUnmanaged", err)
	}
}

func TestWriteNFS_RefusesExistingHostFile(t *testing.T) {
	root := t.TempDir()
	g := NewGenerator(root)
	full := filepath.Join(root, PathNFS)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	original := "/export/media *(ro,sync,no_subtree_check)\n"
	if err := os.WriteFile(full, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	err := g.WriteNFS(context.Background(), []NFSShare{{Name: "media", Hosts: []string{"127.0.0.1"}, Squash: "root_squash"}}, "share", 1, time.Now())
	if !errors.Is(err, ErrExistingHostFile) {
		t.Fatalf("WriteNFS on existing host file = %v, want ErrExistingHostFile", err)
	}
	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("WriteNFS changed the existing host file:\n%s", got)
	}
}
