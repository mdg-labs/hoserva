package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/backup"
	cfg "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

// guardedDisks is the disk provider a bare-metal restore is given: the
// restore may list the attached disks, and nothing else. Formatting, waking
// a disk to poll SMART and spinning one down all fail the test that made the
// call (Q13, doc 10 §1: a restore never formats a disk).
type guardedDisks struct {
	disk.Provider
	t *testing.T

	mu    sync.Mutex
	lists int
}

func (g *guardedDisks) List(ctx context.Context) ([]disk.Disk, error) {
	g.mu.Lock()
	g.lists++
	g.mu.Unlock()
	return g.Provider.List(ctx)
}

func (g *guardedDisks) Format(_ context.Context, dev string, _ disk.FilesystemType) error {
	g.t.Errorf("a restore formatted %s", dev)
	return errors.New("a restore never formats a disk")
}

func (g *guardedDisks) SMART(_ context.Context, dev string, _ disk.SMARTPollMode) (disk.SMARTReport, error) {
	g.t.Errorf("a restore polled SMART on %s", dev)
	return disk.SMARTReport{}, errors.New("a restore never polls a disk")
}

func (g *guardedDisks) Spindown(_ context.Context, dev string) error {
	g.t.Errorf("a restore spun down %s", dev)
	return errors.New("a restore never spins a disk down")
}

func seedRecipient(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	execAll(t, db, fmt.Sprintf(`INSERT OR REPLACE INTO backup_recipient (id, public_recipient, wrapped_identity, check_value, created_at)
		VALUES (1, 'age1%s', CAST('%s-wrapped' AS BLOB), CAST('%s-recipient-check' AS BLOB), '2026-09-01T00:00:00Z')`, name, name, name))
}

// bareMetalBox is a freshly installed box: no array, its own machine key
// check and backup recipient, a fake disk inventory, and hooks recording
// what the restore asked to be regenerated and in which order.
type bareMetalBox struct {
	*importFilesEnv
	disks *disk.FakeProvider
	guard *guardedDisks

	mu    sync.Mutex
	order []string
}

func newBareMetalBox(t *testing.T) *bareMetalBox {
	t.Helper()
	e := newImportFilesEnv(t)
	makeConfigDirs(t, e)
	execAll(t, e.db, `DELETE FROM array_disks`)
	seedRecipient(t, e.db, "box")
	b := &bareMetalBox{importFilesEnv: e, disks: disk.NewFakeProvider()}
	b.guard = &guardedDisks{Provider: b.disks, t: t}
	e.h.Disks = b.guard
	e.h.RegenerateArray = func(context.Context) error {
		b.record("array")
		return nil
	}
	config := e.h.RegenerateConfig
	e.h.RegenerateConfig = func(ctx context.Context) error {
		b.record("config")
		return config(ctx)
	}
	return b
}

func makeConfigDirs(t *testing.T, e *importFilesEnv) {
	t.Helper()
	for _, d := range []string{e.paths.ConfigRoot, e.paths.TemplatesDir, e.paths.StacksDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func (b *bareMetalBox) record(step string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.order = append(b.order, step)
}

func (b *bareMetalBox) steps() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.order)
}

// attach adds the disk the source installation's slot was recorded with, on
// the given device.
func (b *bareMetalBox) attach(dev, uuid, wwn string) {
	b.disks.AddDisk(dev, disk.Disk{WWN: wwn, Serial: "serial-" + uuid, FSUUID: uuid, Filesystem: "xfs", Size: 4 << 40})
}

// attachSourceDisks attaches every disk of the source installation's array
// on device names it did not have.
func (b *bareMetalBox) attachSourceDisks() {
	b.attach("/dev/sdx", "uuid-p1", "wwn-p1")
	b.attach("/dev/sdy", "uuid-d1", "wwn-d1")
	b.attach("/dev/sdz", "uuid-d2", "wwn-d2")
}

// newSourceInstallation is another installation: its own machine key check
// and recipient, a three-disk array, shares, users (one with TOTP) and
// schedules, a notification channel with a sealed credential, and custom
// config, templates and stacks, and a host_config row for each "kind=decision"
// given. It returns it and the archive it exported.
func newSourceInstallation(t *testing.T, hostConfig ...string) (*importFilesEnv, []byte) {
	t.Helper()
	src := newImportFilesEnv(t)
	execAll(t, src.db, `DELETE FROM array_disks`)
	seedMachineKeyCheck(t, src.db, []byte("installation-b-check-value"))
	seedRecipient(t, src.db, "source")
	execAll(t, src.db,
		`INSERT INTO array_settings (id, create_policy, min_free_space, created_at) VALUES (1, 'mfs', '20G', '2026-09-01T00:00:00Z')`,
		`INSERT INTO users (id, username, password_hash, role, totp_secret, totp_confirmed_at, totp_last_step, created_at)
			VALUES ('u1', 'alice', 'hash-a', 'admin', X'aa', '2026-09-01T00:00:00Z', 7, '2026-09-01T00:00:00Z'),
			       ('u2', 'bob', 'hash-b', 'viewer', NULL, NULL, 0, '2026-09-01T00:00:00Z')`,
		`INSERT INTO schedule_chain (id, start_time, weekly_scrub_day, mover_enabled, diff_guard_enabled, sync_enabled, scrub_enabled, config_backup_enabled, updated_at)
			VALUES (1, '03:30', 3, 1, 1, 1, 0, 1, '2026-09-01T00:00:00Z')`,
		`INSERT INTO schedule_jobs (job_id, enabled, frequency, start_time, updated_at) VALUES ('appdata_backup', 1, 'weekly', '02:00', '2026-09-01T00:00:00Z')`,
		`INSERT INTO notify_channels (id, name, type, enabled, config, secret, created_at, updated_at) VALUES ('c1', 'ops', 'gotify', 1, '{}', X'cc', 't', 't')`,
	)
	seedDisk(t, src.db, seededDisk{role: "parity", index: 1, uuid: "uuid-p1", wwn: "wwn-p1"})
	seedDisk(t, src.db, seededDisk{role: "data", index: 1, uuid: "uuid-d1", wwn: "wwn-d1"})
	seedDisk(t, src.db, seededDisk{role: "data", index: 2, uuid: "uuid-d2", wwn: "wwn-d2"})
	insertSentinelShare(t, src.db, "media")
	insertSentinelShare(t, src.db, "photos")
	for _, kd := range hostConfig {
		kind, decision, _ := strings.Cut(kd, "=")
		execAll(t, src.db, fmt.Sprintf(`INSERT INTO host_config (kind, decision, facts, applied_at) VALUES ('%s', '%s', '{}', '2026-09-01T00:00:00Z')`, kind, decision))
	}
	putTree(t, src.paths.ConfigRoot, exportedCustom)
	putTree(t, src.paths.TemplatesDir, exportedTemplates)
	putTree(t, src.paths.StacksDir, map[string]string{"web/docker-compose.yml": "exported compose", "web/meta.json": "exported meta"})
	return src, exportBytes(t, src.h)
}

