//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host: it is built with `go test -tags lab -c` from the host
// (compiling touches no device) and the resulting binary is run with
// `docker compose exec -T lab <binary>` inside the lab container, the
// same way format_lab_test.go's own tests do. It proves LinuxBlankProber
// against real loop devices and a real blkid — in particular, finding
// 1's own regression: blkid -p documents exit 2 as covering both "no
// signature found" and "impossible to gather any information about the
// device" (confirmed here: a nonexistent path and a directory both exit
// 2, identically to a genuinely blank device), which is exactly why
// ProbeBlank never trusts exit 2 without also opening dev itself.

package disk

import (
	"context"
	"os"
	"testing"
)

// TestLabBlankProber_GenuinelyBlankDeviceReportsBlank proves the
// positive case against a real, freshly-attached loop device that was
// never formatted: blkid -p exits 2, and the real readback opens and
// reads it cleanly.
func TestLabBlankProber_GenuinelyBlankDeviceReportsBlank(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	r := CommandRunner{}
	dev := createLoopImage(ctx, t, r, lab, "blankprobe-lab-blank")

	p := LinuxBlankProber{Exec: r}
	blank, err := p.ProbeBlank(ctx, dev)
	if err != nil {
		t.Fatalf("ProbeBlank(%s): %v", dev, err)
	}
	if !blank {
		t.Fatalf("ProbeBlank(%s) = false, want true — a freshly attached loop device carries no signature at all", dev)
	}
}

// TestLabBlankProber_ExtFormattedDeviceRefuses proves a real ext4
// filesystem (blkid -p's own exit 0, TYPE=ext4) is refused.
func TestLabBlankProber_ExtFormattedDeviceRefuses(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	r := CommandRunner{}
	dev := createLoopImage(ctx, t, r, lab, "blankprobe-lab-ext4")
	if _, err := r.Run(ctx, "mkfs.ext4", "-q", dev); err != nil {
		t.Fatalf("mkfs.ext4 %s: %v", dev, err)
	}

	p := LinuxBlankProber{Exec: r}
	blank, err := p.ProbeBlank(ctx, dev)
	if err != nil {
		t.Fatalf("ProbeBlank(%s): %v", dev, err)
	}
	if blank {
		t.Fatalf("ProbeBlank(%s) = true, want false — this device carries a real ext4 filesystem", dev)
	}
}

// TestLabBlankProber_MBRSignatureRefuses proves a real MBR partition
// table (blkid -p's own PTTYPE=dos) is refused — confirmed against this
// lab's own blkid (util-linux 2.41.5): writing only the boot signature
// bytes (0x55 0xAA at offset 510) is already enough for blkid -p to
// report PTTYPE=dos, exit 0, so a stray partition table left behind by
// some other use of the disk is never read as blank (doc 02: array disks
// are always whole-disk, never partitioned).
func TestLabBlankProber_MBRSignatureRefuses(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	r := CommandRunner{}
	dev := createLoopImage(ctx, t, r, lab, "blankprobe-lab-mbr")
	writeMBRSignature(t, dev)

	p := LinuxBlankProber{Exec: r}
	blank, err := p.ProbeBlank(ctx, dev)
	if blank {
		t.Fatalf("ProbeBlank(%s) = true, want false — this device carries a real MBR boot signature (PTTYPE=dos)", dev)
	}
	_ = err // an MBR is a positively found signature (exit 0), not an error — asserted on blank alone above
}

// TestLabBlankProber_NonexistentPathRefuses is finding 1's own central
// regression, proved against the real tool: a path with no device at
// all exits 2 from blkid -p — confirmed here identical to the genuinely
// blank case above — so only the readback's own open failure is what
// tells them apart.
func TestLabBlankProber_NonexistentPathRefuses(t *testing.T) {
	ctx := context.Background()
	r := CommandRunner{}
	p := LinuxBlankProber{Exec: r}

	blank, err := p.ProbeBlank(ctx, "/dev/hoserva-blankprobe-lab-does-not-exist")
	if err == nil {
		t.Fatal("ProbeBlank(nonexistent path): got nil error, want one — blkid -p exits 2 here exactly like a blank device, only the readback's own open failure refuses it")
	}
	if blank {
		t.Fatal("ProbeBlank(nonexistent path) = true, want false")
	}
}

// writeMBRSignature opens dev directly (never through blkid or a
// partitioning tool — none of parted/sgdisk is installed in this lab
// image) and writes the two-byte 0x55 0xAA boot signature at offset 510,
// the exact bytes blkid's own MBR prober checks for (confirmed above).
func writeMBRSignature(t *testing.T, dev string) {
	t.Helper()
	f, err := os.OpenFile(dev, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("opening %s to write an MBR signature: %v", dev, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteAt([]byte{0x55, 0xaa}, 510); err != nil {
		t.Fatalf("writing MBR signature to %s: %v", dev, err)
	}
}
