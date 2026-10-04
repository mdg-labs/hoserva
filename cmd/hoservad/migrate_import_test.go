package main

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

// fakePool is the catch-all of a rebuilt array sequence: it mounts nothing, so
// no test ever creates /mnt/user.
type fakePool struct{ mounts, unmounts *[]string }

func (fakePool) Where() string { return pool.CatchAllPath }
func (p fakePool) Mount(context.Context) error {
	*p.mounts = append(*p.mounts, pool.CatchAllPath)
	return nil
}
func (p fakePool) Unmount(context.Context) error {
	*p.unmounts = append(*p.unmounts, pool.CatchAllPath)
	return nil
}

type importWiring struct {
	w       *containersWiringHarness
	runner  *disk.FakeRunner
	mounter *disk.FakeMounter
	root    string
	mounts  []string
	unmount []string
}

// wireImport wires the migrator and its import the way main.go does, over a
// machine with a parity disk and a data disk and a daemon whose array record has
// been cleared (a fresh install), with the units written under the harness's
// own root and nothing mounted for real.
func wireImport(t *testing.T) *importWiring {
	t.Helper()
	w := newContainersWiringHarness(t)
	for _, q := range []string{"DELETE FROM array_disks", "DELETE FROM array_settings"} {
		if _, err := w.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	disks := disk.NewFakeProvider()
	disks.AddDisk("/dev/sdb", disk.Disk{Serial: "PARSERIAL", Size: 2 << 40, Filesystem: "xfs", FSDevice: "/dev/sdb1", FSUUID: "10000000-0000-4000-8000-000000000001"})
	disks.AddDisk("/dev/sdc", disk.Disk{Serial: "DATASERIAL", Size: 1 << 40, Filesystem: "xfs", FSDevice: "/dev/sdc1", FSUUID: "10000000-0000-4000-8000-000000000002"})
	runner := disk.NewFakeRunner()
	runner.Script("findmnt", []string{"-n", "-o", "UUID", "/mnt/disk1"}, []byte("10000000-0000-4000-8000-000000000002\n"), nil)
	runner.Script("findmnt", []string{"-n", "-o", "OPTIONS", "/mnt/disk1"}, []byte("ro,nosuid,nodev,noexec,noatime\n"), nil)
	runner.Script("findmnt", []string{"-n", "-o", "OPTIONS", pool.CatchAllPath}, []byte("ro,nosuid,nodev,relatime\n"), nil)
	im := &importWiring{w: w, runner: runner, mounter: disk.NewFakeMounter(), root: filepath.Join(w.root, "etc")}

	if err := wireMigration(context.Background(), w.handler, w.registry, disks, disk.NewFakeReadOnlyMounter(), runner, store.NewMigrationSessionStore(w.db), w.root); err != nil {
		t.Fatal(err)
	}
	w.handler.ArrayStore = w.arrays
	// What main.go does before any job can be submitted.
	w.scheduler.SetMigrationPending(w.arrays.MigrationPending)
	shares := store.NewShareStore(w.db)
	rebuild := func(ctx context.Context) error {
		seq, err := newArraySequence(ctx, w.scheduler, w.arrays, shares, disks, runner, nil)
		if err != nil {
			return err
		}
		if seq != nil {
			seq.CatchAll = fakePool{mounts: &im.mounts, unmounts: &im.unmount}
		}
		w.handler.SetArray(seq)
		return nil
	}
	if err := wireMigrationImport(w.handler, w.registry, w.arrays, cfggen.NewGenerator(im.root), runner, im.mounter, rebuild); err != nil {
		t.Fatal(err)
	}
	return im
}

func (im *importWiring) scan(t *testing.T) {
	t.Helper()
	ini := "[\"parity\"]\nidx=\"0\"\nid=\"M_PARSERIAL\"\nsize=\"1000\"\nstatus=\"DISK_OK\"\ntype=\"Parity\"\n[\"disk1\"]\nidx=\"1\"\nid=\"M_DATASERIAL\"\nsize=\"900\"\nstatus=\"DISK_OK\"\ntype=\"Data\"\nfsType=\"xfs\"\n"
	data := flashBackupZipWith(t, "7.3.2", map[string]string{"config/hoserva/disks.ini": ini})
	status, body := im.w.uploadScan(t, data, false)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/scan = %d %s", status, body)
	}
	var queued struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &queued); err != nil {
		t.Fatal(err)
	}
	if done := im.w.awaitJobByID(t, queued.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("scan job = %s %s", done.Status, done.ErrorMessage)
	}
}

