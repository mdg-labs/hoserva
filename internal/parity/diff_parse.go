package parity

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrDiffParse wraps a `snapraid diff -l <file>` log ParseDiff could not
// make sense of.
var ErrDiffParse = errors.New("parity: could not parse snapraid diff output")

// DiffLog is one `snapraid diff -l <log>` run, parsed (doc 02 §2). It is
// the diff alone — everything that log itself carries. The public
// DiffReport additionally needs `status`'s before-counts, since a diff
// log never states how many files a disk had before the changes it
// reports; BuildDiffReport combines the two.
type DiffLog struct {
	// DataMounts maps SnapRAID's own disk id ("d1") to its mount path,
	// read from the log's own config echo.
	DataMounts map[string]string

	Equal, Added, Removed, Updated, Moved, Copied, Restored int

	diskAdd    map[string]int
	diskRemove map[string]int
	diskCopyIn map[string]int

	// removed, added and copiedIn are the same scan lines diskAdd/
	// diskRemove/diskCopyIn tally, kept with their own disk id and
	// relative path — BuildDiffReport projects these into the public
	// DiffReport.RemovedFiles/AddedFiles the threshold guard's manifest
	// accounting (Q15) matches against.
	removed, added, copiedIn []diskPath
}

// diskPath is a SnapRAID disk id ("d1") paired with a path relative to
// that disk's own mount, exactly as one scan line reports it.
type diskPath struct {
	disk, path string
}

// ParseDiff parses a `snapraid diff -l <log>` run's structured log
// (doc 02 §2).
func ParseDiff(log []byte) (DiffLog, error) {
	lines := splitLogLines(log)
	if lines == nil {
		return DiffLog{}, fmt.Errorf("%w: empty log", ErrDiffParse)
	}

	d := DiffLog{
		DataMounts: map[string]string{},
		diskAdd:    map[string]int{},
		diskRemove: map[string]int{},
		diskCopyIn: map[string]int{},
	}
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
				d.DataMounts[id] = path
			}
		case "scan":
			parseScanLine(&d, rest)
		case "summary":
			sawSummary = true
			parseDiffSummaryField(&d, rest)
		}
	}

	if !sawSummary {
		return DiffLog{}, fmt.Errorf("%w: no summary section found", ErrDiffParse)
	}
	return d, nil
}

func parseDiffSummaryField(d *DiffLog, rest string) {
	fields := strings.Split(rest, ":")
	if len(fields) != 2 {
		return
	}
	n, ok := atoiField(fields, 1)
	if !ok {
		return
	}
	switch fields[0] {
	case "equal":
		d.Equal = n
	case "added":
		d.Added = n
	case "removed":
		d.Removed = n
	case "updated":
		d.Updated = n
	case "moved":
		d.Moved = n
	case "copied":
		d.Copied = n
	case "restored":
		d.Restored = n
	}
}

// parseScanLine tallies one `scan:<action>:...` record for the per-disk
// projection BuildDiffReport needs. Only the four actions this project has
// ever seen SnapRAID emit are handled — add, remove, update, copy
// (confirmed against a real snapraid 12.4-1 binary, testdata/parsers). A
// rename ("moved") and an undelete-during-diff ("restored") are real diff
// categories whose totals above always come straight from `summary:` and
// are always accurate — but this lab's SnapRAID build can never actually
// produce a `moved` line to confirm its own scan-line shape against: doc
// 08 §5 found no data-disk UUID is ever available here, so a rename is
// always reported as remove+copy instead. Rather than guess `scan:move:`'s
// field shape, such a line — if one is ever seen on real hardware — is
// left out of the per-disk projection; the aggregate Moved/Restored
// totals above are unaffected either way.
func parseScanLine(d *DiffLog, rest string) {
	action, tail, ok := strings.Cut(rest, ":")
	if !ok {
		return
	}
	switch action {
	case "add":
		disk, path, ok := strings.Cut(tail, ":")
		if ok {
			d.diskAdd[disk]++
			d.added = append(d.added, diskPath{disk, path})
		}
	case "remove":
		disk, path, ok := strings.Cut(tail, ":")
		if ok {
			d.diskRemove[disk]++
			d.removed = append(d.removed, diskPath{disk, path})
		}
	case "copy":
		// tail is "srcdisk:srcpath:dstdisk:dstpath"; srcpath can itself
		// contain ':', so only the first field (srcdisk, unused here), the
		// third (dstdisk) and the fourth (dstpath) are trusted — safe as
		// long as the path doesn't, true of every real capture this parser
		// is tested against.
		parts := strings.SplitN(tail, ":", 4)
		if len(parts) == 4 {
			d.diskCopyIn[parts[2]]++
			d.copiedIn = append(d.copiedIn, diskPath{parts[2], parts[3]})
		}
	}
}

// BuildDiffReport combines before (call Status/ParseStatus first) with a
// parsed diff into the public DiffReport (doc 02 §2) the threshold guard
// consumes. FilesAfter projects what each disk will hold once this diff's
// changes are committed by a sync: before + added + copied-in - removed
// for that disk. MovedByHoserva is always zero here — matching a removal
// to a relocation manifest (Q15) is the guard's own job, not Engine's.
func BuildDiffReport(before StatusReport, d DiffLog) DiffReport {
	report := DiffReport{
		Added:   d.Added,
		Removed: d.Removed,
		Updated: d.Updated,
		Moved:   d.Moved,
		Copied:  d.Copied,
		PerDisk: map[string]DiskDiff{},
	}
	for id, mount := range d.DataMounts {
		b := before.PerDiskFileCount[id]
		after := b + d.diskAdd[id] + d.diskCopyIn[id] - d.diskRemove[id]
		report.PerDisk[filepath.Clean(mount)] = DiskDiff{FilesBefore: b, FilesAfter: after}
	}
	report.RemovedFiles = projectDiffFiles(d.DataMounts, d.removed)
	report.AddedFiles = append(projectDiffFiles(d.DataMounts, d.added), projectDiffFiles(d.DataMounts, d.copiedIn)...)
	return report
}

// projectDiffFiles resolves each diskPath's SnapRAID disk id to its mount
// point (skipping any id the diff log's own "data:" echo never defined,
// which cannot happen for a diff produced against the same config this
// package rendered, but is not a decoding error either), matching
// DiffReport.PerDisk's own key shape.
func projectDiffFiles(mounts map[string]string, files []diskPath) []DiffFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]DiffFile, 0, len(files))
	for _, f := range files {
		mount, ok := mounts[f.disk]
		if !ok {
			continue
		}
		out = append(out, DiffFile{Disk: filepath.Clean(mount), RelPath: f.path})
	}
	return out
}
