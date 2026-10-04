package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// SambaCustomInclude is the user-owned escape hatch every generated
// smb.conf ends with (doc 01 §2). Generator never overwrites that file.
const SambaCustomInclude = "/etc/hoserva/smb.custom.conf"

// PathSambaCustom is SambaCustomInclude relative to Generator.Root
// (production Root is /etc).
const PathSambaCustom = "hoserva/smb.custom.conf"

// SambaServiceUnit is Samba's systemd service unit on Debian (doc 01
// §1's table) — smbd, not nmbd, which nothing in this repo currently
// generates or enables.
const SambaServiceUnit = "smbd.service"

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
	// Access restricts a non-guest share to the accounts it names. Nil
	// renders no user restriction, which only callers that never serve
	// real shares rely on; an Access with no ValidUsers renders the share
	// closed. Guest shares ignore it.
	Access *SambaAccess `json:"access,omitempty"`
}

// SambaAccess is the per-account access of one share, already resolved
// to account names (Hoserva groups have no Unix group, so they are never
// rendered as groups). WriteList names are written only when they are also
// in ValidUsers.
type SambaAccess struct {
	ValidUsers []string `json:"valid_users"`
	WriteList  []string `json:"write_list"`
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
		restricted := s.Access != nil && !s.Guest
		fmt.Fprintf(&b, "   read only = %s\n", sambaYesNo(s.ReadOnly || restricted))
		fmt.Fprintf(&b, "   guest ok = %s\n", sambaYesNo(s.Guest))
		if restricted {
			writeSambaAccess(&b, s)
		}
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

// writeSambaAccess renders the account restriction of a non-guest share.
// An empty `valid users =` would mean "everyone", so a share nobody may
// reach is rendered unavailable instead. Names that cannot be written
// safely are left out, which denies them.
func writeSambaAccess(b *strings.Builder, s SambaShare) {
	valid, listed := sambaUserList(s.Access.ValidUsers)
	if len(valid) == 0 {
		b.WriteString("   available = no\n")
		return
	}
	fmt.Fprintf(b, "   valid users = %s\n", strings.Join(valid, " "))
	if s.ReadOnly {
		return
	}
	var writers []string
	for _, name := range s.Access.WriteList {
		if entry, ok := sambaListEntry(name); ok && listed[name] {
			writers = append(writers, entry)
		}
	}
	if len(writers) > 0 {
		fmt.Fprintf(b, "   write list = %s\n", strings.Join(writers, " "))
	}
}

func sambaUserList(names []string) ([]string, map[string]bool) {
	var out []string
	listed := map[string]bool{}
	for _, name := range names {
		entry, ok := sambaListEntry(name)
		if !ok || listed[name] {
			continue
		}
		listed[name] = true
		out = append(out, entry)
	}
	return out, listed
}

// SambaUserListable reports whether name can be written into a Samba user
// list.
func SambaUserListable(name string) bool {
	_, ok := sambaListEntry(name)
	return ok
}

// sambaListEntry returns name as one entry of a Samba user list, quoted
// when it holds anything but letters, digits, '.', '_' and '-'. A name that
// could be read as something other than one account is refused: a leading
// '@', '+' or '&' names a Unix or NIS group to Samba, a '%' is expanded
// (%U would match every connecting user), and a quote, backslash, comma,
// separator or comment character, or any control character, could end the
// entry or the line.
func sambaListEntry(name string) (string, bool) {
	if name == "" || !utf8.ValidString(name) || name != strings.TrimSpace(name) {
		return "", false
	}
	switch name[0] {
	case '@', '+', '&':
		return "", false
	}
	bare := true
	for _, r := range name {
		if !unicode.IsPrint(r) || strings.ContainsRune("\"'\\%,;#=[]*?`", r) {
			return "", false
		}
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			bare = false
		}
	}
	if bare {
		return name, true
	}
	return `"` + name + `"`, true
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

// EnsureSambaCustomConf creates an empty PathSambaCustom under Root when
// missing. An existing file is left untouched (doc 01 §2).
func (g *Generator) EnsureSambaCustomConf() error {
	path := filepath.Join(g.Root, PathSambaCustom)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("config: stating %s: %w", PathSambaCustom, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("config: creating %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return fmt.Errorf("config: creating %s: %w", PathSambaCustom, err)
	}
	return f.Close()
}