const importBody = `{"confirm":true,"roles":[{"role":"parity","serial":"PARSERIAL"},{"role":"data","serial":"DATASERIAL"}]}`

// POST /migrate/import is reachable through the daemon's own server, job
// registry and scheduler: the job adopts the data disk read-only, records the
// parity disk, leaves the session in the imported phase, and from then on every
// storage job, a new scan and a forget are refused.
func TestMigrationImportWiring_AdoptsOverHTTPAndRefusesWhatCouldWriteAfterwards(t *testing.T) {
	im := wireImport(t)
	w := im.w

	if status, body := w.doBody(t, http.MethodPost, "/migrate/import", importBody); status != http.StatusNotFound || !bytes.Contains(body, []byte("no_migration_report")) {
		t.Fatalf("POST /migrate/import before a scan = %d %s, want 404 no_migration_report", status, body)
	}
	im.scan(t)

	if status, body := w.doBody(t, http.MethodPost, "/migrate/import", strings.Replace(importBody, `"confirm":true`, `"confirm":false`, 1)); status != http.StatusConflict || !bytes.Contains(body, []byte("confirmation_required")) {
		t.Fatalf("POST /migrate/import without confirm = %d %s, want 409 confirmation_required", status, body)
	}
	if len(im.mounter.Mounts) != 0 {
		t.Fatalf("a refused request mounted %v", im.mounter.Mounts)
	}

	status, body := w.doBody(t, http.MethodPost, "/migrate/import", importBody)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/import = %d %s", status, body)
	}
	var queued struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		Class string `json:"class"`
	}
	if err := json.Unmarshal(body, &queued); err != nil || queued.Type != "migration_import" || queued.Class != "topology" {
		t.Fatalf("queued job = %s (%v)", body, err)
	}
	if done := w.awaitJobByID(t, queued.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("import job = %s %s", done.Status, done.ErrorMessage)
	}

	ctx := context.Background()
	settings, disks, err := w.arrays.GetArray(ctx)
	if err != nil || !settings.MigrationPending || len(disks) != 1 || disks[0].Mountpoint != "/mnt/disk1" || disks[0].Serial != "DATASERIAL" {
		t.Fatalf("array = %+v %+v, %v", settings, disks, err)
	}
	recorded, err := w.arrays.RecordedDisks(ctx)
	if err != nil || len(recorded) != 1 || recorded[0].Role != store.ArrayRoleParity || recorded[0].Serial != "PARSERIAL" {
		t.Fatalf("recorded = %+v, %v", recorded, err)
	}
	if len(im.mounter.Mounts) != 1 || !im.mounter.Mounts[0].ReadOnly || im.mounter.Mounts[0].Where != "/mnt/disk1" {
		t.Errorf("mounts = %+v, want the data disk, read-only, and nothing else", im.mounter.Mounts)
	}
	for _, c := range im.runner.Calls() {
		if strings.Contains(strings.Join(c.Args, " "), "/dev/sdb") || strings.HasPrefix(c.Name, "mkfs") {
			t.Errorf("the parity disk was touched: %s %v", c.Name, c.Args)
		}
	}
	if len(im.mounts) != 1 {
		t.Errorf("pool mounts = %v, want the catch-all mounted once through the rebuilt sequence", im.mounts)
	}
	unit, err := os.ReadFile(filepath.Join(im.root, "systemd", "system", "mnt-disk1.mount"))
	if err != nil || !strings.Contains(string(unit), "Options=ro,norecovery,") {
		t.Errorf("mnt-disk1.mount = %s, %v", unit, err)
	}
	if _, err := os.Stat(filepath.Join(im.root, "snapraid.conf")); err == nil {
		t.Error("an import wrote snapraid.conf")
	}

	if v := w.migration(t); v.Phase != "imported" {
		t.Errorf("GET /migrate phase = %q, want imported", v.Phase)
	}
	if status, body := w.do(t, http.MethodGet, "/pool"); status != http.StatusOK || !bytes.Contains(body, []byte("/mnt/disk1")) {
		t.Errorf("GET /pool = %d %s, want the adopted data disk", status, body)
	}

	// From here nothing may write the array or its parity.
	if status, body := w.do(t, http.MethodDelete, "/migrate"); status != http.StatusConflict || !bytes.Contains(body, []byte("migration_in_progress")) {
		t.Errorf("DELETE /migrate = %d %s, want 409 migration_in_progress", status, body)
	}
	if status, body := w.uploadScan(t, flashBackupZip(t, "7.3.2"), false); status != http.StatusConflict || !bytes.Contains(body, []byte("migration_in_progress")) {
		t.Errorf("POST /migrate/scan = %d %s, want 409 migration_in_progress", status, body)
	}
	// The mover is registered by main.go; a stand-in is enough to reach admission.
	w.registry.Register(job.TypeMover, true, func(context.Context, *job.RunContext) error { return nil })
	if status, body := w.doBody(t, http.MethodPost, "/mover/run", ``); status != http.StatusConflict || !bytes.Contains(body, []byte("migration_in_progress")) {
		t.Errorf("POST /mover/run = %d %s, want 409 migration_in_progress", status, body)
	}
}

