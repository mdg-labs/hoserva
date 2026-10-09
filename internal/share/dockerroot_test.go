package share

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/pool"
)

func TestShareDataRoots_NeverTheDockerDataRoot(t *testing.T) {
	cachePath := filepath.Dir(config.DockerDataRootCache)
	reserved := filepath.Base(config.DockerDataRootCache)
	names := []string{reserved, "docker", "Docker", "docker-data", "appdata", ".docker", "media"}
	for _, name := range names {
		for _, mode := range []pool.CacheMode{pool.CacheOnly, pool.CacheThenMove} {
			roots, err := shareDataRoots(name, mode, []string{"/mnt/disk1"}, cachePath)
			if err != nil {
				if !errors.Is(err, ErrInvalidName) {
					t.Fatalf("shareDataRoots(%q, %s) error = %v, want ErrInvalidName or success", name, mode, err)
				}
				continue
			}
			for _, r := range roots {
				if r == config.DockerDataRootCache {
					t.Fatalf("shareDataRoots(%q, %s) = %v, includes Docker's data-root %s", name, mode, roots, config.DockerDataRootCache)
				}
			}
		}
	}
}

func TestCreate_RefusesTheDockerDataRootName(t *testing.T) {
	reserved := filepath.Base(config.DockerDataRootCache)
	for _, mode := range []pool.CacheMode{pool.CacheOnly, pool.CacheThenMove} {
		ctx, svc, _, _ := testService(t)
		_, err := svc.Create(ctx, CreateInput{Name: reserved, CacheMode: mode})
		if !errors.Is(err, ErrInvalidName) {
			t.Fatalf("Create(%q, %s) error = %v, want ErrInvalidName", reserved, mode, err)
		}
		if _, err := svc.Get(ctx, reserved); err == nil {
			t.Fatalf("share %q was stored despite the refusal", reserved)
		}
	}
}

func TestCreate_AcceptsAShareNamedDocker(t *testing.T) {
	ctx, svc, layout, _ := testService(t)
	if _, err := svc.Create(ctx, CreateInput{Name: "docker", CacheMode: pool.CacheOnly}); err != nil {
		t.Fatalf("Create(docker): %v", err)
	}
	roots, err := shareDataRoots("docker", pool.CacheOnly, nil, layout.cache)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 || roots[0] == config.DockerDataRootCache || filepath.Base(roots[0]) != "docker" {
		t.Fatalf("roots = %v, want the docker share's own cache branch", roots)
	}
}

func TestImportFromHost_SkipsTheDockerDataRootName(t *testing.T) {
	ctx, svc, _, _ := testService(t)
	reserved := filepath.Base(config.DockerDataRootCache)
	samba := []byte("[" + reserved + "]\npath = /x\n\n[ok]\nbrowseable = no\n")
	nfs := []byte("/export/" + reserved + " *(ro,sync)\n")
	inserted, err := svc.ImportFromHost(ctx, samba, nfs)
	if err != nil {
		t.Fatal(err)
	}
	if len(inserted) != 1 || inserted[0] != "ok" {
		t.Fatalf("inserted = %v, want [ok]", inserted)
	}
	if _, err := svc.Get(ctx, reserved); err == nil {
		t.Fatalf("share %q was imported", reserved)
	}
}
