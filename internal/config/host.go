package config

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Q76 host-file paths, relative to Generator.Root (which is /etc in
// production and a temp directory in tests — never the development
// host's real /etc from a unit test).
const (
	PathSamba = "samba/smb.conf"
	PathNFS   = "exports"
	PathFstab = "fstab"
)

// DockerDataRootDefault is where Docker keeps containers and images
// unless the user accepted a cache move AND Docker holds none (Q62, Q76).
const DockerDataRootDefault = "/var/lib/docker"

// DockerDataRootCache is the one cache-side data-root DockerDataRoot ever
// moves Docker to (Q62): a single, fixed path, never derived per host or
// per call, so a caller finishing an interrupted move already knows this
// is the only target it could have been.
const DockerDataRootCache = "/mnt/cache/docker"

const (
	KindSamba            = "samba"
	KindNFS              = "nfs"
	KindFstab            = "fstab"
	KindDockerContainers = "docker_containers"
	KindDockerImages     = "docker_images"
)

const (
	DecisionImport = "import"
	DecisionLeave  = "leave"
)

// ErrExistingHostFile is Write's refusal when path already exists on
// disk and Generator has no record of it — Q76's first apply never
// overwrites a file the user has not imported.
var ErrExistingHostFile = errors.New("config: existing host file has not been imported")

var hostFileByKind = map[string]string{
	KindSamba: PathSamba,
	KindNFS:   PathNFS,
	KindFstab: PathFstab,
}

// HostFilePath returns the Root-relative path Generator manages for kind,
// or "" for Docker inventory (not a generated file).
func HostFilePath(kind string) string {
	return hostFileByKind[kind]
}

// DockerRef is one container or image Detect listed.
type DockerRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// HostFile is one Q76 managed file's parsed contents.
type HostFile struct {
	Kind    string
	Path    string
	Present bool
	Items   []string
	Err     error
}

// HostInventory is what onboarding's system check lists (Q76).
type HostInventory struct {
	Samba             HostFile
	NFS               HostFile
	Fstab             HostFile
	DockerContainers  []DockerRef
	DockerImages      []DockerRef
	DockerVolumes     []DockerRef
	DockerNetworks    []DockerRef
	DockerPlugins     []DockerRef
	DockerErr         error
	DockerUnavailable bool
}

// DockerInventory lists Engine containers, images, named volumes,
// user-defined networks (excluding the built-in bridge/host/none) and
// installed plugins. Tests inject a fake; production uses ExecDocker. A
// nil inventory is treated as Docker not installed — no host_docker_*
// doctor checks.
type DockerInventory interface {
	List(ctx context.Context) (containers, images, volumes, networks, plugins []DockerRef, err error)
}

// MemoryDocker is a scriptable fake for tests — it never talks to a
// real Engine.
type MemoryDocker struct {
	Containers []DockerRef
	Images     []DockerRef
	Volumes    []DockerRef
	Networks   []DockerRef
	Plugins    []DockerRef
	Err        error
}

func (m MemoryDocker) List(context.Context) ([]DockerRef, []DockerRef, []DockerRef, []DockerRef, []DockerRef, error) {
	if m.Err != nil {
		return nil, nil, nil, nil, nil, m.Err
	}
	return m.Containers, m.Images, m.Volumes, m.Networks, m.Plugins, nil
}

// ExecDocker runs `docker ps` / `docker images` with a structured argv
// (never a shell). LookPath failure is reported as Docker unavailable,
// not as a probe error.
type ExecDocker struct{}

// dockerInventoryTimeout bounds ExecDocker.List so a stalled Engine
// cannot hold RunDoctor or GetStatus open. It matches doctor's probe
// bound (internal/api.doctorProbeTimeout).
const dockerInventoryTimeout = 8 * time.Second

func (ExecDocker) List(ctx context.Context) ([]DockerRef, []DockerRef, []DockerRef, []DockerRef, []DockerRef, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, nil, nil, nil, nil, errDockerUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, dockerInventoryTimeout)
	defer cancel()
	containers, err := dockerList(ctx, []string{"ps", "-a", "--format", "{{.ID}} {{.Names}}"})
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	images, err := dockerList(ctx, []string{"images", "--format", "{{.ID}} {{.Repository}}:{{.Tag}}"})
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	// Named volumes have no separate ID (their name is the identifier),
	// so dockerList's "no space" fallback sets DockerRef.ID = Name — the
	// same shape ps/images give a container or image (Q76).
	volumes, err := dockerList(ctx, []string{"volume", "ls", "--format", "{{.Name}}"})
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	// type=custom excludes the built-in bridge/host/none networks Docker
	// always creates, so anything this returns is the user's own
	// configuration (#416).
	networks, err := dockerList(ctx, []string{"network", "ls", "--filter", "type=custom", "--format", "{{.ID}} {{.Name}}"})
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	plugins, err := dockerList(ctx, []string{"plugin", "ls", "--format", "{{.ID}} {{.Name}}"})
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	return containers, images, volumes, networks, plugins, nil
}

