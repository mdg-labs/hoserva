package template

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
)

// renderGroup is the host group that owns the GPU render nodes (Q82).
const renderGroup = "render"

// GPUHost reports the GPUs a template's device input can be given (Q82).
type GPUHost interface {
	// RenderDevices lists the host's render nodes, such as
	// /dev/dri/renderD128, in name order.
	RenderDevices(ctx context.Context) ([]string, error)
	// RenderGID is the numeric id of the host's render group.
	RenderGID(ctx context.Context) (string, error)
}

// HostGPU is the GPUHost of a running system. It only reads the directory
// listing and the group database.
type HostGPU struct {
	// DRIDir is the directory holding the render nodes; empty means /dev/dri.
	DRIDir string
}

func (h HostGPU) RenderDevices(context.Context) ([]string, error) {
	dir := h.DRIDir
	if dir == "" {
		dir = "/dev/dri"
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing the host's GPU render devices: %w", err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "renderD") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

func (HostGPU) RenderGID(context.Context) (string, error) {
	g, err := user.LookupGroup(renderGroup)
	if err != nil {
		return "", fmt.Errorf("%w: looking up the host's %q group: %v", ErrGPUUnavailable, renderGroup, err)
	}
	return g.Gid, nil
}
