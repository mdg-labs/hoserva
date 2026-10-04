package migrate

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// diskResult is what reading one disk left: its totals, and the file holding its
// recorded entries.
type diskResult struct {
	m    *member
	base DiskBaseline
	list string
}

// progressLine is how a scan's progress reaches the job: a percentage of the
// whole scan, and a line for the job's log.
func (o ScanOptions) progress(pct int, format string, args ...any) {
	if o.Progress != nil {
		o.Progress(pct, fmt.Sprintf(format, args...))
	}
}

// Of the whole scan's progress, this much is spent before the data disks are
// read and this much on them, shared equally between the disks. The disks'
// share is an estimate: what each takes depends on its file count and sizes.
const (
	progressBeforeDisks = 2
	progressDisks       = 96
)

// recordBaseline reads each clean data disk read-only, one at a time, and records
// the baseline: the walk of every disk, the content hashes of the sample, and the
// paths several disks hold. A disk that cannot be read completely is refused and
// the others go on; nothing of it reaches the baseline. The baseline file is
// published only when the whole scan reached its end.
func (s *Scanner) recordBaseline(ctx context.Context, r *Report, clean []*member, opts ScanOptions) (err error) {
	if len(clean) == 0 {
		r.add(CheckBaseline, StatusWarn, "", "No data disk passed its checks, so no baseline was recorded.")
		return nil
	}
	token, err := newToken()
	if err != nil {
		return err
	}
	scratch := filepath.Join(s.Dir, tmpPrefix+token)
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return fmt.Errorf("creating the scan's scratch directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	rule := sampleRule(opts.FullChecksums)
	bf, err := newBaselineFile(s.Dir, token, rule)
	if err != nil {
		return fmt.Errorf("creating the baseline file: %w", err)
	}
	published := false
	defer func() {
		if !published {
			bf.abort()
		}
	}()

	var done []*diskResult
	for i, m := range clean {
		if err := ctx.Err(); err != nil {
			return err
		}
		lo := progressBeforeDisks + progressDisks*i/len(clean)
		hi := progressBeforeDisks + progressDisks*(i+1)/len(clean)
		res, err := s.readDisk(ctx, m, scratch, rule, func(frac float64, line string) {
			opts.progress(lo+int(float64(hi-lo)*frac), "%s", line)
		})
		switch {
		case err == nil:
			done = append(done, res)
		case errors.Is(err, ErrDiskRelease):
			// A mount that could not be released is the scan's failure, and the
			// one that matters most: it is reported even when the scan was
			// cancelled as well.
			return err
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			r.refuseDisk(m, CheckBaseline, RefuseUnreadable, "it passed its checks but could not be read completely (%s)", briefly(err))
		}
	}
	if len(done) == 0 {
		r.add(CheckBaseline, StatusWarn, "", "No data disk could be read, so no baseline was recorded.")
		return nil
	}

	summary := &BaselineSummary{File: bf.name, Rule: rule}
	for _, d := range done {
		if err := copyEntries(d.list, bf); err != nil {
			return fmt.Errorf("writing %s's entries to the baseline: %w", d.m.subject, err)
		}
		summary.Disks = append(summary.Disks, d.base)
	}
	opts.progress(progressBeforeDisks+progressDisks, "looking for paths that exist on more than one disk")
	if err := findDuplicates(ctx, done, bf, summary); err != nil {
		return err
	}
	if err := bf.finish(); err != nil {
		return fmt.Errorf("finishing the baseline file: %w", err)
	}
	published = true
	r.Baseline = summary
	describeBaseline(r, summary)
	return nil
}

// copyEntries appends a disk's recorded entries to the baseline file.
func copyEntries(list string, bf *baselineFile) error {
	return readRecords(list, func(rec record) error { return bf.w.write(rec) })
}

// readDisk mounts one data disk read-only, walks it, hashes the sample and
// unmounts it. The returned list holds the disk's entries with their hashes.
func (s *Scanner) readDisk(ctx context.Context, m *member, scratch string, rule SampleRule, progress func(frac float64, line string)) (*diskResult, error) {
	slot := slotName(m)
	walkList := filepath.Join(scratch, slot+".walk")
	list := filepath.Join(scratch, slot+".entries")
	var res *diskResult
	err := withDisk(ctx, s.Mounter, *m.disk, m.fs, filepath.Join(s.Dir, mountsDir, dataMount), func(root string) error {
		if free, err := s.free(root); err == nil {
			m.free, m.freeKnown = free, true
		}
		dirs, err := topLevelDirs(root)
		if err != nil {
			return err
		}
		progress(0, fmt.Sprintf("%s: reading the directory tree of %s", m.subject, m.disk.Device))
		w, err := newEntryWriter(walkList)
		if err != nil {
			return err
		}
		walked, hidden, werr := walkDisk(ctx, root, slot, w, func(n int64) {
			progress(0, fmt.Sprintf("%s: %d entries so far", m.subject, n))
		})
		if cerr := w.close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return werr
		}
		progress(0.3, fmt.Sprintf("%s: %d entries found, hashing the sample", m.subject, walked))
		res, err = hashDisk(ctx, root, m, slot, walkList, list, walked, rule, func(frac float64) {
			progress(0.3+0.7*frac, fmt.Sprintf("%s: hashed %.0f%% of the sample", m.subject, frac*100))
		})
		if err != nil {
			return err
		}
		res.base.HiddenSkipped = hidden
		m.dirs, m.dirsRead = dirs, true
		return nil
	})
	_ = os.Remove(walkList)
	if err != nil {
		_ = os.Remove(list)
		return nil, err
	}
	return res, nil
}

