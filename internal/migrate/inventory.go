package migrate

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mdg-labs/hoserva/internal/pool"
)

// Import is what the scan parses out of Unraid's configuration for the later
// steps to seed from: the shares and users the import creates, the templates the
// preview offers, the networks the converter needs and the schedules the
// post-migration checklist offers. It is kept in the session beside the report
// and is not part of the API's report: it holds names and settings, never a
// password, a secret or a file's content.
type Import struct {
	// Shares are the share configs that have a share behind them. An orphan
	// config is reported and not listed here.
	Shares []Share `json:"shares"`
	// Users are the account names in config/passwd, nothing else of them.
	Users           []string         `json:"users"`
	Templates       []TemplateEntry  `json:"templates"`
	ComposeProjects []ComposeProject `json:"composeProjects"`
	Networks        []Network        `json:"networks"`
	Schedules       Schedules        `json:"schedules"`
}

// Share is one config/shares/<name>.cfg. Raw values are kept beside what they
// map to, so the import can show both.
type Share struct {
	Name         string            `json:"name"`
	Allocator    string            `json:"allocator"`
	CreatePolicy pool.CreatePolicy `json:"createPolicy,omitempty"`
	UseCache     string            `json:"useCache"`
	CacheMode    pool.CacheMode    `json:"cacheMode,omitempty"`
	CachePool    string            `json:"cachePool,omitempty"`
	Export       string            `json:"export"`
	Security     string            `json:"security,omitempty"`
	SplitLevel   string            `json:"splitLevel,omitempty"`
	Floor        string            `json:"floor,omitempty"`
	Include      []string          `json:"include,omitempty"`
	Exclude      []string          `json:"exclude,omitempty"`
}

// TemplateClass says what a dockerMan template stands for in the capture.
type TemplateClass string

const (
	ClassAutostart    TemplateClass = "autostart"
	ClassRunning      TemplateClass = "running"
	ClassStopped      TemplateClass = "stopped"
	ClassTemplateOnly TemplateClass = "template_only"
	// ClassUnknown is every template's class when the capture has no usable
	// container list; the preview pre-selects nothing then.
	ClassUnknown TemplateClass = "unknown"
)

// TemplateEntry is one template that parsed: where it is in the source, the
// <Name> its container is matched on, and its class.
type TemplateEntry struct {
	File  string        `json:"file"`
	Name  string        `json:"name"`
	Class TemplateClass `json:"class"`
	// AutostartPosition is the 1-based place on Unraid's autostart list and
	// AutostartWaitSeconds the wait after it; both are 0 off the list.
	AutostartPosition    int `json:"autostartPosition,omitempty"`
	AutostartWaitSeconds int `json:"autostartWaitSeconds,omitempty"`
}

// ComposeProject is a Compose Manager project: the compose.yaml in the source,
// empty when the source has none, and the containers the capture shows it running.
type ComposeProject struct {
	Name       string   `json:"name"`
	File       string   `json:"file,omitempty"`
	Containers []string `json:"containers,omitempty"`
}

// NetworkSubnet is one entry of a network's IPAM configuration.
type NetworkSubnet struct {
	Subnet  string `json:"subnet,omitempty"`
	Gateway string `json:"gateway,omitempty"`
	IPRange string `json:"ipRange,omitempty"`
}