// A mount that fails undoes the adoption through the daemon's own wiring.
func TestMigrationImportWiring_AFailedMountLeavesNoArray(t *testing.T) {
	im := wireImport(t)
	im.mounter.Err = os.ErrPermission
	im.scan(t)
	status, body := im.w.doBody(t, http.MethodPost, "/migrate/import", importBody)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/import = %d %s", status, body)
	}
	var queued struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &queued)
	if done := im.w.awaitJobByID(t, queued.ID); done.Status != job.StatusFailed {
		t.Fatalf("import job = %s, want failed", done.Status)
	}
	if exists, err := im.w.arrays.Exists(context.Background()); err != nil || exists {
		t.Errorf("array exists = %v, %v after a failed import", exists, err)
	}
	if v := im.w.migration(t); v.Phase != "scanned" {
		t.Errorf("phase = %q after a failed import, want scanned", v.Phase)
	}
}

// wireMigrationImport needs the migration session wireMigration builds and the
// handler's array store, and says so instead of leaving every import to answer 501.
func TestWireMigrationImport_FailsWithoutTheSessionOrTheArrayStore(t *testing.T) {
	w := newContainersWiringHarness(t)
	wire := func() error {
		return wireMigrationImport(w.handler, job.NewRegistry(), w.arrays, cfggen.NewGenerator(t.TempDir()), disk.NewFakeRunner(), disk.NewFakeMounter(), func(context.Context) error { return nil })
	}
	if err := wire(); err == nil {
		t.Error("wireMigrationImport succeeded with no migration session")
	}
	if err := wireMigration(context.Background(), w.handler, w.registry, disk.NewFakeProvider(), disk.NewFakeReadOnlyMounter(), disk.NewFakeRunner(), store.NewMigrationSessionStore(w.db), w.root); err != nil {
		t.Fatal(err)
	}
	if err := wire(); err == nil {
		t.Error("wireMigrationImport succeeded although the handler has no array store: every import would answer 501")
	}
	w.handler.ArrayStore = w.arrays
	if err := wire(); err != nil {
		t.Errorf("wireMigrationImport with both = %v", err)
	}
	if _, err := w.scheduler.Submit(context.Background(), job.TypeMigrationImport, nil, []byte(`{}`)); err == nil {
		t.Error("a migration_import job was admitted although this registry never got one")
	}
}

