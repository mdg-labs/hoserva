package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/pool"
)

// TestParityRegistrar_ShareRelocationAbort_RegisteredAndOwnershipChecked
// proves #732's registration: a daemon wired through parityRegistrar.register
// (the call run() makes) cancels an interrupted share relocation through
// Scheduler.Cancel, which reaches the abort registered for
// job.TypeShareRelocation and clears the relocation's own manifest from the
// real store, and leaves a manifest another job wrote alone. Without the
// RegisterAbort in register, Cancel ends the job cancelled with the manifest
// untouched and the first subtest fails.
func TestParityRegistrar_ShareRelocationAbort_RegisteredAndOwnershipChecked(t *testing.T) {
	for _, c := range []struct {
		name     string
		manifest []parity.ManifestEntry
		want     int
	}{
		{"own manifest is cleared", []parity.ManifestEntry{{RelPath: "movies/a.mkv", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/cache"}}, 0},
		{"another share's manifest is left alone", []parity.ManifestEntry{{RelPath: "photos/a.jpg", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/cache"}}, 1},
		{"a rebalance of the same share is left alone", []parity.ManifestEntry{{RelPath: "movies/a.mkv", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/disk2"}}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, env := newParityRegistrationEnv(t)
			if err := os.WriteFile(filepath.Join(env.configRoot, snapraidConfRelPath), []byte("placeholder"), 0o644); err != nil {
				t.Fatalf("writing placeholder snapraid.conf: %v", err)
			}
			engine, err := newSnapraidEngine(env.configRoot, env.parityReg.stateDir, nil)
			if err != nil || engine == nil {
				t.Fatalf("newSnapraidEngine = %v, %v", engine, err)
			}
			env.parityReg.register(engine)

			putMoverTestArray(t, env.parityReg.arrayStore, "20G")
			insertMoverTestShare(t, env.shareStore, "movies", string(pool.CacheOnly))
			if err := engine.Relocation.Replace(ctx, c.manifest, nil); err != nil {
				t.Fatalf("persisting the manifest: %v", err)
			}

			params, err := json.Marshal(job.ShareRelocationParams{Share: "movies", To: "cache"})
			if err != nil {
				t.Fatal(err)
			}
			jobStore := job.NewStore(env.db)
			created := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
			if err := jobStore.Create(ctx, &job.Job{
				ID:          "relocation-1",
				Type:        job.TypeShareRelocation,
				Class:       job.ClassArrayWrite,
				Status:      job.StatusInterrupted,
				Resumable:   true,
				Cancellable: true,
				Params:      params,
				CreatedAt:   created,
				StartedAt:   &created,
			}); err != nil {
				t.Fatalf("creating the interrupted relocation: %v", err)
			}

			if _, err := env.scheduler.Cancel(ctx, "relocation-1"); err != nil {
				t.Fatalf("Cancel: %v", err)
			}
			got, err := jobStore.Get(ctx, "relocation-1")
			if err != nil || got.Status != job.StatusCancelled {
				t.Fatalf("job after Cancel = %+v, %v, want cancelled", got, err)
			}
			manifest, _, err := engine.Relocation.Current(context.Background())
			if err != nil {
				t.Fatalf("Current: %v", err)
			}
			if len(manifest) != c.want {
				t.Fatalf("manifest after Cancel = %+v, want %d entries", manifest, c.want)
			}
		})
	}
}
