package main

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/migrate"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/template"
	migrationpending "github.com/mdg-labs/hoserva/web/fixtures/migration-pending"
)

// mockFlashDevice is the Unraid USB stick this mock offers: a FAT filesystem
// labelled UNRAID that is neither the boot disk nor in the array. The mock reads
// no stick; like production it refuses any other device.
const mockFlashDevice = "/dev/sdu"

// mockMigration is the migration session this mock instance keeps: a report
// and the size of the zip it came from, or the device it was read from. Like
// production's session it holds the report only; the mock keeps no zip.
type mockMigration struct {
	mu         sync.Mutex
	report     *migrate.Report
	size       int64
	device     string
	receivedAt time.Time
	// imported is set once an import has adopted the data disks and cleared
	// when no disk is adopted any more; while it is set, parity, array-write and
	// topology jobs are refused as production's scheduler refuses them. It is read
	// without mu, from handlers that hold the handler's own lock.
	imported atomic.Bool
	// verify is the result of the latest verify, and verifyRuns how many have
	// run: the first fails with the scenario's failing result and every later
	// one passes with its passing result, so a UI sees a mismatch and then the
	// re-run that clears it.
	verify     *apiv1.MigrationVerify
	verifyRuns int
	// roles is the disk-role mapping the import confirmed, which the point of no
	// return resolves again; initialized is set once the point of no return has
	// formatted the former parity and cache disks and finished.
	roles       []disk.AdoptionAssignment
	initialized bool
}

func (m *mockMigration) initializedNow() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.initialized
}

func errMigrationInProgress() error {
	return &mockError{code: "migration_in_progress", statusCode: 409, message: "an Unraid migration is in progress — parity, array-write and topology jobs are refused until its point of no return"}
}

func (m *mockMigration) zipOnly() bool {
	return m.report != nil && m.report.BootMode == "internal"
}

func errMigrationRefusal(code string, status int, err error) error {
	return &mockError{code: code, statusCode: status, message: err.Error()}
}

// mockMigrateError gives an upload refusal the status and code production
// answers it with.
func mockMigrateError(err error) error {
	switch {
	case errors.Is(err, migrate.ErrZipTooLarge):
		return errMigrationRefusal("zip_too_large", 413, err)
	case errors.Is(err, migrate.ErrInvalidZip):
		return errMigrationRefusal("invalid_zip", 400, err)
	case errors.Is(err, migrate.ErrNoDiskCfg):
		return errMigrationRefusal("invalid_flash_backup", 400, err)
	case errors.Is(err, migrate.ErrUnsupportedLayout):
		return errMigrationRefusal("unsupported_layout", 400, err)
	case errors.Is(err, migrate.ErrNotFlashDevice):
		return errMigrationRefusal("invalid_flash_device", 400, err)
	case errors.Is(err, migrate.ErrZipOnly):
		return errMigrationRefusal("zip_only_source", 409, err)
	case errors.Is(err, migrate.ErrImportNoReport):
		return errMigrationRefusal("no_migration_report", 404, err)
	case errors.Is(err, migrate.ErrImportScanUnfinished):
		return errMigrationRefusal("scan_not_finished", 409, err)
	case errors.Is(err, migrate.ErrImportNoGo):
		return errMigrationRefusal("migration_no_go", 409, err)
	case errors.Is(err, migrate.ErrImportNoReview):
		return errMigrationRefusal("scan_outdated", 409, err)
	case errors.Is(err, migrate.ErrVerifyNotPending), errors.Is(err, migrate.ErrParityNotPending), errors.Is(err, job.ErrMigrationUndoNotPending):
		return errMigrationRefusal("no_import_pending", 409, err)
	case errors.Is(err, migrate.ErrVerifyRequired):
		return errMigrationRefusal("verify_required", 409, err)
	case errors.Is(err, disk.ErrUnraidStick):
		return errMigrationRefusal("unraid_stick", 409, err)
	case errors.Is(err, job.ErrAdoptionLayout), migrate.IsImportRoleError(err):
		return errMigrationRefusal("invalid_import_roles", 400, err)
	}
	return err
}

func errNoMigrationReport() error {
	return &mockError{code: "no_migration_report", statusCode: 404, message: migrate.ErrNoReport.Error()}
}