// Network is one Docker network from the capture's networks.json.
type Network struct {
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	Scope      string            `json:"scope,omitempty"`
	EnableIPv6 bool              `json:"enableIPv6,omitempty"`
	Internal   bool              `json:"internal,omitempty"`
	Attachable bool              `json:"attachable,omitempty"`
	Subnets    []NetworkSubnet   `json:"subnets,omitempty"`
	Options    map[string]string `json:"options,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
}

// builtinNetwork reports one of the three networks every Docker daemon has.
func builtinNetwork(name string) bool {
	return name == "bridge" || name == "host" || name == "none"
}

// ParityCheckSchedule is the [parity] section of dynamix.cfg, kept as Unraid
// wrote it: what each key means is the checklist's to interpret.
type ParityCheckSchedule struct {
	// Found is false when the flash has no [parity] section, and the other
	// fields are then empty.
	Found      bool   `json:"found"`
	Mode       string `json:"mode,omitempty"`
	Hour       string `json:"hour,omitempty"`
	DayOfMonth string `json:"dayOfMonth,omitempty"`
	Day        string `json:"day,omitempty"`
	Month      string `json:"month,omitempty"`
	Frequency  string `json:"frequency,omitempty"`
	// Correcting is false when the check is set to write no corrections.
	Correcting bool `json:"correcting"`
}

// Schedules are the settings the post-migration checklist can offer to carry
// over. An empty value or nil means the flash does not have it.
type Schedules struct {
	MoverCron     string              `json:"moverCron,omitempty"`
	ParityCheck   ParityCheckSchedule `json:"parityCheck"`
	SpindownDelay string              `json:"spindownDelay,omitempty"`
	// NotifyAgents are the names of the notification agents configured, never
	// anything inside them: an agent holds its webhook or credentials.
	NotifyAgents []string `json:"notifyAgents,omitempty"`
}

const (
	sharesDir        = "config/shares"
	templatesDir     = "config/plugins/dockerMan/templates-user"
	composeDir       = "config/plugins/compose.manager/projects"
	userScriptsDir   = "config/plugins/user.scripts/scripts"
	userScriptsCron  = "config/plugins/user.scripts/customSchedule.cron"
	captureDir       = "config/hoserva"
	notifyAgentsDir  = "config/plugins/dynamix/notifications/agents"
	dynamixCfg       = "config/plugins/dynamix/dynamix.cfg"
	parityChecksLog  = "config/parity-checks.log"
	parityCheckLimit = 35 * 24 * time.Hour
)

// readOptional reads a file that may be absent. found is false for an absent
// file; any other failure is an error, because a source that cannot be read is
// not one that lacks the file.
func readOptional(src FlashSource, name string) (data []byte, found bool, err error) {
	data, err = src.Read(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// directChildren lists the files directly under dir, as paths from the root.
func directChildren(src FlashSource, dir string) []string {
	var out []string
	for _, n := range src.List(dir) {
		if path.Dir(n) == dir {
			out = append(out, n)
		}
	}
	return out
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// allocationPolicy maps Unraid's allocation method to a create policy (Q11).
// exact is false for High-water, which has no equivalent.
func allocationPolicy(method string) (p pool.CreatePolicy, exact, known bool) {
	switch method {
	case "fillup":
		return pool.FillDisksInOrder, true, true
	case "mostfree":
		return pool.BalanceAcrossDisks, true, true
	case "highwater":
		return pool.BalanceAcrossDisks, false, true
	}
	return "", false, false
}

func allocationLabel(method string) string {
	switch method {
	case "fillup":
		return "Fill-up"
	case "mostfree":
		return "Most-free"
	case "highwater":
		return "High-water"
	}
	return fmt.Sprintf("%q", method)
}

// cacheModeFor maps Unraid's per-share cache setting to a Hoserva cache mode.
func cacheModeFor(setting string) (pool.CacheMode, bool) {
	switch setting {
	case "no":
		return pool.ArrayOnly, true
	case "yes":
		return pool.CacheThenMove, true
	case "only", "prefer":
		return pool.CacheOnly, true
	}
	return "", false
}

// parseShare reads one share config; globals are share.cfg's settings, which
// supply the disks a share lists none for.
func parseShare(name string, data []byte, globals map[string]string) (Share, error) {
	cfg, err := parseCfg(data)
	if err != nil {
		return Share{}, err
	}
	v := cfg.section("")
	sh := Share{
		Name: name, Allocator: v["shareAllocator"], UseCache: v["shareUseCache"], CachePool: v["shareCachePool"],
		Export: v["shareExport"], Security: v["shareSecurity"], SplitLevel: v["shareSplitLevel"], Floor: v["shareFloor"],
		Include: splitList(v["shareInclude"]), Exclude: splitList(v["shareExclude"]),
	}
	if sh.Include == nil {
		sh.Include = splitList(globals["shareUserInclude"])
	}
	if sh.Exclude == nil {
		sh.Exclude = splitList(globals["shareUserExclude"])
	}
	sh.CreatePolicy, _, _ = allocationPolicy(sh.Allocator)
	sh.CacheMode, _ = cacheModeFor(sh.UseCache)
	return sh, nil
}

// templateName reads the <Name> a dockerMan template's container is matched
// on. Nothing else of the template is read: its settings carry secrets.
func templateName(data []byte) (string, error) {
	var t struct {
		XMLName xml.Name
		Name    string `xml:"Name"`
	}
	if err := xml.Unmarshal(data, &t); err != nil {
		return "", errors.New("it is not valid XML")
	}
	if t.XMLName.Local != "Container" {
		return "", fmt.Errorf("its root element is %q, not Container", t.XMLName.Local)
	}
	name := strings.TrimSpace(t.Name)
	if name == "" {
		return "", errors.New("it has no <Name>")
	}
	return name, nil
}

type captureContainer struct {
	Name  string `json:"Name"`
	State struct {
		Status  string `json:"Status"`
		Running bool   `json:"Running"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

const (
	labelManaged = "net.unraid.docker.managed"
	labelCompose = "com.docker.compose.project"
)

type containerOrigin int

const (
	originDockerMan containerOrigin = iota
	originCompose
	originByHand
)

func (c captureContainer) origin() containerOrigin {
	switch {
	case c.Config.Labels[labelManaged] == "dockerman":
		return originDockerMan
	case c.Config.Labels[labelCompose] != "":
		return originCompose
	}
	return originByHand
}

func (c captureContainer) running() bool {
	if c.State.Status != "" {
		return c.State.Status == "running"
	}
	return c.State.Running
}

func parseContainers(data []byte) ([]captureContainer, error) {
	var raw []captureContainer
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, errors.New("it is not a JSON list of docker inspect objects")
	}
	out := raw[:0]
	for _, c := range raw {
		c.Name = strings.TrimPrefix(strings.TrimSpace(c.Name), "/")
		if c.Name == "" {
			return nil, errors.New("an entry has no container name")
		}
		out = append(out, c)
	}
	return out, nil
}