func dumpQueries(t *testing.T, db *sql.DB, queries ...string) string {
	t.Helper()
	var b strings.Builder
	for _, q := range queries {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%.24s: %v\n", q, vals)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
	}
	return b.String()
}

// configuredState is what a restore must reproduce from the archive: shares,
// users, schedules and the array's roles, identities and filesystems. It
// leaves out the disks' device names, which are the attached disks', and the
// secrets sealed under the exporting installation's key.
func configuredState(t *testing.T, db *sql.DB) string {
	return dumpQueries(t, db,
		`SELECT * FROM shares ORDER BY name`,
		`SELECT id, username, password_hash, role, created_at FROM users ORDER BY id`,
		`SELECT * FROM schedule_chain`,
		`SELECT * FROM schedule_jobs ORDER BY job_id`,
		`SELECT create_policy, min_free_space FROM array_settings`,
		`SELECT role, role_index, filesystem, fs_uuid, wwn, serial, weak_identity, mountpoint FROM array_disks ORDER BY role, role_index`,
	)
}

// mappingJSON is a mapping as importConfig's diskMapping field carries it.
func mappingJSON(m apiv1.ConfigImportDiskMapping) apiv1.OptString {
	doc, err := json.Marshal(&m)
	if err != nil {
		panic(err)
	}
	return apiv1.NewOptString(string(doc))
}

func mappingOf(entries ...apiv1.ConfigImportDiskMappingEntry) apiv1.OptString {
	return mappingJSON(apiv1.ConfigImportDiskMapping{Disks: append([]apiv1.ConfigImportDiskMappingEntry{}, entries...)})
}

func entry(role apiv1.ArrayDiskRole, index int64, device string) apiv1.ConfigImportDiskMappingEntry {
	return apiv1.ConfigImportDiskMappingEntry{Role: role, RoleIndex: index, Device: device}
}

func importMapped(archive []byte, m apiv1.OptString) *apiv1.ImportConfigReq {
	req := importReq(archive)
	req.DiskMapping = m
	return req
}

func (b *bareMetalBox) preview(t *testing.T, archive []byte) *apiv1.ConfigImportPreview {
	t.Helper()
	p, err := b.h.PreviewConfigImport(context.Background(), previewReq(archive))
	if err != nil {
		t.Fatalf("PreviewConfigImport: %v", err)
	}
	return p
}

