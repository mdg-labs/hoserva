package main

import (
	"context"
	"os"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// TestMain keeps every test off the real /mnt: the catch-all guard would
// create /mnt/user, so it is a no-op unless a test installs its own.
func TestMain(m *testing.M) {
	catchAllGuard = func(context.Context, disk.Runner, string) error { return nil }
	os.Exit(m.Run())
}
