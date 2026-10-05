package migrate

import (
	"fmt"
	"strings"
	"time"
)

// Status is how one report row reads. Refuse blocks the migration; flag and
// warn need the user's attention before it goes ahead; info and pass do not.
type Status string

const (
	StatusPass   Status = "pass"
	StatusWarn   Status = "warn"
	StatusFlag   Status = "flag"
	StatusRefuse Status = "refuse"
	StatusInfo   Status = "info"
)

// Verdict is the report's go / no-go.
type Verdict string

const (
	VerdictGo             Verdict = "go"
	VerdictGoWithWarnings Verdict = "go_with_warnings"
	VerdictNoGo           Verdict = "no_go"
)

// Check names, one per row group. Later parts of the scan add rows under new
// names without reshaping the report.
const (
	CheckVersion      = "unraid_version"
	CheckCapture      = "capture"
	CheckBootDevice   = "boot_device"
	CheckParity       = "parity_config"
	CheckParitySize   = "parity_size"
	CheckSMART        = "smart"
	CheckIdentity     = "disk_identity"
	CheckMapping      = "disk_mapping"
	CheckUID99        = "uid_99"
	CheckSyncEstimate = "sync_estimate"

	CheckDataDisks    = "data_disks"
	CheckIntegrity    = "disk_integrity"
	CheckBaseline     = "baseline"
	CheckContentSpace = "content_space"

	CheckShares        = "shares"
	CheckCache         = "cache_contents"
	CheckUsers         = "users"
	CheckTemplates     = "docker_templates"
	CheckContainers    = "containers"
	CheckUserScripts   = "user_scripts"
	CheckParityHistory = "parity_history"
	CheckPlugins       = "plugins"
	CheckCustomConfig  = "custom_config"
	CheckSettings      = "settings"
)

// checkTitles gives each check its heading, in the order the document lists
// them. A check with no rows is left out.
var checkTitles = []struct{ check, title string }{
	{CheckVersion, "Unraid version and flash layout"},
	{CheckCapture, "Phase A capture"},
	{CheckBootDevice, "Unraid boot device"},
	{CheckMapping, "Disk serial mapping"},
	{CheckIdentity, "Disk identity"},
	{CheckParity, "Parity configuration"},
	{CheckParitySize, "Parity disk size"},
	{CheckDataDisks, "Data disk filesystems"},
	{CheckIntegrity, "Filesystem integrity, every data disk"},
	{CheckBaseline, "File counts, sizes and sample checksums per disk and share"},
	{CheckContentSpace, "Free space for content files"},
	{CheckSMART, "SMART status"},
	{CheckParityHistory, "Last Unraid parity check"},
	{CheckShares, "Share configuration"},
	{CheckCache, "What would be lost with the cache"},
	{CheckUsers, "User accounts"},
	{CheckTemplates, "Docker templates"},
	{CheckContainers, "Containers"},
	{CheckUserScripts, "User Scripts (plugin)"},
	{CheckPlugins, "Plugins"},
	{CheckCustomConfig, "Custom configuration that is not imported"},
	{CheckSettings, "Schedules and settings to carry over"},
	{CheckUID99, "File ownership: UID 99"},
	{CheckSyncEstimate, "Estimated initial sync duration"},
}

// Row is one finding: which check, how it reads, what it is about (a slot, a
// disk, a pool; empty for the whole system) and what it says. A row names and
// counts; it never quotes a file's content.
type Row struct {
	Check   string `json:"check"`
	Status  Status `json:"status"`
	Subject string `json:"subject,omitempty"`
	Detail  string `json:"detail"`
}

