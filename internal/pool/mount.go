package pool

import (
	"fmt"
	"strings"
)

// Mount is one systemd .mount unit for a mergerfs pool mount — the
// catch-all, a per-share mount, or the mover's array-only write target
// (doc 02 §1, Q12). Unlike disk.MountUnit (a block device mounted by
// filesystem UUID), What is a mergerfs branch list, and RequiresMountsFor
// carries the ordering doc 02 §1 calls for: every per-share mount after
// the catch-all, and every mount after the block-device mounts its own
// branches sit on.
type Mount struct {
	Where             string
	What              string // mergerfs branches, already built by topology.go
	FSName            string
	CreatePolicy      CreatePolicy
	Options           Options
	Description       string
	RequiresMountsFor []string
}

// optionsString returns m's full comma-separated mergerfs option list,
// in doc 02 §1's table order — the same string Render's Options= line
// and Argv's -o argument both use, so the two are never able to drift
// apart.
func (m Mount) optionsString() string {
	return fmt.Sprintf("category.create=%s,%s,fsname=%s", m.CreatePolicy, m.Options.render(), m.FSName)
}

// Render returns m's systemd unit file content — the body a caller
// writes through config.Generator under its own doc 01 §2 header (D4:
// config files are generated, never hand-edited), the same convention
// disk.MountUnit.Render follows for a block-device mount.
func (m Mount) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Unit]\nDescription=%s\n", m.Description)
	if len(m.RequiresMountsFor) > 0 {
		fmt.Fprintf(&b, "RequiresMountsFor=%s\n", strings.Join(m.RequiresMountsFor, " "))
	}
	b.WriteString("\n[Mount]\n")
	fmt.Fprintf(&b, "What=%s\n", m.What)
	fmt.Fprintf(&b, "Where=%s\n", m.Where)
	b.WriteString("Type=fuse.mergerfs\n")
	fmt.Fprintf(&b, "Options=%s\n", m.optionsString())
	return b.String()
}

// Argv returns the argv a caller execs to bring m up directly — never a
// shell (CLAUDE.md) — mirroring scripts/devenv/create-array.sh's own
// mergerfs invocation: no -f, so mergerfs daemonizes on its own and the
// call returns once the mount is live.
func (m Mount) Argv() []string {
	return []string{"mergerfs", "-o", m.optionsString(), m.What, m.Where}
}
