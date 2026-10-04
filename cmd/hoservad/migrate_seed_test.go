package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"
)

const seedIni = "[\"media\"]\nname=\"media\"\n[\"documents\"]\nname=\"documents\"\n[\"Family Photos\"]\nname=\"Family Photos\"\n"

// scanWithConfig scans a Flash Backup that carries the share configs, the
// accounts and the disk roles of wireImport's machine.
func (im *importWiring) scanWithConfig(t *testing.T) {
	t.Helper()
	ini := "[\"parity\"]\nidx=\"0\"\nid=\"M_PARSERIAL\"\nsize=\"1000\"\nstatus=\"DISK_OK\"\ntype=\"Parity\"\n[\"disk1\"]\nidx=\"1\"\nid=\"M_DATASERIAL\"\nsize=\"900\"\nstatus=\"DISK_OK\"\ntype=\"Data\"\nfsType=\"xfs\"\n"
	data := flashBackupZipWith(t, "7.3.2", map[string]string{
		"config/hoserva/disks.ini":  ini,
		"config/hoserva/shares.ini": seedIni,
		"config/share.cfg":          "shareUserExclude=\"\"\n",
		"config/shares/media.cfg": "shareAllocator=\"highwater\"\nshareSplitLevel=\"2\"\nshareFloor=\"50000000\"\nshareUseCache=\"only\"\n" +
			"shareExport=\"e\"\nshareSecurity=\"private\"\nshareReadList=\"bob\"\nshareWriteList=\"alice\"\n",
		"config/shares/documents.cfg":     "shareAllocator=\"fillup\"\nshareUseCache=\"no\"\nshareExport=\"e\"\nshareSecurity=\"public\"\nshareSplitLevel=\"\"\nshareFloor=\"0\"\n",
		"config/shares/Family Photos.cfg": "shareAllocator=\"mostfree\"\nshareUseCache=\"no\"\nshareExport=\"e\"\nshareSecurity=\"public\"\n",
		"config/passwd":                   "root:x:0:0:Console:/root:/bin/bash\nalice:HASH-IN-PASSWD:1000:100:Alice:/dev/null:/bin/false\nbob:x:1001:100::/dev/null:/bin/false\n",
	})
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

type seededShare struct {
	Name         string `json:"name"`
	CacheMode    string `json:"cacheMode"`
	CreatePolicy string `json:"createPolicy"`
	MinFreeSpace string `json:"minFreeSpace"`
	Smb          struct {
		Enabled bool `json:"enabled"`
		Guest   bool `json:"guest"`
	} `json:"smb"`
	Migration *struct {
		TargetCacheMode string   `json:"targetCacheMode"`
		Notes           []string `json:"notes"`
	} `json:"migration"`
}

func (im *importWiring) listShares(t *testing.T) []seededShare {
	t.Helper()
	status, body := im.w.do(t, http.MethodGet, "/shares")
	var out struct {
		Shares []seededShare `json:"shares"`
	}
	if err := json.Unmarshal(body, &out); status != http.StatusOK || err != nil {
		t.Fatalf("GET /shares = %d %s (%v)", status, body, err)
	}
	return out.Shares
}

func (im *importWiring) runImport(t *testing.T) *job.Job {
	t.Helper()
	status, body := im.w.doBody(t, http.MethodPost, "/migrate/import", importBody)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/import = %d %s", status, body)
	}
	var queued struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &queued); err != nil {
		t.Fatal(err)
	}
	return im.w.awaitJobByID(t, queued.ID)
}