// assertRefusedWithNothingWritten imports archive with mapping and requires
// the refusal (status, code) with nothing written: no pre-import backup, the
// live database exactly as before, neither regeneration hook run, no
// restore hold left held and no disk touched.
func (b *bareMetalBox) assertRefusedWithNothingWritten(t *testing.T, archive []byte, mapping apiv1.OptString, status int, code string, inMessage ...string) {
	t.Helper()
	backups := 0
	b.h.Backup.Now = func() time.Time {
		backups++
		return time.Now().UTC()
	}
	before := liveFingerprint(t, b.db) + configuredState(t, b.db)
	filesBefore := b.runtimeSnapshot(t)

	_, err := b.h.ImportConfig(context.Background(), importMapped(archive, mapping))
	ae := importErr(t, err, status, code)
	for _, want := range inMessage {
		if !strings.Contains(ae.message, want) {
			t.Errorf("message %q does not contain %q", ae.message, want)
		}
	}
	if backups != 0 || len(b.preImportArchives(t)) != 0 {
		t.Errorf("the pre-import backup ran before the refusal (%d, %v)", backups, b.preImportArchives(t))
	}
	if steps := b.steps(); len(steps) != 0 {
		t.Errorf("a refused import regenerated %v", steps)
	}
	if after := liveFingerprint(t, b.db) + configuredState(t, b.db); after != before {
		t.Errorf("the live database changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if !slicesEqualMaps(b.runtimeSnapshot(t), filesBefore) {
		t.Error("a refused import changed the config, template or stack files")
	}
	release, err := b.h.Scheduler.BeginDatabaseRestore(context.Background())
	if err != nil {
		t.Fatalf("the restore hold is still held after the refusal: %v", err)
	}
	release()
}

func slicesEqualMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestImportConfig_BareMetal_RebuildsTheSystemFromAnotherInstallationsArchive
// is the safety-critical end-to-end test: another installation's archive,
// restored onto a fresh box through the preview's own mapping, gives a
// system whose shares, users, schedules and array roles equal the exported
// state, whose disks are the attached ones, that keeps this box's machine key
// check and recipient, and that never formatted or polled a disk.
func TestImportConfig_BareMetal_RebuildsTheSystemFromAnotherInstallationsArchive(t *testing.T) {
	ctx := context.Background()
	src, archive := newSourceInstallation(t)
	box := newBareMetalBox(t)
	box.attachSourceDisks()
	// The state the exported archive describes, read before the restore
	// changes anything on the box.
	exported := configuredState(t, src.db)

	p := box.preview(t, archive)
	bm, ok := p.BareMetal.Get()
	if !ok {
		t.Fatal("the preview of a box with no array has no bareMetal block")
	}
	if len(p.Blockers) != 0 || bm.SchemaUpgrade || len(bm.Disks) != 3 || len(bm.DiskMapping.Disks) != 3 {
		t.Fatalf("preview = blockers %+v, upgrade %v, %d disks, %d confirmed; want none, no upgrade, 3 and 3", p.Blockers, bm.SchemaUpgrade, len(bm.Disks), len(bm.DiskMapping.Disks))
	}
	for _, d := range bm.Disks {
		if d.State != apiv1.ConfigImportDiskStateMatched || !d.Device.Set {
			t.Fatalf("disk %s = %s on %q, want matched", d.Name, d.State, d.Device.Value)
		}
	}
	if len(box.steps()) != 0 || box.h.Backup.Destinations == nil {
		t.Fatal("precondition")
	}
	// A preview writes nothing.
	if got := configuredState(t, box.db); strings.Contains(got, "media") {
		t.Fatalf("the preview restored something:\n%s", got)
	}

	report, err := box.h.ImportConfig(ctx, importMapped(archive, mappingJSON(bm.DiskMapping)))
	if err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}

	if got := configuredState(t, box.db); got != exported {
		t.Errorf("the rebuilt system differs from the exported state:\nexported:\n%s\nrestored:\n%s", exported, got)
	}
	devices := dumpQueries(t, box.db, `SELECT role, role_index, device FROM array_disks ORDER BY role, role_index`)
	for _, want := range []string{"[data 1 /dev/sdy]", "[data 2 /dev/sdz]", "[parity 1 /dev/sdx]"} {
		if !strings.Contains(devices, want) {
			t.Errorf("array_disks devices = %s, want %s: the attached disks' own", devices, want)
		}
	}
	if got := dumpQueries(t, box.db, `SELECT CAST(check_value AS TEXT) FROM machine_key_check`); !strings.Contains(got, "installation-a-check-value") {
		t.Errorf("machine_key_check = %s, want this box's own kept", got)
	}
	if got := dumpQueries(t, box.db, `SELECT public_recipient, CAST(wrapped_identity AS TEXT) FROM backup_recipient`); !strings.Contains(got, "age1box") || !strings.Contains(got, "box-wrapped") {
		t.Errorf("backup_recipient = %s, want this box's own kept", got)
	}
	if got := dumpQueries(t, box.db, `SELECT COUNT(*) FROM users WHERE totp_secret IS NOT NULL OR totp_confirmed_at IS NOT NULL`, `SELECT COUNT(*) FROM notify_channels WHERE secret IS NOT NULL`); strings.Contains(got, "[1]") {
		t.Errorf("secrets sealed under the archive's key were kept: %s", got)
	}

	var secrets []string
	for _, n := range report.NotRestored {
		if n.Kind == apiv1.ConfigImportNotRestoredKindDisk {
			t.Errorf("a matched disk is reported not restored: %+v", n)
		}
		if n.Kind == apiv1.ConfigImportNotRestoredKindDatabaseSecret && n.Reason == apiv1.ConfigImportNotRestoredReasonSealedUnderOtherKey {
			secrets = append(secrets, n.Name)
		}
	}
	slices.Sort(secrets)
	if want := []string{"notify_channels.secret (ops)", "users.totp_secret (alice)"}; !slices.Equal(secrets, want) {
		t.Errorf("cleared secrets reported = %v, want %v", secrets, want)
	}

	if got := box.steps(); !slices.Equal(got, []string{"array", "config"}) {
		t.Errorf("regeneration ran %v, want the array's files and then the configs, once each", got)
	}
	if box.atRegen.custom != "exported smb" || !slices.Equal(box.atRegen.shares, []string{"media", "photos"}) {
		t.Errorf("at regeneration: custom = %q, shares = %v, want the restored files and database already in place", box.atRegen.custom, box.atRegen.shares)
	}
	for path, want := range map[string]string{
		filepath.Join(box.paths.ConfigRoot, "smb.custom.conf"):          "exported smb",
		filepath.Join(box.paths.ConfigRoot, "nested", "x.custom.conf"):  "exported nested",
		filepath.Join(box.paths.TemplatesDir, "app", "template.json"):   "exported template",
		filepath.Join(box.paths.StacksDir, "web", "docker-compose.yml"): "exported compose",
		filepath.Join(box.paths.StacksDir, "web", "meta.json"):          "exported meta",
	} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Errorf("%s = %q (err %v), want %q", path, got, err, want)
		}
	}
	if report.PreImportArchive == "" {
		t.Error("no pre-import archive was taken")
	}
	if box.guard.lists == 0 {
		t.Error("the restore never listed the attached disks")
	}
}

// The live database of a box with an array is untouched when the same
// archive is imported: it is another installation's, so it is refused.
func TestImportConfig_BareMetal_ABoxWithAnArrayRefusesTheSameArchiveAndIsUntouched(t *testing.T) {
	_, archive := newSourceInstallation(t)
	e := newImportFilesEnv(t)
	makeConfigDirs(t, e)
	seedArray(t, e.db)
	box := &bareMetalBox{importFilesEnv: e, disks: disk.NewFakeProvider()}
	box.guard = &guardedDisks{Provider: box.disks, t: t}
	e.h.Disks = box.guard
	e.h.RegenerateArray = func(context.Context) error { box.record("array"); return nil }

	box.assertRefusedWithNothingWritten(t, archive, apiv1.OptString{}, 409, "archive_other_installation", "bare-metal")
	if p, err := e.h.PreviewConfigImport(context.Background(), previewReq(archive)); err != nil || p.BareMetal.Set || len(p.Blockers) == 0 || p.Blockers[0].Code != apiv1.ConfigImportBlockerCodeArchiveOtherInstallation {
		t.Fatalf("preview on a box with an array = %+v (err %v), want the in-place blockers and no bareMetal block", p, err)
	}
}