// main.go must wire the import with the real unit mounter of the array's own
// units (never a read-write mount of a source disk), the array sequence's
// rebuild, and the scheduler's question whether an import is pending: without
// the first the operation answers 500 from an unregistered job, without the
// last nothing stops a sync beside a read-only adoption.
func TestMain_WiresTheImportAndTheScheduler(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var imported, gated bool
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == "wireMigrationImport" && len(call.Args) == 7 {
				handler, _ := call.Args[0].(*ast.Ident)
				registry, _ := call.Args[1].(*ast.Ident)
				arrays, _ := call.Args[2].(*ast.Ident)
				mounter, _ := call.Args[5].(*ast.CallExpr)
				ready, _ := call.Args[6].(*ast.Ident)
				if handler != nil && registry != nil && arrays != nil && mounter != nil && ready != nil &&
					handler.Name == "handler" && registry.Name == "registry" && arrays.Name == "arrayStore" && ready.Name == "rebuildArraySequence" {
					if id, ok := mounter.Fun.(*ast.Ident); ok && id.Name == "newArrayDiskMounter" && len(mounter.Args) == 2 {
						if lit, ok := mounter.Args[1].(*ast.CompositeLit); ok {
							if sel, ok := lit.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "SystemdMounter" {
								imported = true
							}
						}
					}
				}
			}
		case *ast.SelectorExpr:
			if fn.Sel.Name == "SetMigrationPending" && len(call.Args) == 1 {
				if recv, ok := fn.X.(*ast.Ident); ok && recv.Name == "scheduler" {
					if arg, ok := call.Args[0].(*ast.SelectorExpr); ok && arg.Sel.Name == "MigrationPending" {
						if x, ok := arg.X.(*ast.Ident); ok && x.Name == "arrayStore" {
							gated = true
						}
					}
				}
			}
		}
		return true
	})
	if !imported {
		t.Error("main.go does not call wireMigrationImport(handler, registry, arrayStore, generator, linuxDisks.Exec, newArrayDiskMounter(linuxDisks.Exec, disk.SystemdMounter{...}), rebuildArraySequence)")
	}
	if !gated {
		t.Error("main.go does not call scheduler.SetMigrationPending(arrayStore.MigrationPending)")
	}
}

