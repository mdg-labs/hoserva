package config

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SambaCustomInclude is the user-owned escape hatch every generated
// smb.conf ends with (doc 01 §2). Generator never writes that file.
const SambaCustomInclude = "/etc/hoserva/smb.custom.conf"

// SambaShare is the SMB slice of a share RenderSambaConf needs. Disabled
// shares are omitted by the caller, not rendered as empty sections.
type SambaShare struct {
	Name               string `json:"name"`
	Guest              bool   `json:"guest"`
	ReadOnly           bool   `json:"read_only"`
	Browseable         bool   `json:"browseable"`
	Recycle            bool   `json:"recycle"`
	TimeMachine        bool   `json:"time_machine"`
	TimeMachineMaxSize string `json:"time_machine_max_size,omitempty"`
}

// SambaState is the JSON shape of testdata/configs/*/state.json for
// smb.conf goldens.
type SambaState struct {
	Shares []SambaShare `json:"shares"`
}

// RenderSambaConf renders an smb.conf from shares (doc 03 §4.2, Q73).
// Share sections are sorted by name. The user-owned include is always
// last. vfs objects on a share that uses the recycle bin include fruit
// as well, because a per-share vfs list replaces the global one.
//
// Every share carries the Q26 ownership masks: `create mask = 0664` with
// `force create mode = 0664`, `directory mask = 2775` with `force
// directory mode = 2775` (setgid, so new subdirectories keep the shared
// group), and `force group = users`. The masks alone only ever remove
// bits from a client-requested mode; without the matching `force …
// mode`, an SMB client that requests 0644 (or a directory without the
// setgid bit) keeps that narrower mode, breaking the shared-group
// ownership model for later filesystem writes. `force group` takes a
// name, not a GID: Debian's base-passwd already defines a system group
// named `users` at GID 100 — the same value Unraid uses — so it needs no
// creation here.
func RenderSambaConf(shares []SambaShare) string {
	ordered := append([]SambaShare(nil), shares...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })

	var b strings.Builder
	b.WriteString("[global]\n")
	b.WriteString("   workgroup = WORKGROUP\n")
	b.WriteString("   server string = Hoserva\n")
	b.WriteString("   security = user\n")
	b.WriteString("   map to guest = Bad User\n")
	b.WriteString("   min protocol = SMB2\n")
	b.WriteString("   ea support = yes\n")
	b.WriteString("   vfs objects = catia fruit streams_xattr\n")
	b.WriteString("   fruit:metadata = stream\n")
	b.WriteString("   fruit:posix_rename = yes\n")
	b.WriteString("   fruit:veto_appledouble = no\n")
	b.WriteString("   fruit:nfs_aces = no\n")
	b.WriteString("   fruit:wipe_intentionally_left_blank_rfork = yes\n")
	b.WriteString("   fruit:delete_empty_adfiles = yes\n")
	b.WriteString("   load printers = no\n")
	b.WriteString("   printing = bsd\n")
	b.WriteString("   printcap name = /dev/null\n")
	b.WriteString("   disable spoolss = yes\n")

	for _, s := range ordered {
		fmt.Fprintf(&b, "\n[%s]\n", s.Name)
		fmt.Fprintf(&b, "   path = /mnt/user/%s\n", s.Name)
		fmt.Fprintf(&b, "   browseable = %s\n", sambaYesNo(s.Browseable))
		fmt.Fprintf(&b, "   read only = %s\n", sambaYesNo(s.ReadOnly))
		fmt.Fprintf(&b, "   guest ok = %s\n", sambaYesNo(s.Guest))
		b.WriteString("   create mask = 0664\n")
		b.WriteString("   force create mode = 0664\n")
		b.WriteString("   directory mask = 2775\n")
		b.WriteString("   force directory mode = 2775\n")
		b.WriteString("   force group = users\n")
		if s.Recycle {
			b.WriteString("   vfs objects = catia fruit streams_xattr recycle\n")
			b.WriteString("   recycle:repository = .recycle\n")
			b.WriteString("   recycle:keeptree = yes\n")
			b.WriteString("   recycle:versions = yes\n")
		}
		if s.TimeMachine {
			b.WriteString("   fruit:time machine = yes\n")
			if s.TimeMachineMaxSize != "" {
				fmt.Fprintf(&b, "   fruit:time machine max size = %s\n", s.TimeMachineMaxSize)
			}
		}
	}

	fmt.Fprintf(&b, "\ninclude = %s\n", SambaCustomInclude)
	return b.String()
}

func sambaYesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// WriteSamba renders smb.conf from shares and writes it through Write
// (D4, PathSamba). An unmanaged or unimported host file is refused
// (ErrUnmanaged / ErrExistingHostFile) — Q76.
func (g *Generator) WriteSamba(ctx context.Context, shares []SambaShare, command string, revision int, now time.Time) error {
	return g.Write(ctx, File{
		Path:    PathSamba,
		Command: command,
		Body:    []byte(RenderSambaConf(shares)),
	}, revision, now)
}