// What makes a box fresh is having no array, not lacking an admin: a box
// with an admin and no array takes the bare-metal branch, which refuses an
// import without the confirmed mapping before anything is written.
func TestImportConfig_BareMetal_FreshMeansNoArrayWhateverAdminsExist(t *testing.T) {
	_, archive := newSourceInstallation(t)
	for name, prepare := range map[string]func(*bareMetalBox){
		"no admin, no array": func(*bareMetalBox) {},
		"an admin, no array": func(b *bareMetalBox) {
			execAll(t, b.db, `INSERT INTO users (id, username, password_hash, role, totp_last_step, created_at) VALUES ('adm', 'root-admin', 'x', 'admin', 0, '2026-09-01T00:00:00Z')`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			box := newBareMetalBox(t)
			box.attachSourceDisks()
			prepare(box)
			box.assertRefusedWithNothingWritten(t, archive, apiv1.OptString{}, 409, "disk_mapping_required")
		})
	}
}

// A mapping that is not exactly what the attached disks give is refused, and
// nothing has been written by then.
func TestImportConfig_BareMetal_AStaleOrWrongMappingIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	_, archive := newSourceInstallation(t)
	right := func() []apiv1.ConfigImportDiskMappingEntry {
		return []apiv1.ConfigImportDiskMappingEntry{
			entry(apiv1.ArrayDiskRoleParity, 1, "/dev/sdx"),
			entry(apiv1.ArrayDiskRoleData, 1, "/dev/sdy"),
			entry(apiv1.ArrayDiskRoleData, 2, "/dev/sdz"),
		}
	}
	for _, tc := range []struct {
		name    string
		mapping func() apiv1.OptString
		want    string
	}{
		{"a disk on the wrong device", func() apiv1.OptString {
			e := right()
			e[1].Device = "/dev/sdz"
			return mappingOf(e...)
		}, "not the confirmed /dev/sdz"},
		{"a matched disk left out", func() apiv1.OptString { return mappingOf(right()[:2]...) }, "was not confirmed"},
		{"an empty mapping for an array that is attached", func() apiv1.OptString { return mappingOf() }, "was not confirmed"},
		{"a slot the archive does not record", func() apiv1.OptString {
			return mappingOf(append(right(), entry(apiv1.ArrayDiskRoleCache, 1, "/dev/sdw"))...)
		}, "records no such disk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := newBareMetalBox(t)
			box.attachSourceDisks()
			box.assertRefusedWithNothingWritten(t, archive, tc.mapping(), 409, "disk_mapping_stale", tc.want)
		})
	}

	t.Run("a disk that was renumbered after the mapping was confirmed", func(t *testing.T) {
		box := newBareMetalBox(t)
		box.attachSourceDisks()
		confirmed := box.preview(t, archive).BareMetal.Value.DiskMapping
		box.disks.Reassign("/dev/sdy", "/dev/sdq")
		box.assertRefusedWithNothingWritten(t, archive, mappingJSON(confirmed), 409, "disk_mapping_stale", "/dev/sdq")
	})

	t.Run("a disk pulled after the mapping was confirmed", func(t *testing.T) {
		box := newBareMetalBox(t)
		box.attachSourceDisks()
		confirmed := box.preview(t, archive).BareMetal.Value.DiskMapping
		// A fresh box's disk inventory has no way to drop a disk; a renumbering
		// to a name outside the mapping is the same change.
		box.disks.Reassign("/dev/sdz", "/dev/sdr")
		box.assertRefusedWithNothingWritten(t, archive, mappingJSON(confirmed), 409, "disk_mapping_stale")
	})
}