// While an import is pending the array sequence the daemon builds is read-only
// all the way down: the disks' units, the check at start and the catch-all.
func TestNewArraySequence_APendingImportIsReadOnlyAndDeviceBound(t *testing.T) {
	ctx := context.Background()
	w := newContainersWiringHarness(t)
	for _, q := range []string{"DELETE FROM array_disks", "DELETE FROM array_settings"} {
		if _, err := w.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	const link = "/dev/disk/by-id/ata-X_DATA1-part1"
	if err := w.arrays.PutPendingArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now()}, []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "u1", Serial: "DATA1", Mountpoint: "/mnt/disk1", MountSource: link},
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/sdd", Filesystem: "ext4", FSUUID: "u2", Serial: "DATA2", Mountpoint: "/mnt/disk2"},
	}, []store.RecordedDisk{{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sdb", Size: 1, Serial: "PAR1"}}); err != nil {
		t.Fatal(err)
	}
	seq, err := newArraySequence(ctx, w.scheduler, w.arrays, store.NewShareStore(w.db), disk.NewFakeProvider(), disk.NewFakeRunner(), nil)
	if err != nil || seq == nil {
		t.Fatalf("newArraySequence = %v, %v", seq, err)
	}
	if len(seq.Disks) != 2 {
		t.Fatalf("disks = %d, want the two data disks and none for the recorded parity", len(seq.Disks))
	}
	first, ok := seq.Disks[0].(guardedSlotMount)
	if !ok {
		t.Fatalf("disk 0 is %T", seq.Disks[0])
	}
	unit := first.inner.(disk.MountUnitController).Unit
	if !unit.ReadOnly || unit.What != link {
		t.Errorf("disk 1 unit = %+v, want read-only and bound to its by-id link", unit)
	}
	check, ok := seq.DiskCheck.(job.ArrayDiskUUIDCheck)
	if !ok || len(check.Disks) != 2 || check.Disks[0].What != link || check.Disks[1].What != "" {
		t.Errorf("start check = %+v, want the first disk confirmed by its device as well as its UUID", seq.DiskCheck)
	}
	mc, ok := seq.CatchAll.(pool.MountController)
	if !ok || !mc.Mnt.ReadOnly || mc.Mnt.What != "/mnt/disk1=RO:/mnt/disk2=RO" {
		t.Errorf("catch-all = %+v, want a read-only pool over RO branches", seq.CatchAll)
	}
	if len(seq.ShareMounts) != 0 {
		t.Errorf("share mounts = %d, want none: shares come with their own part of the import", len(seq.ShareMounts))
	}

	// The same rows of an ordinary array stay read-write.
	if _, err := w.db.Exec("UPDATE array_settings SET migration_pending = 0"); err != nil {
		t.Fatal(err)
	}
	seq, err = newArraySequence(ctx, w.scheduler, w.arrays, store.NewShareStore(w.db), disk.NewFakeProvider(), disk.NewFakeRunner(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if u := seq.Disks[0].(guardedSlotMount).inner.(disk.MountUnitController).Unit; u.ReadOnly {
		t.Errorf("an ordinary array's unit is read-only: %+v", u)
	}
	if mc := seq.CatchAll.(pool.MountController); mc.Mnt.ReadOnly || strings.Contains(mc.Mnt.What, "=RO") {
		t.Errorf("an ordinary array's pool is read-only: %+v", mc.Mnt)
	}
}

// POST /migrate/verify is reachable through the daemon's own server, job registry
// and scheduler: refused until an import is pending, admitted beside the pending
// import that refuses every other topology job, and its job reads the adopted
// disks from the array store, checks their mounts against the kernel's mount
// table through the daemon's runner and records its result in the session. The
// adopted disk's mountpoint is not a directory on this machine, so the verify
// cannot read it and must fail, never pass.
func TestMigrationVerifyWiring_IsReachableOverHTTPWhileTheImportIsPending(t *testing.T) {
	im := wireImport(t)
	w := im.w

	if status, body := w.doBody(t, http.MethodPost, "/migrate/verify", ``); status != http.StatusConflict || !bytes.Contains(body, []byte("no_import_pending")) {
		t.Fatalf("POST /migrate/verify before an import = %d %s, want 409 no_import_pending", status, body)
	}
	im.scan(t)
	status, body := w.doBody(t, http.MethodPost, "/migrate/import", importBody)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/import = %d %s", status, body)
	}
	var imp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &imp); err != nil {
		t.Fatal(err)
	}
	if done := w.awaitJobByID(t, imp.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("import job = %s %s", done.Status, done.ErrorMessage)
	}

	status, body = w.doBody(t, http.MethodPost, "/migrate/verify", ``)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/verify while the import is pending = %d %s, want 200", status, body)
	}
	var queued struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		Class string `json:"class"`
	}
	if err := json.Unmarshal(body, &queued); err != nil || queued.Type != "migration_verify" || queued.Class != "topology" {
		t.Fatalf("queued job = %s (%v)", body, err)
	}
	done := w.awaitJobByID(t, queued.ID)
	if done.Status != job.StatusFailed {
		t.Fatalf("verify job = %s %s, want failed: the adopted disk's mountpoint is not readable here", done.Status, done.ErrorMessage)
	}
	var sawDisk, sawPool bool
	for _, c := range im.runner.Calls() {
		args := strings.Join(c.Args, " ")
		if c.Name == "findmnt" && strings.HasSuffix(args, "OPTIONS /mnt/disk1") {
			sawDisk = true
		}
	}
	for _, c := range im.runner.Calls() {
		if c.Name == "findmnt" && strings.HasSuffix(strings.Join(c.Args, " "), "OPTIONS "+pool.CatchAllPath) {
			sawPool = true
		}
	}
	if !sawDisk || !sawPool {
		t.Errorf("the mount table was not asked about the adopted disk (%v) and the pool (%v) before they were read", sawDisk, sawPool)
	}
	status, body = w.do(t, http.MethodGet, "/migrate")
	var session struct {
		Phase  string `json:"phase"`
		Verify struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"verify"`
	}
	if err := json.Unmarshal(body, &session); status != http.StatusOK || err != nil {
		t.Fatalf("GET /migrate = %d %s (%v)", status, body, err)
	}
	if session.Phase != "verify_failed" || session.Verify.Status != "failed" || !strings.Contains(session.Verify.Error, "disk1") {
		t.Errorf("GET /migrate = %s, want the verify_failed phase and a failed result naming the unreadable disk", body)
	}
}
