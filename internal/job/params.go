package job

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/parity"
)

// DefaultScrubPercent is doc 02 §2's scheduled-scrub default: 8% of the
// array. Used when startScrub omits percent.
const DefaultScrubPercent = 8

// SyncParams is startSync's persisted request payload.
type SyncParams struct {
	DryRun  bool `json:"dryRun"`
	Confirm bool `json:"confirm"`
}

// ScrubParams is startScrub's persisted request payload. Percent nil
// means the engine's existing default (DefaultScrubPercent).
type ScrubParams struct {
	Percent *int `json:"percent,omitempty"`
}

// FixParams is startFix's persisted request payload. Disk is the SnapRAID
// disk index (`hoserva fix --disk N`); nil means the engine's whole-array
// fix.
type FixParams struct {
	Confirm bool `json:"confirm"`
	Disk    *int `json:"disk,omitempty"`
}

// shareRelocationToArray and shareRelocationToCache are
// ShareRelocationParams.To's only valid values, matching
// api/openapi.yaml's StartShareRelocationRequest `to` enum and doc 09
// §2's `hoserva share relocate <share> --to cache|array`.
const (
	shareRelocationToArray = "array"
	shareRelocationToCache = "cache"
)

// DiskUpgradeDataParams is upgradeDisk's persisted Topology-job payload
// for a data-disk upgrade (doc 02 §4 "Larger data disk"): the slot being
// upgraded, the old disk A as SQLite named it at submit (its identity
// and filesystem UUID are fixed here, UR4), the new, larger disk B, the
// typed confirmation planDiskUpgrade's plan required, and a disk-size
// snapshot for the Q20 re-check. B keeps the slot's mountpoint.
type DiskUpgradeDataParams struct {
	Confirmation string            `json:"confirmation"`
	Mountpoint   string            `json:"mountpoint"`
	Old          disk.AssignedDisk `json:"old"`
	Disk         disk.AssignedDisk `json:"disk"`
	Sizes        map[string]int64  `json:"sizes"`
}

// DiskUpgradeParityParams is upgradeDisk's persisted Topology-job payload
// for a parity-disk upgrade (doc 02 §4 "Larger parity disk", #289): the
// existing parity slot being upgraded, the new, larger disk, the fresh
// mountpoint planDiskUpgrade computed for it (never the old slot's own —
// the new parity disk is mounted and verified independently before the
// configuration ever names it, Q71), the typed confirmation, and a
// disk-size snapshot. NewMountpoint is fixed at submit time, not
// recomputed at run time: RunDiskUpgradeParity only ever formats the new
// disk once, on its very first invocation (a resumed run must never
// reformat a disk that may already hold a partial parity-file copy), so
// the mountpoint it was formatted and mounted at must stay the same
// across every resume.
type DiskUpgradeParityParams struct {
	Confirmation  string            `json:"confirmation"`
	Mountpoint    string            `json:"mountpoint"`
	NewMountpoint string            `json:"newMountpoint"`
	Disk          disk.AssignedDisk `json:"disk"`
	Sizes         map[string]int64  `json:"sizes"`
}

// ShareRelocationParams is startShareRelocation's persisted request
// payload: the share to relocate and the direction (doc 09 §2, #239).
type ShareRelocationParams struct {
	Share string `json:"share"`
	To    string `json:"to"`
}

// DiskFormatParams is createArray's persisted Topology-job payload: the
// wizard's plan, sizes from the same Provider.List call that populated
// inventory, and the typed confirmation FormatPlan checks again at run
// time so a queued job cannot skip the guard.
type DiskFormatParams struct {
	Confirmation string              `json:"confirmation"`
	Parity       []disk.AssignedDisk `json:"parity"`
	Data         []disk.AssignedDisk `json:"data"`
	Cache        *disk.AssignedDisk  `json:"cache,omitempty"`
	Sizes        map[string]int64    `json:"sizes"`
	CreatePolicy string              `json:"createPolicy,omitempty"`
	MinFreeSpace string              `json:"minFreeSpace,omitempty"`
}