// The mapping is checked again once the scheduler's restore hold is taken,
// after the pre-import backup: a disk that moved in between, which the first
// check could not see, refuses the restore before the database is written.
func TestImportConfig_BareMetal_TheMappingIsCheckedAgainUnderTheRestoreHold(t *testing.T) {
	ctx := context.Background()
	_, archive := newSourceInstallation(t)
	box := newBareMetalBox(t)
	box.attachSourceDisks()
	confirmed := box.preview(t, archive).BareMetal.Value.DiskMapping
	before := liveFingerprint(t, box.db) + configuredState(t, box.db)
	box.h.Backup.Now = func() time.Time {
		box.disks.Reassign("/dev/sdy", "/dev/sdq")
		return time.Now().UTC()
	}

	_, err := box.h.ImportConfig(ctx, importMapped(archive, mappingJSON(confirmed)))
	_ = importErr(t, err, 409, "disk_mapping_stale")
	if after := liveFingerprint(t, box.db) + configuredState(t, box.db); after != before {
		t.Errorf("the database changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if steps := box.steps(); len(steps) != 0 {
		t.Errorf("regeneration ran after the refusal: %v", steps)
	}
	release, err := box.h.Scheduler.BeginDatabaseRestore(ctx)
	if err != nil {
		t.Fatalf("the restore hold is still held: %v", err)
	}
	release()
}

// An array created after the mapping was confirmed, between the first check
// and the hold, is never restored over.
func TestImportConfig_BareMetal_AnArrayCreatedBeforeTheHoldIsNeverRestoredOver(t *testing.T) {
	ctx := context.Background()
	_, archive := newSourceInstallation(t)
	box := newBareMetalBox(t)
	box.attachSourceDisks()
	confirmed := box.preview(t, archive).BareMetal.Value.DiskMapping
	insertSentinelShare(t, box.db, "sentinel")
	box.h.Backup.Now = func() time.Time {
		seedDisk(t, box.db, seededDisk{role: "data", index: 1, uuid: "uuid-late", wwn: "wwn-late"})
		return time.Now().UTC()
	}

	_, err := box.h.ImportConfig(ctx, importMapped(archive, mappingJSON(confirmed)))
	_ = importErr(t, err, 409, "disk_mapping_stale")
	shares, lerr := store.NewShareStore(box.db).List(ctx)
	if lerr != nil || len(shares) != 1 || shares[0].Name != "sentinel" {
		t.Fatalf("shares after the refusal = %v (err %v), want the sentinel the restore would have replaced", shares, lerr)
	}
}

// Disks that do not match are reported by name and left out of the restored
// array's mounts; nothing adopts the disk that took a slot.
func TestImportConfig_BareMetal_AbsentReplacedAndAmbiguousDisksAreReportedAndNotMatched(t *testing.T) {
	ctx := context.Background()
	_, archive := newSourceInstallation(t)
	box := newBareMetalBox(t)
	// Parity 1 is not attached. Data 1's filesystem is on a disk with another
	// identity. Data 2 has a disk and its clone.
	box.attach("/dev/sdb", "uuid-d1", "wwn-replacement")
	box.attach("/dev/sdc", "uuid-d2", "wwn-d2")
	box.attach("/dev/sdd", "uuid-d2", "wwn-d2")

	p := box.preview(t, archive)
	states := map[string]apiv1.ConfigImportDiskState{}
	for _, d := range p.BareMetal.Value.Disks {
		states[d.Name] = d.State
	}
	want := map[string]apiv1.ConfigImportDiskState{
		"parity disk 1 (/mnt/parity1, WWN wwn-p1)": apiv1.ConfigImportDiskStateAbsent,
		"data disk 1 (/mnt/data1, WWN wwn-d1)":     apiv1.ConfigImportDiskStateReplaced,
		"data disk 2 (/mnt/data2, WWN wwn-d2)":     apiv1.ConfigImportDiskStateAmbiguous,
	}
	for name, state := range want {
		if states[name] != state {
			t.Errorf("preview state of %s = %q, want %q (all: %v)", name, states[name], state, states)
		}
	}
	if got := p.BareMetal.Value.DiskMapping.Disks; len(got) != 0 {
		t.Fatalf("the mapping to confirm = %+v, want none: no disk matched", got)
	}

	report, err := box.h.ImportConfig(ctx, importMapped(archive, mappingJSON(p.BareMetal.Value.DiskMapping)))
	if err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}
	reasons := map[string]apiv1.ConfigImportNotRestoredReason{}
	for _, n := range report.NotRestored {
		if n.Kind == apiv1.ConfigImportNotRestoredKindDisk {
			reasons[n.Name] = n.Reason
		}
	}
	wantReasons := map[string]apiv1.ConfigImportNotRestoredReason{
		"parity disk 1 (/mnt/parity1, WWN wwn-p1)": apiv1.ConfigImportNotRestoredReasonDiskAbsent,
		"data disk 1 (/mnt/data1, WWN wwn-d1)":     apiv1.ConfigImportNotRestoredReasonDiskReplaced,
		"data disk 2 (/mnt/data2, WWN wwn-d2)":     apiv1.ConfigImportNotRestoredReasonDiskAmbiguous,
	}
	for name, reason := range wantReasons {
		if reasons[name] != reason {
			t.Errorf("notRestored reason of %s = %q, want %q (all: %v)", name, reasons[name], reason, reasons)
		}
	}
	// The rows are still the recorded ones: no replacement disk was adopted
	// and no row was pointed at an attached disk.
	if got := dumpQueries(t, box.db, `SELECT role, role_index, fs_uuid, wwn, device FROM array_disks ORDER BY role, role_index`); strings.Contains(got, "wwn-replacement") || strings.Contains(got, "/dev/sdb") || strings.Contains(got, "/dev/sdc") || strings.Contains(got, "/dev/sdd") {
		t.Errorf("array_disks adopted an attached disk:\n%s", got)
	}
	if got := box.steps(); !slices.Equal(got, []string{"array", "config"}) {
		t.Errorf("regeneration ran %v", got)
	}
}

// An archive from a newer schema version is refused, by name, before
// anything is written; the preview says so as a blocker with no mapping.
func TestImportConfig_BareMetal_ANewerArchiveIsRefusedByName(t *testing.T) {
	_, archive := newSourceInstallation(t)
	newer := editArchiveDB(t, archive, func(db *sql.DB) {
		execAll(t, db, `INSERT INTO schema_migrations (version, slug, checksum, applied_at) VALUES ('99999999999999', 'from_the_future', 'x', '2099-01-01T00:00:00Z')`)
	})
	box := newBareMetalBox(t)
	box.attachSourceDisks()
	box.assertRefusedWithNothingWritten(t, newer, mappingOf(), 409, "archive_newer_version", "99999999999999")

	p := box.preview(t, newer)
	if p.BareMetal.Set || len(p.Blockers) != 1 || p.Blockers[0].Code != apiv1.ConfigImportBlockerCodeArchiveNewerVersion {
		t.Fatalf("preview = blockers %+v, bareMetal %v, want the one archive_newer_version blocker and no mapping", p.Blockers, p.BareMetal.Set)
	}
}