// The import job seeds the scan's shares and accounts through the daemon's own
// wiring, and what it creates shows in listShares and listUsers, without one
// create, chmod or chown reaching an adopted disk (#299).
func TestMigrationImportWiring_SeedsSharesAndUsersWithoutWritingToAdoptedDisks(t *testing.T) {
	im := wireImport(t)
	im.scanWithConfig(t)
	done := im.runImport(t)
	if done.Status != job.StatusSucceeded {
		t.Fatalf("import job = %s %s", done.Status, done.ErrorMessage)
	}
	if len(im.shareFS.calls) != 0 {
		t.Errorf("seeding wrote to the adopted disks: %v", im.shareFS.calls)
	}

	shares := im.listShares(t)
	byName := map[string]seededShare{}
	for _, sh := range shares {
		byName[sh.Name] = sh
	}
	if len(shares) != 2 || byName["media"].Name == "" || byName["documents"].Name == "" {
		t.Fatalf("shares = %+v, want media and documents (and not the share named with a space)", shares)
	}
	media := byName["media"]
	if media.CacheMode != "array-only" || media.CreatePolicy != "mfs" || media.MinFreeSpace != "50000000K" || !media.Smb.Enabled || media.Smb.Guest {
		t.Errorf("media = %+v", media)
	}
	if media.Migration == nil || media.Migration.TargetCacheMode != "cache-only" || !strings.Contains(strings.Join(media.Migration.Notes, "\n"), "mapped to Balance across disks") {
		t.Errorf("media migration = %+v, want the deferred cache-only mode and the High-water note", media.Migration)
	}
	if docs := byName["documents"]; docs.CreatePolicy != "ff" || docs.Migration != nil || !docs.Smb.Guest {
		t.Errorf("documents = %+v", docs)
	}

	status, body := im.w.do(t, http.MethodGet, "/users")
	var users struct {
		Users []struct {
			ID            string `json:"id"`
			Username      string `json:"username"`
			Role          string `json:"role"`
			HasCredential bool   `json:"hasCredential"`
		} `json:"users"`
	}
	if err := json.Unmarshal(body, &users); status != http.StatusOK || err != nil {
		t.Fatalf("GET /users = %d %s (%v)", status, body, err)
	}
	ids := map[string]string{}
	for _, u := range users.Users {
		if u.Username == "alice" || u.Username == "bob" {
			if u.Role != "share-only" || u.HasCredential {
				t.Errorf("%s = %+v, want a share-only account without a credential", u.Username, u)
			}
			ids[u.Username] = u.ID
		}
	}
	if len(ids) != 2 {
		t.Fatalf("users = %s, want alice and bob", body)
	}
	status, body = im.w.do(t, http.MethodGet, "/shares/media/permissions")
	if status != http.StatusOK || !bytes.Contains(body, []byte("read-write")) || !bytes.Contains(body, []byte("read-only")) {
		t.Errorf("GET /shares/media/permissions = %d %s, want alice read-write and bob read-only", status, body)
	}

	// A share is a directory of the read-only pool: the only unit is the
	// catch-all, and no share is mounted or has a mover target.
	if units := im.pendingUnits(t); len(units) != 2 || units["mnt-user.mount"] == "" || units["mnt-disk1.mount"] == "" {
		t.Errorf("units = %v, want the data disk and the catch-all", units)
	}
	smb, err := os.ReadFile(filepath.Join(im.root, "samba", "smb.conf"))
	if err != nil || !strings.Contains(string(smb), "[media]") || !strings.Contains(string(smb), "[documents]") || strings.Contains(string(smb), "read only = no") {
		t.Errorf("smb.conf = %s, %v: want both shares exported read-only", smb, err)
	}
	im.shareMounts.mu.Lock()
	nMounts := len(im.shareMounts.mounted)
	im.shareMounts.mu.Unlock()
	if nMounts != 0 {
		t.Errorf("share mounts = %d, want none while the import is pending", nMounts)
	}
	if seq := im.w.handler.CurrentArray(); seq == nil || len(seq.ShareMounts) != 0 || seq.CatchAll == nil {
		t.Fatalf("the rebuilt array sequence = %+v, want the catch-all and no share mounts", seq)
	}
}

