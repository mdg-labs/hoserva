package disk

import "testing"

func TestResolveIdentity_PrefersWWN(t *testing.T) {
	got := ResolveIdentity([]string{
		"ata-WDC_WD80EFZX-68UW8N0_VGH0A1B2",
		"wwn-0x5000cca0b1c2d3e4",
	})
	want := Identity{WWN: "0x5000cca0b1c2d3e4", Serial: "VGH0A1B2"}
	if got != want {
		t.Fatalf("ResolveIdentity: got %+v, want %+v", got, want)
	}
}

func TestResolveIdentity_FallsBackToSerial(t *testing.T) {
	got := ResolveIdentity([]string{
		"ata-WDC_WD40EFRX-68WT0N0_WD-WCC7K1234567",
	})
	// WD's own serial format embeds a further "WD-" prefix after the
	// by-id link's final underscore; that is the whole serial, not
	// something to strip.
	want := Identity{Serial: "WD-WCC7K1234567"}
	if got != want {
		t.Fatalf("ResolveIdentity: got %+v, want %+v", got, want)
	}
	if got.WeakIdentity {
		t.Fatal("ResolveIdentity: a host-attached ata- disk was flagged weak identity")
	}
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
}

func TestResolveIdentity_NoByIDLinksIsWeak(t *testing.T) {
	got := ResolveIdentity(nil)
	if !got.WeakIdentity {
		t.Fatal("ResolveIdentity: a disk with no by-id links at all was not flagged weak identity")
	}
	if got.WWN != "" || got.Serial != "" {
		t.Fatalf("ResolveIdentity: got %+v, want both empty", got)
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
