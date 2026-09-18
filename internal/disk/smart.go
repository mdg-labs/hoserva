package disk

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ATA SMART attribute IDs doc 02 §4 tracks by name (the standard ATA
// SMART attribute table, using smartmontools' own attribute names):
// reallocated and pending sectors, offline uncorrectable, and the
// interface CRC error count.
const (
	attrReallocatedSectorCt  = 5
	attrCurrentPendingSector = 197
	attrOfflineUncorrectable = 198
	attrUDMACRCErrorCount    = 199
)

// ErrSMARTParse wraps a smartctl JSON body ParseSMARTJSON could not make
// sense of — malformed JSON, or JSON missing every field a real
// smartctl -j response carries when it isn't a standby skip.
var ErrSMARTParse = errors.New("disk: could not parse smartctl output")

type smartctlOutput struct {
	Smartctl struct {
		ExitStatus int `json:"exit_status"`
		Messages   []struct {
			String   string `json:"string"`
			Severity string `json:"severity"`
		} `json:"messages"`
	} `json:"smartctl"`
	PowerMode   string `json:"power_mode"`
	Temperature *struct {
		Current int `json:"current"`
	} `json:"temperature"`
	PowerOnTime *struct {
		Hours int `json:"hours"`
	} `json:"power_on_time"`
	SmartStatus *struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`
	AtaSmartAttributes *struct {
		Table []struct {
			ID  int `json:"id"`
			Raw struct {
				Value int64 `json:"value"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
	AtaSmartSelfTestLog *struct {
		Standard *struct {
			Table []struct {
				Status struct {
					Passed *bool `json:"passed"`
				} `json:"status"`
			} `json:"table"`
		} `json:"standard"`
	} `json:"ata_smart_self_test_log"`
	// NvmeSmartHealthInformationLog is smartctl's NVMe health section —
	// an NVMe device has no ata_smart_attributes table at all, so without
	// this, every active NVMe report was rejected by ErrSMARTParse below.
	// MediaErrors is NVMe's own closest equivalent to
	// OfflineUncorrectable: "the number of occurrences where the
	// controller detected an unrecovered data integrity error" — an
	// uncorrectable error already found, the same thing ATA's Offline_
	// Uncorrectable attribute counts. NVMe has no per-sector reallocation
	// or pending-sector concept (the FTL handles bad blocks internally)
	// and no CRC-error attribute, so those SMARTReport fields stay zero
	// for an NVMe disk — that's accurate, not a gap: doc 02 §4's other
	// alert, a failed smart_status.passed, already reflects smartctl's
	// own NVMe critical_warning-derived health verdict, checked below
	// regardless of protocol.
	NvmeSmartHealthInformationLog *struct {
		MediaErrors int64 `json:"media_errors"`
	} `json:"nvme_smart_health_information_log"`
}

// ParseSMARTJSON parses one `smartctl -j` (optionally `-n standby`)
// invocation's stdout into a SMARTReport (doc 02 §4). A disk left asleep
// by `-n standby` comes back with Skipped set and every attribute at its
// zero value — the caller must treat that as "not polled", never as
// "no errors".
func ParseSMARTJSON(data []byte) (SMARTReport, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return SMARTReport{}, fmt.Errorf("%w: empty output", ErrSMARTParse)
	}

	var out smartctlOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return SMARTReport{}, fmt.Errorf("%w: %v", ErrSMARTParse, err)
	}

	if isStandbySkip(out) {
		return SMARTReport{Skipped: true, SpinState: Standby}, nil
	}

	if out.AtaSmartAttributes == nil && out.NvmeSmartHealthInformationLog == nil {
		return SMARTReport{}, fmt.Errorf("%w: no ata_smart_attributes or nvme_smart_health_information_log, and not a standby skip", ErrSMARTParse)
	}

	report := SMARTReport{SpinState: Active}
	if out.Temperature != nil {
		report.TemperatureCelsius = out.Temperature.Current
	}
	if out.PowerOnTime != nil {
		report.PowerOnHours = out.PowerOnTime.Hours
	}
	if out.SmartStatus != nil && !out.SmartStatus.Passed {
		report.SelfTestFailed = true
	}
	if log := out.AtaSmartSelfTestLog; log != nil && log.Standard != nil && len(log.Standard.Table) > 0 {
		if p := log.Standard.Table[0].Status.Passed; p != nil && !*p {
			report.SelfTestFailed = true
		}
	}

	if out.AtaSmartAttributes != nil {
		for _, attr := range out.AtaSmartAttributes.Table {
			switch attr.ID {
			case attrReallocatedSectorCt:
				report.ReallocatedSectors = int(attr.Raw.Value)
			case attrCurrentPendingSector:
				report.PendingSectors = int(attr.Raw.Value)
			case attrOfflineUncorrectable:
				report.OfflineUncorrectable = int(attr.Raw.Value)
			case attrUDMACRCErrorCount:
				report.CRCErrors = int(attr.Raw.Value)
			}
		}
	} else if out.NvmeSmartHealthInformationLog != nil {
		report.OfflineUncorrectable = int(out.NvmeSmartHealthInformationLog.MediaErrors)
	}

	return report, nil
}

// isStandbySkip reports whether out is smartctl's response to a device
// `-n standby` left asleep: neither an ATA attribute table nor an NVMe
// health log was collected, and either the (undocumented but observed)
// power_mode field or one of smartctl's own diagnostic messages says so.
func isStandbySkip(out smartctlOutput) bool {
	if out.AtaSmartAttributes != nil || out.NvmeSmartHealthInformationLog != nil {
		return false
	}
	if strings.EqualFold(out.PowerMode, "STANDBY") || strings.EqualFold(out.PowerMode, "SLEEP") {
		return true
	}
	for _, m := range out.Smartctl.Messages {
		up := strings.ToUpper(m.String)
		if strings.Contains(up, "STANDBY") || strings.Contains(up, "SLEEP MODE") {
			return true
		}
	}
	return false
}