var errDockerUnavailable = errors.New("config: docker is not installed")

func dockerList(ctx context.Context, args []string) ([]DockerRef, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("config: docker %s: %w", args[0], err)
	}
	var refs []DockerRef
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		id, name, ok := strings.Cut(line, " ")
		if !ok {
			id, name = line, line
		}
		refs = append(refs, DockerRef{ID: id, Name: strings.TrimSpace(name)})
	}
	return refs, sc.Err()
}

// Detect reads Q76 host files under root and, if docker is non-nil,
// lists containers and images. Unit tests pass a temp root; production
// passes Generator.Root (/etc).
func Detect(ctx context.Context, root string, docker DockerInventory) (HostInventory, error) {
	if err := ctx.Err(); err != nil {
		return HostInventory{}, err
	}
	inv := HostInventory{
		Samba: detectFile(root, KindSamba, PathSamba, ParseSambaShares),
		NFS:   detectFile(root, KindNFS, PathNFS, ParseNFSExports),
		Fstab: detectFile(root, KindFstab, PathFstab, ParseFstabMounts),
	}
	if docker == nil {
		inv.DockerUnavailable = true
		return inv, nil
	}
	containers, images, volumes, networks, plugins, err := docker.List(ctx)
	if err != nil {
		if errors.Is(err, errDockerUnavailable) {
			inv.DockerUnavailable = true
			return inv, nil
		}
		inv.DockerErr = err
		return inv, nil
	}
	inv.DockerContainers = containers
	inv.DockerImages = images
	inv.DockerVolumes = volumes
	inv.DockerNetworks = networks
	inv.DockerPlugins = plugins
	return inv, nil
}

func detectFile(root, kind, rel string, parse func([]byte) ([]string, error)) HostFile {
	hf := HostFile{Kind: kind, Path: rel}
	raw, err := os.ReadFile(filepath.Join(root, rel))
	if errors.Is(err, os.ErrNotExist) {
		return hf
	}
	if err != nil {
		hf.Err = err
		return hf
	}
	hf.Present = true
	items, err := parse(raw)
	if err != nil {
		hf.Err = err
		return hf
	}
	hf.Items = items
	return hf
}

func (inv HostInventory) File(kind string) HostFile {
	switch kind {
	case KindSamba:
		return inv.Samba
	case KindNFS:
		return inv.NFS
	case KindFstab:
		return inv.Fstab
	default:
		return HostFile{Kind: kind}
	}
}

func (inv HostInventory) Found(kind string) bool {
	switch kind {
	case KindSamba:
		return inv.Samba.Present || inv.Samba.Err != nil
	case KindNFS:
		return inv.NFS.Present || inv.NFS.Err != nil
	case KindFstab:
		return inv.Fstab.Present || inv.Fstab.Err != nil
	case KindDockerContainers, KindDockerImages:
		return !inv.DockerUnavailable
	default:
		return false
	}
}

// SambaShareDetails is one [section] ImportFromHost turns into a shares
// row: the options RenderSambaConf round-trips (doc 03 §4.2, Q73).
type SambaShareDetails struct {
	Name               string
	Guest              bool
	ReadOnly           bool
	Browseable         bool
	Recycle            bool
	TimeMachine        bool
	TimeMachineMaxSize string
}

