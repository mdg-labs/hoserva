//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host — same pattern as snapraid_lab_test.go: built with
// `go test -tags lab -c` from the host (compiling touches no device) and
// run with `docker compose exec -T lab <binary>` inside the lab
// container. It exercises this issue's central acceptance criterion
// directly against real fanotify marks and a real `snapraid diff`,
// mirroring spike S7's own comparison methodology (doc 08 §7): a
// scripted workload's distinct journal entries, summed across every
// marked disk, must equal `diff`'s own changed-file total for the same
// window (Added+Removed+Updated+Copied — Moved is excluded from both
// sides, since this lab can never read a data disk's UUID at all,
// spike S5, so SnapRAID here never classifies a same-disk rename as
// Moved; it reports Removed+Copied instead, and the journal's own
// RENAME_OLD/RENAME_NEW pair agrees with that for the same reason).

package parity

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLabJournal_CountsMatchPreSyncDiff runs a real FAN_MARK_FILESYSTEM
// mark per data disk against `make lab-up`'s own standing array, drives
// a scripted create/delete/rename/modify workload across all three data
// disks, and confirms the journal's own distinct-entry total exactly
// matches `snapraid diff`'s changed-file total for the same window
// (doc 02 §2's acceptance criterion: "counts match the pre-sync diff for
// a scripted workload").
func TestLabJournal_CountsMatchPreSyncDiff(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	engine, mounts := labEngine(t, lab)

	// Baseline: files this test will delete, rename and modify must
	// already exist in a synced state, or diff can't report a change
	// against them at all (an unsynced file created and then deleted or
	// renamed before ever being synced leaves no trace in `diff` — it
	// never existed in the last-synced view). Each disk also gets a
	// filler file this test's workload never touches — without one,
	// every tracked file on that disk changes in the same pass, which
	// trips SnapRAID's own separate "all files previously present are
	// now missing or rewritten" guard (`count_equal == 0`, doc 08 §7's
	// own finding building spike S7) and sync refuses outright, never
	// producing a diff at all. This is a real, general SnapRAID
	// behaviour, not a fanotify concern.
	for i, m := range mounts {
		writeFile(t, filepath.Join(m, "keep.bin"), 1_000+i)
	}
	toDelete := filepath.Join(mounts[2], "docs/to-delete.bin")
	writeFile(t, toDelete, 20_000)
	toRename := filepath.Join(mounts[0], "movies/rename-me.bin")
	writeFile(t, toRename, 20_000)
	toModify := filepath.Join(mounts[1], "backup/modify-me.bin")
	writeFile(t, toModify, 20_000)

	syncOnce(t, ctx, engine)

	journal := NewJournal(FanotifyWatcher{}, "")
	t.Cleanup(func() { _ = journal.Close() })

	diskIDs := []string{"d1", "d2", "d3"}
	for i, id := range diskIDs {
		if err := journal.AddDisk(ctx, id, mounts[i]); err != nil {
			t.Fatalf("AddDisk(%s, %s): %v", id, mounts[i], err)
		}
	}

	// The scripted workload — every change happens strictly after every
	// mark above is armed, so this window's diff and this window's
	// journal entries describe exactly the same set of changes.
	writeFile(t, filepath.Join(mounts[0], "movies/created.bin"), 50_000)
	writeFile(t, filepath.Join(mounts[1], "backup/created2.bin"), 50_000)

	if err := os.Remove(toDelete); err != nil {
		t.Fatalf("removing %s: %v", toDelete, err)
	}

	renamed := filepath.Join(mounts[0], "movies/renamed.bin")
	if err := os.Rename(toRename, renamed); err != nil {
		t.Fatalf("renaming %s to %s: %v", toRename, renamed, err)
	}

	// A later, distinct mtime avoids the sub-second-zero edge case S5
	// found around timestamp comparisons; the size change alone would
	// suffice, but this keeps the workload unambiguous either way.
	time.Sleep(1100 * time.Millisecond)
	writeFile(t, toModify, 30_000)

	// The read loop only updates journal state once its Next() call
	// returns a batch the kernel has already delivered — never a timer —
	// but this goroutine still needs a moment to run after the writes
	// above return. Poll rather than sleep a fixed amount, so this test
	// is not tuned to any particular scheduling latency.
	wantTotal := 6 // 2 create + 1 delete + 2 rename (old+new) + 1 modify
	waitFor(t, 15*time.Second, func() bool {
		total := 0
		for _, id := range diskIDs {
			sum, err := journal.Summary(id)
			if err != nil {
				t.Fatalf("Summary(%s): %v", id, err)
			}
			total += sum.Count
		}
		return total == wantTotal
	})

	for _, id := range diskIDs {
		sum, err := journal.Summary(id)
		if err != nil {
			t.Fatalf("Summary(%s): %v", id, err)
		}
		if sum.Overflowed {
			t.Fatalf("disk %s reported Overflowed=true for this small workload — the journal lost events it shouldn't have", id)
		}
		if !sum.Listening {
			t.Fatalf("disk %s reported Listening=false — its mark stopped delivering mid-test", id)
		}
	}

	diff, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	// Moved is deliberately excluded from this sum — see the file-level
	// doc comment above (spike S5/S7: this lab can never classify a
	// same-disk rename as Moved at all).
	gotDiffTotal := diff.Added + diff.Removed + diff.Updated + diff.Copied
	if gotDiffTotal != wantTotal {
		t.Fatalf("snapraid diff changed total (Added+Removed+Updated+Copied) = %d, want %d (workload wasn't classified as expected): %+v", gotDiffTotal, wantTotal, diff)
	}

	journalTotal := 0
	for _, id := range diskIDs {
		sum, err := journal.Summary(id)
		if err != nil {
			t.Fatalf("Summary(%s): %v", id, err)
		}
		journalTotal += sum.Count
	}
	if journalTotal != gotDiffTotal {
		t.Fatalf("journal distinct-entry total = %d, snapraid diff changed total = %d, want equal", journalTotal, gotDiffTotal)
	}

	files, err := journal.Files("d3")
	if err != nil {
		t.Fatalf("Files(d3): %v", err)
	}
	found := false
	for _, f := range files {
		if f.Name == "to-delete.bin" {
			found = true
			if f.Kind != ChangeDelete {
				t.Fatalf("to-delete.bin's recorded kind = %v, want ChangeDelete", f.Kind)
			}
		}
	}
	if !found {
		t.Fatalf("Files(d3) = %+v, missing the deleted file by name", files)
	}

	// Leave the array synced and the journal reset — this test shares
	// its snapraid.conf/content with the other lab tests in this package
	// (labEngine's own workDir), and finishing with pending changes
	// still on disk would corrupt the next test's own before/after diff.
	// This also exercises ResetSince against a real sync for real (doc
	// 02 §2: "count reset at each successful sync").
	syncOnce(t, ctx, engine)
	for _, id := range diskIDs {
		if err := journal.ResetSince(id); err != nil {
			t.Fatalf("ResetSince(%s): %v", id, err)
		}
		sum, err := journal.Summary(id)
		if err != nil {
			t.Fatalf("Summary(%s) after ResetSince: %v", id, err)
		}
		if sum.Count != 0 {
			t.Fatalf("disk %s Summary.Count after ResetSince = %d, want 0", id, sum.Count)
		}
	}
	finalDiff, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff after the final sync: %v", err)
	}
	if finalDiff.Added != 0 || finalDiff.Removed != 0 || finalDiff.Updated != 0 {
		t.Fatalf("Diff after the final sync reported pending changes: %+v, want none", finalDiff)
	}
}
