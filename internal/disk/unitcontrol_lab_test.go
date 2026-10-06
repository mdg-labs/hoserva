//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never on
// the host. It proves against the real kernel that array stop's own disk
// unmount (MountUnitController, doc 02 §4) leaves no branch bind (#656, doc
// 02 §1) holding the disk's filesystem mounted.

package disk

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// unloadedBindSystemd stands in for systemd in a lab that has none: stopping
// a mount unit unmounts exactly that unit's own mount point, and nothing
// else. That is systemd once a branch bind's unit file is gone and the
// units are reloaded: the bind is still mounted, but nothing binds it to its
// disk any more (no BindsTo=), so stopping the disk's unit leaves it up.
type unloadedBindSystemd struct {
	units map[string]string // unit name → mount point
}

func (s unloadedBindSystemd) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name != "systemctl" || len(args) != 2 || args[0] != "stop" {
		return nil, fmt.Errorf("unexpected call in this lab: %s %s", name, strings.Join(args, " "))
	}
	where, ok := s.units[args[1]]
	if !ok {
		return nil, fmt.Errorf("unit %s is not loaded", args[1])
	}
	return CommandRunner{}.Run(ctx, "umount", where)
}

func TestLabMountUnitController_ArrayStopLeavesNoBranchBindMounted(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	r := CommandRunner{}

	dev := createLoopImage(ctx, t, r, lab, "bind-array-stop")
	if out, err := exec.Command("mkfs.xfs", "-q", dev).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.xfs %s: %v: %s", dev, err, out)
	}
	where := filepath.Join(lab, "mnt", "disk-bind-array-stop")
	bind := BranchBindFor(where).Where
	for _, dir := range []string{where, bind} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	t.Cleanup(func() {
		for _, dir := range []string{bind, where} {
			if _, mounted, _ := ReadMountTarget(dir); mounted {
				_, _ = r.Run(context.Background(), "umount", dir)
			}
			_ = os.Remove(dir)
		}
	})
	if _, err := r.Run(ctx, "mount", dev, where); err != nil {
		t.Fatalf("mount %s %s: %v", dev, where, err)
	}
	// The bind exactly as its unit (BranchBind.Render) or the lab's
	// pool.Mounter makes it.
	if _, err := r.Run(ctx, "mount", "--bind", "-o", "nosymfollow", where, bind); err != nil {
		t.Fatalf("binding %s at %s: %v", where, bind, err)
	}

	systemd := unloadedBindSystemd{units: map[string]string{
		UnitFileName(where): where,
		UnitFileName(bind):  bind,
	}}
	c := MountUnitController{Unit: MountUnit{Where: where}, Runner: systemd}
	if err := c.Unmount(ctx); err != nil {
		t.Fatalf("Unmount: %v", err)
	}

	for _, dir := range []string{bind, where} {
		if _, mounted, err := ReadMountTarget(dir); err != nil || mounted {
			t.Fatalf("%s after array stop: mounted %v, err %v — want not mounted", dir, mounted, err)
		}
	}
	if out, err := exec.Command("findmnt", "-n", "-o", "TARGET", "--source", dev).Output(); err == nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("%s is still mounted at %q after array stop (findmnt err %v)", dev, strings.TrimSpace(string(out)), err)
	}
}
