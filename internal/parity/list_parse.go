package parity

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ErrListParse wraps a `snapraid list -l <file>` log ParseList could not
// make sense of.
var ErrListParse = errors.New("parity: could not parse snapraid list output")

// ListFile is one file `snapraid list` reports as currently tracked
// (doc 02 §1 line 78, #223): the SnapRAID disk id that holds it (the
// `data <name> ...` name from the config, not yet resolved to a mount
// point), its path relative to that disk, and its size in bytes — read
// straight from the content file `sync` just wrote, never a live stat.
// Hardlinks and symlinks (`link_hardlink:`/`link_symlink:` log lines)
// carry no size of their own and are not represented here.
type ListFile struct {
	Disk    string
	RelPath string
	Size    int64
}

// ListReport is a `snapraid list -l <log>` run, parsed.
type ListReport struct {
	// DataMounts maps SnapRAID's own disk id to the mount path the config
	// assigned it, read from the log's own config echo — the same shape
	// DiffLog.DataMounts and StatusReport.DataMounts already use.
	DataMounts map[string]string
	Files      []ListFile
}

// ParseList parses a `snapraid list -l <log>` run's structured log
// (doc 02 §1 line 78, #223: this is how a per-disk directory breakdown is
// computed at sync time, from tracked state, rather than a live walk).
func ParseList(log []byte) (ListReport, error) {
	lines := splitLogLines(log)
	if lines == nil {
		return ListReport{}, fmt.Errorf("%w: empty log", ErrListParse)
	}

	report := ListReport{DataMounts: map[string]string{}}
	sawSummary := false

	for _, line := range lines {
		tag, rest, ok := cutTag(line)
		if !ok {
			continue
		}
		switch tag {
		case "data":
			id, path, ok := strings.Cut(rest, ":")
			if ok {
				report.DataMounts[id] = path
			}
		case "file":
			if f, ok := parseListFileLine(rest); ok {
				report.Files = append(report.Files, f)
			}
		case "summary":
			sawSummary = true
		}
	}

	if !sawSummary {
		return ListReport{}, fmt.Errorf("%w: no summary section found", ErrListParse)
	}
	return report, nil
}

// parseListFileLine parses one `file:<disk>:<relpath>:<size>:<mtime>:...`
// line's own field list (everything after the "file:" tag cutTag already
// removed) — confirmed against a real snapraid 12.4-1 binary in the
// loop-device lab (testdata/parsers/snapraid_list_shares.log): disk,
// then the path, then exactly four trailing numeric fields (size, mtime
// and two fields this package has no use for). The path is rejoined with
// ":" rather than trusted to be field[1] alone, since a relative path can
// itself contain ':' — the same caution diff_parse.go's copy-line parsing
// already takes.
func parseListFileLine(rest string) (ListFile, bool) {
	fields := strings.Split(rest, ":")
	if len(fields) < 6 {
		return ListFile{}, false
	}
	sizeIdx := len(fields) - 4
	disk := fields[0]
	path := strings.Join(fields[1:sizeIdx], ":")
	if disk == "" || path == "" {
		return ListFile{}, false
	}
	size, err := strconv.ParseInt(fields[sizeIdx], 10, 64)
	if err != nil || size < 0 {
		return ListFile{}, false
	}
	return ListFile{Disk: disk, RelPath: path, Size: size}, true
}

// ShareUsage is one share's bytes on one data disk mount point, as of the
// sync that computed it (doc 02 §1 line 78, doc 03 §4.1-4.2, #223) — both
// AggregateShareUsage's own result shape and UsageStore's persisted row
// shape.
type ShareUsage struct {
	Share string
	Disk  string // mount point, matching DiffReport.PerDisk's own keys.
	Bytes int64
}

// AggregateShareUsage groups list's tracked files by top-level path
// segment (the share directory name, D10 — every share is exactly one
// top-level directory per data disk, internal/share's own shareDataRoots)
// per disk, the per-disk directory breakdown doc 02 §1 line 78 describes
// computed once from tracked state, never a live walk. A file with no
// top-level directory of its own — directly at a data disk's root, never
// something internal/share itself creates — belongs to no share and is
// skipped, as is any file on a disk the log's own "data:" echo never
// named.
func AggregateShareUsage(list ListReport) []ShareUsage {
	type key struct{ share, disk string }
	totals := map[key]int64{}
	for _, f := range list.Files {
		share, tail, ok := strings.Cut(f.RelPath, "/")
		if !ok || share == "" || tail == "" {
			continue
		}
		mount, ok := list.DataMounts[f.Disk]
		if !ok {
			continue
		}
		totals[key{share, filepath.Clean(mount)}] += f.Size
	}

	out := make([]ShareUsage, 0, len(totals))
	for k, bytes := range totals {
		out = append(out, ShareUsage{Share: k.share, Disk: k.disk, Bytes: bytes})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Share != out[j].Share {
			return out[i].Share < out[j].Share
		}
		return out[i].Disk < out[j].Disk
	})
	return out
}
