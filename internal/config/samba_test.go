package config

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/config/golden"
)

func TestRenderSambaConf(t *testing.T) {
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
		goldenPath := filepath.Join(dir, "smb.conf.golden")
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
			var state SambaState
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatalf("parsing state.json: %v", err)
			}
			got := RenderSambaConf(state.Shares)
			if !strings.HasSuffix(strings.TrimSpace(got), "include = "+SambaCustomInclude) {
				t.Fatalf("generated smb.conf must end with include = %s", SambaCustomInclude)
			}
			golden.Compare(t, goldenPath, []byte(got))
		})
	}
	if found == 0 {
		t.Fatal("no smb.conf.golden files under testdata/configs")
	}
}

func TestWriteSamba_RefusesUnmanaged(t *testing.T) {
	root := t.TempDir()
	g := NewGenerator(root)
	ctx := context.Background()
	file := File{Path: PathSamba, Command: "share", Body: []byte("[global]\n")}
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	if err := g.Write(ctx, file, 1, now); err != nil {
		t.Fatalf("seed Write: %v", err)
	}
	if err := g.KeepUnmanaged(ctx, PathSamba); err != nil {
		t.Fatalf("KeepUnmanaged: %v", err)
	}
	if err := g.WriteSamba(ctx, []SambaShare{{Name: "media"}}, "share", 2, now); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("WriteSamba on unmanaged = %v, want ErrUnmanaged", err)
	}
}

func TestWriteSamba_RefusesExistingHostFile(t *testing.T) {
	root := t.TempDir()
	g := NewGenerator(root)
	full := filepath.Join(root, PathSamba)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	original := "[media]\npath = /srv/media\n"
	if err := os.WriteFile(full, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	err := g.WriteSamba(context.Background(), []SambaShare{{Name: "media"}}, "share", 1, time.Now())
	if !errors.Is(err, ErrExistingHostFile) {
		t.Fatalf("WriteSamba on existing host file = %v, want ErrExistingHostFile", err)
	}
	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("WriteSamba changed the existing host file:\n%s", got)
	}
}
