package disk

import "testing"

func TestResolveIdentity_PrefersWWN(t *testing.T) {
	got := ResolveIdentity([]string{
		"ata-WDC_WD80EFZX-68UW8N0_VGH0A1B2",
		"wwn-0x5000cca0b1c2d3e4",
	})
	want := Identity{WWN: "0x5000cca0b1c2d3e4", Serial: "VGH0A1B2", ByIDName: "wwn-0x5000cca0b1c2d3e4"}
	if got != want {
		t.Fatalf("ResolveIdentity: got %+v, want %+v", got, want)
	}
	if got.IdentityPath() != "/dev/disk/by-id/wwn-0x5000cca0b1c2d3e4" {
		t.Fatalf("IdentityPath: got %q", got.IdentityPath())
	}
}

func TestResolveIdentity_FallsBackToSerial(t *testing.T) {
	got := ResolveIdentity([]string{
		"ata-WDC_WD40EFRX-68WT0N0_WD-WCC7K1234567",
	})
	// WD's own serial format embeds a further "WD-" prefix after the
	// by-id link's final underscore; that is the whole serial, not
	// something to strip.
	want := Identity{Serial: "WD-WCC7K1234567", ByIDName: "ata-WDC_WD40EFRX-68WT0N0_WD-WCC7K1234567"}
	if got != want {
		t.Fatalf("ResolveIdentity: got %+v, want %+v", got, want)
	}
	if got.WeakIdentity {
		t.Fatal("ResolveIdentity: a host-attached ata- disk was flagged weak identity")
	}
	// A serial-only identity's by-id path must be reconstructible too
	// (this issue): the full link basename is retained, not just the
	// extracted serial lastSegment would otherwise discard.
	if got.IdentityPath() != "/dev/disk/by-id/ata-WDC_WD40EFRX-68WT0N0_WD-WCC7K1234567" {
		t.Fatalf("IdentityPath: got %q", got.IdentityPath())
	}
}

func TestResolveIdentity_VirtioBlkByIDLink(t *testing.T) {
	got := ResolveIdentity([]string{
		"virtio-disk1-hoserva-162-a1",
	})
	want := Identity{Serial: "disk1-hoserva-162-a1", ByIDName: "virtio-disk1-hoserva-162-a1"}
	if got != want {
		t.Fatalf("ResolveIdentity: got %+v, want %+v", got, want)
	}
	if got.WeakIdentity {
		t.Fatal("ResolveIdentity: a virtio- disk was flagged weak identity")
	}
	if got.IdentityPath() != "/dev/disk/by-id/virtio-disk1-hoserva-162-a1" {
		t.Fatalf("IdentityPath: got %q", got.IdentityPath())
	}
}

func TestResolveIdentity_VirtioBlkTruncatedSerialsStillMatch(t *testing.T) {
	// A real L3 guest kernel truncates the exported serial to
	// VIRTIO_BLK_ID_BYTES (20 bytes) before udev ever builds this by-id
	// link (doc 06 §6). create-vm.sh leads with each disk's own role name
	// so that truncation, whatever it does to the trailing lab id, can
	// never make two different array disks resolve to the same by-id
	// link — simulated here for a long, nightly-shaped lab id.
	parity := ResolveIdentity([]string{truncateVirtioSerial("parity1-hoserva-nightly-9876543210-3")})
	disk1 := ResolveIdentity([]string{truncateVirtioSerial("disk1-hoserva-nightly-9876543210-3")})
	if parity.Matches(disk1) {
		t.Fatalf("ResolveIdentity: distinct array disks matched after truncation: %+v vs %+v", parity, disk1)
	}
	if parity.Serial == "" || disk1.Serial == "" {
		t.Fatalf("ResolveIdentity: expected non-empty serials, got %+v and %+v", parity, disk1)
	}
}

// truncateVirtioSerial mimics the guest kernel's own VIRTIO_BLK_ID_BYTES
// truncation (20 bytes, include/uapi/linux/virtio_blk.h) on the serial
// create-vm.sh writes, then builds the by-id link name udev derives
// from it, the same way a real L3 guest's own by-id directory would.
func truncateVirtioSerial(serial string) string {
	const virtioBlkIDBytes = 20
	if len(serial) > virtioBlkIDBytes {
		serial = serial[:virtioBlkIDBytes]
	}
	return "virtio-" + serial
}

func TestResolveIdentity_USBEnclosureIsWeak(t *testing.T) {
	got := ResolveIdentity([]string{
		"usb-WD_easystore_25FB_575836314141304A4A3236-0:0",
	})
	if !got.WeakIdentity {
		t.Fatal("ResolveIdentity: a usb- only disk was not flagged weak identity")
	}
	if got.WWN != "" {
		t.Fatalf("ResolveIdentity: usb-only disk got a WWN: %+v", got)
	}
	if got.Serial != "575836314141304A4A3236" {
		t.Fatalf("ResolveIdentity: got serial %q, want the USB link's trailing segment", got.Serial)
	}
	if got.IdentityPath() != "/dev/disk/by-id/usb-WD_easystore_25FB_575836314141304A4A3236-0:0" {
		t.Fatalf("IdentityPath: got %q", got.IdentityPath())
	}
}

func TestResolveIdentity_NoByIDLinksIsWeak(t *testing.T) {
	got := ResolveIdentity(nil)
	if !got.WeakIdentity {
		t.Fatal("ResolveIdentity: a disk with no by-id links at all was not flagged weak identity")
	}
	if got.WWN != "" || got.Serial != "" {
		t.Fatalf("ResolveIdentity: got %+v, want both empty", got)
	}
	if got.IdentityPath() != "" {
		t.Fatalf("IdentityPath: got %q, want empty — nothing to bind to", got.IdentityPath())
	}
}

func TestResolveIdentity_IgnoresPartitionSuffixConfusion(t *testing.T) {
	// scanByID is responsible for filtering out "-partN" entries before
	// calling ResolveIdentity; this asserts the serial extraction itself
	// still behaves sanely if a caller passes one through by mistake,
	// rather than silently absorbing the partition number into the
	// serial.
	got := ResolveIdentity([]string{"wwn-0x5000cca0b1c2d3e4-part1"})
	if got.WWN != "0x5000cca0b1c2d3e4-part1" {
		t.Fatalf("ResolveIdentity: got %+v", got)
	}
}