// A seed that fails undoes the whole import: no array, no share, no account.
func TestMigrationImportWiring_AFailedSeedLeavesNothingBehind(t *testing.T) {
	im := wireImport(t)
	im.scanWithConfig(t)
	// A host smb.conf Hoserva has not taken over refuses the seed.
	if err := os.MkdirAll(filepath.Join(im.root, "samba"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(im.root, "samba", "smb.conf"), []byte("[global]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := im.runImport(t)
	if done.Status != job.StatusFailed || !strings.Contains(done.ErrorMessage, "seeding the shares and accounts") {
		t.Fatalf("import job = %s %q, want it failed by the seed", done.Status, done.ErrorMessage)
	}
	ctx := context.Background()
	if exists, err := im.w.arrays.Exists(ctx); err != nil || exists {
		t.Errorf("array exists = %v, %v after a failed seed", exists, err)
	}
	if shares := im.listShares(t); len(shares) != 0 {
		t.Errorf("shares left after a failed seed: %+v", shares)
	}
	status, body := im.w.do(t, http.MethodGet, "/users")
	if status != http.StatusOK || bytes.Contains(body, []byte(`"alice"`)) || bytes.Contains(body, []byte(`"bob"`)) {
		t.Errorf("GET /users = %d %s: the accounts of a failed seed are still there", status, body)
	}
	if len(im.shareFS.calls) != 0 {
		t.Errorf("a failed seed wrote to the adopted disks: %v", im.shareFS.calls)
	}
}

// What the job tells the user: every account with the reminder to set a
// password, every share it did not create with the reason, and no password
// material from the flash.
func TestSeedMigration_ReportsEveryAccountAndEverySkippedShare(t *testing.T) {
	im := wireImport(t)
	im.scanWithConfig(t)
	ctx := context.Background()
	if err := im.w.arrays.PutPendingArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now().UTC()}, []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "u1", Serial: "DATASERIAL", Mountpoint: "/mnt/disk1"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := seedMigration(ctx, &out, im.w.handler.Migration.SeedPlan, im.shares); err != nil {
		t.Fatalf("seedMigration: %v", err)
	}
	log := out.String()
	for _, want := range []string{
		"account alice created without a password: set one in the web UI",
		"account bob created without a password: set one in the web UI",
		`share "Family Photos" was not created: the name is not valid for Hoserva`,
		"not renamed",
		"share media is array-only for now; its cache mode cache-only is applied once the cache exists",
		"mapped to Balance across disks",
		"share documents created: create policy ff",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "HASH-IN-PASSWD") {
		t.Error("password material from the flash reached the log")
	}
	out.Reset()
	if err := seedMigration(ctx, &out, im.w.handler.Migration.SeedPlan, im.shares); err != nil {
		t.Fatalf("a second seed: %v", err)
	}
	if !strings.Contains(out.String(), "share media already exists and is left as it is") || !strings.Contains(out.String(), "account alice already exists and is left as it is") {
		t.Errorf("a second seed log:\n%s", out.String())
	}
}

// pendingUnits reads every generated mount unit, by file name.
func (im *importWiring) pendingUnits(t *testing.T) map[string]string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(im.root, "systemd", "system", "*.mount"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no mount units under %s: %v", im.root, err)
	}
	out := map[string]string{}
	for _, p := range paths {
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(p)] = string(body)
	}
	return out
}

// assertPoolUnitsReadOnly fails when the unit that mounts the pool is not
// read-only over the adopted disks, when a unit for a share or a mover target
// exists, or when anything wrote to a disk.
func (im *importWiring) assertPoolUnitsReadOnly(t *testing.T, when string) {
	t.Helper()
	units := im.pendingUnits(t)
	body, ok := units["mnt-user.mount"]
	if !ok {
		t.Errorf("%s: mnt-user.mount is not generated", when)
	} else if !strings.Contains(body, "=RO") || strings.Contains(body, "=RW") || strings.Contains(body, "=NC") || !strings.Contains(body+"\n", ",ro\n") {
		t.Errorf("%s: mnt-user.mount is not read-only over the adopted disks:\n%s", when, body)
	}
	for name := range units {
		if strings.HasPrefix(name, "mnt-user-") || strings.Contains(name, "run-hoserva") {
			t.Errorf("%s: %s is a unit for a share or a mover target", when, name)
		}
	}
	if len(im.shareFS.calls) != 0 {
		t.Errorf("%s: the share service wrote to the adopted disks: %v", when, im.shareFS.calls)
	}
}

