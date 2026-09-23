package config

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// NFSServiceUnit is nfs-kernel-server's systemd service unit on Debian
// (doc 01 §1's table).
const NFSServiceUnit = "nfs-kernel-server.service"

// NFSShare is the NFS slice of a share RenderNFSExports needs. Disabled
// shares are omitted by the caller, not rendered as empty lines.
type NFSShare struct {
	Name   string   `json:"name"`
	Hosts  []string `json:"hosts"`
	Squash string   `json:"squash"`
}

// NFSState is the JSON shape of testdata/configs/*/state.json for
// exports goldens.
type NFSState struct {
	Shares []NFSShare `json:"shares"`
}

// RenderNFSExports renders an /etc/exports file from shares (doc 03
// §4.2). Share lines are sorted by name; hosts on a line are sorted.
// Docs do not specify an NFS equivalent of SambaCustomInclude, so this
// file has none (doc 01 §2).
func RenderNFSExports(shares []NFSShare) string {
	ordered := append([]NFSShare(nil), shares...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })

	var b strings.Builder
	for _, s := range ordered {
		hosts := append([]string(nil), s.Hosts...)
		sort.Strings(hosts)
		fmt.Fprintf(&b, "/mnt/user/%s", s.Name)
		for _, h := range hosts {
			fmt.Fprintf(&b, " %s(rw,sync,no_subtree_check,%s)", nfsClientToken(h), s.Squash)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// nfsClientToken is the client field of an exports(5) line. IPv6
// addresses and networks must be square-bracketed; hostnames and IPv4
// are emitted as given.
func nfsClientToken(h string) string {
	if ip := net.ParseIP(h); ip != nil && ip.To4() == nil {
		return "[" + h + "]"
	}
	if ip, ipnet, err := net.ParseCIDR(h); err == nil && ip.To4() == nil {
		ones, _ := ipnet.Mask.Size()
		return fmt.Sprintf("[%s]/%d", ip.String(), ones)
	}
	return h
}

// WriteNFS renders /etc/exports from shares and writes it through Write
// (D4, PathNFS). An unmanaged or unimported host file is refused
// (ErrUnmanaged / ErrExistingHostFile) — Q76.
func (g *Generator) WriteNFS(ctx context.Context, shares []NFSShare, command string, revision int, now time.Time) error {
	return g.Write(ctx, File{
		Path:    PathNFS,
		Command: command,
		Body:    []byte(RenderNFSExports(shares)),
	}, revision, now)
}