// An archive from an older schema version is upgraded by the migration
// runner on a copy of its database and then restored: every fixture database
// of a released schema, imported onto a fresh box, comes out at head with its
// data.
func TestImportConfig_BareMetal_AnOlderArchiveIsUpgradedAndRestored(t *testing.T) {
	ctx := context.Background()
	fixtures, err := filepath.Glob("../../testdata/db/*.db")
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("no fixtures under testdata/db (%v)", err)
	}
	migrations, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	head := migrations[len(migrations)-1].Version

	for _, fixture := range fixtures {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			archive := archiveOfDatabase(t, fixture)
			box := newBareMetalBox(t)

			p := box.preview(t, archive)
			if !p.BareMetal.Value.SchemaUpgrade || p.Archive.SchemaVersion == head {
				t.Fatalf("preview = upgrade %v, archive schema %s; want an upgrade from an older one", p.BareMetal.Value.SchemaUpgrade, p.Archive.SchemaVersion)
			}
			if len(p.Blockers) != 0 {
				t.Fatalf("blockers = %+v", p.Blockers)
			}

			if _, err := box.h.ImportConfig(ctx, importMapped(archive, mappingJSON(p.BareMetal.Value.DiskMapping))); err != nil {
				t.Fatalf("ImportConfig: %v", err)
			}
			var version string
			if err := box.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != head {
				t.Errorf("schema version after the import = %q (err %v), want head %s", version, err, head)
			}
			want := dumpQueries(t, openReadOnly(t, fixture), `SELECT installation_id FROM schema_info`, `SELECT id, username FROM users ORDER BY id`)
			got := dumpQueries(t, box.db, `SELECT installation_id FROM schema_info`, `SELECT id, username FROM users ORDER BY id`)
			if got != want {
				t.Errorf("the fixture's data after the restore:\n%s\nwant:\n%s", got, want)
			}
			if got := dumpQueries(t, box.db, `SELECT CAST(check_value AS TEXT) FROM machine_key_check`); !strings.Contains(got, "installation-a-check-value") {
				t.Errorf("machine_key_check = %s, want this box's own", got)
			}
		})
	}
}

func openReadOnly(t *testing.T, path string) *sql.DB {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+abs+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// archiveOfDatabase is a config archive whose state.db is a copy of the
// database at path, whatever schema version that is.
func archiveOfDatabase(t *testing.T, path string) []byte {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "old.db")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	staging := t.TempDir()
	if _, err := backup.BuildArchive(context.Background(), db, backup.Paths{}, nil, nil, "old-host", "0.0.1", time.Now().UTC(), staging); err != nil {
		t.Fatalf("BuildArchive: %v", err)
	}
	out := filepath.Join(t.TempDir(), "old.tar.zst")
	if err := packTarZst(staging, out); err != nil {
		t.Fatal(err)
	}
	archive, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return archive
}

// A failure after the database was restored is reported with the pre-import
// archive to go back to, as the in-place restore reports it.
func TestImportConfig_BareMetal_AFailureRegeneratingTheArrayFilesNamesThePreImportArchive(t *testing.T) {
	ctx := context.Background()
	_, archive := newSourceInstallation(t)
	box := newBareMetalBox(t)
	box.attachSourceDisks()
	box.h.RegenerateArray = func(context.Context) error {
		box.record("array")
		return errors.New("disk mount units are not writable")
	}
	confirmed := box.preview(t, archive).BareMetal.Value.DiskMapping

	_, err := box.h.ImportConfig(ctx, importMapped(archive, mappingJSON(confirmed)))
	ae := importErr(t, err, 500, "import_failed")
	if !strings.Contains(ae.message, "disk mount units are not writable") || !strings.Contains(ae.message, ".pre-import.") {
		t.Errorf("message = %q, want the failure and the pre-import archive", ae.message)
	}
	if got := box.steps(); !slices.Equal(got, []string{"array", "config"}) {
		t.Errorf("regeneration ran %v: the configs are still regenerated after the array files failed", got)
	}
}

// A daemon that could not restore onto a fresh box says so, and before
// anything is written.
func TestImportConfig_BareMetal_WithoutTheHooksItNeedsIsNotConfigured(t *testing.T) {
	_, archive := newSourceInstallation(t)
	for name, unset := range map[string]func(*bareMetalBox){
		"no disk provider":      func(b *bareMetalBox) { b.h.Disks = nil },
		"no array regeneration": func(b *bareMetalBox) { b.h.RegenerateArray = nil },
	} {
		t.Run(name, func(t *testing.T) {
			box := newBareMetalBox(t)
			box.attachSourceDisks()
			unset(box)
			box.assertRefusedWithNothingWritten(t, archive, mappingOf(), 501, "not_configured")
			if _, err := box.h.PreviewConfigImport(context.Background(), previewReq(archive)); err == nil {
				t.Error("the preview answered on a daemon that cannot restore")
			}
		})
	}
}

// A field that is not a mapping is refused before anything is written, and a
// mapping sent to an installation that has an array is refused rather than
// ignored: an import into an array restores no disks.
func TestImportConfig_BareMetal_ADiskMappingIsValidatedAndOnlyTakenWhereItApplies(t *testing.T) {
	_, archive := newSourceInstallation(t)

	t.Run("not a mapping", func(t *testing.T) {
		for _, doc := range []string{`not json`, `{}`, `{"disks":[{"role":"spare","roleIndex":1,"device":"/dev/sdb"}]}`, `{"disks":[{"role":"data","roleIndex":0,"device":"/dev/sdb"}]}`, `{"disks":[{"role":"data","roleIndex":1,"device":""}]}`, `{"disks":[],"extra":1}`, `{"disks":[]} trailing`} {
			box := newBareMetalBox(t)
			box.attachSourceDisks()
			box.assertRefusedWithNothingWritten(t, archive, apiv1.NewOptString(doc), 400, "invalid_disk_mapping")
		}
	})

	t.Run("an installation with an array", func(t *testing.T) {
		e := newImportFilesEnv(t)
		makeConfigDirs(t, e)
		seedArray(t, e.db)
		box := &bareMetalBox{importFilesEnv: e, disks: disk.NewFakeProvider()}
		box.guard = &guardedDisks{Provider: box.disks, t: t}
		e.h.Disks = box.guard
		e.h.RegenerateArray = func(context.Context) error { return nil }
		box.assertRefusedWithNothingWritten(t, archive, mappingOf(), 409, "disk_mapping_not_applicable")
	})
}

// hostBox is a fresh box whose OS came with a package-default smb.conf and
// exports file, which its own manifest has never heard of: the state a
// bare-metal restore lands on (#471).
const (
	packageSmbConf = "[global]\n   workgroup = WORKGROUP\n"
	packageExports = "# /etc/exports: the package default\n"
)

