package template

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/store"
)

func updateRig(t *testing.T, curatedRevision int) (*Sources, string) {
	t.Helper()
	g := newSourceRig(t)
	root := g.sources.Curated.(DirCatalog).Root
	index := `{"schema":1,"serial":7,"templates":[{"id":"jellyfin","revision":` + strconv.Itoa(curatedRevision) + `,"title":"Jellyfin","categories":[],"docs":"https://example.com"}]}`
	writeFile(t, root+"/index.json", index)
	writeFile(t, root+"/jellyfin/"+ComposeFile, sourceCompose(t, "jellyfin", curatedRevision))
	return g.sources, root
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckUpdate_ANewerRevisionIsOfferedWithADiffAndNothingIsWritten(t *testing.T) {
	ctx := context.Background()
	sources, root := updateRig(t, 2)
	installed := sourceCompose(t, "jellyfin", 1)
	before := tree(t, root)

	got, err := CheckUpdate(ctx, sources, InstalledStack{Name: "media", Source: SourceCurated, TemplateID: "jellyfin", Revision: "1", Compose: installed})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != UpdateAvailable || got.InstalledRevision != 1 || got.AvailableRevision != 2 || got.ManuallyEdited {
		t.Fatalf("update = %+v", got)
	}
	if got.Kind != store.CatalogSourceCurated || !got.Signed {
		t.Fatalf("badge = %q signed %v, want the curated one", got.Kind, got.Signed)
	}
	if !strings.Contains(got.Diff, "-  revision: 1\n+  revision: 2\n") || !strings.HasPrefix(got.Diff, "--- docker-compose.yml (installed, revision 1)\n+++ compose.yaml (revision 2)\n") {
		t.Fatalf("diff:\n%s", got.Diff)
	}
	equalTrees(t, tree(t, root), before)
}

func TestCheckUpdate_AManuallyEditedStackIsMarkedAndDiffedAgainstTheEditedFile(t *testing.T) {
	ctx := context.Background()
	sources, _ := updateRig(t, 2)
	edited := strings.Replace(sourceCompose(t, "jellyfin", 1), "restart: unless-stopped", "restart: always", 1)
	if edited == sourceCompose(t, "jellyfin", 1) {
		t.Fatal("the fixture has no restart line to edit")
	}
	got, err := CheckUpdate(ctx, sources, InstalledStack{Name: "media", Source: SourceCurated, TemplateID: "jellyfin", Revision: "1", Compose: edited, ManuallyEdited: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != UpdateAvailable || !got.ManuallyEdited {
		t.Fatalf("update = %+v, want available and marked manually edited", got)
	}
	if !strings.Contains(got.Diff, "-    restart: always") || !strings.Contains(got.Diff, "+    restart: unless-stopped") {
		t.Fatalf("the diff is not against the edited file:\n%s", got.Diff)
	}
}

func TestCheckUpdate_NoNewerRevisionOffersNoDiff(t *testing.T) {
	ctx := context.Background()
	sources, _ := updateRig(t, 2)
	for _, rev := range []string{"2", "3"} {
		got, err := CheckUpdate(ctx, sources, InstalledStack{Source: SourceCurated, TemplateID: "jellyfin", Revision: rev, Compose: "x"})
		if err != nil || got.Status != UpdateUpToDate || got.Diff != "" {
			t.Errorf("revision %s: %+v, %v, want up to date with no diff", rev, got, err)
		}
	}
}

func TestCheckUpdate_StacksThatCannotBeComparedSayWhy(t *testing.T) {
	ctx := context.Background()
	sources, _ := updateRig(t, 2)
	cases := map[string]struct {
		st   InstalledStack
		want UpdateStatus
	}{
		"no template":           {InstalledStack{Compose: "x"}, UpdateNoTemplate},
		"no source":             {InstalledStack{TemplateID: "jellyfin", Revision: "1"}, UpdateNoTemplate},
		"revision not a number": {InstalledStack{Source: SourceCurated, TemplateID: "jellyfin", Revision: "latest"}, UpdateNoTemplate},
		"revision zero":         {InstalledStack{Source: SourceCurated, TemplateID: "jellyfin", Revision: "0"}, UpdateNoTemplate},
		"source removed":        {InstalledStack{Source: "src-0123456789", TemplateID: "jellyfin", Revision: "1"}, UpdateSourceRemoved},
		"source never existed":  {InstalledStack{Source: "elsewhere", TemplateID: "jellyfin", Revision: "1"}, UpdateSourceRemoved},
		"template removed":      {InstalledStack{Source: SourceCurated, TemplateID: "gone", Revision: "1"}, UpdateTemplateRemoved},
	}
	for name, c := range cases {
		got, err := CheckUpdate(ctx, sources, c.st)
		if err != nil || got.Status != c.want || got.Diff != "" {
			t.Errorf("%s: %+v, %v, want %s", name, got, err, c.want)
		}
	}
}

func TestCheckUpdate_AnUnreadableCatalogFailsTheCheckNeverUpToDate(t *testing.T) {
	ctx := context.Background()
	sources, root := updateRig(t, 2)
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	got, err := CheckUpdate(ctx, sources, InstalledStack{Source: SourceCurated, TemplateID: "jellyfin", Revision: "1"})
	if !errors.Is(err, ErrCatalogUnavailable) {
		t.Fatalf("CheckUpdate = %+v, %v, want ErrCatalogUnavailable", got, err)
	}
}

func TestCheckUpdate_ASourceRemovedAndReAddedIsNotTheSameSource(t *testing.T) {
	ctx := context.Background()
	g := newSourceRig(t)
	host, url := g.newHost(t)
	host.serve(buildArchive(t, sourceEntries(t, 1, map[string]int{"risky-agent": 2})), nil, "")
	a, err := g.sources.Add(ctx, AddRequest{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	st := InstalledStack{Source: a.ID, TemplateID: "risky-agent", Revision: "1", Compose: sourceCompose(t, "risky-agent", 1)}
	got, err := CheckUpdate(ctx, g.sources, st)
	if err != nil || got.Status != UpdateAvailable || got.Kind != store.CatalogSourceUserAdded || got.Signed {
		t.Fatalf("update = %+v, %v, want available, user-added and unsigned", got, err)
	}
	if err := g.sources.Remove(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := g.sources.Add(ctx, AddRequest{URL: url}); err != nil {
		t.Fatal(err)
	}
	got, err = CheckUpdate(ctx, g.sources, st)
	if err != nil || got.Status != UpdateSourceRemoved {
		t.Fatalf("update after the source was removed and added again = %+v, %v, want source_removed", got, err)
	}
}
