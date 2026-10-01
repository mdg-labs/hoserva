package template

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const fixtureIndex = `{
  "schema": 1,
  "serial": 7,
  "generatedAt": "2026-10-01T11:14:10Z",
  "templates": [
    {"id": "risky-agent", "revision": 1, "title": "Risky agent", "categories": ["system"], "icon": "icon.svg", "docs": "https://example.com/agent/docs"},
    {"id": "jellyfin", "revision": 1, "title": "Jellyfin", "categories": ["media"], "icon": "icon.svg", "docs": "https://docs.linuxserver.io/images/docker-jellyfin/"}
  ]
}`

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixtureCatalog is a catalog directory holding the fixture templates and an
// index.json of its own.
func fixtureCatalog(t *testing.T, index string) DirCatalog {
	t.Helper()
	root := t.TempDir()
	for _, id := range []string{"jellyfin", "risky-agent", "aio-notes", "render-box"} {
		for _, f := range []string{ComposeFile, "icon.svg"} {
			copyFile(t, filepath.Join(fixtureDir, id, f), filepath.Join(root, id, f))
		}
	}
	if err := os.WriteFile(filepath.Join(root, indexFile), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	return DirCatalog{Root: root, Source: SourceCurated}
}

func TestDirCatalogIndexListsTheTemplatesInTheIndexsOrder(t *testing.T) {
	c := fixtureCatalog(t, fixtureIndex)
	got, err := c.Index(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Serial != 7 || !got.GeneratedAt.Equal(time.Date(2026, 10, 1, 11, 14, 10, 0, time.UTC)) {
		t.Errorf("serial %d, generatedAt %v", got.Serial, got.GeneratedAt)
	}
	if len(got.Templates) != 2 || got.Templates[0].ID != "risky-agent" || got.Templates[1].ID != "jellyfin" {
		t.Fatalf("templates = %+v, want risky-agent then jellyfin", got.Templates)
	}
	j := got.Templates[1]
	if j.Revision != 1 || j.Title != "Jellyfin" || len(j.Categories) != 1 || j.Categories[0] != "media" || j.Docs != "https://docs.linuxserver.io/images/docker-jellyfin/" {
		t.Errorf("jellyfin = %+v", j)
	}
	if c.Name() != SourceCurated {
		t.Errorf("Name = %q", c.Name())
	}
}

func TestDirCatalogIndexIsUnavailableNeverEmpty(t *testing.T) {
	good := func(t *testing.T) DirCatalog { return fixtureCatalog(t, fixtureIndex) }
	cases := map[string]func(t *testing.T) DirCatalog{
		"no catalog directory": func(t *testing.T) DirCatalog {
			return DirCatalog{Root: filepath.Join(t.TempDir(), "absent"), Source: SourceCurated}
		},
		"no index.json": func(t *testing.T) DirCatalog {
			c := good(t)
			if err := os.Remove(filepath.Join(c.Root, indexFile)); err != nil {
				t.Fatal(err)
			}
			return c
		},
		"index.json is a symlink": func(t *testing.T) DirCatalog {
			c := good(t)
			outside := filepath.Join(t.TempDir(), "index.json")
			if err := os.WriteFile(outside, []byte(fixtureIndex), 0o644); err != nil {
				t.Fatal(err)
			}
			_ = os.Remove(filepath.Join(c.Root, indexFile))
			if err := os.Symlink(outside, filepath.Join(c.Root, indexFile)); err != nil {
				t.Fatal(err)
			}
			return c
		},
		"index.json is a directory": func(t *testing.T) DirCatalog {
			c := good(t)
			_ = os.Remove(filepath.Join(c.Root, indexFile))
			if err := os.Mkdir(filepath.Join(c.Root, indexFile), 0o755); err != nil {
				t.Fatal(err)
			}
			return c
		},
		"index.json is a pipe": func(t *testing.T) DirCatalog {
			c := good(t)
			_ = os.Remove(filepath.Join(c.Root, indexFile))
			if err := syscall.Mkfifo(filepath.Join(c.Root, indexFile), 0o644); err != nil {
				t.Fatal(err)
			}
			return c
		},
		"not JSON":  func(t *testing.T) DirCatalog { return fixtureCatalog(t, "nope") },
		"no serial": func(t *testing.T) DirCatalog { return fixtureCatalog(t, `{"schema":1,"templates":[]}`) },
		"a template id that is no id": func(t *testing.T) DirCatalog {
			return fixtureCatalog(t, `{"schema":1,"serial":1,"templates":[{"id":"../x"}]}`)
		},
		"a template listed twice": func(t *testing.T) DirCatalog {
			return fixtureCatalog(t, `{"schema":1,"serial":1,"templates":[{"id":"a"},{"id":"a"}]}`)
		},
		"larger than the cap": func(t *testing.T) DirCatalog {
			return fixtureCatalog(t, `{"schema":1,"serial":1,"templates":[],"pad":"`+strings.Repeat("x", maxIndexBytes)+`"}`)
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := build(t).Index(context.Background())
			if !errors.Is(err, ErrCatalogUnavailable) {
				t.Fatalf("Index = %+v, %v, want ErrCatalogUnavailable", got, err)
			}
		})
	}
}

func TestDirCatalogIndexOfAnIndexWithNoTemplatesIsEmptyNotUnavailable(t *testing.T) {
	got, err := fixtureCatalog(t, `{"schema":1,"serial":3,"templates":[]}`).Index(context.Background())
	if err != nil || got.Serial != 3 || len(got.Templates) != 0 || !got.GeneratedAt.IsZero() {
		t.Fatalf("Index = %+v, %v", got, err)
	}
}

func TestDirCatalogIconServesTheFileWithAContentTypeFromTheAllowList(t *testing.T) {
	c := fixtureCatalog(t, fixtureIndex)
	want, err := os.ReadFile(filepath.Join(fixtureDir, "jellyfin", "icon.svg"))
	if err != nil {
		t.Fatal(err)
	}
	icon, err := c.Icon(context.Background(), "jellyfin")
	if err != nil || icon.ContentType != "image/svg+xml" || string(icon.Data) != string(want) {
		t.Fatalf("Icon = %q %d bytes, %v", icon.ContentType, len(icon.Data), err)
	}

	for file, wantType := range map[string]string{"icon.png": "image/png", "icon.webp": "image/webp", "icon.jpg": "image/jpeg", "icon.JPEG": "image/jpeg"} {
		root := c.Root
		compose, err := os.ReadFile(filepath.Join(root, "jellyfin", ComposeFile))
		if err != nil {
			t.Fatal(err)
		}
		swapped := strings.Replace(string(compose), "icon: icon.svg", "icon: "+file, 1)
		if swapped == string(compose) {
			t.Fatal("the fixture's icon line changed")
		}
		if err := os.WriteFile(filepath.Join(root, "jellyfin", ComposeFile), []byte(swapped), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "jellyfin", file), []byte("img"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := c.Icon(context.Background(), "jellyfin")
		if err != nil || got.ContentType != wantType || string(got.Data) != "img" {
			t.Errorf("%s: Icon = %q %q, %v, want %s", file, got.ContentType, got.Data, err, wantType)
		}
		if err := os.WriteFile(filepath.Join(root, "jellyfin", ComposeFile), compose, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDirCatalogIconNeverFollowsASymlinkOutOfTheTemplateDirectory(t *testing.T) {
	c := fixtureCatalog(t, fixtureIndex)
	secret := filepath.Join(t.TempDir(), "secret.svg")
	if err := os.WriteFile(secret, []byte("<svg>not yours</svg>"), 0o644); err != nil {
		t.Fatal(err)
	}
	icon := filepath.Join(c.Root, "jellyfin", "icon.svg")
	if err := os.Remove(icon); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, icon); err != nil {
		t.Fatal(err)
	}
	got, err := c.Icon(context.Background(), "jellyfin")
	if !errors.Is(err, ErrIconNotFound) || len(got.Data) != 0 {
		t.Fatalf("Icon through a symlink = %q, %v, want ErrIconNotFound", got.Data, err)
	}

	if err := os.Remove(icon); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(c.Root, "risky-agent", "icon.svg"), icon); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Icon(context.Background(), "jellyfin"); !errors.Is(err, ErrIconNotFound) {
		t.Fatalf("Icon through a symlink inside the catalog: err = %v, want ErrIconNotFound", err)
	}
}

func TestDirCatalogIconRefusesWhatIsNotAServableFile(t *testing.T) {
	cases := map[string]func(t *testing.T, c DirCatalog, icon string){
		"missing": func(t *testing.T, c DirCatalog, icon string) {
			if err := os.Remove(icon); err != nil {
				t.Fatal(err)
			}
		},
		"a directory": func(t *testing.T, c DirCatalog, icon string) {
			if err := os.Remove(icon); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(icon, 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"a pipe": func(t *testing.T, c DirCatalog, icon string) {
			if err := os.Remove(icon); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(icon, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"over the size cap": func(t *testing.T, c DirCatalog, icon string) {
			if err := os.WriteFile(icon, make([]byte, maxIconBytes+1), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"an extension outside the allow-list": func(t *testing.T, c DirCatalog, icon string) {
			compose := filepath.Join(c.Root, "jellyfin", ComposeFile)
			data, err := os.ReadFile(compose)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(compose, []byte(strings.Replace(string(data), "icon: icon.svg", "icon: icon.html", 1)), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(c.Root, "jellyfin", "icon.html"), []byte("<script>1</script>"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := fixtureCatalog(t, fixtureIndex)
			mutate(t, c, filepath.Join(c.Root, "jellyfin", "icon.svg"))
			if _, err := c.Icon(context.Background(), "jellyfin"); !errors.Is(err, ErrIconNotFound) {
				t.Fatalf("err = %v, want ErrIconNotFound", err)
			}
		})
	}

	c := fixtureCatalog(t, fixtureIndex)
	if _, err := c.Icon(context.Background(), "nope"); !errors.Is(err, ErrTemplateNotFound) {
		t.Errorf("an unknown template: err = %v, want ErrTemplateNotFound", err)
	}
}

func TestShowReturnsTheRawComposeAndThePrivilegeSummary(t *testing.T) {
	c := fixtureCatalog(t, fixtureIndex)
	plain, err := Show(context.Background(), c, "jellyfin")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(fixtureDir, "jellyfin", ComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	if plain.Compose != string(raw) || plain.Source != SourceCurated || plain.ID != "jellyfin" || plain.Revision != 1 || plain.Title != "Jellyfin" {
		t.Errorf("detail = %+v", plain)
	}
	if len(plain.Privileges) != 0 {
		t.Errorf("jellyfin asks for %+v, want nothing beyond an ordinary container", plain.Privileges)
	}

	risky, err := Show(context.Background(), c, "risky-agent")
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, p := range risky.Privileges {
		kinds[p.Kind] = true
		if p.Description == "" || p.Service != "agent" {
			t.Errorf("privilege %+v lacks its service or description", p)
		}
	}
	for _, want := range []string{PrivilegePrivileged, PrivilegeHostNetwork, PrivilegeDockerSocket, PrivilegeAddedCaps, PrivilegeNoConfinement, PrivilegeGroupAdd} {
		if !kinds[want] {
			t.Errorf("risky-agent's summary lacks %s: %+v", want, risky.Privileges)
		}
	}
}

func TestShowComputesThePrivilegesWithEachInputsDefault(t *testing.T) {
	const compose = `services:
  app:
    image: x:1
    privileged: ${PRIV}
x-hoserva:
  schema: 1
  id: probe
  revision: 1
  title: Probe
  categories: [system]
  icon: icon.svg
  docs: https://example.com/docs
  inputs:
    PRIV: { kind: string, default: "%s" }
`
	for def, want := range map[string]bool{"true": true, "false": false} {
		d, err := Show(context.Background(), MapCatalog{Source: "s", Templates: map[string]string{"probe": strings.Replace(compose, "%s", def, 1)}}, "probe")
		if err != nil {
			t.Fatal(err)
		}
		if got := len(d.Privileges) == 1 && d.Privileges[0].Kind == PrivilegePrivileged; got != want || (!want && len(d.Privileges) != 0) {
			t.Errorf("PRIV default %q: privileges = %+v, want privileged reported = %v", def, d.Privileges, want)
		}
	}
}

func TestShowRefusesAnEntryThatFailsTheTemplateRules(t *testing.T) {
	c := MapCatalog{Source: "s", Templates: map[string]string{
		"broken":   "services: {}\n",
		"mismatch": strings.Replace(mustRead(t, "jellyfin"), "id: jellyfin", "id: other", 1),
	}}
	for _, id := range []string{"broken", "mismatch"} {
		if _, err := Show(context.Background(), c, id); !errors.Is(err, ErrInvalidTemplate) {
			t.Errorf("%s: err = %v, want ErrInvalidTemplate", id, err)
		}
	}
	if _, err := Show(context.Background(), c, "nope"); !errors.Is(err, ErrTemplateNotFound) {
		t.Errorf("an unknown id: err = %v, want ErrTemplateNotFound", err)
	}
}

func TestMapCatalogListsAndServesIcons(t *testing.T) {
	jf := mustRead(t, "jellyfin")
	c := MapCatalog{
		Source: "mem", Serial: 5,
		Templates: map[string]string{"jellyfin": jf, "broken": "services: {}\n"},
		Icons:     map[string][]byte{"jellyfin": []byte("<svg/>")},
	}
	idx, err := c.Index(context.Background())
	if err != nil || idx.Serial != 5 || len(idx.Templates) != 2 || idx.Templates[0].ID != "broken" || idx.Templates[1].Title != "Jellyfin" || idx.Templates[1].Revision != 1 {
		t.Fatalf("Index = %+v, %v", idx, err)
	}
	icon, err := c.Icon(context.Background(), "jellyfin")
	if err != nil || icon.ContentType != "image/svg+xml" || string(icon.Data) != "<svg/>" {
		t.Errorf("Icon = %+v, %v", icon, err)
	}
	if _, err := c.Icon(context.Background(), "nope"); !errors.Is(err, ErrTemplateNotFound) {
		t.Errorf("unknown: %v", err)
	}
	c.Icons = nil
	if _, err := c.Icon(context.Background(), "jellyfin"); !errors.Is(err, ErrIconNotFound) {
		t.Errorf("no icon file: %v", err)
	}
}

func TestEmbeddedSnapshotCatalogListsAndShowsEveryTemplate(t *testing.T) {
	archive, sig, err := EmbeddedSnapshot()
	if err != nil {
		t.Fatalf("%v — run `make catalog-snapshot` first", err)
	}
	store := CatalogStore{Dir: filepath.Join(t.TempDir(), "catalog")}
	if _, err := store.Seed(archive, sig); err != nil {
		t.Fatal(err)
	}
	c := DirCatalog{Root: store.Dir, Source: SourceCurated}
	idx, err := c.Index(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if serial, ok, err := store.Serial(); err != nil || !ok || idx.Serial != serial {
		t.Errorf("Index serial %d, store serial %d, %v", idx.Serial, serial, err)
	}
	if len(idx.Templates) == 0 {
		t.Fatal("the embedded catalog lists no template")
	}
	for _, e := range idx.Templates {
		d, err := Show(context.Background(), c, e.ID)
		if err != nil {
			t.Errorf("%s: Show: %v", e.ID, err)
			continue
		}
		if d.Revision != e.Revision || d.Title != e.Title || d.Docs != e.Docs || d.Source != SourceCurated {
			t.Errorf("%s: the index says %+v, the template says %+v", e.ID, e, d)
		}
		if icon, err := c.Icon(context.Background(), e.ID); err != nil || len(icon.Data) == 0 {
			t.Errorf("%s: Icon: %v", e.ID, err)
		}
	}
}