// Plan reconstructs the TopologyPlan FormatPlan consumes.
func (p DiskFormatParams) Plan() disk.TopologyPlan {
	return disk.TopologyPlan{Parity: p.Parity, Data: p.Data, Cache: p.Cache}
}

// DiskAddParams is addDisk's persisted Topology-job payload (doc 02 §4
// "Adding a disk"): the disk to format or adopt, the typed confirmation
// planDiskAdd's own plan required, and every currently-listed disk's size
// from that same disk.Provider.List call — RunDiskAdd re-validates Q20
// against the array's current topology plus this snapshot rather than
// trusting the queued plan alone, the same defense-in-depth
// createArray's own FormatPlan already applies to DiskFormatParams.
type DiskAddParams struct {
	Confirmation string            `json:"confirmation"`
	Disk         disk.AssignedDisk `json:"disk"`
	Sizes        map[string]int64  `json:"sizes"`
}

// RebalanceParams is startRebalance's persisted job payload: the exact
// plan `planRebalance`/`startRebalance` most recently computed with
// `cache.PlanRebalance` immediately before this job was submitted (doc 09
// §3) — RunRebalance takes it as a value rather than recomputing it
// (cache.RebalancePlan's own doc comment), so what this job runs is
// exactly what the confirming request most recently saw, never a plan a
// client supplied directly (which could name branches this job has no
// business touching).
type RebalanceParams struct {
	Plan cache.RebalancePlan `json:"plan"`
}

// EvacuationParams is evacuateDisk's persisted job payload: the disk
// being evacuated and the exact plan `planDiskEvacuation`/`evacuateDisk`
// most recently computed for it with `cache.PlanEvacuation` (doc 09 §4),
// the same "the job runs the plan as a value, never recomputes or trusts
// a client-supplied one" shape RebalanceParams uses.
type EvacuationParams struct {
	Mountpoint string              `json:"mountpoint"`
	Plan       cache.RebalancePlan `json:"plan"`
}

// DiskRemoveParams is finishDiskRemoval's persisted Topology-job payload
// (doc 09 §4 steps 7-9, #358): the evacuated data disk's slot and the
// typed confirmation, which RunDiskRemove checks again itself.
type DiskRemoveParams struct {
	Mountpoint   string `json:"mountpoint"`
	Confirmation string `json:"confirmation"`
}

// DiskReplaceParams is replaceDisk's persisted Topology-job payload
// (doc 02 §4 "Replacing a failed disk"): the existing data-disk slot being
// replaced, the replacement disk, the typed confirmation planDiskReplace's
// own plan required, and a disk-size snapshot for the same Q20 re-check
// DiskAddParams carries.
type DiskReplaceParams struct {
	Confirmation string            `json:"confirmation"`
	Mountpoint   string            `json:"mountpoint"`
	Disk         disk.AssignedDisk `json:"disk"`
	Sizes        map[string]int64  `json:"sizes"`
}