type autostartEntry struct {
	position, wait int
}

// parseAutostart reads Unraid's autostart list: one container name per line,
// and optionally the seconds to wait after starting it. bad counts lines that
// are neither.
func parseAutostart(data []byte) (entries map[string]autostartEntry, bad int) {
	entries = map[string]autostartEntry{}
	pos := 0
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		wait := 0
		if len(f) > 2 {
			bad++
			continue
		}
		if len(f) == 2 {
			n, err := strconv.Atoi(f[1])
			if err != nil || n < 0 {
				bad++
				continue
			}
			wait = n
		}
		if _, dup := entries[f[0]]; dup {
			continue
		}
		pos++
		entries[f[0]] = autostartEntry{position: pos, wait: wait}
	}
	return entries, bad
}

func parseNetworks(data []byte) ([]Network, error) {
	var raw []struct {
		Name       string `json:"Name"`
		Driver     string `json:"Driver"`
		Scope      string `json:"Scope"`
		EnableIPv6 bool   `json:"EnableIPv6"`
		Internal   bool   `json:"Internal"`
		Attachable bool   `json:"Attachable"`
		IPAM       struct {
			Config []struct {
				Subnet  string `json:"Subnet"`
				Gateway string `json:"Gateway"`
				IPRange string `json:"IPRange"`
			} `json:"Config"`
		} `json:"IPAM"`
		Options map[string]string `json:"Options"`
		Labels  map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, errors.New("it is not a JSON list of docker network inspect objects")
	}
	out := make([]Network, 0, len(raw))
	for _, n := range raw {
		if n.Name == "" {
			return nil, errors.New("an entry has no network name")
		}
		net := Network{
			Name: n.Name, Driver: n.Driver, Scope: n.Scope, EnableIPv6: n.EnableIPv6,
			Internal: n.Internal, Attachable: n.Attachable, Options: n.Options, Labels: n.Labels,
		}
		for _, c := range n.IPAM.Config {
			net.Subnets = append(net.Subnets, NetworkSubnet{Subnet: c.Subnet, Gateway: c.Gateway, IPRange: c.IPRange})
		}
		out = append(out, net)
	}
	return out, nil
}

// parseUsers reads the account names in config/passwd: Unraid's own accounts
// start at UID 1000, below which are root, nobody and the system's. Only the
// name is kept; the line's other fields are not read into anything.
func parseUsers(data []byte) (names []string, bad int) {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ":")
		if len(f) < 4 || f[0] == "" {
			bad++
			continue
		}
		uid, err := strconv.Atoi(f[2])
		if err != nil {
			bad++
			continue
		}
		if uid >= 1000 && uid < 65534 {
			names = append(names, f[0])
		}
	}
	sort.Strings(names)
	return names, bad
}

