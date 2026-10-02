package cache

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/parity"
)

func TestRelocateToCache_ResumedRunStillReportsWhatAnEarlierRunLeftBehind(t *testing.T) {
	s := relocateShare(t, "appdata", 1)
	held := filepath.Join(s.Branches[0], "held.db")
	plain := filepath.Join(s.Branches[0], "plain.bin")
	mustWrite(t, held, "held open")
	mustWrite(t, plain, "moves")
	old := time.Now().Add(-time.Hour)
	for _, p := range []string{held, plain} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	open := NewFakeOpenChecker()
	open.SetOpen(held, true)
	deps := testDeps(open)
	deps.Sync = func(context.Context, []parity.ManifestEntry) error { return nil }

	stop := make(chan struct{})
	var last []byte
	hooks := RunHooks{StopRequested: stop, SaveCheckpoint: func(b []byte) error {
		last = append([]byte(nil), b...)
		var cp RelocateCheckpoint
		if err := json.Unmarshal(b, &cp); err != nil {
			t.Fatal(err)
		}
		if cp.Phase == RelocatePhaseSyncing {
			select {
			case <-stop:
			default:
				close(stop)
			}
		}
		return nil
	}}
	first, err := RelocateToCache(context.Background(), s, Config{}, deps, hooks, nil)
	if err != nil || !first.Interrupted {
		t.Fatalf("first run: interrupted=%v err=%v", first.Interrupted, err)
	}

	second, err := RelocateToCache(context.Background(), s, Config{}, deps, RunHooks{}, last)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(held); err != nil {
		t.Fatalf("the held file should still be on the array: %v", err)
	}
	if !second.Incomplete() {
		t.Fatalf("resumed relocation reports complete while held.db is still on the array: %+v", second.Entries)
	}
	e, ok := resultFor(second, "held.db")
	if !ok || e.Result != ResultLeftBehind {
		t.Fatalf("held.db entry = %+v (found %v), want %s", e, ok, ResultLeftBehind)
	}
}

func TestRelocateToArray_ResumedRunStillReportsWhatAnEarlierRunLeftBehind(t *testing.T) {
	s := newShare(t, "appdata")
	held := filepath.Join(s.CachePath, "a-held.db")
	mustWrite(t, held, "held open")
	mustWrite(t, filepath.Join(s.CachePath, "b.bin"), "moves")
	mustWrite(t, filepath.Join(s.CachePath, "c.bin"), "moves too")
	open := NewFakeOpenChecker()
	open.SetOpen(held, true)
	deps := testDeps(open)

	stop := make(chan struct{})
	var last []byte
	hooks := RunHooks{StopRequested: stop, SaveCheckpoint: func(b []byte) error {
		last = append([]byte(nil), b...)
		var cp Checkpoint
		if err := json.Unmarshal(b, &cp); err != nil {
			t.Fatal(err)
		}
		if cp.LastPath == "b.bin" {
			close(stop)
		}
		return nil
	}}
	first, err := RelocateToArray(context.Background(), s, Config{}, deps, hooks, nil)
	if err != nil || !first.Interrupted {
		t.Fatalf("first run: interrupted=%v err=%v", first.Interrupted, err)
	}

	second, err := RelocateToArray(context.Background(), s, Config{}, deps, RunHooks{}, last)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(held); err != nil {
		t.Fatalf("the held file should still be on the cache: %v", err)
	}
	if !second.Incomplete() {
		t.Fatalf("resumed relocation reports complete while a-held.db is still on the cache: %+v", second.Entries)
	}
	e, ok := resultFor(second, "a-held.db")
	if !ok || e.Result != ResultLeftBehind {
		t.Fatalf("a-held.db entry = %+v (found %v), want %s", e, ok, ResultLeftBehind)
	}
}