// ValidateParams checks params against t at Submit time (not in SQL).
// Empty or null is valid for types whose payload is optional. TypeFix
// requires confirm=true, so an absent payload is rejected. Types that
// have no payload reject any other JSON.
func ValidateParams(t Type, params []byte) error {
	params = bytes.TrimSpace(params)
	if len(params) == 0 || string(params) == "null" {
		if t == TypeFix {
			return fmt.Errorf("job: fix params require confirm=true")
		}
		if t == TypeDiskFormat {
			return fmt.Errorf("job: disk_format params require confirmation")
		}
		if t == TypeShareRelocation {
			return fmt.Errorf("job: share_relocation params require share and to")
		}
		if t == TypeDiskAdd {
			return fmt.Errorf("job: disk_add params require confirmation")
		}
		if t == TypeDiskReplace {
			return fmt.Errorf("job: disk_replace params require confirmation")
		}
		if t == TypeDiskUpgradeData {
			return fmt.Errorf("job: disk_upgrade_data params require confirmation")
		}
		if t == TypeDiskUpgradeParity {
			return fmt.Errorf("job: disk_upgrade_parity params require confirmation")
		}
		if t == TypeRebalance {
			return fmt.Errorf("job: rebalance params require a plan")
		}
		if t == TypeEvacuation {
			return fmt.Errorf("job: evacuation params require a mountpoint and a plan")
		}
		if t == TypeDiskRemove {
			return fmt.Errorf("job: disk_remove params require a mountpoint and confirmation")
		}
		return nil
	}
	switch t {
	case TypeSync:
		_, err := decodeSyncParams(params)
		return err
	case TypeScrub:
		_, err := decodeScrubParams(params)
		return err
	case TypeFix:
		_, err := decodeFixParams(params)
		return err
	case TypeDiskFormat:
		_, err := decodeDiskFormatParams(params)
		return err
	case TypeDiskAdd:
		_, err := decodeDiskAddParams(params)
		return err
	case TypeDiskReplace:
		_, err := decodeDiskReplaceParams(params)
		return err
	case TypeDiskRemove:
		_, err := decodeDiskRemoveParams(params)
		return err
	case TypeDiskUpgradeData:
		_, err := decodeDiskUpgradeDataParams(params)
		return err
	case TypeDiskUpgradeParity:
		_, err := decodeDiskUpgradeParityParams(params)
		return err
	case TypeShareRelocation:
		_, err := decodeShareRelocationParams(params)
		return err
	case TypeRebalance:
		_, err := decodeRebalanceParams(params)
		return err
	case TypeEvacuation:
		_, err := decodeEvacuationParams(params)
		return err
	case TypeACMEIssue:
		_, err := decodeACMEIssueParams(params)
		return err
	default:
		return fmt.Errorf("job: type %s does not take params", t)
	}
}

// SyncOptsFromParams maps persisted startSync params onto SyncOpts.
// Confirm only authorizes proceeding past a fresh threshold-guard block;
// it never skips the evaluation itself (doc 02 §2).
func SyncOptsFromParams(params []byte) (parity.SyncOpts, error) {
	p, err := decodeSyncParams(bytes.TrimSpace(params))
	if err != nil {
		return parity.SyncOpts{}, err
	}
	return parity.SyncOpts{DryRun: p.DryRun, Confirm: p.Confirm}, nil
}

// ScrubPercentFromParams maps persisted startScrub params onto Engine.Scrub's
// percent argument. Omitted percent is DefaultScrubPercent.
func ScrubPercentFromParams(params []byte) (int, error) {
	p, err := decodeScrubParams(bytes.TrimSpace(params))
	if err != nil {
		return 0, err
	}
	if p.Percent == nil {
		return DefaultScrubPercent, nil
	}
	return *p.Percent, nil
}

// FixOptsFromParams maps persisted startFix params onto FixOpts. Disk
// index N becomes SnapRAID's data-disk label "dN" (Layout.Render).
func FixOptsFromParams(params []byte) (parity.FixOpts, error) {
	p, err := decodeFixParams(bytes.TrimSpace(params))
	if err != nil {
		return parity.FixOpts{}, err
	}
	opts := parity.FixOpts{}
	if p.Disk != nil {
		opts.Disk = fmt.Sprintf("d%d", *p.Disk)
	}
	return opts, nil
}

func decodeSyncParams(params []byte) (SyncParams, error) {
	if len(params) == 0 || string(params) == "null" {
		return SyncParams{}, nil
	}
	var p SyncParams
	if err := decodeJSON(params, &p); err != nil {
		return SyncParams{}, err
	}
	return p, nil
}

func decodeScrubParams(params []byte) (ScrubParams, error) {
	if len(params) == 0 || string(params) == "null" {
		return ScrubParams{}, nil
	}
	var p ScrubParams
	if err := decodeJSON(params, &p); err != nil {
		return ScrubParams{}, err
	}
	if p.Percent != nil && (*p.Percent < 1 || *p.Percent > 100) {
		return ScrubParams{}, fmt.Errorf("job: scrub percent must be between 1 and 100")
	}
	return p, nil
}

