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

func TestEnsureSambaCustomConf(t *testing.T) {
	g := NewGenerator(t.TempDir())
	path := filepath.Join(g.Root, PathSambaCustom)
	if err := g.EnsureSambaCustomConf(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("size = %d, want 0", info.Size())
	}
	if err := os.WriteFile(path, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := g.EnsureSambaCustomConf(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep\n" {
		t.Fatalf("clobbered existing custom conf: %q", got)
	}
}

func sambaSection(t *testing.T, conf, name string) string {
	t.Helper()
	_, rest, ok := strings.Cut(conf, "\n["+name+"]\n")
	if !ok {
		t.Fatalf("no [%s] section in:\n%s", name, conf)
	}
	section, _, _ := strings.Cut(rest, "\n[")
	section, _, _ = strings.Cut(section, "\ninclude = ")
	return section
}

func TestRenderSambaConf_AccessRestrictsNonGuestShares(t *testing.T) {
	conf := RenderSambaConf([]SambaShare{
		{Name: "rw", Access: &SambaAccess{ValidUsers: []string{"alice", "bob"}, WriteList: []string{"alice"}}},
		{Name: "ro", ReadOnly: true, Access: &SambaAccess{ValidUsers: []string{"alice"}, WriteList: []string{"alice"}}},
		{Name: "closed", Access: &SambaAccess{}},
		{Name: "guest", Guest: true, Access: &SambaAccess{ValidUsers: []string{"alice"}}},
		{Name: "unset"},
		{Name: "writer-not-valid", Access: &SambaAccess{ValidUsers: []string{"alice"}, WriteList: []string{"mallory"}}},
	})

	rw := sambaSection(t, conf, "rw")
	for _, want := range []string{"read only = yes", "valid users = alice bob", "write list = alice"} {
		if !strings.Contains(rw, want) {
			t.Errorf("[rw] lacks %q:\n%s", want, rw)
		}
	}
	if ro := sambaSection(t, conf, "ro"); strings.Contains(ro, "write list") || !strings.Contains(ro, "read only = yes") {
		t.Errorf("[ro] must stay read-only whatever its grants:\n%s", ro)
	}
	closed := sambaSection(t, conf, "closed")
	if !strings.Contains(closed, "available = no") || strings.Contains(closed, "valid users") || strings.Contains(closed, "read only = no") {
		t.Errorf("[closed] must be unavailable, and an empty list must never render as `valid users =`:\n%s", closed)
	}
	if guest := sambaSection(t, conf, "guest"); strings.Contains(guest, "valid users") || strings.Contains(guest, "available") || !strings.Contains(guest, "read only = no") || !strings.Contains(guest, "guest ok = yes") {
		t.Errorf("[guest] keeps its rendering:\n%s", guest)
	}
	if unset := sambaSection(t, conf, "unset"); strings.Contains(unset, "valid users") || !strings.Contains(unset, "read only = no") {
		t.Errorf("[unset] with no Access renders as before:\n%s", unset)
	}
	if w := sambaSection(t, conf, "writer-not-valid"); strings.Contains(w, "write list") {
		t.Errorf("a writer that is not in valid users must not be rendered:\n%s", w)
	}
}

func TestRenderSambaConf_AccessNamesCannotInjectConfig(t *testing.T) {
	bad := []string{
		"@staff", "+wheel", "&nis", "%U", "%S", "a%Ub", "eve, root", "eve,root",
		"x\nforce user = root", "x\r\n[evil]", "say \"hi\"", "it's", `back\slash`, "tab\tbed",
		"a;b", "a#b", "a=b", "a[b", "*", "a?b", "`id`", " lead", "trail ", "", "bad\xffutf8",
	}
	conf := RenderSambaConf([]SambaShare{{Name: "s", Access: &SambaAccess{
		ValidUsers: append([]string{"good", "two words", "j.doe-1_x", "jürgen"}, bad...),
		WriteList:  append([]string{"good"}, bad...),
	}}})
	section := sambaSection(t, conf, "s")
	if want := "   valid users = good \"two words\" j.doe-1_x \"jürgen\"\n"; !strings.Contains(section, want) {
		t.Errorf("section lacks %q:\n%s", want, section)
	}
	if want := "   write list = good\n"; !strings.Contains(section, want) {
		t.Errorf("section lacks %q:\n%s", want, section)
	}
	if strings.Count(conf, "\n[") != 1 || strings.Contains(conf, "force user") || strings.Contains(conf, "evil") {
		t.Errorf("a username injected configuration:\n%s", conf)
	}
	for _, name := range bad {
		if SambaUserListable(name) {
			t.Errorf("SambaUserListable(%q) = true", name)
		}
	}
	if !SambaUserListable("two words") || !SambaUserListable("good") {
		t.Error("ordinary names must be listable")
	}

	allBad := RenderSambaConf([]SambaShare{{Name: "s", Access: &SambaAccess{ValidUsers: bad, WriteList: bad}}})
	if section := sambaSection(t, allBad, "s"); !strings.Contains(section, "available = no") || strings.Contains(section, "valid users") {
		t.Errorf("a share whose every name is refused must be closed, not unrestricted:\n%s", section)
	}
}
