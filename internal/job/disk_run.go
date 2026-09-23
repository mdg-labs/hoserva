package job

import (
	"context"
	"fmt"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

// arrayCreateCommand is the `hoserva <command>` the doc 01 §2 header on
// files generated after create-array names.
const arrayCreateCommand = "array create"

// DiskFormatDeps is everything RunDiskFormat needs after FormatPlan
// succeeds: persist topology in SQLite (D4) and drive the existing
// generators so mount units and snapraid.conf come from that state, not
// from job-params JSON.
type DiskFormatDeps struct {
	Provider  disk.Provider
	Runner    disk.Runner
	Store     *store.ArrayStore
	Generator *config.Generator
	Mounter   disk.UnitMounter
	// ArrayReady, when set, runs once applyArrayFromStore has persisted
	// topology and mounted every physical disk: hoservad uses it to
	// rebuild its own job.ArraySequence from that freshly written
	// topology, the same construction a restart already does at startup,
	// so a live `POST /disks/array` leaves `array start` (and, once
	// called, `GET /pool`) working without a restart (#262). Optional —
	// tests that only exercise FormatPlan/apply need no such rebuild.
	ArrayReady func(ctx context.Context) error
	// Now, when set, stamps generated-file headers; nil uses time.Now.
	Now func() time.Time
}

// RunDiskFormat is the RunFunc hoservad registers for TypeDiskFormat: it
// reads persisted DiskFormatParams, calls disk.FormatPlan when no array
// topology exists yet, and only after that succeeds writes topology to
// SQLite and generates disk mount units, mergerfs pool units and
// snapraid.conf from that state (D4, D1). If PutArray has already
// succeeded and apply then failed, a retry of the same devices/roles
// skips FormatPlan and re-applies from SQLite only (Q29: this job type
// is not resumable from format). A retry whose plan does not match
// stored topology still returns store.ErrArrayExists and formats
// nothing. Confirmation and Validate run again on the format path so a
// queued job cannot skip the guard the handler already enforced
// (doc 03 §3.1 step 6). A failed FormatPlan writes no mount units and
// leaves SQLite with no array topology.
func RunDiskFormat(d DiskFormatDeps) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		params, err := decodeDiskFormatParams(rc.Params())
		if err != nil {
			return err
		}
		if d.Store == nil || d.Generator == nil || d.Mounter == nil {
			return fmt.Errorf("job: disk_format is missing array apply dependencies")
		}

		plan := params.Plan()

		exists, err := d.Store.Exists(ctx)
		if err != nil {
			return err
		}
		if exists {
			settings, disks, err := d.Store.GetArray(ctx)
			if err != nil {
				return err
			}
			if !planMatchesStored(plan, disks) {
				return store.ErrArrayExists
			}
			if err := applyArrayFromStore(ctx, d.Store, d.Generator, d.Mounter, settings.CreatedAt); err != nil {
				return err
			}
			return runArrayReady(ctx, d)
		}
		if err := plan.CheckConfirmation(params.Confirmation); err != nil {
			return err
		}
		if err := plan.Validate(params.Sizes); err != nil {
			return err
		}
		if err := disk.CheckFormatTargets(plan); err != nil {
			return err
		}
		if _, err := snapraidLayout(plan).Render(); err != nil {
			return err
		}

		if err := disk.FormatPlan(ctx, d.Provider, d.Runner, plan, params.Sizes, params.Confirmation); err != nil {
			return err
		}

		uuids, err := filesystemUUIDs(ctx, d.Runner, plan)
		if err != nil {
			return err
		}
		units, err := disk.MountPlan(plan, uuids)
		if err != nil {
			return err
		}

		now := time.Now
		if d.Now != nil {
			now = d.Now
		}
		created := now()

		if err := d.Store.PutArray(ctx, store.ArraySettings{
			CreatePolicy: arrayCreatePolicy(params),
			MinFreeSpace: arrayMinFreeSpace(params),
			CreatedAt:    created,
		}, arrayDisksFromPlan(plan, uuids, units)); err != nil {
			return err
		}

		if err := applyArrayFromStore(ctx, d.Store, d.Generator, d.Mounter, created); err != nil {
			return err
		}
		return runArrayReady(ctx, d)
	}
}

// runArrayReady calls d.ArrayReady, if set, once applyArrayFromStore has
// succeeded — a nil hook (every test that only exercises FormatPlan/apply)
// is a no-op, never a required dependency the way Store/Generator/Mounter
// are.
func runArrayReady(ctx context.Context, d DiskFormatDeps) error {
	if d.ArrayReady == nil {
		return nil
	}
	return d.ArrayReady(ctx)
}