func decodeFixParams(params []byte) (FixParams, error) {
	if len(params) == 0 || string(params) == "null" {
		return FixParams{}, fmt.Errorf("job: fix params require confirm=true")
	}
	var p FixParams
	if err := decodeJSON(params, &p); err != nil {
		return FixParams{}, err
	}
	if !p.Confirm {
		return FixParams{}, fmt.Errorf("job: fix params require confirm=true")
	}
	if p.Disk != nil && *p.Disk < 1 {
		return FixParams{}, fmt.Errorf("job: fix disk index must be >= 1")
	}
	return p, nil
}

func decodeDiskFormatParams(params []byte) (DiskFormatParams, error) {
	if len(params) == 0 || string(params) == "null" {
		return DiskFormatParams{}, fmt.Errorf("job: disk_format params require confirmation")
	}
	var p DiskFormatParams
	if err := decodeJSON(params, &p); err != nil {
		return DiskFormatParams{}, err
	}
	if p.Confirmation == "" {
		return DiskFormatParams{}, fmt.Errorf("job: disk_format params require confirmation")
	}
	if len(p.Parity) == 0 && len(p.Data) == 0 && p.Cache == nil {
		return DiskFormatParams{}, fmt.Errorf("job: disk_format params require a plan")
	}
	return p, nil
}

func decodeDiskAddParams(params []byte) (DiskAddParams, error) {
	if len(params) == 0 || string(params) == "null" {
		return DiskAddParams{}, fmt.Errorf("job: disk_add params require confirmation")
	}
	var p DiskAddParams
	if err := decodeJSON(params, &p); err != nil {
		return DiskAddParams{}, err
	}
	if p.Confirmation == "" {
		return DiskAddParams{}, fmt.Errorf("job: disk_add params require confirmation")
	}
	if p.Disk.Device == "" {
		return DiskAddParams{}, fmt.Errorf("job: disk_add params require a device")
	}
	return p, nil
}

func decodeDiskReplaceParams(params []byte) (DiskReplaceParams, error) {
	if len(params) == 0 || string(params) == "null" {
		return DiskReplaceParams{}, fmt.Errorf("job: disk_replace params require confirmation")
	}
	var p DiskReplaceParams
	if err := decodeJSON(params, &p); err != nil {
		return DiskReplaceParams{}, err
	}
	if p.Confirmation == "" {
		return DiskReplaceParams{}, fmt.Errorf("job: disk_replace params require confirmation")
	}
	if p.Mountpoint == "" || p.Disk.Device == "" {
		return DiskReplaceParams{}, fmt.Errorf("job: disk_replace params require mountpoint and a replacement device")
	}
	return p, nil
}

func decodeDiskUpgradeDataParams(params []byte) (DiskUpgradeDataParams, error) {
	if len(params) == 0 || string(params) == "null" {
		return DiskUpgradeDataParams{}, fmt.Errorf("job: disk_upgrade_data params require confirmation")
	}
	var p DiskUpgradeDataParams
	if err := decodeJSON(params, &p); err != nil {
		return DiskUpgradeDataParams{}, err
	}
	if p.Confirmation == "" {
		return DiskUpgradeDataParams{}, fmt.Errorf("job: disk_upgrade_data params require confirmation")
	}
	if p.Mountpoint == "" || p.Disk.Device == "" {
		return DiskUpgradeDataParams{}, fmt.Errorf("job: disk_upgrade_data params require mountpoint and a replacement device")
	}
	if p.Old.FSUUID == "" {
		return DiskUpgradeDataParams{}, fmt.Errorf("job: disk_upgrade_data params require the old disk's filesystem UUID")
	}
	return p, nil
}

