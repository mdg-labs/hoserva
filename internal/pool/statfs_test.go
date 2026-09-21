package pool

import (
	"context"
	"testing"
)

// TestStatfsSpaceStatter_ReadsRealFilesystem confirms the real statter
// against an ordinary directory — a plain statfs(2) read of whatever
// filesystem holds it, no mount, no device and no directory walk
// involved, so this runs safely on the host (CLAUDE.md).
func TestStatfsSpaceStatter_ReadsRealFilesystem(t *testing.T) {
	stat, err := StatfsSpaceStatter{}.StatSpace(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("StatSpace: %v", err)
	}
	if stat.TotalBytes <= 0 {
		t.Fatalf("TotalBytes = %d, want > 0", stat.TotalBytes)
	}
	if stat.FreeBytes < 0 || stat.FreeBytes > stat.TotalBytes {
		t.Fatalf("FreeBytes = %d, want between 0 and TotalBytes (%d)", stat.FreeBytes, stat.TotalBytes)
	}
}

func TestStatfsSpaceStatter_MissingPath(t *testing.T) {
	if _, err := (StatfsSpaceStatter{}).StatSpace(context.Background(), "/no/such/path/hoserva-space-test"); err == nil {
		t.Fatal("StatSpace on a missing path: want an error, got nil")
	}
}

func TestStatfsSpaceStatter_RespectsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (StatfsSpaceStatter{}).StatSpace(ctx, t.TempDir()); err == nil {
		t.Fatal("StatSpace with a cancelled context: want an error, got nil")
	}
}
