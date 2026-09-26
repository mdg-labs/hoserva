package pool

import (
	"strings"
	"testing"
)

func TestStorageTargetUnit_Render(t *testing.T) {
	u := StorageTargetUnit{DiskMountUnits: []string{"mnt-disk1.mount", "mnt-parity1.mount", "mnt-cache.mount"}}
	got := u.Render()

	for _, want := range []string{
		"Description=Hoserva storage ready",
		"Wants=mnt-disk1.mount mnt-parity1.mount mnt-cache.mount",
		"After=mnt-disk1.mount mnt-parity1.mount mnt-cache.mount",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Render() = %q, want it to contain %q", got, want)
		}
	}

	// Asserted as whole lines, not incidental substrings: the target must
	// carry a hard Requires=/After= on the readiness unit itself — that is
	// what actually gates activation, not the soft Wants=/After= on the
	// disk mounts (Q69).
	for _, wantLine := range []string{
		"Requires=" + StorageReadyUnitName,
		"After=" + StorageReadyUnitName,
	} {
		found := false
		for _, line := range strings.Split(got, "\n") {
			if line == wantLine {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("Render() = %q, want a line exactly %q", got, wantLine)
		}
	}

	if strings.Contains(got, "Requires=mnt-") {
		t.Fatalf("Render() = %q, must never put a hard Requires= directly on a disk mount unit (Q69: a missing disk must not block the target after acknowledgement — only the readiness unit gates)", got)
	}
}

func TestStorageTargetUnit_Render_NoDisks(t *testing.T) {
	got := StorageTargetUnit{}.Render()
	if strings.Contains(got, "Wants=") || strings.Contains(got, "After=mnt-") {
		t.Fatalf("Render() with no disks = %q, want no Wants=/After= lines for disk mounts", got)
	}
	if !strings.Contains(got, "Requires="+StorageReadyUnitName) {
		t.Fatalf("Render() with no disks = %q, still want the hard Requires= on the readiness unit", got)
	}
}

// TestStorageReadyUnit_Render proves the unit's own content never encodes
// disk.StorageGate.Ready() itself: ExecStart is always the same fixed
// existence test against StorageReadyFlagPath, the runtime flag cmd/
// hoservad alone writes and removes (doc 02 §1, Q69) — a caller has
// nothing to pass in because there is nothing left here that varies with
// readiness.
func TestStorageReadyUnit_Render(t *testing.T) {
	got := StorageReadyUnit{}.Render()

	for _, want := range []string{
		"Description=Hoserva storage readiness gate",
		"After=" + HoservadServiceUnit,
		"[Service]",
		"Type=oneshot",
		"RemainAfterExit=yes",
		"ExecStart=/usr/bin/test -e " + StorageReadyFlagPath,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Render() = %q, want it to contain %q", got, want)
		}
	}
}

func TestServiceDropIn_Render(t *testing.T) {
	d := ServiceDropIn{Unit: "smbd.service"}
	got := d.Render()

	for _, want := range []string{"After=" + StorageTargetUnitName, "BindsTo=" + StorageTargetUnitName} {
		if !strings.Contains(got, want) {
			t.Fatalf("Render() = %q, want it to contain %q", got, want)
		}
	}
}

func TestServiceDropIn_DropInPath(t *testing.T) {
	d := ServiceDropIn{Unit: "docker.service"}
	want := "systemd/system/docker.service.d/10-hoserva-storage.conf"
	if got := d.DropInPath(); got != want {
		t.Fatalf("DropInPath() = %q, want %q", got, want)
	}
}

func TestDependentServiceUnits_CoversDoc02EveryStorageDependentService(t *testing.T) {
	want := map[string]bool{
		"smbd.service":              true,
		"nfs-kernel-server.service": true,
		"docker.service":            true,
		"libvirtd.service":          true,
	}
	if len(DependentServiceUnits) != len(want) {
		t.Fatalf("DependentServiceUnits = %v, want exactly %v", DependentServiceUnits, want)
	}
	for _, u := range DependentServiceUnits {
		if !want[u] {
			t.Fatalf("DependentServiceUnits contains unexpected unit %q", u)
		}
	}
}
