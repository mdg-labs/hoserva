package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestComputeUsageBreakdown_EmptyMountReturnsNil(t *testing.T) {
	got, err := ComputeUsageBreakdown("", nil, time.Now())
	if err != nil || got != nil {
		t.Fatalf("got (%v, %v), want (nil, nil)", got, err)
	}
}

func TestComputeUsageBreakdown_ClassifiesByMode(t *testing.T) {
	root := t.TempDir()
	appdata := filepath.Join(root, "appdata")
	movies := filepath.Join(root, "movies")
	if err := os.MkdirAll(appdata, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(movies, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appdata, "a"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(movies, "b"), []byte("12"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "other"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	at := time.Date(2026, 9, 23, 3, 0, 0, 0, time.UTC)
	got, err := ComputeUsageBreakdown(root, []UsageShare{
		{Name: "appdata", Path: appdata, Mode: "cache-only"},
		{Name: "movies", Path: movies, Mode: "cache-then-move"},
	}, at)
	if err != nil {
		t.Fatalf("ComputeUsageBreakdown: %v", err)
	}
	if got.AppdataBytes != 5 || got.PendingMovesBytes != 2 {
		t.Fatalf("appdata/pending = %d/%d, want 5/2", got.AppdataBytes, got.PendingMovesBytes)
	}
	if got.OtherBytes < 1 {
		t.Fatalf("OtherBytes = %d, want at least 1", got.OtherBytes)
	}
	if !got.ComputedAt.Equal(at) {
		t.Fatalf("ComputedAt = %v, want %v", got.ComputedAt, at)
	}
}
