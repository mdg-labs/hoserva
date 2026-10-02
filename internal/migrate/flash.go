package migrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Unraid's two parity slots: slot 0 and slot 29 (disk.cfg's diskIdSlot.N keys).
const (
	parity1Slot = 0
	parity2Slot = 29
)

// ErrNoDiskCfg is returned when the source has no usable config/disk.cfg, the
// one file without which the layout cannot be read at all. A report is never
// produced from such a source, whatever override is given.
var ErrNoDiskCfg = errors.New("the Flash Backup has no usable config/disk.cfg")

// Slot is one entry of the capture's disks.ini: the slot, and the identity of
// the disk Unraid had in it. disk.cfg holds no serials; the assignment itself
// is in a binary file that is never read.
type Slot struct {
	Name    string `json:"name"`
	Index   int    `json:"index"`
	ID      string `json:"id"`
	SizeKiB int64  `json:"sizeKiB"`
	Status  string `json:"status"`
	Type    string `json:"type"`
	FsType  string `json:"fsType"`
	Device  string `json:"device"`
}

// Role is the slot's role in the array, taken from disks.ini's type only.
type Role string

const (
	RoleParity Role = "parity"
	RoleData   Role = "data"
	RoleCache  Role = "cache"
	RoleOther  Role = "other"
)

// Role classes the slot by its disks.ini type.
func (s Slot) Role() Role {
	switch strings.ToLower(s.Type) {
	case "parity":
		return RoleParity
	case "data":
		return RoleData
	case "cache":
		return RoleCache
	default:
		return RoleOther
	}
}

// Assigned reports whether a disk is in the slot: disks.ini lists every slot,
// the empty ones with status DISK_NP and no id.
func (s Slot) Assigned() bool {
	return s.ID != "" && !strings.HasPrefix(strings.ToUpper(s.Status), "DISK_NP")
}

// PoolMember is one device of a pool (cache pool) from config/pools/<name>.cfg.
type PoolMember struct {
	Pool string `json:"pool"`
	ID   string `json:"id"`
}

// BootDevice is a device the capture names as holding Unraid's boot pool.
type BootDevice struct {
	Name   string `json:"name"`
	Serial string `json:"serial"`
	Model  string `json:"model"`
	Size   string `json:"size"`
}

// Boot is the capture's boot mode: "usb", "internal", or empty when the
// capture does not say.
type Boot struct {
	Mode               string       `json:"mode"`
	Filesystem         string       `json:"filesystem"`
	Devices            []BootDevice `json:"devices"`
	Mirrored           bool         `json:"mirrored"`
	SharedWithDataPool bool         `json:"shared_with_data_pool"`
}

// WritableLayer is one container's writable-layer size from the capture;
// Bytes is nil when the prepare script could not measure it.
type WritableLayer struct {
	Container string `json:"container"`
	Bytes     *int64 `json:"bytes"`
}

// CaptureDocker is the capture's account of Docker: whether it was running,
// where its storage sat ("cache", "array", "boot-pool", "other", "none" or
// "unknown") and the writable-layer size per container. The layers are empty
// when Docker was not running.
type CaptureDocker struct {
	State             string          `json:"state"`
	DirectoryLocation string          `json:"directory_location"`
	WritableLayers    []WritableLayer `json:"writable_layers"`
}

// Capture is config/hoserva/capture.json, written by the Phase A prepare script.
type Capture struct {
	UnraidVersion      string        `json:"unraid_version"`
	CapturedAt         string        `json:"captured_at"`
	Boot               Boot          `json:"boot"`
	Docker             CaptureDocker `json:"docker"`
	LibvirtImgLocation string        `json:"libvirt_img_location"`
}

// DiskCfg is config/disk.cfg: the array's global settings and per-slot
// filesystem types. It holds no disk identities.
type DiskCfg struct {
	FsTypes     map[int]string
	ParitySlots []int
}

// Flash is what the scan reads from a source without any disk access.
type Flash struct {
	// Version is the Unraid version from the first line of changes.txt, empty
	// when that line does not carry one.
	Version string
	// HasKernel reports a bzimage at the source's root, which every layout the
	// allowlist knows has (Q24).
	HasKernel bool
	DiskCfg   DiskCfg

	// Slots is disks.ini; HaveDisksINI is false when the capture has none.
	Slots        []Slot
	HaveDisksINI bool
	// DisksINIProblem says why disks.ini was present but unusable.
	DisksINIProblem string

	Pools []PoolMember
	// PoolProblems names each pool config that could not be read.
	PoolProblems []string

	// Capture is nil when capture.json is absent or unreadable;
	// CaptureProblem then says which.
	Capture        *Capture
	CaptureProblem string
}