// mockMigrationReport is a completed scan of a single-parity array with one data
// disk whose filesystem check failed: what the UI shows after a scan, with that
// refused disk, one SMART finding so a flagged row has something to render, and
// one row group per part of the configuration inventory, including the flagged
// containers and shares. flash is the uploaded zip's capture, which production
// reads the boot mode and the capture's state from; nil is the seeded session,
// which booted from the stick with a capture that is there and fresh. A scan
// that follows an earlier one finds disk3 repaired (disk3Fixed: its filesystem
// was checked clean after the first report refused it), so the report no longer
// says no-go and an import can go ahead.
func mockMigrationReport(flash *migrate.Flash, version string, unverified bool, at time.Time, disk3Fixed bool) *migrate.Report {
	r := &migrate.Report{GeneratedAt: at, UnraidVersion: version, UnverifiedLayout: unverified, BootMode: "usb"}
	add := func(check string, st migrate.Status, subject, detail string) {
		r.Rows = append(r.Rows, migrate.Row{Check: check, Status: st, Subject: subject, Detail: detail})
	}
	if unverified {
		add(migrate.CheckVersion, migrate.StatusWarn, "", "This Unraid version or flash layout is not one Hoserva has been verified against. The scan went ahead only because --unverified-layout was given; this override is recorded in the report (Q24).")
	} else {
		add(migrate.CheckVersion, migrate.StatusPass, "", "Unraid "+version+", flash layout recognised.")
	}
	add(migrate.CheckBootDevice, migrate.StatusInfo, "", "Unraid boots from a USB stick. Keep the stick: it is the rollback.")
	add(migrate.CheckMapping, migrate.StatusPass, "parity", "serial EXAMPLE_PARITY is Unraid parity slot 0; this machine has it as /dev/sdb, 8.0 TiB.")
	add(migrate.CheckMapping, migrate.StatusPass, "disk1", "serial EXAMPLE_DISK1 is Unraid disk 1 (xfs); this machine has it as /dev/sdc, 4.0 TiB.")
	add(migrate.CheckMapping, migrate.StatusFlag, "disk2", "Unraid had a disk with serial EXAMPLE_DISK2 here, and no disk on this machine has this serial or WWN. Attach it before the import.")
	add(migrate.CheckMapping, migrate.StatusPass, "disk3", "serial EXAMPLE_DISK3 is Unraid disk 3 (xfs); this machine has it as /dev/sdd, 4.0 TiB.")
	add(migrate.CheckMapping, migrate.StatusPass, "disk4", "serial EXAMPLE_DISK4 is Unraid disk 4 (xfs); this machine has it as /dev/sde, 2.0 TiB.")
	add(migrate.CheckMapping, migrate.StatusPass, "pool cache", "serial EXAMPLE_CACHE is a pool device; this machine has it as /dev/nvme0n1, 500.0 GiB.")
	add(migrate.CheckIdentity, migrate.StatusPass, "parity", "/dev/sdb has a WWN or serial.")
	add(migrate.CheckIdentity, migrate.StatusFlag, "disk4", "/dev/sde has only a weak identity (a USB enclosure hides its serial): it can be a data disk, matched by filesystem UUID and size (Q21).")
	add(migrate.CheckDataDisks, migrate.StatusInfo, "parity", "A parity slot: /dev/sdb is never mounted or checked, and its role comes from its slot, never from its filesystem. Its device reports xfs: a parity disk's bytes are an XOR of the data disks', which can leave a valid-looking superblock.")
	add(migrate.CheckDataDisks, migrate.StatusPass, "disk1", "xfs on /dev/sdc, a single filesystem; the flash and the device agree on it.")
	add(migrate.CheckDataDisks, migrate.StatusPass, "disk3", "xfs on /dev/sdd, a single filesystem; the flash and the device agree on it.")
	add(migrate.CheckDataDisks, migrate.StatusPass, "disk4", "xfs on /dev/sde, a single filesystem; the flash and the device agree on it.")
	add(migrate.CheckIntegrity, migrate.StatusPass, "disk1", "The read-only xfs check of /dev/sdc is clean.")
	disk3Refusal := "disk3 is not adopted: its read-only xfs check failed (xfs_repair -n exit status 1: a bad free-space B-tree block in allocation group 0). Computing parity over a damaged filesystem would keep the damage. An XFS disk that was not unmounted cleanly has a log that needs replaying: start Unraid, stop the array cleanly, and scan again. Hoserva never replays a log on a disk it does not own yet."
	if disk3Fixed {
		disk3Refusal = ""
		add(migrate.CheckIntegrity, migrate.StatusPass, "disk3", "The read-only xfs check of /dev/sdd is clean.")
	} else {
		add(migrate.CheckIntegrity, migrate.StatusRefuse, "disk3", disk3Refusal)
	}
	add(migrate.CheckIntegrity, migrate.StatusPass, "disk4", "The read-only xfs check of /dev/sde is clean.")
	add(migrate.CheckBaseline, migrate.StatusInfo, "", "Recorded for the verify phase, with the session. Content hashes: every file of 1.0 MiB or less is hashed, plus a deterministic 1 in 100 (at least 200) of the larger files on each disk, chosen by a stable hash of the path. Every file's size, every symlink with its target and every special file by type is recorded as well.")
	add(migrate.CheckBaseline, migrate.StatusInfo, "disk1", "48210 files (3.6 TiB), 12 symlinks and 0 special files; 31044 files (96.2 GiB) hashed.")
	add(migrate.CheckBaseline, migrate.StatusInfo, "media", "41007 files (3.4 TiB), 0 symlinks and 0 special files, on disk1.")
	add(migrate.CheckContentSpace, migrate.StatusPass, "", "3 content-file copies can be placed (Q18): the boot device, 2 data disks, with room for about 9.4 MiB for the content file (48210 files, a planning figure of 200 bytes per file and 24 per 256 KiB block).")
	add(migrate.CheckParity, migrate.StatusInfo, "", "1 parity disk(s). Parity is rewritten from scratch either way.")
	add(migrate.CheckParitySize, migrate.StatusPass, "", "The smallest parity disk (8.0 TiB) is at least as large as the largest data disk (4.0 TiB).")
	add(migrate.CheckSMART, migrate.StatusPass, "parity", "/dev/sdb has no reallocated or pending sectors.")
	add(migrate.CheckSMART, migrate.StatusFlag, "disk1", "Recommend aborting: /dev/sdc has 8 reallocated and 0 pending sectors, and the unprotected window of the migration is when a marginal disk fails.")
	add(migrate.CheckParityHistory, migrate.StatusPass, "", "The last parity check, on 2026-09-25 (from the capture's var.ini), completed clean with 0 errors.")
	add(migrate.CheckShares, migrate.StatusInfo, "", "3 shares configured. Each share's allocation method and cache setting are mapped below (Q11).")
	add(migrate.CheckShares, migrate.StatusFlag, "media", "allocation High-water: no exact equivalent, mapped to Balance across disks (mfs) (Q11); cache setting no maps to array-only; exported over SMB (e); directory on disk1.")
	add(migrate.CheckShares, migrate.StatusInfo, "backup", "allocation Fill-up maps to Fill disks in order (ff); cache setting no maps to array-only; not exported over SMB; never on disk3; directory on disk1.")
	add(migrate.CheckShares, migrate.StatusInfo, "documents", "allocation Most-free maps to Balance across disks (mfs); cache setting yes maps to cache-then-move; exported over SMB (e); directory on disk1.")
	add(migrate.CheckCache, migrate.StatusWarn, "appdata", "Docker keeps container data under /mnt/user/appdata/; cache setting prefer; its directory is on disk1, pool cache. Phase A step 5 must move it to the array before the cache is re-created.")
	add(migrate.CheckCache, migrate.StatusInfo, "Docker storage", "Docker's directory (/mnt/user/system/docker/dockerdir) is on the cache. It is not moved: images are pulled again when containers are recreated. What does not come back is each container's writable layer (doc 04 §5). No container's writable layer holds data.")
	add(migrate.CheckUsers, migrate.StatusInfo, "", "2 user accounts: alice, bob. Names only are read; passwords cannot be carried over, so each is set again at the import (doc 05 §4 step 4).")
	add(migrate.CheckTemplates, migrate.StatusInfo, "", "2 templates parsed: 1 autostart, 1 running, 0 stopped, 0 template only. 2 are installed; a template with no container is a record of an app once installed.")
	add(migrate.CheckTemplates, migrate.StatusInfo, "", "Of the 2 installed templates, 1 convert cleanly and 1 with warnings (Q36).")
	add(migrate.CheckTemplates, migrate.StatusInfo, "gateway", "Converts with 2 warnings to review (flagged_path, missing_network). Open its preview before recreating the container.")
	add(migrate.CheckContainers, migrate.StatusInfo, "", "1 Compose Manager project previewed with its own compose.yaml and not converted.")
	add(migrate.CheckContainers, migrate.StatusInfo, "", "8 containers in the capture: 6 from the Docker page (dockerMan), 1 from Compose Manager, 1 created by hand.")
	add(migrate.CheckContainers, migrate.StatusFlag, "dbtool", "A dockerMan container with no template whose <Name> matches. It cannot be converted: open it on the Docker page, edit it and apply to save its template, then run the prepare script again (doc 05 §4 step 2).")
	add(migrate.CheckContainers, migrate.StatusFlag, "handmade", "Created by hand (docker run), so it has no template to convert. Recreate it from its run command.")
	add(migrate.CheckUserScripts, migrate.StatusInfo, "", "1 User Scripts entry found. They are listed, never executed or translated (Q83): recreate what is still wanted as a cron job or systemd timer (doc 05 §4 step 24).")
	add(migrate.CheckUserScripts, migrate.StatusInfo, "nightly-report", "Scheduled in customSchedule.cron (30 2 * * *), so it runs while enabled.")
	add(migrate.CheckPlugins, migrate.StatusInfo, "user.scripts", "Installed. Hoserva counterpart: its scripts are listed, never executed or translated (Q83).")
	add(migrate.CheckCustomConfig, migrate.StatusWarn, "smb-extra.conf", "2 lines of custom Samba configuration. It is not imported: look at the lines on the Unraid server and recreate any that are still wanted.")
	add(migrate.CheckSettings, migrate.StatusInfo, "mover schedule", "40 3 * * *. It can be offered as Hoserva's mover schedule.")
	add(migrate.CheckUID99, migrate.StatusPass, "", "UID 99 is free for the hoserva-apps user.")
	add(migrate.CheckSyncEstimate, migrate.StatusInfo, "", "About 2.8 hours for 4.0 TiB of data disks, if they are full, at an assumed 400 MB/s. It is a planning figure, not a measurement: the first sync is a long job, and it runs only when you start it.")
	r.Verdict = migrate.VerdictNoGo
	if disk3Fixed {
		r.Verdict = migrate.VerdictGoWithWarnings
	}
	r.Import = mockMigrationImport()
	r.Review = mockReview(flash, at, disk3Refusal)
	return r
}

