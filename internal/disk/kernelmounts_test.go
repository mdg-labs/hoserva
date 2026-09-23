package disk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testMountInfo = `22 1 8:2 / / rw,relatime shared:1 - ext4 /dev/sda2 rw
40 22 7:0 / /mnt/disk1 rw,relatime shared:20 - xfs /dev/loop0 rw
41 22 7:1 / /mnt/with\040space rw,relatime shared:21 - xfs /dev/loop1 rw
`

func writeMountInfo(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("writing mountinfo: %v", err)
	}
	return p
}

func TestParseMountInfo_DecodesEscapedMountpoints(t *testing.T) {
	points, err := ParseMountInfo(strings.NewReader(testMountInfo))
	if err != nil {
		t.Fatalf("ParseMountInfo: %v", err)
	}
	want := []string{"/", "/mnt/disk1", "/mnt/with space"}
	if strings.Join(points, "|") != strings.Join(want, "|") {
		t.Fatalf("points = %q, want %q", points, want)
	}
}

// TestKernelMounts_IsMounted_MissingPathIsNotMounted covers doc 02 §4 UR5:
// a path that does not exist counts as unmounted, and the answer comes
// from the mount table, never from umount's exit status. The runner here
// fails every umount with the real util-linux exit 32 text for a path
// that is not a mountpoint; IsMounted never calls it.
func TestKernelMounts_IsMounted_MissingPathIsNotMounted(t *testing.T) {
	r := NewFakeRunner()
	missing := filepath.Join(t.TempDir(), "never-created", "mnt", "disk1")
	r.Script("umount", []string{missing}, nil, errors.New("exit status 32: umount: "+missing+": no mount point specified."))
	k := KernelMounts{Runner: r, MountInfo: writeMountInfo(t, testMountInfo)}

	mounted, err := k.IsMounted(context.Background(), missing)
	if err != nil || mounted {
		t.Fatalf("IsMounted(missing) = %v, %v; want false, nil", mounted, err)
	}
	mounted, err = k.IsMounted(context.Background(), "/mnt/disk1/")
	if err != nil || !mounted {
		t.Fatalf("IsMounted(/mnt/disk1/) = %v, %v; want true, nil", mounted, err)
	}
	if err := k.UnmountOnce(context.Background(), missing); err == nil {
		t.Fatal("UnmountOnce(missing) = nil, want the scripted exit 32 reported")
	}
}

func TestKernelMounts_IsMounted_UnreadableTableIsAnError(t *testing.T) {
	k := KernelMounts{Runner: NewFakeRunner(), MountInfo: filepath.Join(t.TempDir(), "absent")}
	if _, err := k.IsMounted(context.Background(), "/mnt/disk1"); err == nil {
		t.Fatal("IsMounted with an unreadable mount table = nil error, want an error (never read as unmounted)")
	}
}

func TestConfirmMountedUUID(t *testing.T) {
	r := NewFakeRunner()
	r.Script("findmnt", []string{"-n", "-o", "UUID", "/mnt/disk1"}, []byte("aaaa\n"), nil)
	if err := ConfirmMountedUUID(context.Background(), r, "/mnt/disk1", "aaaa"); err != nil {
		t.Fatalf("ConfirmMountedUUID(match) = %v", err)
	}
	if err := ConfirmMountedUUID(context.Background(), r, "/mnt/disk1", "bbbb"); err == nil {
		t.Fatal("ConfirmMountedUUID(mismatch) = nil, want an error")
	}
}