// Report is the written go / no-go document the scan produces.
type Report struct {
	GeneratedAt   time.Time `json:"generatedAt"`
	UnraidVersion string    `json:"unraidVersion,omitempty"`
	// UnverifiedLayout is true when the scan went ahead only because
	// --unverified-layout overrode a refusal of the version or flash layout
	// (Q24).
	UnverifiedLayout bool `json:"unverifiedLayout"`
	// BootMode is the capture's boot mode ("usb" or "internal"), empty when the
	// capture does not say. The session keeps it, so a later scan can tell that
	// the zip is the only source (Q25) without reading a source again.
	BootMode string  `json:"bootMode,omitempty"`
	Verdict  Verdict `json:"verdict"`
	Rows     []Row   `json:"rows"`
	// Baseline is what the data disks held, for the verify phase: the totals per
	// disk and share and the file the entries are in. It is nil when no disk was
	// read.
	Baseline *BaselineSummary `json:"baseline,omitempty"`
	// Review is the structured data of the Review step, from the same scan as
	// Rows. It is nil in a report made before it existed.
	Review *Review `json:"review,omitempty"`
	// Import is the parsed configuration the later steps seed from. It is kept
	// with the report in the session and is not part of the API's report.
	Import Import `json:"import"`
	// Containers is Phase D's record of the stacks created from this report and
	// how far each has come (containers.go). It is not part of a scan: a new
	// scan carries the earlier report's record over, so the stacks already
	// created stay known.
	Containers *ContainerFlow `json:"containers,omitempty"`
}

func (r *Report) add(check string, st Status, subject, format string, args ...any) {
	r.Rows = append(r.Rows, Row{Check: check, Status: st, Subject: subject, Detail: fmt.Sprintf(format, args...)})
}

func (r *Report) conclude() {
	r.Verdict = VerdictGo
	for _, row := range r.Rows {
		switch row.Status {
		case StatusRefuse:
			r.Verdict = VerdictNoGo
			return
		case StatusWarn, StatusFlag:
			r.Verdict = VerdictGoWithWarnings
		}
	}
}

func (r *Report) count(st Status) int {
	n := 0
	for _, row := range r.Rows {
		if row.Status == st {
			n++
		}
	}
	return n
}

var verdictText = map[Verdict]string{
	VerdictGo:             "GO: nothing in this scan blocks the migration.",
	VerdictGoWithWarnings: "GO WITH WARNINGS: nothing blocks the migration, but the flagged items below need your attention first.",
	VerdictNoGo:           "NO-GO: at least one finding blocks the migration. Fix the items marked refuse before going on.",
}

// Markdown renders the report as the document the user reads before
// committing to the migration.
func (r *Report) Markdown() string {
	var b strings.Builder
	b.WriteString("# Hoserva migration scan report\n\n")
	if r.UnverifiedLayout {
		b.WriteString("> **Unverified layout.** This Unraid version or flash layout is not one Hoserva has been verified against. ")
		b.WriteString("The scan ran only because `--unverified-layout` overrode the refusal. Every finding below rests on a layout nobody has checked.\n\n")
	}
	fmt.Fprintf(&b, "Generated %s.", r.GeneratedAt.UTC().Format(time.RFC3339))
	if r.UnraidVersion != "" {
		fmt.Fprintf(&b, " Unraid %s.", mdText(r.UnraidVersion))
	}
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "**%s**\n\n", verdictText[r.Verdict])
	fmt.Fprintf(&b, "%d refuse, %d flag, %d warn.\n", r.count(StatusRefuse), r.count(StatusFlag), r.count(StatusWarn))

	known := map[string]bool{}
	for _, t := range checkTitles {
		known[t.check] = true
		r.writeSection(&b, t.title, func(row Row) bool { return row.Check == t.check })
	}
	r.writeSection(&b, "Other findings", func(row Row) bool { return !known[row.Check] })
	return b.String()
}

func (r *Report) writeSection(b *strings.Builder, title string, match func(Row) bool) {
	var rows []Row
	for _, row := range r.Rows {
		if match(row) {
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(b, "\n## %s\n\n| Status | Subject | Detail |\n|---|---|---|\n", title)
	for _, row := range rows {
		fmt.Fprintf(b, "| %s | %s | %s |\n", row.Status, mdText(row.Subject), mdText(row.Detail))
	}
}

// mdText makes text from the source (a serial, a share name) safe inside a
// Markdown table cell: no row break, no raw HTML, no cell separator.
func mdText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	return strings.NewReplacer("|", "\\|", "<", "&lt;", ">", "&gt;", "`", "'").Replace(s)
}
