package template

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// iconless is the jellyfin fixture without its icon line.
func iconless(t *testing.T) string {
	t.Helper()
	src := strings.Replace(readFixture(t), "  icon: icon.svg\n", "", 1)
	if strings.Contains(src, "icon:") {
		t.Fatal("the fixture's icon line changed")
	}
	return src
}

func TestATemplateWithoutAnIconLintsCleanAndOneNamingAMissingFileDoesNot(t *testing.T) {
	src := iconless(t)
	dir := catalogFrom(t, "jellyfin", src)
	if err := os.Remove(filepath.Join(dir, "jellyfin", "icon.svg")); err != nil {
		t.Fatal(err)
	}
	if got := lintStrings(t, dir); len(got) != 0 {
		t.Fatalf("a template without an icon must lint clean, got:\n%s", strings.Join(got, "\n"))
	}

	dir = catalogFrom(t, "jellyfin", readFixture(t))
	if err := os.Remove(filepath.Join(dir, "jellyfin", "icon.svg")); err != nil {
		t.Fatal(err)
	}
	if got := lintStrings(t, dir); len(got) != 1 || !strings.Contains(got[0], `names "icon.svg", which is not a file`) {
		t.Errorf("a named icon that is missing: %q", got)
	}

	empty := strings.Replace(readFixture(t), "  icon: icon.svg\n", "  icon: \"\"\n", 1)
	if got := lintStrings(t, catalogFrom(t, "jellyfin", empty)); len(got) == 0 {
		t.Error("an empty icon name must still be refused")
	}
}

func TestADirCatalogServesATemplateWithoutAnIconButNotItsIcon(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "jellyfin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "jellyfin", ComposeFile), []byte(iconless(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, indexFile), []byte(`{"schema":1,"serial":1,"templates":[{"id":"jellyfin","revision":1,"title":"Jellyfin","categories":["media"],"docs":"https://docs.linuxserver.io/images/docker-jellyfin/"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := DirCatalog{Root: root, Source: SourceCurated}
	ctx := context.Background()
	idx, err := c.Index(ctx)
	if err != nil || len(idx.Templates) != 1 || idx.Templates[0].ID != "jellyfin" {
		t.Fatalf("Index = %+v, %v", idx, err)
	}
	if d, err := Show(ctx, c, "jellyfin"); err != nil || d.ID != "jellyfin" {
		t.Fatalf("Show = %+v, %v", d, err)
	}
	if _, err := c.Icon(ctx, "jellyfin"); !errors.Is(err, ErrIconNotFound) || errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("Icon of a template without one: err = %v, want ErrIconNotFound", err)
	}
}

func TestATemplateWithoutAnIconInstalls(t *testing.T) {
	in, stacks := newInstaller(t)
	in.Catalog = MapCatalog{Templates: map[string]string{"jellyfin": iconless(t)}}
	if _, _, err := in.Install(context.Background(), PlanRequest{ID: "jellyfin"}); err != nil {
		t.Fatal(err)
	}
	if len(stacks.created) != 1 {
		t.Fatalf("created %d stacks, want 1", len(stacks.created))
	}
}
