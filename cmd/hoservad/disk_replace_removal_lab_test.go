//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host: it is built with `go test -tags lab -c` from the host
// (compiling touches no device) and the resulting binary is run with
// `docker compose exec -T lab <binary>` inside the lab container.
//
// It is #384's own data-loss regression: a data disk that is evacuated
// (or unpooled, once a refused finish has already advanced it that far)
// and then genuinely dies still has a product path to rebuild a file
// SnapRAID recorded on it outside every share — written directly at the
// disk's own root after its evacuation already succeeded, since #367
// refuses planDiskEvacuation outright while such content is present
// going in — so nothing before this issue could ever recover it.
// p384StartDaemon is p358StartDaemon (disk_remove_lab_test.go),
// reproduced here because this file cannot reach that function's local,
// unexported *job.Registry to add a second registration to it after
// construction — with one addition: TypeDiskReplace is registered
// exactly the way main.go's own run() registers it (Parity:
// currentParityEngine{handler: handler}, so the job's parity dependency
// is never a startup-time snapshot, matching main.go's own #265 fix).
// p384Setup is p369SetupWithOutsideShareFile(outsideShareFile=true)
// (disk_remove_lab_test.go), reproduced for the same reason, calling
// p384StartDaemon instead and recording the orphan file's own checksum
// in env.files so this file's own test can prove the rebuild restores it
// byte for byte rather than merely "some file". Every other helper
// (p358Share/p358Disk2/p358Sha/p265CreateLoopImage/p369DetachDiskLoop/
// shareRelocLabDir/p358LiveBranches/…) is used unmodified from
// disk_remove_lab_test.go and share_relocation_lab_test.go — both in
// this same package, never edited by this issue.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// p384StartDaemon is p358StartDaemon (disk_remove_lab_test.go) with
// TypeDiskReplace also registered — see this file's own header comment
// for why it cannot be added to that function's daemon after the fact.
func p384StartDaemon(t *testing.T, stateDir, configRoot string, counter *p358SyncCounter) *p358Daemon {
	t.Helper()
	ctx := context.Background()
	exec := disk.CommandRunner{}

	db, err := openDatabase(stateDir)
	if err != nil {
		t.Fatalf("openDatabase: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := applyMigrations(ctx, db, stateDir); err != nil {
		t.Fatalf("applyMigrations: %v", err)
	}
	registry := job.NewRegistry()
	jobStore := job.NewStore(db)
	scheduler := job.NewScheduler(jobStore, job.NewLogStore(filepath.Join(stateDir, "logs")), job.NewHub(), registry)
	if err := scheduler.RecoverFromRestart(ctx); err != nil {
		t.Fatalf("RecoverFromRestart: %v", err)
	}
	arrays := store.NewArrayStore(db)
	shares := store.NewShareStore(db)
	provider := &disk.LinuxProvider{Lister: disk.NewLister(), Exec: exec}
	handler := &api.Handler{Scheduler: scheduler, Store: jobStore, ArrayStore: arrays, Disks: provider}
	rebuild := func(ctx context.Context) error {
		seq, err := newArraySequence(ctx, scheduler, arrays, shares, provider, exec)
		if err != nil {
			return err
		}
		handler.SetArray(seq)
		return nil
	}
	if err := rebuild(ctx); err != nil {
		t.Fatalf("newArraySequence at startup: %v", err)
	}

	generator := cfggen.NewGenerator(configRoot)
	parityReg := &parityRegistrar{
		configRoot: configRoot,
		stateDir:   stateDir,
		db:         db,
		registry:   registry,
		handler:    handler,
		shareStore: shares,
		arrayStore: arrays,
		chainGuard: &diffGuardHolder{},
		generator:  generator,
		diskUnits:  disk.DirectMounter{Runner: exec},
		mounts:     disk.KernelMounts{Runner: exec},
	}
	engine, err := newSnapraidEngine(configRoot, stateDir, counter)
	if err != nil {
		t.Fatalf("newSnapraidEngine: %v", err)
	}
	var usages share.UsageReader
	if engine != nil {
		parityReg.register(engine)
		usages = engine.Usage
	}
	shareService := newShareService(shares, arrays, generator, pool.SystemdMounter{Runner: exec}, usages)
	shareService.PostCommit = rebuild
	topologyChanged := wireTopologyHooks(shareService, rebuild, parityReg, handler)
	registry.Register(job.TypeDiskFormat, false, job.RunDiskFormat(job.DiskFormatDeps{
		Provider:   provider,
		Runner:     exec,
		Store:      arrays,
		Generator:  generator,
		Mounter:    disk.DirectMounter{Runner: exec},
		ArrayReady: topologyChanged,
	}))
	// The one addition over p358StartDaemon: registered the same way
	// main.go's own run() registers it, so this daemon's disk_replace job
	// resolves its parity dependency per call (currentParityEngine),
	// never a nil startup-time snapshot (#265).
	registry.Register(job.TypeDiskReplace, true, job.RunDiskReplace(job.DiskReplaceDeps{
		Provider:   provider,
		Runner:     exec,
		Store:      arrays,
		Generator:  generator,
		Mounter:    disk.DirectMounter{Runner: exec},
		Parity:     currentParityEngine{handler: handler},
		ArrayReady: topologyChanged,
	}))
	return &p358Daemon{db: db, arrays: arrays, shares: shares, scheduler: scheduler, handler: handler, rebuild: rebuild}
}

// p384OrphanPath is the file #369's own scenario leaves on disk2 outside
// every share: written directly at disk2's own root, never under
// p358share, only once disk2's own evacuation has already succeeded
// (#367 refuses planDiskEvacuation outright while such content is
// present going in), so nothing inspects it again until finishDiskRemoval.
const p384OrphanPath = p358Disk2 + "/orphan-outside-share.bin"

// p384Setup is p369SetupWithOutsideShareFile(outsideShareFile=true)
// (disk_remove_lab_test.go), calling p384StartDaemon instead and
// recording the orphan file's own checksum in env.files — p369's own
// version never does, since nothing before #384 ever needed to prove
// what a rebuild of it restores.
func p384Setup(t *testing.T, name string) *p358Env {
	t.Helper()
	lab := shareRelocLabDir(t)
	ctx := context.Background()
	exec := disk.CommandRunner{}
	p358ResetBootContentDir(t)
	p359ClaimCatchAllPath(t)

	devs := map[string]string{}
	for _, role := range []string{"parity", "data1", "data2", "data3"} {
		devs[role] = p265CreateLoopImage(ctx, t, exec, lab, name+"-"+role, "400M")
	}
	t.Cleanup(func() {
		for _, m := range []string{p358Disk1, p358Disk2, p358Disk3, p358Parity} {
			p265UnmountIfMounted(exec, m)
		}
	})

	env := &p358Env{stateDir: t.TempDir(), configRoot: t.TempDir(), counter: &p358SyncCounter{}, files: map[string]string{}}
	env.d = p384StartDaemon(t, env.stateDir, env.configRoot, env.counter)

	size := int64(400 << 20)
	format := job.DiskFormatParams{
		Parity: []disk.AssignedDisk{{Device: devs["parity"], Filesystem: disk.XFS}},
		Data: []disk.AssignedDisk{
			{Device: devs["data1"], Filesystem: disk.XFS},
			{Device: devs["data2"], Filesystem: disk.XFS},
			{Device: devs["data3"], Filesystem: disk.XFS},
		},
		Sizes:        map[string]int64{devs["parity"]: size, devs["data1"]: size, devs["data2"]: size, devs["data3"]: size},
		CreatePolicy: "mfs",
		MinFreeSpace: "1M",
	}
	format.Confirmation = format.Plan().Confirmation()
	params, err := json.Marshal(format)
	if err != nil {
		t.Fatalf("marshaling DiskFormatParams: %v", err)
	}
	created, err := env.d.scheduler.Submit(ctx, job.TypeDiskFormat, nil, params)
	if err != nil {
		t.Fatalf("Submit(disk_format): %v", err)
	}
	if finished := env.d.await(t, created.ID); finished.Status != job.StatusSucceeded {
		t.Fatalf("create-array = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	env.d.engine(t).Runner = env.counter

	if env.d.handler.CurrentArray() == nil {
		t.Fatal("no array sequence after create-array")
	}
	created2 := time.Now().UTC()
	if err := env.d.shares.Insert(ctx, store.Share{Name: p358Share, CacheMode: "array-only", CreatePolicy: "mfs", CreatedAt: created2, UpdatedAt: created2}); err != nil {
		t.Fatalf("inserting share: %v", err)
	}
	for _, m := range []string{p358Disk1, p358Disk2, p358Disk3} {
		if err := os.MkdirAll(filepath.Join(m, p358Share), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if err := env.d.rebuild(ctx); err != nil {
		t.Fatalf("rebuilding the array sequence: %v", err)
	}
	mounter := pool.Mounter{Runner: exec}
	seq := env.d.handler.CurrentArray()
	for _, m := range append([]job.ArrayMount{seq.CatchAll}, seq.ShareMounts...) {
		mc, ok := m.(pool.MountController)
		if !ok {
			t.Fatalf("pool mount %s is %T, want pool.MountController", m.Where(), m)
		}
		p359MountAndTrack(t, mounter, mc.Mnt)
	}

	for _, spec := range []struct {
		mount, prefix string
		n             int
	}{{p358Disk1, "a", 8}, {p358Disk2, "b", 5}, {p358Disk3, "c", 8}} {
		for i := 0; i < spec.n; i++ {
			path := filepath.Join(spec.mount, p358Share, fmt.Sprintf("%s-%02d.bin", spec.prefix, i))
			shareRelocLabWriteFile(t, path, 100_000)
			if spec.mount != p358Disk2 {
				env.files[path] = p358Sha(t, path)
			}
		}
	}
	env.sync(t, false)

	res, err := env.d.handler.PlanDiskEvacuation(ctx, &apiv1.EvacuateDiskPlanRequest{Mountpoint: p358Disk2})
	if err != nil {
		t.Fatalf("PlanDiskEvacuation: %v", err)
	}
	plan, ok := res.(*apiv1.EvacuationPlan)
	if !ok {
		t.Fatalf("PlanDiskEvacuation = %T, want *apiv1.EvacuationPlan", res)
	}
	if len(plan.Moves) != 5 {
		t.Fatalf("evacuation plan moves %d files, want disk2's 5", len(plan.Moves))
	}
	evac, err := env.d.handler.EvacuateDisk(ctx, &apiv1.EvacuateDiskRequest{Mountpoint: p358Disk2, Confirmation: plan.Confirmation})
	if err != nil {
		t.Fatalf("EvacuateDisk: %v", err)
	}
	if finished := env.d.await(t, evac.ID.String()); finished.Status != job.StatusSucceeded {
		t.Fatalf("evacuation = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	if state := env.d.removalState(t, p358Disk2); state != store.RemovalStateEvacuated {
		t.Fatalf("disk2 after the evacuation = %q, want evacuated", state)
	}
	for _, m := range plan.Moves {
		path := filepath.Join(m.TargetBranch, m.RelPath)
		env.files[path] = p358Sha(t, path)
	}
	// Written directly at disk2's own root, now that the evacuation
	// above has already succeeded: #367 refuses planDiskEvacuation
	// itself when such content is present going in, so it can only ever
	// appear afterward (disk_remove_lab_test.go's own outsideShareFile
	// case). A second sync records it in SnapRAID before disk2's loop
	// device is detached the way a dead drive would leave it, so this
	// file's own test can prove a rebuild restores it byte for byte.
	shareRelocLabWriteFile(t, p384OrphanPath, 100_000)
	env.files[p384OrphanPath] = p358Sha(t, p384OrphanPath)
	env.sync(t, false)
	return env
}

// TestLabDiskReplace_EvacuatedDiskDetachedAndRefused_ReplaceRebuildsOntoNewDisk
// is #384's own acceptance scenario: disk2 already reached "evacuated"
// when a synced file (p384OrphanPath) appears outside every share,
// written directly at its own root and synced — bypassing the pool the
// same way a direct write to the disk's own mountpoint always did, since
// #367 refuses the evacuation itself outright while such content is
// present going in. Its loop device is then detached,
// the way a drive that died would leave it. finishDiskRemoval still
// refuses (#369's own regression, unchanged by this issue): SnapRAID
// still records the file, so dropping the disk from snapraid.conf would
// lose the parity's last copy of it. Only then is the slot replaced with
// a fresh loop device: the replace job abandons the removal in the same
// store write that adopts the replacement (doc 09 §4 "Other
// operations…", #384), the slot rejoins the live pool, and `snapraid fix`
// reconstructs the orphan file from parity onto the new disk — asserted
// here against its checksum from before the disk ever died, not merely
// that some file of the same name exists.
func TestLabDiskReplace_EvacuatedDiskDetachedAndRefused_ReplaceRebuildsOntoNewDisk(t *testing.T) {
	ctx := context.Background()
	e := p384Setup(t, "p384replace")
	wantSha := e.files[p384OrphanPath]
	if wantSha == "" {
		t.Fatalf("no recorded checksum for %s before the disk died", p384OrphanPath)
	}

	p369DetachDiskLoop(ctx, t, e)

	refused := e.d.finish(t)
	if refused.Status != job.StatusFailed || !strings.Contains(refused.ErrorMessage, "still records") {
		t.Fatalf("disk_remove = %s (%s), want failed on SnapRAID still recording a file outside every share", refused.Status, refused.ErrorMessage)
	}
	switch state := e.d.removalState(t, p358Disk2); state {
	case store.RemovalStateEvacuated, store.RemovalStateUnpooled:
	default:
		t.Fatalf("disk2 after the refused finish = %q, want evacuated or unpooled", state)
	}

	lab := shareRelocLabDir(t)
	exec := disk.CommandRunner{}
	replacement := p265CreateLoopImage(ctx, t, exec, lab, "p384replace-data2-replacement", "400M")
	const loopSize = int64(400) << 20

	// The replaceDisk API handler derives its Q19/Q20/Q23 size map from a
	// live disk.Provider.List — the same real inventory scan
	// confirmTargetIdentityUnchanged's own doc comment notes never
	// reports a loop device at all (internal/disk/enumerate.go's own
	// blockDeviceSkipPrefixes), so a lab loop device's own size is never
	// in it. internal/job's own TypeDiskReplace lab tests
	// (disk_lifecycle_lab_test.go) hit the same gap and submit
	// DiskReplaceParams straight through the scheduler with an explicit
	// Sizes map instead; this test does the same, exercising the exact
	// job (TypeDiskReplace/RunDiskReplace) the replaceDisk handler queues
	// unchanged in production. ValidateDiskReplacement only ever sizes
	// the slot's own remaining parity and data disks, never cache, so
	// disk2's own old device (leaving) and the cache disk are both
	// omitted.
	_, currentDisks, err := e.d.arrays.GetArray(ctx)
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	sizes := map[string]int64{replacement: loopSize}
	for _, d := range currentDisks {
		if d.Mountpoint == p358Disk2 {
			continue
		}
		switch d.Role {
		case store.ArrayRoleParity, store.ArrayRoleData:
			sizes[d.Device] = loopSize
		}
	}

	replacementDisk := disk.AssignedDisk{Device: replacement, Filesystem: disk.XFS}
	replaceParams := job.DiskReplaceParams{
		Confirmation: job.SingleDiskConfirmation(replacementDisk),
		Mountpoint:   p358Disk2,
		Disk:         replacementDisk,
		Sizes:        sizes,
	}
	replaceParamsJSON, err := json.Marshal(replaceParams)
	if err != nil {
		t.Fatalf("marshaling DiskReplaceParams: %v", err)
	}
	rj, err := e.d.scheduler.Submit(ctx, job.TypeDiskReplace, nil, replaceParamsJSON)
	if err != nil {
		t.Fatalf("Submit(disk_replace): %v", err)
	}
	finished := e.d.await(t, rj.ID)
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("disk_replace = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	switched, err := e.d.arrays.GetDataDiskByMountpoint(ctx, p358Disk2)
	if err != nil {
		t.Fatalf("GetDataDiskByMountpoint(%s): %v", p358Disk2, err)
	}
	if switched.Device != replacement {
		t.Fatalf("disk2 device = %q, want %q", switched.Device, replacement)
	}
	if switched.RemovalState != "" {
		t.Fatalf("disk2 removal_state = %q after the replace, want cleared", switched.RemovalState)
	}

	rejoined := false
	for _, b := range p358LiveBranches(t, pool.CatchAllPath) {
		if b == p358Disk2 {
			rejoined = true
		}
	}
	if !rejoined {
		t.Fatal("disk2 did not rejoin the live pool's catch-all branches after the replace")
	}

	gotSha := p358Sha(t, p384OrphanPath)
	if gotSha != wantSha {
		t.Fatalf("rebuilt %s sha256 = %s, want %s (the checksum from before the disk died)", p384OrphanPath, gotSha, wantSha)
	}
}