// mockReview is the structured data of the rows above, for the disks and shares
// they name. The boot mode and the capture's state are production's own
// decision over the uploaded zip's capture, so a zip that says Unraid booted
// internally, or whose capture is missing, unreadable or older than a template,
// gives the state it gives there.
func mockReview(flash *migrate.Flash, at time.Time, disk3Refusal string) *migrate.Review {
	const (
		tib = int64(1) << 40
		gib = int64(1) << 30
	)
	f := &migrate.Flash{Capture: &migrate.Capture{Boot: migrate.Boot{Mode: "usb"}, CapturedAt: at.Add(-3 * time.Hour).Format(time.RFC3339)}}
	if flash != nil {
		f = flash
	}
	weak := true
	strong := false
	rv := &migrate.Review{Boot: f.ReviewBoot(), Capture: f.ReviewCapture()}
	disk3 := migrate.ReviewDisk{Slot: "disk3", DiskNumber: 3, UnraidID: "EXAMPLE_DISK3", UnraidRole: migrate.UnraidData, Device: "/dev/sdd", Serial: "EXAMPLE_DISK3", ByID: "ata-EXAMPLE_DISK3", Model: "EXAMPLE 4TB", Size: 4 * tib, Filesystem: "xfs", WeakIdentity: &strong}
	if disk3Refusal != "" {
		disk3.Refused, disk3.RefusalCode, disk3.Refusal = true, migrate.RefuseIntegrity, disk3Refusal
	} else {
		disk3.ProposedRole = migrate.ProposeData
	}
	rv.Disks = []migrate.ReviewDisk{
		{Slot: "parity", UnraidID: "EXAMPLE_PARITY", UnraidRole: migrate.UnraidParity, ProposedRole: migrate.ProposeParity, Device: "/dev/sdb", Serial: "EXAMPLE_PARITY", WWN: "0x5000c500a1b2c3d4", ByID: "ata-EXAMPLE_PARITY", Model: "EXAMPLE 8TB", Size: 8 * tib, Filesystem: "xfs", WeakIdentity: &strong},
		{Slot: "disk1", DiskNumber: 1, UnraidID: "EXAMPLE_DISK1", UnraidRole: migrate.UnraidData, ProposedRole: migrate.ProposeData, Device: "/dev/sdc", Serial: "EXAMPLE_DISK1", ByID: "ata-EXAMPLE_DISK1", Model: "EXAMPLE 4TB", Size: 4 * tib, Filesystem: "xfs", WeakIdentity: &strong},
		{Slot: "disk2", DiskNumber: 2, UnraidID: "EXAMPLE_DISK2", UnraidRole: migrate.UnraidData, Size: 4 * tib, Problem: "no disk on this machine has this serial or WWN"},
		disk3,
		{Slot: "disk4", DiskNumber: 4, UnraidID: "EXAMPLE_DISK4", UnraidRole: migrate.UnraidData, ProposedRole: migrate.ProposeData, Device: "/dev/sde", Serial: "EXAMPLE_DISK4", Model: "EXAMPLE USB 2TB", Size: 2 * tib, Filesystem: "xfs", WeakIdentity: &weak},
		{Slot: "pool cache", UnraidID: "EXAMPLE_CACHE", UnraidRole: migrate.UnraidCache, ProposedRole: migrate.ProposeCache, Device: "/dev/nvme0n1", Serial: "EXAMPLE_CACHE", ByID: "nvme-EXAMPLE_CACHE", Model: "EXAMPLE NVMe 500GB", Size: 500 * gib, Filesystem: "btrfs", WeakIdentity: &strong},
	}
	// This machine's disks that make a boot row. The stick is attached whatever
	// the boot mode, as production lists any Unraid stick it finds; the disks of
	// an internal boot are the ones the capture names by serial, which the mock
	// attaches as NVMe devices. An internal boot that shares its disk with the
	// cache is that disk as the cache pool's row, as production builds it.
	machine := []disk.Disk{{Device: mockFlashDevice, Size: 16 * disk.GB, Model: "SanDisk Cruzer Fit", Serial: "4C530001240603119335", Filesystem: disk.UnraidStickFilesystem, Label: disk.UnraidStickLabel}}
	if rv.Boot.Mode == "internal" {
		shared := rv.Boot.SharedWithCache != nil && *rv.Boot.SharedWithCache
		for i, bd := range f.Capture.Boot.Devices {
			if bd.Serial == "" {
				continue
			}
			machine = append(machine, disk.Disk{Device: fmt.Sprintf("/dev/nvme%dn1", i+1), Serial: bd.Serial, Model: bd.Model, Size: 500 * gib, Filesystem: "zfs_member", UnraidBoot: true})
		}
		// Debian is installed on the shared disk: the shared NVMe of doc 01 §6.
		if shared && len(machine) > 1 {
			machine[1].Boot = true
			b := machine[1]
			for i := range rv.Disks {
				if rv.Disks[i].Slot == "pool cache" {
					hostBoot := b.Boot
					rv.Disks[i].HostBoot = &hostBoot
					rv.Disks[i].UnraidID, rv.Disks[i].Device, rv.Disks[i].Serial, rv.Disks[i].ByID = b.Serial, b.Device, b.Serial, "nvme-"+b.Serial
					rv.Disks[i].WWN, rv.Disks[i].Model, rv.Disks[i].Size = "", b.Model, b.Size
				}
			}
		}
	}
	rv.Disks = migrate.AddBootDisks(rv.Disks, f, machine)
	rv.Disks = append(rv.Disks, migrate.ReviewDisk{UnraidRole: migrate.UnraidUnassigned, Device: "/dev/sdf", Serial: "EXAMPLE_SPARE", Model: "EXAMPLE 1TB", Size: tib, Filesystem: "ext4", WeakIdentity: &strong})
	// Every row with a disk of this machine says whether it is the one the mock
	// boots from, which only the shared NVMe above is.
	for i := range rv.Disks {
		if rv.Disks[i].Device != "" && rv.Disks[i].HostBoot == nil {
			notBoot := false
			rv.Disks[i].HostBoot = &notBoot
		}
	}
	rv.Shares = []migrate.SharePreview{
		{Name: "media", AllocationMethod: "highwater", HighWater: true, Include: []string{}, Exclude: []string{}, WarningCount: 1},
		{Name: "backup", AllocationMethod: "fillup", Include: []string{}, Exclude: []string{"disk3"}},
		{Name: "documents", AllocationMethod: "mostfree", Include: []string{}, Exclude: []string{}},
	}
	return rv
}