func newHostBox(t *testing.T) *bareMetalBox {
	t.Helper()
	box := newBareMetalBox(t)
	box.attachSourceDisks()
	etc := filepath.Join(box.root, "etc")
	box.h.Generator = cfg.NewGenerator(etc)
	putTree(t, etc, map[string]string{cfg.PathSamba: packageSmbConf, cfg.PathNFS: packageExports})
	return box
}

func (b *bareMetalBox) hostFile(rel string) string {
	got, _ := os.ReadFile(filepath.Join(b.root, "etc", rel))
	return string(got)
}

func (b *bareMetalBox) hostStatus(t *testing.T, rel string) cfg.Status {
	t.Helper()
	st, err := b.h.Generator.Check(context.Background(), rel)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (b *bareMetalBox) mappingFor(t *testing.T, archive []byte) apiv1.OptString {
	t.Helper()
	bm, ok := b.preview(t, archive).BareMetal.Get()
	if !ok {
		t.Fatal("no bareMetal block")
	}
	return mappingJSON(bm.DiskMapping)
}

// extractPreImport unpacks the one pre-import archive and returns its tree.
func (b *bareMetalBox) extractPreImport(t *testing.T) string {
	t.Helper()
	names := b.preImportArchives(t)
	if len(names) != 1 {
		t.Fatalf("pre-import archives = %v, want one", names)
	}
	tree, err := backup.ExtractVerifiedArchive(filepath.Join(b.dest, names[0]))
	if err != nil {
		t.Fatalf("the pre-import archive does not verify: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tree) })
	return tree
}

func TestPreviewConfigImport_BareMetal_NamesTheHostFilesTheRestoreWillReplace(t *testing.T) {
	_, archive := newSourceInstallation(t, "samba=import", "nfs=import")
	box := newHostBox(t)
	before := box.hostFile(cfg.PathSamba)

	p := box.preview(t, archive)
	var notes []apiv1.ConfigImportNote
	for _, n := range p.Notes {
		if n.Code == apiv1.ConfigImportNoteCode(backup.NoteHostFilesReplaced) {
			notes = append(notes, n)
		}
	}
	if len(notes) != 1 {
		t.Fatalf("notes = %+v, want exactly one host_files_replaced", p.Notes)
	}
	for _, want := range []string{"Samba configuration (" + filepath.Join(box.root, "etc", "samba", "smb.conf") + ")", "NFS exports (" + filepath.Join(box.root, "etc", "exports") + ")"} {
		if !strings.Contains(notes[0].Message, want) {
			t.Errorf("note %q lacks %q", notes[0].Message, want)
		}
	}
	if box.hostFile(cfg.PathSamba) != before || box.hostStatus(t, cfg.PathSamba) != cfg.StatusUnknown {
		t.Error("the preview changed the host file or the manifest")
	}
}

func TestPreviewConfigImport_BareMetal_NamesNothingItWillNotReplace(t *testing.T) {
	for name, tc := range map[string]struct {
		rows  []string
		setup func(*bareMetalBox)
	}{
		"leave":       {rows: []string{"samba=leave", "nfs=leave"}},
		"no decision": {},
		"no file on host": {rows: []string{"samba=import"}, setup: func(b *bareMetalBox) {
			if err := os.Remove(filepath.Join(b.root, "etc", cfg.PathSamba)); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(b.root, "etc", cfg.PathNFS)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			_, archive := newSourceInstallation(t, tc.rows...)
			box := newHostBox(t)
			if tc.setup != nil {
				tc.setup(box)
			}
			for _, n := range box.preview(t, archive).Notes {
				if n.Code == apiv1.ConfigImportNoteCode(backup.NoteHostFilesReplaced) {
					t.Errorf("unexpected note: %s", n.Message)
				}
			}
		})
	}
}

// The restore saves what it is about to replace, records the restored
// decisions before the regeneration writes anything, and leaves a file the
// archive marks leave, or has no decision for, exactly as it found it.
func TestImportConfig_BareMetal_SavesThenTakesOverOnlyWhatTheArchiveImports(t *testing.T) {
	_, archive := newSourceInstallation(t, "samba=import", "nfs=leave")
	box := newHostBox(t)
	var atRegen []cfg.Status
	box.regenHook = func() {
		atRegen = append(atRegen, box.hostStatus(t, cfg.PathSamba), box.hostStatus(t, cfg.PathNFS))
	}

	if _, err := box.h.ImportConfig(context.Background(), importMapped(archive, box.mappingFor(t, archive))); err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}

	tree := box.extractPreImport(t)
	if got, err := os.ReadFile(filepath.Join(tree, "host", "samba", "smb.conf")); err != nil || string(got) != packageSmbConf {
		t.Errorf("the pre-import archive's host/samba/smb.conf = %q, %v; want the replaced file", got, err)
	}
	if _, err := os.Stat(filepath.Join(tree, "host", "exports")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the pre-import archive saved a file the restore leaves alone: %v", err)
	}
	if len(atRegen) != 2 || atRegen[0] != cfg.StatusManaged || atRegen[1] != cfg.StatusUnmanaged {
		t.Errorf("manifest when the configs were regenerated = %v, want smb.conf managed and exports unmanaged already", atRegen)
	}
	if box.hostFile(cfg.PathNFS) != packageExports {
		t.Errorf("a file the archive marks leave changed: %q", box.hostFile(cfg.PathNFS))
	}
	if err := box.h.Generator.Write(context.Background(), cfg.File{Path: cfg.PathSamba, Command: "c", Body: []byte("[media]\n")}, 1, time.Now()); err != nil {
		t.Errorf("the taken-over file cannot be written: %v", err)
	}
	if err := box.h.Generator.Write(context.Background(), cfg.File{Path: cfg.PathNFS, Command: "c", Body: []byte("x")}, 1, time.Now()); !errors.Is(err, cfg.ErrUnmanaged) {
		t.Errorf("Write of the left file = %v, want ErrUnmanaged", err)
	}
}

func TestImportConfig_BareMetal_AKindWithNoDecisionStaysUnrecordedAndUntouched(t *testing.T) {
	_, archive := newSourceInstallation(t, "samba=import")
	box := newHostBox(t)
	if _, err := box.h.ImportConfig(context.Background(), importMapped(archive, box.mappingFor(t, archive))); err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}
	if st := box.hostStatus(t, cfg.PathNFS); st != cfg.StatusUnknown || box.hostFile(cfg.PathNFS) != packageExports {
		t.Errorf("exports: status %v, content %q; want unrecorded and untouched", st, box.hostFile(cfg.PathNFS))
	}
	if err := box.h.Generator.Write(context.Background(), cfg.File{Path: cfg.PathNFS, Command: "c", Body: []byte("x")}, 1, time.Now()); !errors.Is(err, cfg.ErrExistingHostFile) {
		t.Errorf("Write of a file no decision covers = %v, want ErrExistingHostFile", err)
	}
}

// A host file that cannot be saved in the pre-import archive stops the
// restore before the hold, the database and the file itself are touched.
func TestImportConfig_BareMetal_AHostFileThatCannotBeSavedRefusesTheRestoreUntouched(t *testing.T) {
	_, archive := newSourceInstallation(t, "samba=import")
	box := newHostBox(t)
	smb := filepath.Join(box.root, "etc", cfg.PathSamba)
	if err := os.Remove(smb); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(smb, 0o755); err != nil {
		t.Fatal(err)
	}
	mapping := box.mappingFor(t, archive)
	before := liveFingerprint(t, box.db) + configuredState(t, box.db)

	_, err := box.h.ImportConfig(context.Background(), importMapped(archive, mapping))
	if err == nil || !strings.Contains(err.Error(), "backing up before import") {
		t.Fatalf("ImportConfig = %v, want the pre-import backup to refuse it", err)
	}
	if after := liveFingerprint(t, box.db) + configuredState(t, box.db); after != before {
		t.Error("the live database changed")
	}
	if steps := box.steps(); len(steps) != 0 {
		t.Errorf("regeneration ran %v", steps)
	}
	if st := box.hostStatus(t, cfg.PathSamba); st != cfg.StatusUnknown {
		t.Errorf("the manifest recorded the file: %v", st)
	}
	if names := box.preImportArchives(t); len(names) != 0 {
		t.Errorf("an archive without the file was kept: %v", names)
	}
}

// With no backup destination enabled the pre-import backup is a no-op, which
// for a restore that replaces a file means there would be no copy of it.
func TestImportConfig_BareMetal_NoBackupDestinationRefusesToReplaceAHostFile(t *testing.T) {
	_, archive := newSourceInstallation(t, "samba=import")
	box := newHostBox(t)
	mapping := box.mappingFor(t, archive)
	box.h.Backup.Destinations = nil
	before := liveFingerprint(t, box.db) + configuredState(t, box.db)

	_, err := box.h.ImportConfig(context.Background(), importMapped(archive, mapping))
	_ = importErr(t, err, 409, "host_files_not_saved")
	if after := liveFingerprint(t, box.db) + configuredState(t, box.db); after != before {
		t.Error("the live database changed")
	}
	if steps := box.steps(); len(steps) != 0 {
		t.Errorf("regeneration ran %v", steps)
	}
	if st := box.hostStatus(t, cfg.PathSamba); st != cfg.StatusUnknown || box.hostFile(cfg.PathSamba) != packageSmbConf {
		t.Errorf("the host file or the manifest changed: %v %q", st, box.hostFile(cfg.PathSamba))
	}
}

// A restore that replaces nothing the host has saves nothing extra and
// behaves as before.
func TestImportConfig_BareMetal_WithNoHostFilesTheArchiveHoldsNone(t *testing.T) {
	_, archive := newSourceInstallation(t)
	box := newHostBox(t)
	if _, err := box.h.ImportConfig(context.Background(), importMapped(archive, box.mappingFor(t, archive))); err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}
	tree := box.extractPreImport(t)
	if _, err := os.Stat(filepath.Join(tree, "host")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a host/ directory was saved: %v", err)
	}
	if st := box.hostStatus(t, cfg.PathSamba); st != cfg.StatusUnknown {
		t.Errorf("the manifest recorded a file: %v", st)
	}
}

