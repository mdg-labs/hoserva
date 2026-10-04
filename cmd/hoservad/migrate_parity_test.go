package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/migrate"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/store"
)

// mountScripts is the kernel's mount table of the point of no return's test: a
// mount scripts what findmnt then reports for it, as the kernel would, and an
// unmount forgets it.
type mountScripts struct {
	mu       sync.Mutex
	runner   *disk.FakeRunner
	arrays   *store.ArrayStore
	mounts   []string
	unmounts []string
}

func (m *mountScripts) record(where, uuid, opts string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mounts = append(m.mounts, where+" "+strings.SplitN(opts, ",", 2)[0])
	m.runner.Script("findmnt", []string{"-n", "-o", "UUID", where}, []byte(uuid+"\n"), nil)
	m.runner.Script("findmnt", []string{"-n", "-o", "OPTIONS", where}, []byte(opts+"\n"), nil)
}

// Mount is the unit mounter main.go gives the job.
func (m *mountScripts) Mount(_ context.Context, u disk.MountUnit) error {
	opts := "rw,relatime"
	if u.ReadOnly {
		opts = "ro,nosuid,nodev,noexec,noatime"
	}
	m.record(u.Where, u.UUID, opts)
	return nil
}

func (m *mountScripts) Unmount(_ context.Context, u disk.MountUnit) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unmounts = append(m.unmounts, u.Where)
	return nil
}

// scriptPool is the catch-all of the rebuilt array sequence: it mounts nothing
// for real, and is read-only while the migration is pending.
type scriptPool struct{ m *mountScripts }

func (scriptPool) Where() string { return pool.CatchAllPath }
func (p scriptPool) Mount(ctx context.Context) error {
	pending, _ := p.m.arrays.MigrationPending(ctx)
	opts := "rw,relatime"
	if pending {
		opts = "ro,nosuid,nodev,relatime"
	}
	p.m.record(pool.CatchAllPath, "pool", opts)
	return nil
}
func (p scriptPool) Unmount(context.Context) error {
	p.m.mu.Lock()
	defer p.m.mu.Unlock()
	p.m.unmounts = append(p.m.unmounts, pool.CatchAllPath)
	return nil
}

// recordingFS is the share service's filesystem once the migration is past its
// point of no return: it accepts the writes the step makes and writes nothing,
// so no test ever creates a directory under /mnt.
type recordingFS struct {
	share.OSFS
	mu    sync.Mutex
	calls []string
}

func (f *recordingFS) note(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	return nil
}
func (f *recordingFS) MkdirAll(path string, _ os.FileMode) error { return f.note("MkdirAll " + path) }
func (f *recordingFS) Chmod(path string, _ os.FileMode) error    { return f.note("Chmod " + path) }
func (f *recordingFS) Chown(path string, _, _ int) error         { return f.note("Chown " + path) }

// refusingSnapraid is the engine's runner in the test: a sync the daemon queues
// reaches it and fails, so no test ever runs a real snapraid.
type refusingSnapraid struct{}

func (refusingSnapraid) Start(context.Context, string, ...string) (parity.Process, error) {
	return nil, errors.New("the test runs no snapraid")
}

type parityWiring struct {
	*importWiring
	table    *mountScripts
	reg      *parityRegistrar
	fs       *recordingFS
	sessions *store.MigrationSessionStore
}

const (
	pwParityUUID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	pwData1UUID  = "10000000-0000-4000-8000-000000000002"
	pwData2UUID  = "10000000-0000-4000-8000-000000000003"
)

const parityImportBody = `{"confirm":true,"roles":[{"role":"parity","serial":"PARSERIAL"},{"role":"data","serial":"DATASERIAL"},{"role":"data","serial":"DATA2SERIAL"}]}`

