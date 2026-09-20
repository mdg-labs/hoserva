package job

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

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
