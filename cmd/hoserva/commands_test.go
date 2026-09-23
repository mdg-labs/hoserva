package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRootCmdHasUpdateRollbackAndReboot(t *testing.T) {
	root := rootCmd()
	for _, name := range []string{"update", "rollback", "reboot"} {
		if _, _, err := root.Find([]string{name}); err != nil {
			t.Fatalf("find %s: %v", name, err)
		}
	}
}

func TestRootCmdHasNetwork(t *testing.T) {
	root := rootCmd()
	network, _, err := root.Find([]string{"network"})
	if err != nil {
		t.Fatalf("find network: %v", err)
	}
	for _, name := range []string{"apply", "confirm"} {
		if _, _, err := network.Find([]string{name}); err != nil {
			t.Fatalf("find network %s: %v", name, err)
		}
	}
}

func TestRootCmdHasShare(t *testing.T) {
	root := rootCmd()
	share, _, err := root.Find([]string{"share"})
	if err != nil {
		t.Fatalf("find share: %v", err)
	}
	for _, name := range []string{"list", "get", "create", "rm", "rm-data", "browse"} {
		if _, _, err := share.Find([]string{name}); err != nil {
			t.Fatalf("find share %s: %v", name, err)
		}
	}
}

func TestShareRmRequiresConfirm(t *testing.T) {
	assertRequiresConfirm(t, []string{"share", "rm", "media"})
}

func TestRootCmdHasArrayStopAndStart(t *testing.T) {
	root := rootCmd()
	array, _, err := root.Find([]string{"array"})
	if err != nil {
		t.Fatalf("find array: %v", err)
	}
	for _, name := range []string{"stop", "start"} {
		if _, _, err := array.Find([]string{name}); err != nil {
			t.Fatalf("find array %s: %v", name, err)
		}
	}
}

func TestRootCmdHasMoverRun(t *testing.T) {
	root := rootCmd()
	mover, _, err := root.Find([]string{"mover"})
	if err != nil {
		t.Fatalf("find mover: %v", err)
	}
	if _, _, err := mover.Find([]string{"run"}); err != nil {
		t.Fatalf("find mover run: %v", err)
	}
}

func TestRebootRequiresConfirm(t *testing.T) {
	assertRequiresConfirm(t, []string{"reboot"})
}

func TestArrayStopRequiresConfirm(t *testing.T) {
	assertRequiresConfirm(t, []string{"array", "stop"})
}

func TestUpdateRequiresConfirm(t *testing.T) {
	assertRequiresConfirm(t, []string{"update"})
}

func TestRollbackRequiresConfirm(t *testing.T) {
	assertRequiresConfirm(t, []string{"rollback"})
}

func TestUpdateCheckDoesNotRequireConfirm(t *testing.T) {
	root := rootCmd()
	update, _, err := root.Find([]string{"update"})
	if err != nil {
		t.Fatalf("find update: %v", err)
	}
	if update.Flags().Lookup("check") == nil {
		t.Fatal("update has no --check flag")
	}
}

func TestRootCmdHasDiskAddAndReplace(t *testing.T) {
	root := rootCmd()
	disk, _, err := root.Find([]string{"disk"})
	if err != nil {
		t.Fatalf("find disk: %v", err)
	}
	for _, name := range []string{"add", "replace", "upgrade"} {
		if _, _, err := disk.Find([]string{name}); err != nil {
			t.Fatalf("find disk %s: %v", name, err)
		}
	}
	if _, _, err := root.Find([]string{"disk", "add", "plan"}); err != nil {
		t.Fatalf("find disk add plan: %v", err)
	}
	if _, _, err := root.Find([]string{"disk", "replace", "plan"}); err != nil {
		t.Fatalf("find disk replace plan: %v", err)
	}
	if _, _, err := root.Find([]string{"disk", "upgrade", "plan"}); err != nil {
		t.Fatalf("find disk upgrade plan: %v", err)
	}
}

func TestDiskAddRequiresConfirm(t *testing.T) {
	assertRequiresConfirm(t, []string{"disk", "add", "--device", "/dev/sdx"})
}

func TestDiskReplaceRequiresConfirm(t *testing.T) {
	assertRequiresConfirm(t, []string{"disk", "replace", "--mountpoint", "/mnt/disk2", "--device", "/dev/sdx"})
}

func TestDiskUpgradeRequiresConfirm(t *testing.T) {
	assertRequiresConfirm(t, []string{"disk", "upgrade", "--mountpoint", "/mnt/disk2", "--device", "/dev/sdx"})
}

func assertRequiresConfirm(t *testing.T, args []string) {
	t.Helper()
	root := rootCmd()
	cmd, _, err := root.Find(args)
	if err != nil {
		t.Fatalf("find %s: %v", strings.Join(args, " "), err)
	}
	if cmd.Flags().Lookup("confirm") == nil {
		t.Fatalf("%s has no --confirm flag", strings.Join(args, " "))
	}

	var errBuf bytes.Buffer
	root.SetArgs(args)
	root.SetOut(&errBuf)
	root.SetErr(&errBuf)
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("%s without --confirm: %v", strings.Join(args, " "), err)
	}
}