// ReadFlash reads everything the scan needs from src without touching a disk. A
// source without a usable disk.cfg is refused with ErrNoDiskCfg; every other
// absence is recorded for the report to say.
func ReadFlash(src FlashSource) (*Flash, error) {
	f := &Flash{}

	raw, err := src.Read("config/disk.cfg")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: it is not in the zip", ErrNoDiskCfg)
		}
		return nil, err
	}
	cfg, err := parseCfg(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoDiskCfg, err)
	}
	if len(cfg.keys[""]) == 0 {
		return nil, fmt.Errorf("%w: it holds no settings", ErrNoDiskCfg)
	}
	f.DiskCfg = diskCfgFrom(cfg.section(""))

	if data, err := src.Read("changes.txt"); err == nil {
		f.Version = versionFromChanges(data)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, n := range src.List("") {
		if n == "bzimage" {
			f.HasKernel = true
		}
	}

	if err := f.readDisksINI(src); err != nil {
		return nil, err
	}
	if err := f.readPools(src); err != nil {
		return nil, err
	}
	if err := f.readCapture(src); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *Flash) readDisksINI(src FlashSource) error {
	data, err := src.Read("config/hoserva/disks.ini")
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	ini, err := parseCfg(data)
	if err != nil {
		f.DisksINIProblem = err.Error()
		return nil
	}
	f.HaveDisksINI = true
	for _, name := range ini.sections {
		if name == "" {
			continue
		}
		s := ini.section(name)
		idx, _ := strconv.Atoi(s["idx"])
		size, _ := strconv.ParseInt(s["size"], 10, 64)
		f.Slots = append(f.Slots, Slot{
			Name: name, Index: idx, ID: s["id"], SizeKiB: size,
			Status: s["status"], Type: s["type"], FsType: s["fsType"], Device: s["device"],
		})
	}
	return nil
}

func (f *Flash) readPools(src FlashSource) error {
	for _, n := range src.List("config/pools") {
		if path.Dir(n) != "config/pools" || !strings.HasSuffix(n, ".cfg") {
			continue
		}
		pool := strings.TrimSuffix(path.Base(n), ".cfg")
		data, err := src.Read(n)
		if err != nil {
			return err
		}
		cfg, err := parseCfg(data)
		if err != nil {
			f.PoolProblems = append(f.PoolProblems, fmt.Sprintf("%s: %v", pool, err))
			continue
		}
		flat := cfg.section("")
		for _, k := range cfg.keys[""] {
			if k == "diskId" || strings.HasPrefix(k, "diskId.") {
				if v := flat[k]; v != "" {
					f.Pools = append(f.Pools, PoolMember{Pool: pool, ID: v})
				}
			}
		}
	}
	return nil
}

func (f *Flash) readCapture(src FlashSource) error {
	data, err := src.Read("config/hoserva/capture.json")
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var c Capture
	if err := json.Unmarshal(data, &c); err != nil {
		f.CaptureProblem = "capture.json is not valid JSON"
		return nil
	}
	f.Capture = &c
	return nil
}

func diskCfgFrom(flat map[string]string) DiskCfg {
	d := DiskCfg{FsTypes: map[int]string{}}
	for k, v := range flat {
		switch {
		case strings.HasPrefix(k, "diskFsType."):
			if n, err := strconv.Atoi(strings.TrimPrefix(k, "diskFsType.")); err == nil {
				d.FsTypes[n] = v
			}
		case strings.HasPrefix(k, "diskIdSlot."):
			n, err := strconv.Atoi(strings.TrimPrefix(k, "diskIdSlot."))
			if err == nil && (n == parity1Slot || n == parity2Slot) && v != "" {
				d.ParitySlots = append(d.ParitySlots, n)
			}
		}
	}
	if len(d.ParitySlots) == 2 && d.ParitySlots[0] > d.ParitySlots[1] {
		d.ParitySlots[0], d.ParitySlots[1] = d.ParitySlots[1], d.ParitySlots[0]
	}
	return d
}

var versionLine = regexp.MustCompile(`^#*\s*Version\s+(\d+)\.(\d+)(?:\.(\d+))?(\S*)`)

// versionFromChanges reads the Unraid version from changes.txt's first line,
// "# Version 7.3.2 2026-09-10". It returns "" for a line that has none.
func versionFromChanges(data []byte) string {
	line, _, _ := strings.Cut(strings.TrimPrefix(string(data), "\ufeff"), "\n")
	m := versionLine.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return ""
	}
	v := m[1] + "." + m[2]
	if m[3] != "" {
		v += "." + m[3]
	}
	return v + m[4]
}

// supportedVersion reports whether v is on the allowlist: 6.12.x or any 7.x
// (Q24).
func supportedVersion(v string) bool {
	m := versionLine.FindStringSubmatch("Version " + v)
	if m == nil {
		return false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return major == 7 || (major == 6 && minor == 12)
}

// cfgFile is a parsed Unraid .cfg or .ini file: `key="value"` lines, with
// `["section"]` headers in disks.ini. Keys before the first header are in the
// section "".
type cfgFile struct {
	sections []string
	keys     map[string][]string
	values   map[string]map[string]string
}

func (c *cfgFile) section(name string) map[string]string {
	if v, ok := c.values[name]; ok {
		return v
	}
	return map[string]string{}
}

var (
	cfgKey     = regexp.MustCompile(`^[A-Za-z0-9_.\-]+$`)
	cfgSection = regexp.MustCompile(`^\[\s*"?([^"\]]*)"?\s*\]$`)
)

func parseCfg(data []byte) (*cfgFile, error) {
	if !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
		return nil, errors.New("it is not text")
	}
	c := &cfgFile{keys: map[string][]string{}, values: map[string]map[string]string{"": {}}}
	c.sections = []string{""}
	cur := ""
	for i, line := range strings.Split(strings.TrimPrefix(string(data), "\ufeff"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if m := cfgSection.FindStringSubmatch(line); m != nil {
			cur = m[1]
			if _, ok := c.values[cur]; !ok {
				c.values[cur] = map[string]string{}
				c.sections = append(c.sections, cur)
			}
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !cfgKey.MatchString(key) {
			return nil, fmt.Errorf("line %d is not a key=value setting", i+1)
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = val[1 : len(val)-1]
		}
		if _, dup := c.values[cur][key]; !dup {
			c.keys[cur] = append(c.keys[cur], key)
		}
		c.values[cur][key] = val
	}
	return c, nil
}