// wireParity wires the migrator, its import and the point of no return the way
// main.go does, over a machine with a parity disk and two data disks (Q18 needs
// the third content-file copy a second data disk gives) and a daemon whose
// parity registrar has not seen a snapraid.conf: so the test shows sync, scrub
// and fix registered by the job itself, with no restart.
func wireParity(t *testing.T) *parityWiring {
	t.Helper()
	im := wireImport(t)
	w := im.w
	im.disks.AddDisk("/dev/sdd", disk.Disk{Serial: "DATA2SERIAL", Size: 1 << 40, Filesystem: "xfs", FSDevice: "/dev/sdd1", FSUUID: pwData2UUID})
	im.runner.Script("findmnt", []string{"-n", "-o", "UUID", "/mnt/disk2"}, []byte(pwData2UUID+"\n"), nil)
	im.runner.Script("findmnt", []string{"-n", "-o", "OPTIONS", "/mnt/disk2"}, []byte("ro,nosuid,nodev,noexec,noatime\n"), nil)
	im.runner.Script("blkid", []string{"-p", "-s", "UUID", "-o", "value", "/dev/sdb"}, []byte(pwParityUUID+"\n"), nil)
	w.scheduler.SetMigrationPending(w.arrays.MigrationUnfinished)

	table := &mountScripts{runner: im.runner, arrays: w.arrays}
	shares := store.NewShareStore(w.db)
	rebuild := func(ctx context.Context) error {
		seq, err := newArraySequence(ctx, w.scheduler, w.arrays, shares, im.disks, im.runner, nil)
		if err != nil {
			return err
		}
		if seq != nil {
			seq.CatchAll = scriptPool{m: table}
		}
		w.handler.SetArray(seq)
		return nil
	}
	reg := &parityRegistrar{
		configRoot: im.root, stateDir: t.TempDir(), db: w.db, registry: w.registry, handler: w.handler, shareStore: shares,
		arrayStore: w.arrays, chainGuard: &diffGuardHolder{}, generator: im.generator, diskUnits: disk.NewFakeMounter(),
		mounts: job.NewFakeMountTable(), snapraidRunner: refusingSnapraid{},
	}
	// The strict hook main.go builds, over this test's own rebuild: the sequence
	// rebuilt from the record, then the registrar notices the snapraid.conf the job
	// has just written.
	reg.arrayReady = func(ctx context.Context) error {
		if err := rebuild(ctx); err != nil {
			return err
		}
		return reg.ensure(ctx)
	}
	fs := &recordingFS{}
	im.shares.FS = fs
	im.shares.PostCommit = rebuild
	if err := wireMigrationParity(w.handler, w.registry, w.scheduler, im.disks, w.arrays, im.generator, im.runner, table, reg.callArrayReady, im.shares); err != nil {
		t.Fatal(err)
	}
	return &parityWiring{importWiring: im, table: table, reg: reg, fs: fs, sessions: store.NewMigrationSessionStore(w.db)}
}

func (pw *parityWiring) scanAndImport(t *testing.T) {
	t.Helper()
	w := pw.w
	ini := "[\"parity\"]\nidx=\"0\"\nid=\"M_PARSERIAL\"\nsize=\"1000\"\nstatus=\"DISK_OK\"\ntype=\"Parity\"\n" +
		"[\"disk1\"]\nidx=\"1\"\nid=\"M_DATASERIAL\"\nsize=\"900\"\nstatus=\"DISK_OK\"\ntype=\"Data\"\nfsType=\"xfs\"\n" +
		"[\"disk2\"]\nidx=\"2\"\nid=\"M_DATA2SERIAL\"\nsize=\"900\"\nstatus=\"DISK_OK\"\ntype=\"Data\"\nfsType=\"xfs\"\n"
	status, body := w.uploadScan(t, flashBackupZipWith(t, "7.3.2", map[string]string{"config/hoserva/disks.ini": ini}), false)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/scan = %d %s", status, body)
	}
	var q struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &q)
	if done := w.awaitJobByID(t, q.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("scan job = %s %s", done.Status, done.ErrorMessage)
	}
	status, body = w.doBody(t, http.MethodPost, "/migrate/import", parityImportBody)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/import = %d %s", status, body)
	}
	_ = json.Unmarshal(body, &q)
	if done := w.awaitJobByID(t, q.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("import job = %s %s", done.Status, done.ErrorMessage)
	}
}