// walkDisk records every file, symlink and special file under root's
// non-hidden top-level entries, in the order a walk that reads each directory in
// name order visits them. A symlink is recorded with its target and never
// followed; a hidden top-level entry (.Trash-* and the like) is not a share and
// is left out, as Unraid leaves it out. It returns how many entries it wrote and
// the hidden directories it skipped. Any entry it cannot read is an error: a
// baseline with a hole in it is not one.
func walkDisk(ctx context.Context, root, slot string, w *entryWriter, progress func(n int64)) (written int64, hidden []string, err error) {
	top, err := os.ReadDir(root)
	if err != nil {
		return 0, nil, err
	}
	emit := func(p string, d fs.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		e, err := entryFor(p, d)
		if err != nil {
			return err
		}
		e.Disk = slot
		e.setName(filepath.ToSlash(rel))
		if err := w.write(e.record()); err != nil {
			return err
		}
		written++
		if written%100000 == 0 {
			progress(written)
		}
		return nil
	}
	for _, t := range top {
		name := t.Name()
		if strings.HasPrefix(name, ".") {
			if t.IsDir() {
				hidden = append(hidden, name)
			}
			continue
		}
		p := filepath.Join(root, name)
		if !t.IsDir() {
			if err := emit(p, t); err != nil {
				return written, hidden, err
			}
			continue
		}
		err := filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			return emit(path, d)
		})
		if err != nil {
			return written, hidden, err
		}
	}
	return written, hidden, nil
}

func (e *BaselineEntry) setName(rel string) {
	if isUTF8(rel) {
		e.Path, e.RawPath = rel, nil
		return
	}
	e.Path, e.RawPath = "", []byte(rel)
}

func isUTF8(s string) bool {
	_, raw := encodeName(s)
	return raw == ""
}

// entryFor builds the entry for a directory entry that is not a directory: a
// regular file with its size, a symlink with its target, or a special file with
// its type.
func entryFor(path string, d fs.DirEntry) (BaselineEntry, error) {
	t := d.Type()
	switch {
	case t.IsRegular():
		info, err := d.Info()
		if err != nil {
			return BaselineEntry{}, err
		}
		return BaselineEntry{Kind: KindFile, Size: info.Size()}, nil
	case t&fs.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return BaselineEntry{}, err
		}
		e := BaselineEntry{Kind: KindSymlink}
		if isUTF8(target) {
			e.Target = target
		} else {
			e.RawTarget = []byte(target)
		}
		return e, nil
	case t&fs.ModeNamedPipe != 0:
		return BaselineEntry{Kind: KindSpecial, Special: "fifo"}, nil
	case t&fs.ModeSocket != 0:
		return BaselineEntry{Kind: KindSpecial, Special: "socket"}, nil
	case t&fs.ModeDevice != 0 && t&fs.ModeCharDevice != 0:
		return BaselineEntry{Kind: KindSpecial, Special: "chardev"}, nil
	case t&fs.ModeDevice != 0:
		return BaselineEntry{Kind: KindSpecial, Special: "blockdev"}, nil
	}
	return BaselineEntry{Kind: KindSpecial, Special: "other"}, nil
}