// mockMigrationImport is what the report's scan parsed, matching the rows and
// the share preview above: three shares and two accounts, which the import
// seeds.
func mockMigrationImport() migrate.Import {
	return migrate.Import{
		Shares: []migrate.Share{
			{Name: "backup", Allocator: "fillup", CreatePolicy: pool.FillDisksInOrder, UseCache: "no", CacheMode: pool.ArrayOnly, Export: "-", Security: "private", Exclude: []string{"disk3"}, ReadList: []string{"bob"}},
			{Name: "documents", Allocator: "mostfree", CreatePolicy: pool.BalanceAcrossDisks, UseCache: "yes", CacheMode: pool.CacheThenMove, Export: "e", Security: "public"},
			{Name: "media", Allocator: "highwater", CreatePolicy: pool.BalanceAcrossDisks, UseCache: "no", CacheMode: pool.ArrayOnly, Export: "e", Security: "private", SplitLevel: "2", Floor: "50000000", ReadList: []string{"bob"}, WriteList: []string{"alice"}},
		},
		Users: []string{"alice", "bob"},
	}
}

// seedMockMigration creates the shares and accounts the import seeds, from the
// same plan production's job builds, and skips a name that already exists as
// production does.
func (h *handler) seedMockMigration(plan migrate.SeedPlan) {
	now := time.Now().UTC().Truncate(time.Second)
	h.usersMu.Lock()
	ids := map[string]uuid.UUID{}
	for _, u := range h.users {
		ids[strings.ToLower(u.Username)] = u.ID
	}
	for _, name := range plan.Users {
		if _, ok := ids[name]; ok {
			continue
		}
		u := apiv1.UserSummary{ID: uuid.New(), Username: name, Role: apiv1.UserRoleShareOnly, CreatedAt: now}
		u.LastLogin.SetToNull()
		h.users[u.ID] = u
		ids[name] = u.ID
	}
	h.usersMu.Unlock()

	h.mu.Lock()
	created := map[string]share.SeedShare{}
	for _, sh := range plan.Shares {
		if _, ok := h.shares[sh.Name]; ok {
			continue
		}
		s := apiv1.Share{
			Name:         apiv1.ShareName(sh.Name),
			Path:         "/mnt/user/" + sh.Name,
			CacheMode:    apiv1.ShareCacheModeArrayOnly,
			CreatePolicy: apiv1.ArrayCreatePolicy(sh.CreatePolicy),
			Smb:          apiv1.ShareSMB{Enabled: sh.SMB.Enabled, Guest: sh.SMB.Guest, Browseable: sh.SMB.Browseable},
			Nfs:          defaultShareNFS(sh.Name),
			Usage:        apiv1.NilShareUsage{Null: true},
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if sh.MinFreeSpace != "" {
			s.MinFreeSpace = apiv1.NewOptString(sh.MinFreeSpace)
		}
		if sh.TargetCacheMode != "" || len(sh.Notes) > 0 {
			m := apiv1.ShareMigration{Notes: append([]string{}, sh.Notes...)}
			if sh.TargetCacheMode != "" {
				m.TargetCacheMode = apiv1.NewOptShareCacheMode(apiv1.ShareCacheMode(sh.TargetCacheMode))
			}
			s.Migration = apiv1.NewOptShareMigration(m)
		}
		h.shares[sh.Name] = s
		created[sh.Name] = sh
	}
	h.mu.Unlock()

	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	for name, sh := range created {
		result := apiv1.SharePermissionsResult{Users: []apiv1.UserPermissionEntry{}, Groups: []apiv1.GroupPermissionEntry{}}
		for _, a := range sh.Access {
			id := ids[a.Username]
			result.Users = append(result.Users, apiv1.UserPermissionEntry{UserId: id, Username: a.Username, Access: apiv1.ShareAccessLevel(a.Access)})
			user := h.userSharePermissions[id]
			user.Permissions = append(user.Permissions, apiv1.UserSharePermission{ShareName: apiv1.ShareName(name), Access: apiv1.ShareAccessLevel(a.Access)})
			h.userSharePermissions[id] = user
		}
		h.sharePermissions[apiv1.ShareName(name)] = result
	}
}

func seededMigration(scenario string) *mockMigration {
	m := &mockMigration{}
	if scenario == "migration-pending" {
		at := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
		m.report, m.size, m.receivedAt = mockMigrationReport(nil, "7.3.2", false, at, false), 612<<20, at
	}
	return m
}

func (h *handler) StartMigrationScan(ctx context.Context, req *apiv1.StartMigrationScanReq) (*apiv1.Job, error) {
	if req == nil || req.File.File == nil {
		return nil, &mockError{code: "file_required", statusCode: 400, message: "the Flash Backup zip is required as the file part"}
	}
	unverified := req.UnverifiedLayout.Or(false)
	var size countingReader
	size.r = req.File.File
	flash, err := migrate.InspectUpload(&size, migrate.ScanOptions{UnverifiedLayout: unverified})
	if err != nil {
		return nil, mockMigrateError(err)
	}
	j, err := h.queueMockJobIn(apiv1.JobTypeMigrationScan, apiv1.JobClassTopology)
	if err != nil {
		return nil, err
	}
	// The mock has no scheduler, so the scan is done as soon as it is queued.
	now := time.Now().UTC().Truncate(time.Second)
	h.mu.Lock()
	j.Status = apiv1.JobStatusSucceeded
	j.StartedAt = apiv1.NewOptNilDateTime(now)
	j.FinishedAt = apiv1.NewOptNilDateTime(now)
	h.jobs[j.ID] = *j
	h.mu.Unlock()

	h.migration.mu.Lock()
	rescan := h.migration.report != nil
	h.migration.mu.Unlock()
	report := mockMigrationReport(flash, flash.Version, flash.LayoutProblem() != "", now, rescan)
	if flash.Capture != nil {
		report.BootMode = flash.Capture.Boot.Mode
	}
	h.migration.mu.Lock()
	h.migration.report = report
	h.migration.size, h.migration.device, h.migration.receivedAt = size.n, "", now
	h.migration.mu.Unlock()
	return j, nil
}

// StartMigrationDeviceScan answers as production does for the stick: a device
// that is not the one UNRAID-labelled FAT disk on offer is refused, and so is
// any stick once the session's capture says Unraid booted internally.
func (h *handler) StartMigrationDeviceScan(ctx context.Context, req *apiv1.StartMigrationDeviceScanReq) (*apiv1.Job, error) {
	if req == nil || req.Device == "" {
		return nil, &mockError{code: "invalid_flash_device", statusCode: 400, message: "the device is required"}
	}
	h.migration.mu.Lock()
	zipOnly := h.migration.zipOnly()
	h.migration.mu.Unlock()
	if zipOnly {
		return nil, errMigrationRefusal("zip_only_source", 409, migrate.ErrZipOnly)
	}
	if req.Device != mockFlashDevice {
		return nil, errMigrationRefusal("invalid_flash_device", 400, fmt.Errorf("%w: %s is not a FAT filesystem labelled UNRAID on a disk outside the array", migrate.ErrNotFlashDevice, req.Device))
	}
	j, err := h.queueMockJobIn(apiv1.JobTypeMigrationScan, apiv1.JobClassTopology)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Truncate(time.Second)
	h.mu.Lock()
	j.Status = apiv1.JobStatusSucceeded
	j.StartedAt = apiv1.NewOptNilDateTime(now)
	j.FinishedAt = apiv1.NewOptNilDateTime(now)
	h.jobs[j.ID] = *j
	h.mu.Unlock()

	h.migration.mu.Lock()
	h.migration.report = mockMigrationReport(nil, "7.3.2", req.UnverifiedLayout.Or(false), now, h.migration.report != nil)
	h.migration.size, h.migration.device, h.migration.receivedAt = 0, req.Device, now
	h.migration.mu.Unlock()
	return j, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (h *handler) GetMigration(ctx context.Context) (*apiv1.Migration, error) {
	h.migration.mu.Lock()
	defer h.migration.mu.Unlock()
	out := &apiv1.Migration{Phase: apiv1.MigrationPhaseNone, FlashDevices: []apiv1.MigrationFlashDevice{}, ZipOnly: h.migration.zipOnly()}
	if r := h.migration.report; r != nil {
		out.Phase = apiv1.MigrationPhaseScanned
		out.SourceReceivedAt = apiv1.NewOptDateTime(h.migration.receivedAt)
		if h.migration.device != "" {
			out.SourceDevice = apiv1.NewOptString(h.migration.device)
		} else {
			out.SourceSize = apiv1.NewOptInt64(h.migration.size)
		}
		out.Report = apiv1.NewOptMigrationReport(mockMigrationReportToAPI(r))
	}
	if h.migration.imported.Load() {
		out.Phase = apiv1.MigrationPhaseImported
		if v := h.migration.verify; v != nil {
			out.Verify = apiv1.NewOptMigrationVerify(*v)
			out.Phase = apiv1.MigrationPhaseVerifyFailed
			if v.Status == apiv1.MigrationVerifyStatusPassed {
				out.Phase = apiv1.MigrationPhaseVerified
				out.ParityInit = apiv1.NewOptMigrationParityInit(mockParityInit(h.migration.report, h.migration.roles))
			}
		}
	}
	if !out.ZipOnly {
		out.FlashDevices = append(out.FlashDevices, apiv1.MigrationFlashDevice{
			Device: mockFlashDevice, Size: 16 * disk.GB,
			Model: apiv1.NewOptString("SanDisk Cruzer Fit"), Serial: apiv1.NewOptString("4C530001240603119335"),
		})
	}
	return out, nil
}

// StartMigrationVerify answers as production does before the job: nothing is
// verified unless an import is pending. The job it queues has finished at once,
// as the scans do. The first run fails with the scenario's failing result and
// the job failed, every later run passes.
func (h *handler) StartMigrationVerify(ctx context.Context) (*apiv1.Job, error) {
	if !h.migration.imported.Load() {
		return nil, mockMigrateError(migrate.ErrVerifyNotPending)
	}
	j, err := h.queueMockJobIn(apiv1.JobTypeMigrationVerify, apiv1.JobClassTopology)
	if err != nil {
		return nil, err
	}
	h.migration.mu.Lock()
	h.migration.verifyRuns++
	name := "verify-failed.json"
	if h.migration.verifyRuns > 1 {
		name = "verify-passed.json"
	}
	h.migration.mu.Unlock()
	raw, err := migrationpending.Verify.ReadFile(name)
	if err != nil {
		return nil, err
	}
	var result apiv1.MigrationVerify
	if err := result.UnmarshalJSON(raw); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", name, err)
	}
	if err := result.Validate(); err != nil {
		return nil, fmt.Errorf("%s does not match the API's MigrationVerify: %w", name, err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	result.StartedAt, result.FinishedAt = now, apiv1.NewOptDateTime(now)
	h.mu.Lock()
	j.StartedAt = apiv1.NewOptNilDateTime(now)
	j.FinishedAt = apiv1.NewOptNilDateTime(now)
	j.Status = apiv1.JobStatusSucceeded
	if result.Status != apiv1.MigrationVerifyStatusPassed {
		j.Status = apiv1.JobStatusFailed
		j.Error = apiv1.NewOptNilError(apiv1.Error{Code: "job_failed", Message: "migration verify: " + migrate.ErrVerifyMismatch.Error()})
	}
	h.jobs[j.ID] = *j
	h.mu.Unlock()
	h.migration.mu.Lock()
	h.migration.verify = &result
	h.migration.mu.Unlock()
	return j, nil
}

// mockParityPlan resolves the confirmed mapping again over this mock's disks,
// with the checks production's migrate.Service.PlanParityInit makes of the
// recorded disks: every rule of the import, and no cache that is a partition of
// a possibly mirrored Unraid boot pool.
func mockParityPlan(report *migrate.Report, roles []disk.AdoptionAssignment) (disk.AdoptionPlan, error) {
	if report == nil || report.Review == nil {
		return disk.AdoptionPlan{}, migrate.ErrImportNoReview
	}
	listed := mockMachineDisks(report.Review)
	plan, err := migrate.PlanFromReview(report.Review, listed, roles)
	if err != nil {
		return disk.AdoptionPlan{}, err
	}
	if err := migrate.CheckBootCacheNotMirrored(report.Review, listed, plan.Plan); err != nil {
		return disk.AdoptionPlan{}, err
	}
	return plan.Plan, nil
}

// mockParityInit is what getMigration offers for the point of no return once a
// verify passed: production's migrate.ParityInitOf over the mock's plan, or why
// there is none.
func mockParityInit(report *migrate.Report, roles []disk.AdoptionAssignment) apiv1.MigrationParityInit {
	var info *migrate.ParityInit
	if plan, err := mockParityPlan(report, roles); err != nil {
		info = &migrate.ParityInit{Problem: err.Error(), Window: migrate.ParityInitWindow}
		if report != nil {
			info.Rollback = migrate.RollbackNotes(report.Review)
		}
	} else {
		info = migrate.ParityInitOf(plan, report.Review)
	}
	opt := func(s string) apiv1.OptString {
		if s == "" {
			return apiv1.OptString{}
		}
		return apiv1.NewOptString(s)
	}
	out := apiv1.MigrationParityInit{
		Finishing: info.Finishing, Confirmation: opt(info.Confirmation), Problem: opt(info.Problem), UnprotectedWindow: info.Window,
		Rollback: append([]string{}, info.Rollback...), Erases: make([]apiv1.MigrationParityErase, 0, len(info.Erases)),
	}
	for _, e := range info.Erases {
		item := apiv1.MigrationParityErase{Role: apiv1.MigrationParityEraseRole(e.Role), Device: e.Device, Serial: opt(e.Serial), Wwn: opt(e.WWN), Partition: e.Partition}
		if e.Size > 0 {
			item.Size = apiv1.NewOptInt64(e.Size)
		}
		out.Erases = append(out.Erases, item)
	}
	return out
}

// InitializeMigrationParity answers as production does, in production's order:
// nothing is initialised unless an import is pending (no_import_pending) and its
// latest verify passed (verify_required), whatever the request carries; then the
// typed confirmation must be the one the plan computes (confirmation_required).
// The job it queues has finished at once, as the others do, and leaves the array
// past its point of no return.
func (h *handler) InitializeMigrationParity(ctx context.Context, req *apiv1.MigrationInitializeParityRequest) (*apiv1.Job, error) {
	if !h.migration.imported.Load() {
		return nil, mockMigrateError(migrate.ErrParityNotPending)
	}
	h.migration.mu.Lock()
	report, roles, verify := h.migration.report, h.migration.roles, h.migration.verify
	h.migration.mu.Unlock()
	if verify == nil || verify.Status != apiv1.MigrationVerifyStatusPassed {
		return nil, mockMigrateError(fmt.Errorf("%w: the latest verify did not pass", migrate.ErrVerifyRequired))
	}
	plan, err := mockParityPlan(report, roles)
	if err != nil {
		return nil, mockMigrateError(err)
	}
	if req == nil || req.Confirmation == "" || req.Confirmation != plan.ParityInitConfirmation() {
		return nil, &mockError{code: "confirmation_required", statusCode: 409, message: "this operation requires an explicit confirmation"}
	}
	j, err := h.queueMockJobIn(apiv1.JobTypeMigrationParity, apiv1.JobClassTopology)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Truncate(time.Second)
	h.mu.Lock()
	j.Status = apiv1.JobStatusSucceeded
	j.StartedAt = apiv1.NewOptNilDateTime(now)
	j.FinishedAt = apiv1.NewOptNilDateTime(now)
	h.jobs[j.ID] = *j
	h.mu.Unlock()
	h.migration.mu.Lock()
	h.migration.initialized, h.migration.verify = true, nil
	h.migration.mu.Unlock()
	h.migration.imported.Store(false)
	return j, nil
}

func mockMigrationReportToAPI(r *migrate.Report) apiv1.MigrationReport {
	out := apiv1.MigrationReport{
		GeneratedAt: r.GeneratedAt, UnverifiedLayout: r.UnverifiedLayout, Verdict: apiv1.MigrationVerdict(r.Verdict),
		Rows: make([]apiv1.MigrationReportRow, 0, len(r.Rows)),
	}
	if r.UnraidVersion != "" {
		out.UnraidVersion = apiv1.NewOptString(r.UnraidVersion)
	}
	for _, row := range r.Rows {
		item := apiv1.MigrationReportRow{Check: row.Check, Status: apiv1.MigrationCheckStatus(row.Status), Detail: row.Detail}
		if row.Subject != "" {
			item.Subject = apiv1.NewOptString(row.Subject)
		}
		out.Rows = append(out.Rows, item)
	}
	if r.Review != nil {
		out.Review = apiv1.NewOptMigrationReview(mockMigrationReviewToAPI(r.Review))
	}
	return out
}

func mockMigrationReviewToAPI(rv *migrate.Review) apiv1.MigrationReview {
	out := apiv1.MigrationReview{
		Disks: make([]apiv1.MigrationDisk, 0, len(rv.Disks)), Shares: make([]apiv1.MigrationSharePreview, 0, len(rv.Shares)),
		Capture: apiv1.MigrationCapture{State: apiv1.MigrationCaptureState(rv.Capture.State)},
	}
	opt := func(s string) apiv1.OptString {
		if s == "" {
			return apiv1.OptString{}
		}
		return apiv1.NewOptString(s)
	}
	for _, d := range rv.Disks {
		item := apiv1.MigrationDisk{
			Slot: opt(d.Slot), UnraidId: opt(d.UnraidID), Device: opt(d.Device), Serial: opt(d.Serial), Wwn: opt(d.WWN),
			ById: opt(d.ByID), Model: opt(d.Model), Filesystem: opt(d.Filesystem), Problem: opt(d.Problem), Refusal: opt(d.Refusal),
			Refused: d.Refused,
		}
		if d.DiskNumber > 0 {
			item.DiskNumber = apiv1.NewOptInt(d.DiskNumber)
		}
		if d.Size > 0 {
			item.Size = apiv1.NewOptInt64(d.Size)
		}
		if d.UnraidRole != "" {
			item.UnraidRole = apiv1.NewOptMigrationUnraidRole(apiv1.MigrationUnraidRole(d.UnraidRole))
		}
		if d.ProposedRole != "" {
			item.ProposedRole = apiv1.NewOptMigrationProposedRole(apiv1.MigrationProposedRole(d.ProposedRole))
		}
		if d.RefusalCode != "" {
			item.RefusalCode = apiv1.NewOptMigrationRefusalCode(apiv1.MigrationRefusalCode(d.RefusalCode))
		}
		if d.WeakIdentity != nil {
			item.WeakIdentity = apiv1.NewOptBool(*d.WeakIdentity)
		}
		if d.UnraidBoot {
			item.UnraidBoot = apiv1.NewOptBool(true)
		}
		if d.HostBoot != nil {
			item.HostBoot = apiv1.NewOptBool(*d.HostBoot)
		}
		out.Disks = append(out.Disks, item)
	}
	for _, sh := range rv.Shares {
		out.Shares = append(out.Shares, apiv1.MigrationSharePreview{
			Name: sh.Name, AllocationMethod: opt(sh.AllocationMethod), HighWater: sh.HighWater,
			Include: sh.Include, Exclude: sh.Exclude, WarningCount: sh.WarningCount,
		})
	}
	if rv.Boot.Mode != "" {
		out.Boot.Mode = apiv1.NewOptMigrationBootMode(apiv1.MigrationBootMode(rv.Boot.Mode))
	}
	if rv.Boot.Mirrored != nil {
		out.Boot.Mirrored = apiv1.NewOptBool(*rv.Boot.Mirrored)
	}
	if rv.Boot.SharedWithCache != nil {
		out.Boot.SharedWithCache = apiv1.NewOptBool(*rv.Boot.SharedWithCache)
	}
	if rv.Capture.CapturedAt != nil {
		out.Capture.CapturedAt = apiv1.NewOptDateTime(*rv.Capture.CapturedAt)
	}
	return out
}

func (h *handler) GetMigrationReport(ctx context.Context) (apiv1.GetMigrationReportOK, error) {
	h.migration.mu.Lock()
	defer h.migration.mu.Unlock()
	if h.migration.report == nil {
		return apiv1.GetMigrationReportOK{}, errNoMigrationReport()
	}
	return apiv1.GetMigrationReportOK{Data: strings.NewReader(h.migration.report.Markdown())}, nil
}

// undoMigrationImport answers as production does: nothing is undone unless an
// import is pending (no_import_pending). The job it queues has finished at once,
// as the others do, and leaves the session scanned again with the verify result
// forgotten. The shares and accounts the import seeded stay, as in production.
func (h *handler) undoMigrationImport() (*apiv1.Job, error) {
	if !h.migration.imported.Load() {
		return nil, mockMigrateError(job.ErrMigrationUndoNotPending)
	}
	j, err := h.queueMockJobIn(apiv1.JobTypeMigrationImport, apiv1.JobClassTopology)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Truncate(time.Second)
	h.mu.Lock()
	j.Status = apiv1.JobStatusSucceeded
	j.StartedAt = apiv1.NewOptNilDateTime(now)
	j.FinishedAt = apiv1.NewOptNilDateTime(now)
	h.jobs[j.ID] = *j
	h.mu.Unlock()
	h.migration.mu.Lock()
	h.migration.roles, h.migration.verify = nil, nil
	h.migration.mu.Unlock()
	h.migration.imported.Store(false)
	return j, nil
}

func (h *handler) ForgetMigration(ctx context.Context) error {
	if h.migration.imported.Load() {
		return errMigrationInProgress()
	}
	h.migration.mu.Lock()
	defer h.migration.mu.Unlock()
	h.migration.report, h.migration.size, h.migration.device, h.migration.receivedAt = nil, 0, "", time.Time{}
	return nil
}

// mockComposeProject is the compose.yaml of the one Compose Manager project the
// mock's report lists.
const mockComposeProject = "services:\n  web:\n    image: example/web:1.0\n    container_name: stack-web\n    volumes:\n      - /mnt/user/appdata/stack:/data\n"

// mockMigrationTemplate is one template of the migration-pending scenario, converted by
// the production converter with the capture's br0 network.
type mockMigrationTemplate struct {
	file     string
	class    apiv1.MigrationTemplateClass
	position int
	conv     *template.Conversion
}

func mockMigrationTemplates() ([]mockMigrationTemplate, error) {
	classes := map[string]mockMigrationTemplate{
		"my-photos.xml":  {class: apiv1.MigrationTemplateClassAutostart, position: 1},
		"my-gateway.xml": {class: apiv1.MigrationTemplateClassRunning},
	}
	networks := []template.NetworkDef{{
		Name: "br0", Driver: "ipvlan", Subnet: "192.168.50.0/24", Gateway: "192.168.50.1", Parent: "ens20",
		Options: map[string]string{"ipvlan_mode": "l2", "parent": "ens20"},
	}}
	entries, err := fs.ReadDir(migrationpending.Templates, ".")
	if err != nil {
		return nil, err
	}
	var out []mockMigrationTemplate
	for _, e := range entries {
		data, err := fs.ReadFile(migrationpending.Templates, e.Name())
		if err != nil {
			return nil, err
		}
		conv, err := template.ConvertUnraid(data, template.ConvertOptions{Networks: networks})
		if err != nil {
			return nil, fmt.Errorf("converting the fixture template %s: %w", e.Name(), err)
		}
		t := classes[e.Name()]
		t.file, t.conv = e.Name(), conv
		out = append(out, t)
	}
	return out, nil
}

func actionWarnings(c *template.Conversion) int {
	n := 0
	for _, w := range c.Warnings {
		if w.Class != template.WarnWritableLayer && w.Class != template.WarnNote {
			n++
		}
	}
	return n
}

func (h *handler) requireMigrationReport() error {
	h.migration.mu.Lock()
	defer h.migration.mu.Unlock()
	if h.migration.report == nil {
		return errNoMigrationReport()
	}
	return nil
}

func (h *handler) ListMigrationTemplates(ctx context.Context) (*apiv1.MigrationTemplates, error) {
	if err := h.requireMigrationReport(); err != nil {
		return nil, err
	}
	templates, err := mockMigrationTemplates()
	if err != nil {
		return nil, err
	}
	out := &apiv1.MigrationTemplates{
		Templates: make([]apiv1.MigrationTemplateSummary, 0, len(templates)),
		ComposeProjects: []apiv1.MigrationComposeProjectSummary{
			{Name: "stack", Containers: []string{"stack-web"}, Status: apiv1.MigrationTemplateStatusPreviewed},
		},
	}
	out.Counts.ComposeProjects = 1
	for _, t := range templates {
		status := apiv1.MigrationTemplateStatusClean
		if !t.conv.Clean() {
			status = apiv1.MigrationTemplateStatusWarnings
			out.Counts.WithWarnings++
		} else {
			out.Counts.Clean++
		}
		item := apiv1.MigrationTemplateSummary{
			Name: t.conv.Metadata.Title, File: t.file, Class: t.class, Counted: true,
			Status: status, WarningCount: actionWarnings(t.conv),
		}
		if t.position > 0 {
			item.AutostartPosition = apiv1.NewOptInt(t.position)
		}
		out.Templates = append(out.Templates, item)
	}
	return out, nil
}

func (h *handler) GetMigrationTemplate(ctx context.Context, params apiv1.GetMigrationTemplateParams) (*apiv1.MigrationTemplatePreview, error) {
	if err := h.requireMigrationReport(); err != nil {
		return nil, err
	}
	h.migration.mu.Lock()
	fromStick := h.migration.device != ""
	h.migration.mu.Unlock()
	if fromStick {
		return nil, errMigrationRefusal("template_source_unavailable", 409, migrate.ErrSourceUnavailable)
	}
	opt := func(s string) apiv1.OptString {
		if s == "" {
			return apiv1.OptString{}
		}
		return apiv1.NewOptString(s)
	}
	if params.Name == "stack" {
		return &apiv1.MigrationTemplatePreview{
			Kind: apiv1.MigrationTemplatePreviewKindComposeProject, Name: "stack", Status: apiv1.MigrationTemplateStatusPreviewed,
			Source: mockComposeProject, Compose: apiv1.NewOptString(mockComposeProject),
			Warnings: []apiv1.ConversionWarning{}, Privileges: []apiv1.TemplatePrivilege{},
		}, nil
	}
	templates, err := mockMigrationTemplates()
	if err != nil {
		return nil, err
	}
	for _, t := range templates {
		if t.file != params.Name {
			continue
		}
		out := &apiv1.MigrationTemplatePreview{
			Kind: apiv1.MigrationTemplatePreviewKindTemplate, Name: t.file, Title: apiv1.NewOptString(t.conv.Metadata.Title),
			Class: apiv1.NewOptMigrationTemplateClass(t.class), Counted: apiv1.NewOptBool(true), Status: apiv1.MigrationTemplateStatusClean,
			Source: t.conv.Source, Compose: apiv1.NewOptString(t.conv.Compose),
			Warnings: make([]apiv1.ConversionWarning, len(t.conv.Warnings)), Privileges: make([]apiv1.TemplatePrivilege, len(t.conv.Privileges)),
		}
		if !t.conv.Clean() {
			out.Status = apiv1.MigrationTemplateStatusWarnings
		}
		for i, w := range t.conv.Warnings {
			out.Warnings[i] = apiv1.ConversionWarning{Class: apiv1.ConversionWarningClass(w.Class), Message: w.Message, Detail: opt(w.Detail), Command: opt(w.Command)}
		}
		for i, pr := range t.conv.Privileges {
			out.Privileges[i] = apiv1.TemplatePrivilege{Kind: apiv1.TemplatePrivilegeKind(pr.Kind), Service: pr.Service, Description: pr.Description, Detail: opt(pr.Detail)}
		}
		return out, nil
	}
	return nil, &mockError{code: "template_not_found", statusCode: 404, message: migrate.ErrTemplateNotFound.Error()}
}

// mockMachineDisks is this mock machine's disk inventory as production's
// listing would give it for the scenario's review: one disk per row with a
// device, each with a filesystem on its first partition, a by-id link when the
// row has one, and a UUID of its own. The stick is the FAT disk labelled
// UNRAID.
func mockMachineDisks(rv *migrate.Review) []disk.Disk {
	var out []disk.Disk
	for _, r := range rv.Disks {
		if r.Device == "" {
			continue
		}
		d := disk.Disk{
			Device: r.Device, Serial: r.Serial, WWN: r.WWN, ByIDName: r.ByID, Model: r.Model, Size: r.Size,
			Filesystem: r.Filesystem, FSDevice: r.Device + "1", UnraidBoot: r.UnraidBoot,
		}
		if r.WeakIdentity != nil {
			d.WeakIdentity = *r.WeakIdentity
		}
		if r.HostBoot != nil {
			d.Boot = *r.HostBoot
		}
		if r.ByID != "" {
			d.FSByIDName = r.ByID + "-part1"
		}
		if r.UnraidBoot && r.ByID != "" && r.Size > 0 {
			d.UnraidDataPartition = &disk.BootPartition{
				Device: r.Device + "p4", Size: r.Size / 2, ByIDName: r.ByID + "-part4",
				PartUUID: fmt.Sprintf("%s-04", r.Serial),
			}
		}
		if r.Device == mockFlashDevice {
			d.Filesystem, d.Label = disk.UnraidStickFilesystem, disk.UnraidStickLabel
		}
		if d.Filesystem != "" {
			h := fnv.New64a()
			_, _ = h.Write([]byte(r.Device + r.Serial))
			d.FSUUID = fmt.Sprintf("00000000-0000-4000-8000-%012x", h.Sum64()&0xffffffffffff)
		}
		out = append(out, d)
	}
	return out
}

// arrayScenario is the scenario the pool answers for. migration-pending is the
// machine at the start of the import: Unraid's disks are not adopted yet, so it
// serves no array until an import has recorded one; every other scenario serves
// its own.
func (h *handler) arrayScenario() string {
	if h.scenario == "migration-pending" && !h.migration.imported.Load() && !h.migration.initializedNow() {
		return "fresh-install"
	}
	return h.scenario
}

func errArrayExistsNotPending() error {
	return &mockError{code: "array_exists", statusCode: 409, message: "an array already exists and is not an Unraid import waiting for its point of no return"}
}

// StartMigrationImport answers as production does: the same checks of the
// report and of the mapping (migrate.CheckImportable and migrate.PlanFromReview,
// over this mock's own disks) with the same error codes, and the same refusal
// of an array that is not a pending import's (array_exists), which is every
// scenario's array but migration-pending's once imported. The job it queues has
// finished at once, as the scans do, and leaves the session in the imported
// phase. With undo it takes a pending import back instead.
func (h *handler) StartMigrationImport(ctx context.Context, req *apiv1.MigrationImportRequest) (*apiv1.Job, error) {
	if req == nil || !req.Confirm {
		return nil, &mockError{code: "confirmation_required", statusCode: 409, message: "this operation requires an explicit confirmation"}
	}
	if req.Undo.Or(false) {
		if len(req.Roles) != 0 {
			return nil, errMigrationRefusal("invalid_import_roles", 400, errors.New("an undo takes no disk-role mapping"))
		}
		return h.undoMigrationImport()
	}
	if len(req.Roles) == 0 {
		return nil, errMigrationRefusal("invalid_import_roles", 400, errors.New("the import needs a disk-role mapping: at least one disk"))
	}
	assignments := make([]disk.AdoptionAssignment, 0, len(req.Roles))
	for _, r := range req.Roles {
		assignments = append(assignments, disk.AdoptionAssignment{
			Role: disk.AdoptionRole(r.Role), Serial: r.Serial.Or(""), WWN: r.Wwn.Or(""),
			ByIDName: r.ById.Or(""), PartUUID: r.PartUuid.Or(""),
		})
	}
	h.migration.mu.Lock()
	report := h.migration.report
	h.migration.mu.Unlock()
	if err := migrate.CheckImportable(report, false); err != nil {
		return nil, mockMigrateError(err)
	}
	ip, err := migrate.PlanFromReview(report.Review, mockMachineDisks(report.Review), assignments)
	if err != nil {
		return nil, mockMigrateError(err)
	}
	if mockArrayDisks(h.arrayScenario()) != nil && !h.migration.imported.Load() {
		return nil, errArrayExistsNotPending()
	}
	if err := job.CheckAdoptionLayout(ip.Plan); err != nil {
		return nil, mockMigrateError(err)
	}
	j, err := h.queueMockJobIn(apiv1.JobTypeMigrationImport, apiv1.JobClassTopology)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Truncate(time.Second)
	h.mu.Lock()
	j.Status = apiv1.JobStatusSucceeded
	j.StartedAt = apiv1.NewOptNilDateTime(now)
	j.FinishedAt = apiv1.NewOptNilDateTime(now)
	h.jobs[j.ID] = *j
	h.mu.Unlock()
	// An import that runs again forgets the verify result, as production's does.
	h.migration.mu.Lock()
	h.migration.roles, h.migration.verify = assignments, nil
	h.migration.mu.Unlock()
	h.migration.imported.Store(true)
	h.seedMockMigration(report.Import.SeedPlan())
	return j, nil
}
