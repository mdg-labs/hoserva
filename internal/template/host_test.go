package template

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mdg-labs/hoserva/internal/container"
)

const netHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

// procNet writes a /proc/net directory: a listening 0.0.0.0:8080, an
// established connection from local port 443 (not a listener), a UDP socket
// bound to 5353 and a connected UDP socket on 6000 (not bound alone).
func procNet(t *testing.T, withV6 bool) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(netHeader+body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("tcp",
		"   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0\n"+
			"   1: 0100007F:01BB 0100007F:D2A2 01 00000000:00000000 00:00000000 00000000     0        0 2 1 0000000000000000 100 0 0 10 0\n")
	write("udp",
		"   0: 00000000:14E9 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 3 2 0000000000000000 0\n"+
			"   1: 0100007F:1770 0100007F:0035 01 00000000:00000000 00:00000000 00000000     0        0 4 2 0000000000000000 0\n")
	if withV6 {
		write("tcp6", "   0: 00000000000000000000000000000000:232A 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 5 1 0000000000000000 100 0 0 10 0\n")
		write("udp6", "")
	}
	return dir
}

func TestHostPortsCombinesRunningContainersAndHostListeners(t *testing.T) {
	provider := container.NewFakeProvider()
	provider.AddContainer(container.Container{ID: "a", Name: "a", State: "running", Ports: []container.Port{{HostPort: 9000, ContainerPort: 80, Protocol: "tcp"}, {ContainerPort: 81, Protocol: "tcp"}}})
	provider.AddContainer(container.Container{ID: "b", Name: "b", State: "exited", Ports: []container.Port{{HostPort: 9100, ContainerPort: 80, Protocol: "tcp"}}})
	got, err := HostPorts{Containers: provider, ProcNet: procNet(t, true)}.UsedPorts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []int{9000, 8080, 5353, 9002} {
		if !got[p] {
			t.Errorf("port %d should be taken: %v", p, got)
		}
	}
	for _, p := range []int{9100, 443, 6000, 81} {
		if got[p] {
			t.Errorf("port %d should be free: %v", p, got)
		}
	}
}

func TestHostPortsToleratesMissingIPv6TablesOnly(t *testing.T) {
	provider := container.NewFakeProvider()
	if _, err := (HostPorts{Containers: provider, ProcNet: procNet(t, false)}).UsedPorts(context.Background()); err != nil {
		t.Fatalf("no tcp6/udp6: %v", err)
	}
	dir := procNet(t, true)
	if err := os.Remove(filepath.Join(dir, "tcp")); err != nil {
		t.Fatal(err)
	}
	if _, err := (HostPorts{Containers: provider, ProcNet: dir}).UsedPorts(context.Background()); err == nil {
		t.Fatal("a missing tcp table must fail, not report every port free")
	}
}

func TestHostPortsFailsWhenDockerCannotBeAsked(t *testing.T) {
	provider := container.NewFakeProvider()
	provider.SetUnavailable(nil)
	if _, err := (HostPorts{Containers: provider, ProcNet: procNet(t, true)}).UsedPorts(context.Background()); !errors.Is(err, container.ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	if _, err := (HostPorts{ProcNet: procNet(t, true)}).UsedPorts(context.Background()); !errors.Is(err, container.ErrUnavailable) {
		t.Errorf("no provider: err = %v, want ErrUnavailable", err)
	}
}

func TestNextFreePortSkipsEverythingTaken(t *testing.T) {
	if p, ok := nextFreePort(8080, map[int]bool{8081: true}, map[int]bool{8082: true}); !ok || p != 8083 {
		t.Errorf("got %d, %v", p, ok)
	}
	if _, ok := nextFreePort(65535, map[int]bool{}); ok {
		t.Error("there is no port above 65535")
	}
}

func TestHostGPUListsOnlyRenderNodes(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"renderD129", "card0", "renderD128"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := HostGPU{DRIDir: dir}.RenderDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != filepath.Join(dir, "renderD128") || got[1] != filepath.Join(dir, "renderD129") {
		t.Errorf("devices = %v", got)
	}
	none, err := HostGPU{DRIDir: filepath.Join(dir, "absent")}.RenderDevices(context.Background())
	if err != nil || len(none) != 0 {
		t.Errorf("a host with no /dev/dri offers nothing: %v, %v", none, err)
	}
}

func TestDirCatalogReadsOnlyPlainTemplateDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real", ComposeFile), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, ComposeFile), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "filelink"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, ComposeFile), filepath.Join(root, "filelink", ComposeFile)); err != nil {
		t.Fatal(err)
	}
	c := DirCatalog{Root: root, Source: "s"}
	if e, err := c.Entry(context.Background(), "real"); err != nil || e.Source != "s" || string(e.Data) != "services: {}\n" {
		t.Errorf("real: %+v, %v", e, err)
	}
	for _, id := range []string{"missing", "linked", "filelink", "../real", "Real", "real/x", ""} {
		if _, err := c.Entry(context.Background(), id); !errors.Is(err, ErrTemplateNotFound) {
			t.Errorf("%q: err = %v, want ErrTemplateNotFound", id, err)
		}
	}
}