// hashDisk reads a disk's walk back, hashes the files the sample rule picks and
// writes every entry, with its hash where it has one, to list. total is how many
// entries the walk wrote.
func hashDisk(ctx context.Context, root string, m *member, slot, walkList, list string, total int64, rule SampleRule, progress func(frac float64)) (*diskResult, error) {
	threshold, hasLarge, err := sampleThreshold(walkList, rule)
	if err != nil {
		return nil, fmt.Errorf("choosing the sample: %w", err)
	}
	w, err := newEntryWriter(list)
	if err != nil {
		return nil, err
	}
	base := DiskBaseline{Slot: slot, Filesystem: string(m.fs)}
	shares := map[string]*ShareTotals{}
	var processed int64
	err = readRecords(walkList, func(rec record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		e, err := rec.entry()
		if err != nil {
			return err
		}
		if e.Kind == KindFile {
			picked := rule.Full || e.Size <= rule.SmallFileBytes || (hasLarge && pathKey(e.name()) <= threshold)
			if picked {
				h, err := hashFile(ctx, filepath.Join(root, filepath.FromSlash(e.name())))
				if err != nil {
					return fmt.Errorf("hashing %s: %w", e.name(), err)
				}
				e.SHA256 = h
				base.Hashed++
				base.HashedBytes += e.Size
			}
		}
		share := e.Share()
		if share == "" {
			base.RootEntries++
		}
		t := shares[share]
		if t == nil {
			t = &ShareTotals{Share: share}
			shares[share] = t
		}
		t.add(&e)
		switch e.Kind {
		case KindFile:
			base.Files++
			base.Bytes += e.Size
		case KindSymlink:
			base.Symlinks++
		default:
			base.Special++
		}
		if err := w.write(e.record()); err != nil {
			return err
		}
		processed++
		if processed%20000 == 0 && total > 0 {
			progress(float64(processed) / float64(total))
		}
		return nil
	})
	if cerr := w.close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	for _, t := range shares {
		base.Shares = append(base.Shares, *t)
	}
	sort.Slice(base.Shares, func(i, j int) bool { return base.Shares[i].Share < base.Shares[j].Share })
	return &diskResult{m: m, base: base, list: list}, nil
}

// head is the next unread entry of one disk's list.
type head struct {
	disk string
	name string
	it   *recordReader
}

type headHeap []*head

func (h headHeap) Len() int { return len(h) }
func (h headHeap) Less(i, j int) bool {
	if c := comparePaths(h[i].name, h[j].name); c != 0 {
		return c < 0
	}
	return h[i].disk < h[j].disk
}
func (h headHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *headHeap) Push(x any)   { *h = append(*h, x.(*head)) }
func (h *headHeap) Pop() any {
	old := *h
	v := old[len(old)-1]
	*h = old[:len(old)-1]
	return v
}

// advance reads the next entry into hd, and reports false at the end of its list.
func (hd *head) advance() (bool, error) {
	rec, ok, err := hd.it.next()
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	name, _, err := decodeName(rec.P, rec.PB)
	if err != nil {
		return false, err
	}
	hd.name = name
	return true, nil
}

// findDuplicates merges the disks' lists, each in walk order, and records every
// path more than one disk holds: mergerfs shows only one copy of it, and
// which one the user sees is not something to leave unsaid. A path that is a
// file on one disk and a symlink on another is as much a duplicate as two files.
func findDuplicates(ctx context.Context, done []*diskResult, bf *baselineFile, sum *BaselineSummary) error {
	if len(done) < 2 {
		return nil
	}
	var h headHeap
	var readers []*recordReader
	defer func() {
		for _, it := range readers {
			it.close()
		}
	}()
	for _, d := range done {
		it, err := openRecords(d.list)
		if err != nil {
			return err
		}
		readers = append(readers, it)
		hd := &head{disk: d.base.Slot, it: it}
		ok, err := hd.advance()
		if err != nil {
			return err
		}
		if ok {
			h = append(h, hd)
		}
	}
	heap.Init(&h)
	var n int64
	for h.Len() > 0 {
		if n++; n%100000 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		first := heap.Pop(&h).(*head)
		same := []*head{first}
		for h.Len() > 0 && comparePaths(h[0].name, first.name) == 0 {
			same = append(same, heap.Pop(&h).(*head))
		}
		if len(same) > 1 {
			dup := DuplicatePath{Path: first.name}
			for _, hd := range same {
				dup.Disks = append(dup.Disks, hd.disk)
			}
			sort.Strings(dup.Disks)
			sum.Duplicates++
			if len(sum.DuplicateSample) < maxListedDuplicates {
				sum.DuplicateSample = append(sum.DuplicateSample, dup)
			}
			if err := bf.duplicate(dup); err != nil {
				return err
			}
		}
		for _, hd := range same {
			ok, err := hd.advance()
			if err != nil {
				return err
			}
			if ok {
				heap.Push(&h, hd)
			}
		}
	}
	return nil
}
