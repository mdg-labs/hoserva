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