// NFSExportDetails is one exports(5) line: Path is the original export
// path, Name is filepath.Base(Path) for the shares table (D10 regenerates
// /mnt/user/<name>), plus hosts and squash (doc 03 §4.2).
type NFSExportDetails struct {
	Path   string
	Name   string
	Hosts  []string
	Squash string
}

// ParseSambaShares returns [section] names that are not Samba's own
// global/printers sections. Comments and include= lines are ignored.
func ParseSambaShares(raw []byte) ([]string, error) {
	details, err := ParseSambaShareDetails(raw)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(details))
	for _, d := range details {
		names = append(names, d.Name)
	}
	return names, nil
}

// ParseSambaShareDetails returns every non-global/printers section with
// the SMB options Hoserva stores. Unrecognised keys are ignored. Defaults
// match a new share's SMB slice (browseable, not guest, not read-only)
// when a key is absent.
func ParseSambaShareDetails(raw []byte) ([]SambaShareDetails, error) {
	var (
		shares  []SambaShareDetails
		current *SambaShareDetails
	)
	flush := func() {
		if current != nil {
			shares = append(shares, *current)
			current = nil
		}
	}
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			flush()
			end := strings.IndexByte(line, ']')
			if end < 2 {
				continue
			}
			name := line[1:end]
			switch strings.ToLower(name) {
			case "global", "printers", "print$":
				current = nil
				continue
			}
			current = &SambaShareDetails{Name: name, Browseable: true}
			continue
		}
		if current == nil {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		switch key {
		case "guest ok", "guest only":
			current.Guest = sambaBool(val)
		case "read only":
			current.ReadOnly = sambaBool(val)
		case "writeable", "writable":
			current.ReadOnly = !sambaBool(val)
		case "browseable", "browsable":
			current.Browseable = sambaBool(val)
		case "vfs objects":
			for _, obj := range strings.Fields(val) {
				if strings.EqualFold(obj, "recycle") {
					current.Recycle = true
				}
			}
		case "fruit:time machine":
			current.TimeMachine = sambaBool(val)
		case "fruit:time machine max size":
			current.TimeMachineMaxSize = val
		}
	}
	flush()
	return shares, sc.Err()
}

func sambaBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yes", "true", "1":
		return true
	default:
		return false
	}
}

// ParseNFSExports returns the exported paths from an /etc/exports file.
func ParseNFSExports(raw []byte) ([]string, error) {
	details, err := ParseNFSExportDetails(raw)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(details))
	for _, d := range details {
		paths = append(paths, d.Path)
	}
	return paths, nil
}

// ParseNFSExportDetails returns every exports(5) line with its share
// name (filepath.Base of the export path), client hosts, and squash
// option. A missing squash defaults to root_squash.
func ParseNFSExportDetails(raw []byte) ([]NFSExportDetails, error) {
	var out []NFSExportDetails
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		path := fields[0]
		name := filepath.Base(path)
		hosts := make([]string, 0, len(fields)-1)
		squash := "root_squash"
		for _, spec := range fields[1:] {
			// exports(5) allows one default-options field, beginning
			// with '-', between the path and the clients. It is not a
			// client host; its squash setting is the default for the line.
			if strings.HasPrefix(spec, "-") {
				if s := nfsSquashFromOpts(strings.TrimPrefix(spec, "-")); s != "" {
					squash = s
				}
				continue
			}
			host, opts := splitNFSClient(spec)
			if host == "" {
				continue
			}
			hosts = append(hosts, host)
			if s := nfsSquashFromOpts(opts); s != "" {
				squash = s
			}
		}
		out = append(out, NFSExportDetails{Path: path, Name: name, Hosts: hosts, Squash: squash})
	}
	return out, sc.Err()
}

func splitNFSClient(spec string) (host, opts string) {
	if i := strings.IndexByte(spec, '('); i >= 0 {
		host = spec[:i]
		opts = strings.TrimSuffix(spec[i+1:], ")")
		return host, opts
	}
	return spec, ""
}

