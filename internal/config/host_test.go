package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestParseSambaSharesSkipsGlobalAndPrinters(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/parsers/host_smb.conf")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseSambaShares(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"media", "homes"}
	if len(got) != len(want) {
		t.Fatalf("shares = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("shares = %v, want %v", got, want)
		}
	}
}

func TestParseNFSExports(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/parsers/host_exports")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseNFSExports(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/export/media", "/export/backup"}
	if len(got) != len(want) {
		t.Fatalf("exports = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("exports = %v, want %v", got, want)
		}
	}
}

func TestParseFstabMountsSkipsPseudoAndKeepsBind(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/parsers/host_fstab")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseFstabMounts(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/", "/boot/efi", "/mnt/media", "/mnt/data"}
	if len(got) != len(want) {
		t.Fatalf("mounts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mounts = %v, want %v", got, want)
		}
	}
}

func TestDetectReadsFixturesUnderTempRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "samba"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyTestdata(t, "../../testdata/parsers/host_smb.conf", filepath.Join(root, PathSamba))
	copyTestdata(t, "../../testdata/parsers/host_exports", filepath.Join(root, PathNFS))
	copyTestdata(t, "../../testdata/parsers/host_fstab", filepath.Join(root, PathFstab))

	docker := MemoryDocker{
		Containers: []DockerRef{{ID: "abc", Name: "jellyfin"}},
		Images:     []DockerRef{{ID: "def", Name: "library/nginx:latest"}},
	}
	inv, err := Detect(context.Background(), root, docker)
	if err != nil {
		t.Fatal(err)
	}
	if !inv.Found(KindSamba) || len(inv.Samba.Items) != 2 {
		t.Fatalf("samba = %+v", inv.Samba)
	}
	if !inv.Found(KindNFS) || len(inv.NFS.Items) != 2 {
		t.Fatalf("nfs = %+v", inv.NFS)
	}
	if !inv.Found(KindFstab) || len(inv.Fstab.Items) != 4 {
		t.Fatalf("fstab = %+v", inv.Fstab)
	}
	if !inv.Found(KindDockerContainers) || inv.DockerContainers[0].Name != "jellyfin" {
		t.Fatalf("containers = %+v", inv.DockerContainers)
	}
	if !inv.Found(KindDockerImages) || inv.DockerImages[0].Name != "library/nginx:latest" {
		t.Fatalf("images = %+v", inv.DockerImages)
	}
}

func TestDetectOmitsMissingFilesAndNilDocker(t *testing.T) {
	inv, err := Detect(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Found(KindSamba) || inv.Found(KindNFS) || inv.Found(KindFstab) {
		t.Fatalf("empty root still reported files: %+v", inv)
	}
	if !inv.DockerUnavailable || inv.Found(KindDockerContainers) || inv.Found(KindDockerImages) {
		t.Fatalf("nil docker should be unavailable: %+v", inv)
	}
}

func TestDetectDoesNotReadTheHostEtc(t *testing.T) {
	root := t.TempDir()
	inv, err := Detect(context.Background(), root, MemoryDocker{})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Samba.Present || inv.NFS.Present || inv.Fstab.Present {
		t.Fatal("Detect reported host files from an empty temp root — it must not fall back to the development host's /etc")
	}
}

func TestDockerDataRootStaysWhenContainersExist(t *testing.T) {
	inv := HostInventory{DockerContainers: []DockerRef{{ID: "a", Name: "x"}}}
	if got := DockerDataRoot(inv, true, true); got != DockerDataRootDefault {
		t.Fatalf("data-root = %q, want %s", got, DockerDataRootDefault)
	}
}

func TestDockerDataRootStaysWithoutCache(t *testing.T) {
	inv := HostInventory{}
	if got := DockerDataRoot(inv, true, false); got != DockerDataRootDefault {
		t.Fatalf("data-root = %q, want %s", got, DockerDataRootDefault)
	}
}

func TestDockerDataRootCacheWhenEmptyAndAccepted(t *testing.T) {
	inv := HostInventory{}
	if got := DockerDataRoot(inv, true, true); got != "/mnt/cache/docker" {
		t.Fatalf("data-root = %q, want /mnt/cache/docker", got)
	}
}

func copyTestdata(t *testing.T, src, dst string) {
	t.Helper()
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}