func (pw *parityWiring) setVerify(t *testing.T, status string) {
	t.Helper()
	ctx := context.Background()
	row, _, err := pw.sessions.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status == "" {
		row.Verify = nil
	} else if row.Verify, err = json.Marshal(migrate.VerifyResult{Status: status, StartedAt: time.Now(), FinishedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := pw.sessions.Put(ctx, row); err != nil {
		t.Fatal(err)
	}
}

func (pw *parityWiring) assertNothingFormatted(t *testing.T, when string) {
	t.Helper()
	if calls := pw.disks.FormatCalls(); len(calls) != 0 {
		t.Errorf("%s: formatted %v", when, calls)
	}
	for _, c := range pw.runner.Calls() {
		if strings.HasPrefix(c.Name, "mkfs") {
			t.Errorf("%s: ran %s %v", when, c.Name, c.Args)
		}
	}
}

// POST /migrate/initialize-parity is reachable through the daemon's own server,
// job registry and scheduler, and does the whole point of no return: refused
// without a passing verify and without the exact typed confirmation, each time
// with nothing formatted; then formats the parity disk and no data disk,
// records the array, mounts the data disks read-write, generates snapraid.conf,
// registers sync, scrub and fix with no restart, and queues the initial sync.
func TestMigrationParityWiring_CrossesThePointOfNoReturnOverHTTP(t *testing.T) {
	pw := wireParity(t)
	w := pw.w
	ctx := context.Background()

	if status, body := w.doBody(t, http.MethodPost, "/migrate/initialize-parity", `{"confirmation":"ERASE /dev/sdb"}`); status != http.StatusConflict || !bytes.Contains(body, []byte("no_import_pending")) {
		t.Fatalf("POST /migrate/initialize-parity before an import = %d %s, want 409 no_import_pending", status, body)
	}
	pw.scanAndImport(t)
	if v := w.migration(t); v.Phase != "imported" {
		t.Fatalf("phase = %q, want imported", v.Phase)
	}

	for name, verify := range map[string]string{"no verify": "", "a failed verify": migrate.VerifyFailed} {
		pw.setVerify(t, verify)
		status, body := w.doBody(t, http.MethodPost, "/migrate/initialize-parity", `{"confirmation":"ERASE /dev/sdb"}`)
		if status != http.StatusConflict || !bytes.Contains(body, []byte("verify_required")) {
			t.Fatalf("%s: POST /migrate/initialize-parity = %d %s, want 409 verify_required", name, status, body)
		}
	}
	pw.assertNothingFormatted(t, "without a passing verify")

	pw.setVerify(t, migrate.VerifyPassed)
	status, body := w.do(t, http.MethodGet, "/migrate")
	var session struct {
		Phase      string `json:"phase"`
		ParityInit struct {
			Confirmation      string   `json:"confirmation"`
			UnprotectedWindow string   `json:"unprotectedWindow"`
			Rollback          []string `json:"rollback"`
			Erases            []struct {
				Role   string `json:"role"`
				Device string `json:"device"`
			} `json:"erases"`
		} `json:"parityInit"`
	}
	if err := json.Unmarshal(body, &session); status != http.StatusOK || err != nil {
		t.Fatalf("GET /migrate = %d %s (%v)", status, body, err)
	}
	pi := session.ParityInit
	if session.Phase != "verified" || pi.Confirmation != "ERASE /dev/sdb" || len(pi.Erases) != 1 || pi.Erases[0].Device != "/dev/sdb" || pi.UnprotectedWindow == "" || len(pi.Rollback) == 0 {
		t.Fatalf("GET /migrate = %s, want the verified phase and the parity disk as the only device erased", body)
	}
	if status, body := w.doBody(t, http.MethodPost, "/migrate/initialize-parity", `{"confirmation":"ERASE /dev/sdb, /dev/sdc"}`); status != http.StatusConflict || !bytes.Contains(body, []byte("confirmation_required")) {
		t.Fatalf("a confirmation naming a data disk = %d %s, want 409 confirmation_required", status, body)
	}
	pw.assertNothingFormatted(t, "a wrong confirmation")
	if _, err := os.Stat(filepath.Join(pw.root, "snapraid.conf")); err == nil {
		t.Fatal("snapraid.conf exists before the point of no return")
	}

	status, body = w.doBody(t, http.MethodPost, "/migrate/initialize-parity", `{"confirmation":"ERASE /dev/sdb"}`)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/initialize-parity = %d %s", status, body)
	}
	var queued struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		Class string `json:"class"`
	}
	if err := json.Unmarshal(body, &queued); err != nil || queued.Type != "migration_parity" || queued.Class != "topology" {
		t.Fatalf("queued job = %s (%v)", body, err)
	}
	done := w.awaitJobByID(t, queued.ID)
	if done.Status != job.StatusSucceeded {
		t.Fatalf("migration_parity job = %s %s", done.Status, done.ErrorMessage)
	}

	if calls := pw.disks.FormatCalls(); len(calls) != 1 || calls[0] != "/dev/sdb" {
		t.Errorf("formatted %v, want only the parity disk /dev/sdb", calls)
	}
	settings, disks, err := w.arrays.GetArray(ctx)
	if err != nil || settings.MigrationPending || len(disks) != 3 {
		t.Fatalf("array = %+v %+v, %v", settings, disks, err)
	}
	var parityRow store.ArrayDisk
	for _, d := range disks {
		if d.Role == store.ArrayRoleParity {
			parityRow = d
		}
	}
	if parityRow.Mountpoint != "/mnt/parity1" || parityRow.FSUUID != pwParityUUID || parityRow.Serial != "PARSERIAL" {
		t.Errorf("parity row = %+v", parityRow)
	}
	if unfinished, err := w.arrays.MigrationUnfinished(ctx); err != nil || unfinished {
		t.Errorf("MigrationUnfinished = %v, %v after the job", unfinished, err)
	}
	if len(pw.table.mounts) < 4 {
		t.Errorf("mounted %v, want the data disks, the parity disk and the pool", pw.table.mounts)
	}
	for _, m := range pw.table.mounts {
		if strings.HasSuffix(m, " ro") {
			t.Errorf("mounted %v: nothing is mounted read-only after the point of no return", pw.table.mounts)
		}
	}
	conf, err := os.ReadFile(filepath.Join(pw.root, "snapraid.conf"))
	if err != nil || !strings.Contains(string(conf), "parity /mnt/parity1/snapraid.parity\n") || !strings.Contains(string(conf), "data d2 /mnt/disk2/\n") {
		t.Errorf("snapraid.conf = %s, %v", conf, err)
	}

	// With no restart: sync, scrub and fix are registered by this job's own
	// ArrayReady hook, and the sync it queued was admitted by the scheduler (a
	// sync submitted before the job finished would have been refused).
	for _, typ := range []job.Type{job.TypeSync, job.TypeScrub, job.TypeFix} {
		assertAlreadyRegistered(t, w.registry, typ)
	}
	jobs, err := w.handler.Store.List(ctx, job.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var syncID string
	for _, j := range jobs {
		if j.Type == job.TypeSync {
			syncID = j.ID
		}
	}
	if syncID == "" {
		t.Fatal("the initial sync was not queued")
	}
	if sj := w.awaitJobByID(t, syncID); sj.Status != job.StatusFailed || !strings.Contains(sj.ErrorMessage, "the test runs no snapraid") {
		t.Errorf("the queued sync ended %s: %q, want it to have reached the engine (the test's runner refuses it)", sj.Status, sj.ErrorMessage)
	}
	if v := w.migration(t); v.Phase != "scanned" {
		t.Errorf("phase = %q after the point of no return, want scanned", v.Phase)
	}
}

// main.go must wire the point of no return with the real provider, the array
// disks' own unit mounter, the strict topology hook the parity registrar keeps
// (parityReg.callArrayReady: it is what registers sync, scrub and fix once
// snapraid.conf exists, and it returns a failed live update) and the scheduler
// that queues the initial sync; and the scheduler must ask whether a migration
// is unfinished, not only whether one is pending.
func TestMain_WiresThePointOfNoReturnAndTheUnfinishedMigrationGate(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var wired, gated bool
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == "wireMigrationParity" && len(call.Args) == 10 {
				name := func(i int) string {
					id, _ := call.Args[i].(*ast.Ident)
					if id == nil {
						return ""
					}
					return id.Name
				}
				ready, _ := call.Args[8].(*ast.SelectorExpr)
				mounter, _ := call.Args[7].(*ast.CallExpr)
				if name(0) == "handler" && name(1) == "registry" && name(2) == "scheduler" && name(3) == "disks" && name(4) == "arrayStore" && name(9) == "shareService" &&
					ready != nil && ready.Sel.Name == "callArrayReady" && mounter != nil {
					if id, ok := mounter.Fun.(*ast.Ident); ok && id.Name == "newArrayDiskMounter" {
						wired = true
					}
				}
			}
		case *ast.SelectorExpr:
			if fn.Sel.Name == "SetMigrationPending" && len(call.Args) == 1 {
				if recv, ok := fn.X.(*ast.Ident); ok && recv.Name == "scheduler" {
					if arg, ok := call.Args[0].(*ast.SelectorExpr); ok && arg.Sel.Name == "MigrationUnfinished" {
						gated = true
					}
				}
			}
		}
		return true
	})
	if !wired {
		t.Error("main.go does not call wireMigrationParity(handler, registry, scheduler, disks, arrayStore, generator, linuxDisks.Exec, newArrayDiskMounter(...), parityReg.callArrayReady, shareService)")
	}
	if !gated {
		t.Error("main.go does not call scheduler.SetMigrationPending(arrayStore.MigrationUnfinished)")
	}
}

func TestWireMigrationParity_FailsWithoutTheSessionOrTheArrayStore(t *testing.T) {
	w := newContainersWiringHarness(t)
	wire := func() error {
		return wireMigrationParity(w.handler, job.NewRegistry(), w.scheduler, disk.NewFakeProvider(), w.arrays, nil, disk.NewFakeRunner(), disk.NewFakeMounter(), func(context.Context) error { return nil }, &share.Service{})
	}
	if err := wire(); err == nil {
		t.Error("wireMigrationParity succeeded with no migration session")
	}
	if err := wireMigration(context.Background(), w.handler, w.registry, disk.NewFakeProvider(), disk.NewFakeReadOnlyMounter(), disk.NewFakeRunner(), store.NewMigrationSessionStore(w.db), w.root); err != nil {
		t.Fatal(err)
	}
	if err := wire(); err == nil {
		t.Error("wireMigrationParity succeeded although the handler has no array store")
	}
	w.handler.ArrayStore = w.arrays
	if err := wire(); err != nil {
		t.Errorf("wireMigrationParity with both = %v", err)
	}
}
