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
	root := rootCmd()
	reboot, _, err := root.Find([]string{"reboot"})
	if err != nil {
		t.Fatalf("find reboot: %v", err)
	}
	if reboot.Flags().Lookup("confirm") == nil {
		t.Fatal("reboot has no --confirm flag")
	}

	var errBuf bytes.Buffer
	root.SetArgs([]string{"reboot"})
	root.SetOut(&errBuf)
	root.SetErr(&errBuf)
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("reboot without --confirm: %v", err)
	}
}

func TestArrayStopRequiresConfirm(t *testing.T) {
	root := rootCmd()
	stop, _, err := root.Find([]string{"array", "stop"})
	if err != nil {
		t.Fatalf("find array stop: %v", err)
	}
	if stop.Flags().Lookup("confirm") == nil {
		t.Fatal("array stop has no --confirm flag")
	}

	var errBuf bytes.Buffer
	root.SetArgs([]string{"array", "stop"})
	root.SetOut(&errBuf)
	root.SetErr(&errBuf)
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("array stop without --confirm: %v", err)
	}
}
