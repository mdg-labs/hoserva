package disk

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// corpusDir holds real-schema smartctl -j output fixtures (doc 06 §2),
// authored against smartctl's own documented JSON format rather than
// captured from hardware — this project never runs smartctl against a
// real block device (CLAUDE.md), so no genuinely captured corpus exists.
const corpusDir = "../../testdata/parsers"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(corpusDir, name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

func TestParseSMARTJSON_Healthy(t *testing.T) {
	report, err := ParseSMARTJSON(readFixture(t, "smartctl_ata_healthy.json"))
	if err != nil {
		t.Fatalf("ParseSMARTJSON: %v", err)
	}
	want := SMARTReport{
		SpinState:            Active,
		ReallocatedSectors:   0,
		PendingSectors:       0,
		OfflineUncorrectable: 0,
		CRCErrors:            0,
		TemperatureCelsius:   32,
		PowerOnHours:         9412,
		SelfTestFailed:       false,
	}
	if report != want {
		t.Fatalf("ParseSMARTJSON: got %+v, want %+v", report, want)
	}
}

func TestParseSMARTJSON_Standby_SkipsWithoutAttributeData(t *testing.T) {
	report, err := ParseSMARTJSON(readFixture(t, "smartctl_ata_standby.json"))
	if err != nil {
		t.Fatalf("ParseSMARTJSON: %v", err)
	}
	if !report.Skipped {
		t.Fatal("ParseSMARTJSON: standby response not marked Skipped")
	}
	if report.SpinState != Standby {
		t.Fatalf("ParseSMARTJSON: SpinState = %v, want Standby", report.SpinState)
	}
	if report.ReallocatedSectors != 0 || report.PendingSectors != 0 || report.CRCErrors != 0 {
		t.Fatalf("ParseSMARTJSON: a skipped report carried attribute data: %+v", report)
	}
}

func TestParseSMARTJSON_WarningReallocated(t *testing.T) {
	report, err := ParseSMARTJSON(readFixture(t, "smartctl_ata_warning_reallocated.json"))
	if err != nil {
		t.Fatalf("ParseSMARTJSON: %v", err)
	}
	if report.ReallocatedSectors != 8 {
		t.Errorf("ReallocatedSectors = %d, want 8", report.ReallocatedSectors)
	}
	if report.PendingSectors != 2 {
		t.Errorf("PendingSectors = %d, want 2", report.PendingSectors)
	}
	if report.OfflineUncorrectable != 1 {
		t.Errorf("OfflineUncorrectable = %d, want 1", report.OfflineUncorrectable)
	}
	if report.CRCErrors != 3 {
		t.Errorf("CRCErrors = %d, want 3", report.CRCErrors)
	}
	if report.TemperatureCelsius != 47 {
		t.Errorf("TemperatureCelsius = %d, want 47", report.TemperatureCelsius)
	}
	if report.SelfTestFailed {
		t.Error("SelfTestFailed = true, want false")
	}
}

func TestParseSMARTJSON_SelfTestFailed(t *testing.T) {
	report, err := ParseSMARTJSON(readFixture(t, "smartctl_ata_selftest_failed.json"))
	if err != nil {
		t.Fatalf("ParseSMARTJSON: %v", err)
	}
	if !report.SelfTestFailed {
		t.Fatal("SelfTestFailed = false, want true")
	}
	if report.ReallocatedSectors != 2048 {
		t.Errorf("ReallocatedSectors = %d, want 2048", report.ReallocatedSectors)
	}
}

// TestParseSMARTJSON_NVMeHealthy proves an NVMe report (no
// ata_smart_attributes at all — an NVMe disk never has one) is no longer
// rejected by ErrSMARTParse: nvme_smart_health_information_log alone must
// be enough to produce an Active report.
func TestParseSMARTJSON_NVMeHealthy(t *testing.T) {
	report, err := ParseSMARTJSON(readFixture(t, "smartctl_nvme_healthy.json"))
	if err != nil {
		t.Fatalf("ParseSMARTJSON: %v", err)
	}
	want := SMARTReport{
		SpinState:            Active,
		OfflineUncorrectable: 0,
		TemperatureCelsius:   37,
		PowerOnHours:         1234,
		SelfTestFailed:       false,
	}
	if report != want {
		t.Fatalf("ParseSMARTJSON: got %+v, want %+v", report, want)
	}
}

// TestParseSMARTJSON_NVMeMediaErrors is the NVMe half of doc 02 §4's own
// alert rules: media_errors is NVMe's closest equivalent to ATA's
// Offline_Uncorrectable (an uncorrectable data-integrity error already
// detected), and the fixture's failing smart_status.passed must still
// surface as SelfTestFailed — that check runs regardless of protocol.
func TestParseSMARTJSON_NVMeMediaErrors(t *testing.T) {
	report, err := ParseSMARTJSON(readFixture(t, "smartctl_nvme_media_errors.json"))
	if err != nil {
		t.Fatalf("ParseSMARTJSON: %v", err)
	}
	if report.OfflineUncorrectable != 6 {
		t.Errorf("OfflineUncorrectable = %d, want 6 (from nvme_smart_health_information_log.media_errors)", report.OfflineUncorrectable)
	}
	if !report.SelfTestFailed {
		t.Error("SelfTestFailed = false, want true (smart_status.passed is false)")
	}
	if report.ReallocatedSectors != 0 || report.PendingSectors != 0 || report.CRCErrors != 0 {
		t.Errorf("an NVMe report carried ATA-only attribute data: %+v", report)
	}
}

func TestParseSMARTJSON_NoSMARTSupport(t *testing.T) {
	_, err := ParseSMARTJSON(readFixture(t, "smartctl_no_smart_support.json"))
	if !errors.Is(err, ErrSMARTParse) {
		t.Fatalf("ParseSMARTJSON(no smart_support): got %v, want ErrSMARTParse", err)
	}
}

func TestParseSMARTJSON_Malformed(t *testing.T) {
	_, err := ParseSMARTJSON(readFixture(t, "smartctl_malformed.json"))
	if !errors.Is(err, ErrSMARTParse) {
		t.Fatalf("ParseSMARTJSON(malformed): got %v, want ErrSMARTParse", err)
	}
}

func TestParseSMARTJSON_Empty(t *testing.T) {
	_, err := ParseSMARTJSON(readFixture(t, "smartctl_empty.json"))
	if !errors.Is(err, ErrSMARTParse) {
		t.Fatalf("ParseSMARTJSON(empty): got %v, want ErrSMARTParse", err)
	}
}

func TestParseSMARTJSON_NotJSON(t *testing.T) {
	_, err := ParseSMARTJSON([]byte("smartctl: command not found\n"))
	if !errors.Is(err, ErrSMARTParse) {
		t.Fatalf("ParseSMARTJSON(garbage): got %v, want ErrSMARTParse", err)
	}
}