func decodeDiskUpgradeParityParams(params []byte) (DiskUpgradeParityParams, error) {
	if len(params) == 0 || string(params) == "null" {
		return DiskUpgradeParityParams{}, fmt.Errorf("job: disk_upgrade_parity params require confirmation")
	}
	var p DiskUpgradeParityParams
	if err := decodeJSON(params, &p); err != nil {
		return DiskUpgradeParityParams{}, err
	}
	if p.Confirmation == "" {
		return DiskUpgradeParityParams{}, fmt.Errorf("job: disk_upgrade_parity params require confirmation")
	}
	if p.Mountpoint == "" || p.Disk.Device == "" {
		return DiskUpgradeParityParams{}, fmt.Errorf("job: disk_upgrade_parity params require mountpoint and a replacement device")
	}
	if p.NewMountpoint == "" || p.NewMountpoint == p.Mountpoint {
		return DiskUpgradeParityParams{}, fmt.Errorf("job: disk_upgrade_parity params require a distinct newMountpoint")
	}
	return p, nil
}

func decodeShareRelocationParams(params []byte) (ShareRelocationParams, error) {
	if len(params) == 0 || string(params) == "null" {
		return ShareRelocationParams{}, fmt.Errorf("job: share_relocation params require share and to")
	}
	var p ShareRelocationParams
	if err := decodeJSON(params, &p); err != nil {
		return ShareRelocationParams{}, err
	}
	if p.Share == "" {
		return ShareRelocationParams{}, fmt.Errorf("job: share_relocation params require share")
	}
	if p.To != shareRelocationToArray && p.To != shareRelocationToCache {
		return ShareRelocationParams{}, fmt.Errorf("job: share_relocation params require to to be %q or %q", shareRelocationToCache, shareRelocationToArray)
	}
	return p, nil
}

func decodeRebalanceParams(params []byte) (RebalanceParams, error) {
	if len(params) == 0 || string(params) == "null" {
		return RebalanceParams{}, fmt.Errorf("job: rebalance params require a plan")
	}
	var p RebalanceParams
	if err := decodeJSON(params, &p); err != nil {
		return RebalanceParams{}, err
	}
	return p, nil
}

func decodeDiskRemoveParams(params []byte) (DiskRemoveParams, error) {
	if len(params) == 0 || string(params) == "null" {
		return DiskRemoveParams{}, fmt.Errorf("job: disk_remove params require a mountpoint and confirmation")
	}
	var p DiskRemoveParams
	if err := decodeJSON(params, &p); err != nil {
		return DiskRemoveParams{}, err
	}
	if p.Mountpoint == "" || p.Confirmation == "" {
		return DiskRemoveParams{}, fmt.Errorf("job: disk_remove params require a mountpoint and confirmation")
	}
	return p, nil
}

func decodeEvacuationParams(params []byte) (EvacuationParams, error) {
	if len(params) == 0 || string(params) == "null" {
		return EvacuationParams{}, fmt.Errorf("job: evacuation params require a mountpoint and a plan")
	}
	var p EvacuationParams
	if err := decodeJSON(params, &p); err != nil {
		return EvacuationParams{}, err
	}
	if p.Mountpoint == "" {
		return EvacuationParams{}, fmt.Errorf("job: evacuation params require a mountpoint")
	}
	return p, nil
}

// ACMEIssueParams is configureLetsEncrypt / unattended renewal's payload.
type ACMEIssueParams struct {
	Renew bool `json:"renew,omitempty"`
}

// ACMERenewFromParams reports whether this acme_issue job is a renewal
// (notify on failure, never replace the live cert with a self-signed one).
func ACMERenewFromParams(params []byte) (bool, error) {
	p, err := decodeACMEIssueParams(bytes.TrimSpace(params))
	if err != nil {
		return false, err
	}
	return p.Renew, nil
}

func decodeACMEIssueParams(params []byte) (ACMEIssueParams, error) {
	if len(params) == 0 || string(params) == "null" {
		return ACMEIssueParams{}, nil
	}
	var p ACMEIssueParams
	if err := decodeJSON(params, &p); err != nil {
		return ACMEIssueParams{}, err
	}
	return p, nil
}

func decodeJSON(params []byte, dest any) error {
	dec := json.NewDecoder(bytes.NewReader(params))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return fmt.Errorf("job: decoding params: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("job: params must be a single JSON object")
	}
	return nil
}