// cronScriptLine matches only the line the User Scripts plugin writes: a
// schedule, the plugin's startCustom.php, the script's path and optionally
// output redirects. The plugin writes the folder name unquoted and keeps its
// spaces, so the folder is everything between "/scripts/" and "/script". The
// schedule is captured as raw fields and checked by cronScheduleFields; any
// other line is a hand-written cron command and may hold a credential, so it
// is never read further.
var cronScriptLine = regexp.MustCompile(`^(@[a-z]+|\S+(?:\s+\S+){4})\s+(?:\S*/)?startCustom\.php\s+\S*/scripts/([^/]+)/script` +
	`(?:\s+(?:[0-9]?>>?\s*[^\s;&|]+|&>>?\s*[^\s;&|]+|[0-9]?>&[0-9]))*\s*$`)

var cronShorthand = map[string]bool{
	"@reboot": true, "@yearly": true, "@annually": true, "@monthly": true,
	"@weekly": true, "@daily": true, "@midnight": true, "@hourly": true,
}

var (
	cronNumeric = regexp.MustCompile(`^[0-9*/,-]+$`)
	cronNamed   = regexp.MustCompile(`(?i)^(?:[0-9*/,-]|jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec|sun|mon|tue|wed|thu|fri|sat)+$`)
)

// cronScheduleFields reports whether sched is one @-shorthand or exactly five
// fields made only of digits, "*", "/", "," and "-", with month and weekday
// names allowed in the last two fields, so the schedule can be reported
// without quoting anything but a schedule.
func cronScheduleFields(sched string) bool {
	if strings.HasPrefix(sched, "@") {
		return cronShorthand[sched]
	}
	fields := strings.Fields(sched)
	if len(fields) != 5 {
		return false
	}
	for i, f := range fields {
		re := cronNumeric
		if i >= 3 {
			re = cronNamed
		}
		if !re.MatchString(f) {
			return false
		}
	}
	return true
}

// parseCustomCron reads customSchedule.cron: each line is a schedule (five
// fields, or one @-shorthand such as @daily) and the command that runs one
// script. It returns the schedule per script folder and the 1-based numbers of
// the lines it could not read, never their text.
func parseCustomCron(data []byte) (sched map[string]string, unreadable []int) {
	sched = map[string]string{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := cronScriptLine.FindStringSubmatch(line)
		if m == nil || !cronScheduleFields(m[1]) {
			unreadable = append(unreadable, i+1)
			continue
		}
		if _, dup := sched[m[2]]; !dup {
			sched[m[2]] = strings.Join(strings.Fields(m[1]), " ")
		}
	}
	return sched, unreadable
}

// parityCheckEntry is one line of parity-checks.log: when the check ended, its
// exit code and the errors it found.
type parityCheckEntry struct {
	at   time.Time
	exit int
	errs int
}

// parseParityLog reads parity-checks.log, lines of
// "2026 Jul 06 03:00:02|duration|speed|exit code|errors", and returns the last
// check and how many lines were not one. Unraid's date carries no time zone, so
// it is read as UTC: the age it is judged by is in weeks.
func parseParityLog(data []byte) (last *parityCheckEntry, bad int) {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Split(line, "|")
		if len(f) < 5 {
			bad++
			continue
		}
		at, err := time.Parse("2006 Jan _2 15:04:05", strings.TrimSpace(f[0]))
		exit, err2 := strconv.Atoi(strings.TrimSpace(f[3]))
		errs, err3 := strconv.Atoi(strings.TrimSpace(f[4]))
		if err != nil || err2 != nil || err3 != nil {
			bad++
			continue
		}
		if last == nil || at.After(last.at) {
			last = &parityCheckEntry{at: at, exit: exit, errs: errs}
		}
	}
	return last, bad
}

func trimmedLines(data []byte) []string {
	var out []string
	for _, l := range bytes.Split(data, []byte("\n")) {
		if s := strings.TrimSpace(string(l)); s != "" {
			out = append(out, s)
		}
	}
	return out
}