func nfsSquashFromOpts(opts string) string {
	for _, o := range strings.Split(opts, ",") {
		switch strings.TrimSpace(o) {
		case "root_squash", "no_root_squash", "all_squash":
			return strings.TrimSpace(o)
		}
	}
	return ""
}

var fstabSkipTypes = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true,
	"tmpfs": true, "overlay": true, "squashfs": true, "autofs": true,
	"cgroup": true, "cgroup2": true, "debugfs": true, "securityfs": true,
	"pstore": true, "efivarfs": true, "mqueue": true, "hugetlbfs": true,
	"fusectl": true, "bpf": true, "tracefs": true, "configfs": true,
	"ramfs": true, "swap": true,
}

// ParseFstabMounts returns mountpoints from fstab entries that are not
// pseudo-filesystems. Bind mounts (type none + bind) are included.
func ParseFstabMounts(raw []byte) ([]string, error) {
	var mounts []string
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		fstype := fields[2]
		opts := ""
		if len(fields) > 3 {
			opts = fields[3]
		}
		if isBind(opts) || (!fstabSkipTypes[fstype] && fstype != "none") {
			mounts = append(mounts, fields[1])
		}
	}
	return mounts, sc.Err()
}

func isBind(opts string) bool {
	for _, o := range strings.Split(opts, ",") {
		if strings.TrimSpace(o) == "bind" {
			return true
		}
	}
	return false
}

// FactsJSON is the SQLite blob ApplyHostConfig stores for kind.
func FactsJSON(inv HostInventory, kind string) ([]byte, error) {
	switch kind {
	case KindSamba:
		return json.Marshal(map[string]any{"path": PathSamba, "shares": inv.Samba.Items})
	case KindNFS:
		return json.Marshal(map[string]any{"path": PathNFS, "exports": inv.NFS.Items})
	case KindFstab:
		return json.Marshal(map[string]any{"path": PathFstab, "mounts": inv.Fstab.Items})
	case KindDockerContainers:
		return json.Marshal(map[string]any{"containers": inv.DockerContainers, "dataRoot": DockerDataRootDefault})
	case KindDockerImages:
		return json.Marshal(map[string]any{"images": inv.DockerImages, "dataRoot": DockerDataRootDefault})
	default:
		return nil, fmt.Errorf("config: unknown host-config kind %q", kind)
	}
}

// DockerDataRoot reports where Docker's data-root stays after a Q76
// apply: the cache path is allowed only when the user accepted a move
// (import on both docker categories), Docker holds no containers,
// images, named volumes, user-defined networks or plugins, and a cache
// disk exists. A named volume, network or plugin blocks the move even
// though there is nothing to "import" for it — #413/#416: moving the
// data-root without that state moving too strands it under the old
// root, invisible to Docker from then on (swarm state is not counted —
// Hoserva does not support swarm, D6). A failed inventory listing
// (DockerErr) fails closed the same way, never as "Docker holds
// nothing". This function never moves data.
func DockerDataRoot(inv HostInventory, acceptedMove, hasCache bool) string {
	if !acceptedMove || !hasCache ||
		len(inv.DockerContainers) > 0 || len(inv.DockerImages) > 0 || len(inv.DockerVolumes) > 0 ||
		len(inv.DockerNetworks) > 0 || len(inv.DockerPlugins) > 0 || inv.DockerErr != nil {
		return DockerDataRootDefault
	}
	return DockerDataRootCache
}

// KindFromCheckID maps a DoctorCheck.id (host_samba, or host_samba_*) to
// the host_config.kind stored in SQLite.
func KindFromCheckID(id string) (string, bool) {
	switch {
	case id == "host_samba" || strings.HasPrefix(id, "host_samba_"):
		return KindSamba, true
	case id == "host_nfs" || strings.HasPrefix(id, "host_nfs_"):
		return KindNFS, true
	case id == "host_fstab" || strings.HasPrefix(id, "host_fstab_"):
		return KindFstab, true
	case id == "host_docker_containers" || strings.HasPrefix(id, "host_docker_containers_"):
		return KindDockerContainers, true
	case id == "host_docker_images" || strings.HasPrefix(id, "host_docker_images_"):
		return KindDockerImages, true
	default:
		return "", false
	}
}