// An in-place restore is the host's own manifest's business: it neither
// records nor saves a host file, whatever the archive's host_config says.
func TestImportConfig_InPlaceLeavesTheHostManifestAndFilesAlone(t *testing.T) {
	e := newImportFilesEnv(t)
	execAll(t, e.db, `INSERT INTO host_config (kind, decision, facts, applied_at) VALUES ('samba', 'import', '{}', '2026-09-01T00:00:00Z')`)
	archive := e.seedAndExport(t)
	etc := filepath.Join(e.root, "etc")
	e.h.Generator = cfg.NewGenerator(etc)
	putTree(t, etc, map[string]string{cfg.PathSamba: packageSmbConf})

	if _, err := e.h.ImportConfig(context.Background(), importReq(archive)); err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}
	if st, err := e.h.Generator.Check(context.Background(), cfg.PathSamba); err != nil || st != cfg.StatusUnknown {
		t.Errorf("Check = %v, %v; an in-place restore recorded a host file", st, err)
	}
	if _, err := os.Stat(filepath.Join(etc, ".hoserva", "manifest.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("an in-place restore wrote a manifest: %v", err)
	}
	names := e.preImportArchives(t)
	if len(names) != 1 {
		t.Fatalf("pre-import archives = %v", names)
	}
	tree, err := backup.ExtractVerifiedArchive(filepath.Join(e.dest, names[0]))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tree) }()
	if _, err := os.Stat(filepath.Join(tree, "host")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("an in-place restore saved host files: %v", err)
	}
}