func (im *importWiring) importSeeded(t *testing.T) {
	t.Helper()
	im.scanWithConfig(t)
	if done := im.runImport(t); done.Status != job.StatusSucceeded {
		t.Fatalf("import job = %s %s", done.Status, done.ErrorMessage)
	}
	im.assertPoolUnitsReadOnly(t, "right after the import")
}

// An import pending its point of no return must survive everything that
// regenerates the pool's files from the share service: a share create, update
// or delete through the API, and the topology hook the disk-topology jobs and
// a config import run. None writes a read-write unit or creates a directory on
// an adopted disk (comment on #299).
func TestPendingMigration_ShareChangesAndTheTopologyHookKeepThePoolReadOnly(t *testing.T) {
	im := wireImport(t)
	im.importSeeded(t)
	w := im.w

	if status, body := w.doBody(t, http.MethodPost, "/shares", `{"name":"fresh","cacheMode":"array-only"}`); status != http.StatusConflict || !bytes.Contains(body, []byte("migration_in_progress")) {
		t.Errorf("POST /shares while pending = %d %s, want 409 migration_in_progress", status, body)
	}
	if status, body := w.doBody(t, http.MethodPatch, "/shares/media", `{"smb":{"enabled":true,"guest":false,"readOnly":false,"browseable":false,"recycle":false,"timeMachine":false}}`); status != http.StatusOK {
		t.Errorf("PATCH /shares/media while pending = %d %s", status, body)
	}
	im.assertPoolUnitsReadOnly(t, "after updateShare")
	if status, body := w.doBody(t, http.MethodPost, "/shares/media/data/delete", `{"confirmation":"media"}`); status != http.StatusConflict || !bytes.Contains(body, []byte("migration_in_progress")) {
		t.Errorf("POST /shares/media/data/delete while pending = %d %s, want 409 migration_in_progress", status, body)
	}
	if status, body := w.doBody(t, http.MethodDelete, "/shares/documents", `{"confirm":true}`); status != http.StatusNoContent {
		t.Errorf("DELETE /shares/documents while pending = %d %s", status, body)
	}
	im.assertPoolUnitsReadOnly(t, "after deleteShare")

	parityReg := &parityRegistrar{configRoot: im.root, stateDir: t.TempDir(), db: w.db, registry: w.registry, handler: w.handler, shareStore: store.NewShareStore(w.db), arrayStore: w.arrays, chainGuard: &diffGuardHolder{}}
	hook := newTopologyChangedHook(im.shares, im.rebuild, parityReg, w.handler, failOnLiveUpdateFailure)
	if err := hook(context.Background()); err != nil {
		t.Fatalf("the topology hook while pending: %v", err)
	}
	im.assertPoolUnitsReadOnly(t, "after the topology hook")
}

// POST /config/import runs RegenerateConfig, which is the topology hook then
// the NUT files: it must not turn the pending pool read-write either.
func TestPendingMigration_ConfigImportRegenerationKeepsThePoolReadOnly(t *testing.T) {
	im := wireImport(t)
	im.importSeeded(t)
	parityReg := &parityRegistrar{configRoot: im.root, stateDir: t.TempDir(), db: im.w.db, registry: im.w.registry, handler: im.w.handler, shareStore: store.NewShareStore(im.w.db), arrayStore: im.w.arrays, chainGuard: &diffGuardHolder{}}
	hook := newTopologyChangedHook(im.shares, im.rebuild, parityReg, im.w.handler, failOnLiveUpdateFailure)
	wireConfigImport(im.w.handler, hook, func(context.Context) error { return nil })
	// The harness has no UPS service, so the NUT half reports that and nothing else.
	err := im.w.handler.RegenerateConfig(context.Background())
	if err == nil || !strings.Contains(err.Error(), "UPS settings service is not configured") || strings.Contains(err.Error(), "regenerating share configuration") {
		t.Fatalf("RegenerateConfig = %v, want only the missing UPS service", err)
	}
	im.assertPoolUnitsReadOnly(t, "after RegenerateConfig")
}
