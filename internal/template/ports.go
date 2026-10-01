package template

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mdg-labs/hoserva/internal/container"
)

// PortSource reports the host ports that are taken.
type PortSource interface {
	UsedPorts(ctx context.Context) (map[int]bool, error)
}

// HostPorts is the PortSource of a running system: the host ports that
// containers publish or are configured to publish, whether they run or are
// stopped, and the ports the host itself listens on.
type HostPorts struct {
	Containers container.Provider
	// ProcNet is the directory holding tcp, tcp6, udp and udp6; empty means
	// /proc/net.
	ProcNet string
}

// UsedPorts fails, and never reports a port free, when either source cannot
// be read: a port nobody could check is not known to be free.
func (h HostPorts) UsedPorts(ctx context.Context) (map[int]bool, error) {
	if h.Containers == nil {
		return nil, fmt.Errorf("template: %w: no container provider to read published ports from", container.ErrUnavailable)
	}
	all, err := h.Containers.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing containers to find the ports they publish: %w", err)
	}
	used := map[int]bool{}
	for _, c := range all {
		ports := c.Ports
		switch c.State {
		case "running", "restarting", "paused":
		default:
			// The listing reports no published ports for a container that
			// is not running, but it binds its configured ones when it
			// starts.
			ports, err = h.Containers.ConfiguredPorts(ctx, c.ID)
			if errors.Is(err, container.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("reading the ports of the stopped container %s: %w", c.Name, err)
			}
		}
		for _, p := range ports {
			if p.HostPort != 0 {
				used[int(p.HostPort)] = true
			}
		}
	}
	dir := h.ProcNet
	if dir == "" {
		dir = "/proc/net"
	}
	for _, f := range []struct {
		name string
		// state is the socket state that means the port is taken: listening
		// for TCP, bound and unconnected for UDP.
		state string
		// optional files are absent when that address family is disabled.
		optional bool
	}{{"tcp", "0A", false}, {"tcp6", "0A", true}, {"udp", "07", false}, {"udp6", "07", true}} {
		ports, err := readNetPorts(filepath.Join(dir, f.name), f.state)
		if err != nil {
			if f.optional && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		for _, p := range ports {
			used[p] = true
		}
	}
	return used, nil
}

// readNetPorts returns the local ports of the sockets in one /proc/net
// table that are in state.
func readNetPorts(file, state string) ([]int, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("reading the host's sockets: %w", err)
	}
	defer func() { _ = f.Close() }()
	var out []int
	sc := bufio.NewScanner(f)
	sc.Scan() // the header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || fields[3] != state {
			continue
		}
		_, port, ok := strings.Cut(fields[1], ":")
		if !ok {
			return nil, fmt.Errorf("reading %s: unexpected address %q", file, fields[1])
		}
		n, err := strconv.ParseUint(port, 16, 16)
		if err != nil {
			return nil, fmt.Errorf("reading %s: unexpected port in %q", file, fields[1])
		}
		out = append(out, int(n))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", file, err)
	}
	return out, nil
}

// nextFreePort is the lowest port above from that is neither used nor
// already given to another input of the same install.
func nextFreePort(from int, taken ...map[int]bool) (int, bool) {
	for p := from + 1; p <= 65535; p++ {
		free := true
		for _, m := range taken {
			if m[p] {
				free = false
				break
			}
		}
		if free {
			return p, true
		}
	}
	return 0, false
}