func arrayCreatePolicy(p DiskFormatParams) string {
	if p.CreatePolicy != "" {
		return p.CreatePolicy
	}
	return string(pool.DefaultCreatePolicy)
}

func arrayMinFreeSpace(p DiskFormatParams) string {
	if p.MinFreeSpace != "" {
		return p.MinFreeSpace
	}
	return pool.DefaultOptions().MinFreeSpace
}

func filesystemUUIDs(ctx context.Context, r disk.Runner, plan disk.TopologyPlan) (map[string]string, error) {
	uuids := make(map[string]string)
	read := func(d disk.AssignedDisk) error {
		dev := d.Device
		if p := (disk.Identity{ByIDName: d.ByIDName}).IdentityPath(); p != "" {
			dev = p
		}
		uuid, err := disk.FilesystemUUID(ctx, r, dev)
		if err != nil {
			return err
		}
		uuids[d.Device] = uuid
		return nil
	}
	for _, d := range plan.Parity {
		if err := read(d); err != nil {
			return nil, err
		}
	}
	for _, d := range plan.Data {
		if err := read(d); err != nil {
			return nil, err
		}
	}
	if plan.Cache != nil {
		if err := read(*plan.Cache); err != nil {
			return nil, err
		}
	}
	return uuids, nil
}

func arrayDisksFromPlan(plan disk.TopologyPlan, uuids map[string]string, units []disk.MountUnit) []store.ArrayDisk {
	byWhere := make(map[string]disk.MountUnit, len(units))
	for _, u := range units {
		byWhere[u.Where] = u
	}
	var out []store.ArrayDisk
	appendRole := func(role string, disks []disk.AssignedDisk, whereFor func(int) string) {
		for i, d := range disks {
			where := whereFor(i)
			out = append(out, store.ArrayDisk{
				Role:         role,
				RoleIndex:    i + 1,
				Device:       d.Device,
				Filesystem:   string(d.Filesystem),
				FSUUID:       uuids[d.Device],
				WWN:          d.WWN,
				Serial:       d.Serial,
				ByIDName:     d.ByIDName,
				WeakIdentity: d.WeakIdentity,
				Mountpoint:   byWhere[where].Where,
			})
		}
	}
	appendRole(store.ArrayRoleData, plan.Data, func(i int) string { return fmt.Sprintf("/mnt/disk%d", i+1) })
	appendRole(store.ArrayRoleParity, plan.Parity, func(i int) string { return fmt.Sprintf("/mnt/parity%d", i+1) })
	if plan.Cache != nil {
		appendRole(store.ArrayRoleCache, []disk.AssignedDisk{*plan.Cache}, func(int) string { return "/mnt/cache" })
	}
	return out
}

// planMatchesStored reports whether plan names the same devices in the
// same roles (and role indexes) as topology already persisted in SQLite.
// A match is the gate for a silent re-apply; anything else is a
// different array and must not format, write units, or remount.
func planMatchesStored(plan disk.TopologyPlan, disks []store.ArrayDisk) bool {
	type assignment struct {
		device string
		role   string
		index  int
	}
	want := make(map[assignment]struct{}, len(plan.Parity)+len(plan.Data)+1)
	add := func(role string, assigned []disk.AssignedDisk) {
		for i, d := range assigned {
			want[assignment{d.Device, role, i + 1}] = struct{}{}
		}
	}
	add(store.ArrayRoleParity, plan.Parity)
	add(store.ArrayRoleData, plan.Data)
	if plan.Cache != nil {
		add(store.ArrayRoleCache, []disk.AssignedDisk{*plan.Cache})
	}
	if len(want) != len(disks) {
		return false
	}
	for _, d := range disks {
		if _, ok := want[assignment{d.Device, d.Role, d.RoleIndex}]; !ok {
			return false
		}
	}
	return true
}

func snapraidLayout(plan disk.TopologyPlan) parity.Layout {
	l := parity.Layout{
		ParityMounts: make([]string, len(plan.Parity)),
		DataMounts:   make([]string, len(plan.Data)),
	}
	for i := range plan.Parity {
		l.ParityMounts[i] = fmt.Sprintf("/mnt/parity%d", i+1)
	}
	for i := range plan.Data {
		l.DataMounts[i] = fmt.Sprintf("/mnt/disk%d", i+1)
	}
	if plan.Cache != nil {
		l.CacheMount = "/mnt/cache"
	}
	return l
}
