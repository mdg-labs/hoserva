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
	DockerErr         error
	DockerUnavailable bool
}

// DockerInventory lists Engine containers and images. Tests inject a
// fake; production uses ExecDocker. A nil inventory is treated as
// Docker not installed — no host_docker_* doctor checks.
type DockerInventory interface {
	List(ctx context.Context) (containers, images []DockerRef, err error)
}

// MemoryDocker is a scriptable fake for tests — it never talks to a
// real Engine.
type MemoryDocker struct {
	Containers []DockerRef
	Images     []DockerRef
	Err        error
}

func (m MemoryDocker) List(context.Context) ([]DockerRef, []DockerRef, error) {
	if m.Err != nil {
		return nil, nil, m.Err
	}
	return m.Containers, m.Images, nil
}

// ExecDocker runs `docker ps` / `docker images` with a structured argv
// (never a shell). LookPath failure is reported as Docker unavailable,
// not as a probe error.
type ExecDocker struct{}

// dockerInventoryTimeout bounds ExecDocker.List so a stalled Engine
// cannot hold RunDoctor or GetStatus open. It matches doctor's probe
// bound (internal/api.doctorProbeTimeout).
const dockerInventoryTimeout = 8 * time.Second

func (ExecDocker) List(ctx context.Context) ([]DockerRef, []DockerRef, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, nil, errDockerUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, dockerInventoryTimeout)
	defer cancel()
	containers, err := dockerList(ctx, []string{"ps", "-a", "--format", "{{.ID}} {{.Names}}"})
	if err != nil {
		return nil, nil, err
	}
	images, err := dockerList(ctx, []string{"images", "--format", "{{.ID}} {{.Repository}}:{{.Tag}}"})
	if err != nil {
		return nil, nil, err
	}
	return containers, images, nil
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
	containers, images, err := docker.List(ctx)
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

// ParseSambaShares returns [section] names that are not Samba's own
// global/printers sections. Comments and include= lines are ignored.
func ParseSambaShares(raw []byte) ([]string, error) {
	var shares []string
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if !strings.HasPrefix(line, "[") {
			continue
		}
		end := strings.IndexByte(line, ']')
		if end < 2 {
			continue
		}
		name := line[1:end]
		switch strings.ToLower(name) {
		case "global", "printers", "print$":
			continue
		}
		shares = append(shares, name)
	}
	return shares, sc.Err()
}

// ParseNFSExports returns the exported paths from an /etc/exports file.
func ParseNFSExports(raw []byte) ([]string, error) {
	var paths []string
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
		paths = append(paths, fields[0])
	}
	return paths, sc.Err()
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
// (import on both docker categories), Docker holds no containers or
// images, and a cache disk exists. This function never moves data.
func DockerDataRoot(inv HostInventory, acceptedMove, hasCache bool) string {
	if !acceptedMove || !hasCache || len(inv.DockerContainers) > 0 || len(inv.DockerImages) > 0 || inv.DockerErr != nil {
		return DockerDataRootDefault
	}
	return "/mnt/cache/docker"
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
