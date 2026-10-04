package migrate

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The verify phase (doc 05 §4 step 16): after the import has adopted the data
// disks read-only, every disk is walked through its own read-only mount and every
// share through the pool, and what is found is compared with the scan's
// baseline: file counts, total bytes, every file's size, every symlink's target
// and every special file's type, and a fresh sha256 of exactly the files the
// baseline hashed. Any difference, and any file or directory that cannot be
// read, fails the verify. Nothing is written to a source disk: the only things
// written are the scratch lists under the session's directory and the result in
// the session's row.
//
// The baseline is millions of rows on a real array, so nothing here holds it in
// memory: it is split into one list per disk, and each comparison is a merge of
// two lists in walk order.

// Verify statuses.
const (
	VerifyRunning = "running"
	VerifyPassed  = "passed"
	VerifyFailed  = "failed"
)

// maxListedPaths caps each list of paths a result keeps; the total says how many
// there were.
const maxListedPaths = 50

var (
	// ErrVerifyNotPending is returned when verify is asked for and no Unraid
	// import is waiting for its point of no return.
	ErrVerifyNotPending = errors.New("there is no adopted Unraid array to verify: import the data disks first")
	// ErrVerifyNotConfigured is returned when this daemon cannot find the adopted
	// disks or confirm their mounts.
	ErrVerifyNotConfigured = errors.New("this daemon cannot verify an adopted array")
	// ErrVerifyMismatch is returned by a verify that completed and found the
	// adopted array different from the baseline.
	ErrVerifyMismatch = errors.New("the adopted array differs from the scan's baseline")
)

// VerifyCounts is what a scope holds. Bytes is the sum of the regular files'
// sizes.
type VerifyCounts struct {
	Files    int64 `json:"files"`
	Symlinks int64 `json:"symlinks"`
	Special  int64 `json:"special"`
	Bytes    int64 `json:"bytes"`
}

func (c *VerifyCounts) add(e *BaselineEntry) {
	switch e.Kind {
	case KindFile:
		c.Files++
		c.Bytes += e.Size
	case KindSymlink:
		c.Symlinks++
	default:
		c.Special++
	}
}

// VerifyList is a capped list of paths with the number there were.
type VerifyList struct {
	Total int64    `json:"total"`
	Paths []string `json:"paths,omitempty"`
}

func (l *VerifyList) add(path string) {
	l.Total++
	if len(l.Paths) < maxListedPaths {
		l.Paths = append(l.Paths, path)
	}
}

// VerifyScope is the comparison of one disk, or of one share through the pool.
// A share named "" is the files directly in the pool's root.
type VerifyScope struct {
	Name string `json:"name"`
	// Problem says why the scope could not be compared at all.
	Problem  string       `json:"problem,omitempty"`
	Expected VerifyCounts `json:"expected"`
	Found    VerifyCounts `json:"found"`
	// Hashed is how many of the baseline's sample were hashed again.
	Hashed          int64      `json:"hashed"`
	Missing         VerifyList `json:"missing"`
	Extra           VerifyList `json:"extra"`
	SizeChanged     VerifyList `json:"sizeChanged"`
	ChecksumChanged VerifyList `json:"checksumChanged"`
	// Changed lists a path whose kind, symlink target or special-file type
	// differs from the baseline's.
	Changed VerifyList `json:"changed"`
}

// Passed is true when the scope was compared and nothing differs.
func (s *VerifyScope) Passed() bool {
	return s.Problem == "" && s.Expected == s.Found && s.Missing.Total == 0 && s.Extra.Total == 0 &&
		s.SizeChanged.Total == 0 && s.ChecksumChanged.Total == 0 && s.Changed.Total == 0
}

// VerifyResult is what the verify phase recorded. Status is VerifyRunning while
// it runs, and a result that is not VerifyPassed blocks everything that waits for
// a good verify.
type VerifyResult struct {
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	// Disks has one scope per baseline disk and per adopted disk with no
	// baseline; Shares one per top-level directory of the pool and of the
	// baseline.
	Disks  []VerifyScope `json:"disks,omitempty"`
	Shares []VerifyScope `json:"shares,omitempty"`
	// Duplicates counts the paths the baseline has on more than one disk, which
	// the pool shows once, from the first disk. DuplicateSample lists the first of
	// them. They are never counted as missing or extra.
	Duplicates      int64           `json:"duplicates"`
	DuplicateSample []DuplicatePath `json:"duplicateSample,omitempty"`
}

func (r *VerifyResult) summary() string {
	var bad int
	for _, group := range [][]VerifyScope{r.Disks, r.Shares} {
		for i := range group {
			if !group[i].Passed() {
				bad++
			}
		}
	}
	return fmt.Sprintf("%d disk or share comparisons failed", bad)
}

// AdoptedDisk is one data disk of the adopted array.
type AdoptedDisk struct {
	Serial     string
	WWN        string
	Mountpoint string
}

// Adoption is the adopted array as the verify phase walks it: its data disks in
// the pool's branch order, and where the pool is mounted.
type Adoption struct {
	Disks []AdoptedDisk
	Pool  string
}

// CheckVerify returns nil when a verify can start: an import is pending its
// point of no return and the session names a baseline.
func (s *Service) CheckVerify(ctx context.Context) error {
	_, err := s.verifiable(ctx)
	return err
}

func (s *Service) verifiable(ctx context.Context) (*Report, error) {
	if s.Pending == nil {
		return nil, ErrVerifyNotPending
	}
	pending, err := s.Pending(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading whether an import is pending: %w", err)
	}
	if !pending {
		return nil, ErrVerifyNotPending
	}
	s.mu.Lock()
	sess, err := s.load(ctx)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if sess.Report == nil || sess.Report.Baseline == nil {
		return nil, ErrNoBaseline
	}
	if _, err := s.baselineName(ctx); err != nil {
		return nil, err
	}
	return sess.Report, nil
}

// RunVerify is the migration_verify job. It clears the session's earlier result,
// records that a verify is running, compares the adopted array with the baseline
// and records the outcome, whether it passed, found a mismatch or could not
// finish. It returns nil only for a passing verify.
func (s *Service) RunVerify(ctx context.Context, out io.Writer) error {
	if s.Adopted == nil || s.ConfirmReadOnly == nil {
		return ErrVerifyNotConfigured
	}
	report, err := s.verifiable(ctx)
	if err != nil {
		return err
	}
	started := time.Now().UTC()
	if err := s.setVerify(ctx, &VerifyResult{Status: VerifyRunning, StartedAt: started}); err != nil {
		return err
	}
	res, runErr := s.compare(ctx, out, report)
	switch {
	case runErr == nil:
	case errors.Is(runErr, context.Canceled):
		res = &VerifyResult{Status: VerifyFailed, Error: "the verify was cancelled before it finished"}
	default:
		res = &VerifyResult{Status: VerifyFailed, Error: runErr.Error()}
	}
	res.StartedAt, res.FinishedAt = started, time.Now().UTC()
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.setVerify(wctx, res); err != nil {
		return errors.Join(runErr, fmt.Errorf("recording the verify result: %w", err))
	}
	if runErr != nil {
		return runErr
	}
	if res.Status != VerifyPassed {
		return fmt.Errorf("%w: %s", ErrVerifyMismatch, res.summary())
	}
	return nil
}

// setVerify replaces the session's verify result. It is refused when the session
// has no report, which a forgotten session has not.
func (s *Service) setVerify(ctx context.Context, v *VerifyResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.load(ctx)
	if err != nil {
		return err
	}
	if sess.Report == nil {
		return ErrNoReport
	}
	sess.Verify = v
	return s.save(ctx, sess)
}

// verifyOutput is where a verify says what it is doing: a line per step in the
// job's log and the job's percentage.
type verifyOutput struct {
	out  io.Writer
	pct  func(int)
	last int
}

func newVerifyOutput(ctx context.Context, out io.Writer) *verifyOutput {
	pct, _ := ctx.Value(progressKey{}).(func(int))
	return &verifyOutput{out: out, pct: pct, last: -1}
}

func (o *verifyOutput) say(p int, format string, args ...any) {
	if o.out != nil {
		_, _ = fmt.Fprintf(o.out, format+"\n", args...)
	}
	if o.pct != nil && p != o.last {
		o.last = p
		o.pct(p)
	}
}

// mapDisks pairs each baseline disk with the adopted disk that has the same
// serial or WWN, through the report's disk table, which names the serial of each
// slot. A disk with no partner, on either side, is a problem, not a skip.
func mapDisks(report *Report, ad Adoption) (baseline map[string]int, orphans []int, err error) {
	if report.Review == nil {
		return nil, nil, errors.New("the report has no disk table to match the baseline's disks to the adopted ones: scan again")
	}
	baseline = map[string]int{}
	used := map[int]bool{}
	for _, d := range report.Baseline.Disks {
		for _, row := range report.Review.Disks {
			if row.Slot != d.Slot {
				continue
			}
			for i, a := range ad.Disks {
				if used[i] {
					continue
				}
				same := a.Serial != "" && a.Serial == row.Serial
				if a.WWN != "" && row.WWN != "" {
					same = strings.EqualFold(a.WWN, row.WWN)
				}
				if same {
					baseline[d.Slot] = i
					used[i] = true
				}
			}
		}
	}
	for i := range ad.Disks {
		if !used[i] {
			orphans = append(orphans, i)
		}
	}
	return baseline, orphans, nil
}

// compare walks the adopted disks and the pool and compares them with the
// baseline. Any error is returned and never read as a pass.
func (s *Service) compare(ctx context.Context, out io.Writer, report *Report) (*VerifyResult, error) {
	say := newVerifyOutput(ctx, out)
	ad, err := s.Adopted(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the adopted array: %w", err)
	}
	if ad.Pool == "" || len(ad.Disks) == 0 {
		return nil, errors.New("the adopted array has no data disks or no pool")
	}
	base, orphans, err := mapDisks(report, ad)
	if err != nil {
		return nil, err
	}
	for _, i := range base {
		if err := s.ConfirmReadOnly(ctx, ad.Disks[i].Mountpoint); err != nil {
			return nil, fmt.Errorf("%s is not confirmed read-only, so it is not read: %w", ad.Disks[i].Mountpoint, err)
		}
	}
	if err := s.ConfirmReadOnly(ctx, ad.Pool); err != nil {
		return nil, fmt.Errorf("the pool at %s is not confirmed read-only, so it is not read: %w", ad.Pool, err)
	}

	br, err := s.OpenBaseline(ctx)
	if err != nil {
		return nil, err
	}
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	scratch := filepath.Join(s.Dir, tmpPrefix+"verify-"+token)
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return nil, fmt.Errorf("creating the verify's scratch directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	say.say(1, "splitting the baseline by disk")
	lists, err := br.splitByDisk(ctx, scratch)
	if err != nil {
		return nil, fmt.Errorf("reading the baseline: %w", err)
	}
	for i, d := range report.Baseline.Disks {
		if lists[d.Slot] != "" {
			continue
		}
		empty := filepath.Join(scratch, fmt.Sprintf("empty-%d.list", i))
		w, err := newEntryWriter(empty)
		if err == nil {
			err = w.close()
		}
		if err != nil {
			return nil, fmt.Errorf("writing the empty list of %s: %w", d.Slot, err)
		}
		lists[d.Slot] = empty
	}

	res := &VerifyResult{}
	checksums := map[string]*VerifyList{}
	n := len(report.Baseline.Disks) + len(orphans)
	for k, d := range report.Baseline.Disks {
		scope := VerifyScope{Name: d.Slot}
		i, ok := base[d.Slot]
		switch {
		case !ok:
			scope.Problem = "this disk is in the scan's baseline and no adopted disk has its serial"
		default:
			lo, hi := 2+66*k/n, 2+66*(k+1)/n
			say.say(lo, "%s: reading %s and comparing it with the baseline", d.Slot, ad.Disks[i].Mountpoint)
			if err := compareDisk(ctx, &scope, ad.Disks[i].Mountpoint, d.Slot, lists[d.Slot], scratch, checksums); err != nil {
				return nil, fmt.Errorf("%s: %w", d.Slot, err)
			}
			say.say(hi, "%s: %d files, %d symlinks and %d special files found; %d sample files hashed again", d.Slot, scope.Found.Files, scope.Found.Symlinks, scope.Found.Special, scope.Hashed)
		}
		res.Disks = append(res.Disks, scope)
	}
	for _, i := range orphans {
		res.Disks = append(res.Disks, VerifyScope{Name: filepath.Base(ad.Disks[i].Mountpoint), Problem: "this adopted disk has no disk in the scan's baseline"})
	}

	say.say(70, "reading the pool at %s and comparing it with the baseline's union of the disks", ad.Pool)
	branches := make([]string, 0, len(report.Baseline.Disks))
	for _, d := range report.Baseline.Disks {
		branches = append(branches, d.Slot)
	}
	rank := func(slot string) int {
		if i, ok := base[slot]; ok {
			return i
		}
		return len(ad.Disks)
	}
	sort.SliceStable(branches, func(a, b int) bool { return rank(branches[a]) < rank(branches[b]) })
	shares, err := comparePool(ctx, res, ad.Pool, branches, lists, scratch, checksums)
	if err != nil {
		return nil, fmt.Errorf("the pool: %w", err)
	}
	res.Shares = shares
	say.say(99, "%d paths are on more than one disk and are shown once through the pool", res.Duplicates)

	res.Status = VerifyPassed
	for _, group := range [][]VerifyScope{res.Disks, res.Shares} {
		for i := range group {
			if !group[i].Passed() {
				res.Status = VerifyFailed
			}
		}
	}
	say.say(100, "verify %s", res.Status)
	return res, nil
}

// compareDisk walks the disk mounted at root and merges the walk with the
// disk's baseline list. A sample file's checksum that differs is also noted
// under its share in checksums.
func compareDisk(ctx context.Context, scope *VerifyScope, root, slot, baselineList, scratch string, checksums map[string]*VerifyList) error {
	walk := filepath.Join(scratch, slot+".walk")
	w, err := newEntryWriter(walk)
	if err != nil {
		return err
	}
	_, _, werr := walkDisk(ctx, root, slot, w, func(int64) {})
	if cerr := w.close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("reading the disk: %w", werr)
	}
	return mergeLists(ctx, baselineList, walk, nil, mergeHandlers{
		missing: func(e *BaselineEntry) {
			scope.Expected.add(e)
			scope.Missing.add(e.name())
		},
		extra: func(e *BaselineEntry) {
			scope.Found.add(e)
			scope.Extra.add(e.name())
		},
		both: func(want, got *BaselineEntry) error {
			scope.Expected.add(want)
			scope.Found.add(got)
			switch kind := diffEntry(want, got); kind {
			case diffSize:
				scope.SizeChanged.add(got.name())
			case diffOther:
				scope.Changed.add(got.name())
			default:
				if want.Kind == KindFile && want.SHA256 != "" {
					sum, err := hashFile(ctx, filepath.Join(root, filepath.FromSlash(got.name())))
					if err != nil {
						return fmt.Errorf("hashing %s: %w", got.name(), err)
					}
					scope.Hashed++
					if sum != want.SHA256 {
						scope.ChecksumChanged.add(got.name())
						l := checksums[got.Share()]
						if l == nil {
							l = &VerifyList{}
							checksums[got.Share()] = l
						}
						l.add(got.name())
					}
				}
			}
			return nil
		},
	})
}

// comparePool walks the pool and merges the walk with the union of the baseline's
// disks, branches in the pool's order, a path on several of them taken from the
// first. It records the duplicates in res and returns one scope per share.
func comparePool(ctx context.Context, res *VerifyResult, pool string, branches []string, lists map[string]string, scratch string, checksums map[string]*VerifyList) ([]VerifyScope, error) {
	walk := filepath.Join(scratch, "pool.walk")
	w, err := newEntryWriter(walk)
	if err != nil {
		return nil, err
	}
	_, _, werr := walkDisk(ctx, pool, "pool", w, func(int64) {})
	if cerr := w.close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return nil, fmt.Errorf("reading the pool: %w", werr)
	}
	union, err := openUnion(branches, lists, func(d DuplicatePath) {
		res.Duplicates++
		if len(res.DuplicateSample) < maxListedPaths {
			res.DuplicateSample = append(res.DuplicateSample, d)
		}
	})
	if err != nil {
		return nil, err
	}
	defer union.close()

	scopes := map[string]*VerifyScope{}
	of := func(share string) *VerifyScope {
		s := scopes[share]
		if s == nil {
			s = &VerifyScope{Name: share}
			scopes[share] = s
		}
		return s
	}
	err = mergeLists(ctx, "", walk, union, mergeHandlers{
		missing: func(e *BaselineEntry) {
			s := of(e.Share())
			s.Expected.add(e)
			s.Missing.add(e.name())
		},
		extra: func(e *BaselineEntry) {
			s := of(e.Share())
			s.Found.add(e)
			s.Extra.add(e.name())
		},
		both: func(want, got *BaselineEntry) error {
			s := of(got.Share())
			s.Expected.add(want)
			s.Found.add(got)
			switch diffEntry(want, got) {
			case diffSize:
				s.SizeChanged.add(got.name())
			case diffOther:
				s.Changed.add(got.name())
			}
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	for share, l := range checksums {
		s := of(share)
		s.ChecksumChanged.Total += l.Total
		s.ChecksumChanged.Paths = append(s.ChecksumChanged.Paths, l.Paths...)
		if len(s.ChecksumChanged.Paths) > maxListedPaths {
			s.ChecksumChanged.Paths = s.ChecksumChanged.Paths[:maxListedPaths]
		}
	}
	names := make([]string, 0, len(scopes))
	for n := range scopes {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]VerifyScope, 0, len(names))
	for _, n := range names {
		out = append(out, *scopes[n])
	}
	return out, nil
}

type diffKind int

const (
	diffNone diffKind = iota
	diffSize
	diffOther
)

// diffEntry says how got differs from want. A symlink is the same only with the
// same target, a special file only of the same type, a file only of the same
// size.
func diffEntry(want, got *BaselineEntry) diffKind {
	switch {
	case want.Kind != got.Kind:
		return diffOther
	case want.Kind == KindSymlink:
		if want.Target != got.Target || string(want.RawTarget) != string(got.RawTarget) {
			return diffOther
		}
	case want.Kind == KindSpecial:
		if want.Special != got.Special {
			return diffOther
		}
	case want.Size != got.Size:
		return diffSize
	}
	return diffNone
}

// entryStream is a sequence of entries in walk order.
type entryStream interface {
	next() (BaselineEntry, bool, error)
	close()
}

type listStream struct{ rr *recordReader }

func openList(path string) (*listStream, error) {
	rr, err := openRecords(path)
	if err != nil {
		return nil, err
	}
	return &listStream{rr: rr}, nil
}

func (l *listStream) next() (BaselineEntry, bool, error) {
	rec, ok, err := l.rr.next()
	if err != nil || !ok {
		return BaselineEntry{}, false, err
	}
	e, err := rec.entry()
	return e, err == nil, err
}

func (l *listStream) close() { l.rr.close() }

type mergeHandlers struct {
	missing func(want *BaselineEntry)
	extra   func(got *BaselineEntry)
	both    func(want, got *BaselineEntry) error
}

// mergeLists merges the expected entries, from the list at wantList or from
// want, with the list at gotList, both in walk order, and calls a handler for
// each path: expected and not found, found and not expected, or both.
func mergeLists(ctx context.Context, wantList, gotList string, want entryStream, h mergeHandlers) error {
	if want == nil {
		l, err := openList(wantList)
		if err != nil {
			return err
		}
		want = l
		defer want.close()
	}
	got, err := openList(gotList)
	if err != nil {
		return err
	}
	defer got.close()

	w, wok, err := want.next()
	if err != nil {
		return err
	}
	g, gok, err := got.next()
	if err != nil {
		return err
	}
	var n int64
	for wok || gok {
		if n++; n%50000 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		switch {
		case !gok || (wok && comparePaths(w.name(), g.name()) < 0):
			h.missing(&w)
			if w, wok, err = want.next(); err != nil {
				return err
			}
		case !wok || comparePaths(w.name(), g.name()) > 0:
			h.extra(&g)
			if g, gok, err = got.next(); err != nil {
				return err
			}
		default:
			if err := h.both(&w, &g); err != nil {
				return err
			}
			if w, wok, err = want.next(); err != nil {
				return err
			}
			if g, gok, err = got.next(); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

// unionHead is the next unread entry of one branch's list.
type unionHead struct {
	rank int
	disk string
	e    BaselineEntry
	it   *listStream
}

type unionHeap []*unionHead

func (h unionHeap) Len() int { return len(h) }
func (h unionHeap) Less(i, j int) bool {
	if c := comparePaths(h[i].e.name(), h[j].e.name()); c != 0 {
		return c < 0
	}
	return h[i].rank < h[j].rank
}
func (h unionHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *unionHeap) Push(x any)   { *h = append(*h, x.(*unionHead)) }
func (h *unionHeap) Pop() any {
	old := *h
	v := old[len(old)-1]
	*h = old[:len(old)-1]
	return v
}

// unionStream is the entries of every branch as the pool shows them: a path on
// several branches once, as the first branch has it, which onDup is told of.
type unionStream struct {
	h     unionHeap
	all   []*listStream
	onDup func(DuplicatePath)
}

func openUnion(branches []string, lists map[string]string, onDup func(DuplicatePath)) (*unionStream, error) {
	u := &unionStream{onDup: onDup}
	for rank, b := range branches {
		it, err := openList(lists[b])
		if err != nil {
			u.close()
			return nil, err
		}
		u.all = append(u.all, it)
		e, ok, err := it.next()
		if err != nil {
			u.close()
			return nil, err
		}
		if ok {
			u.h = append(u.h, &unionHead{rank: rank, disk: b, e: e, it: it})
		}
	}
	heap.Init(&u.h)
	return u, nil
}

func (u *unionStream) close() {
	for _, it := range u.all {
		it.close()
	}
}

func (u *unionStream) next() (BaselineEntry, bool, error) {
	if u.h.Len() == 0 {
		return BaselineEntry{}, false, nil
	}
	first := heap.Pop(&u.h).(*unionHead)
	same := []*unionHead{first}
	for u.h.Len() > 0 && comparePaths(u.h[0].e.name(), first.e.name()) == 0 {
		same = append(same, heap.Pop(&u.h).(*unionHead))
	}
	winner := first.e
	if len(same) > 1 {
		d := DuplicatePath{Path: winner.name()}
		for _, hd := range same {
			d.Disks = append(d.Disks, hd.disk)
		}
		sort.Strings(d.Disks)
		u.onDup(d)
	}
	for _, hd := range same {
		e, ok, err := hd.it.next()
		if err != nil {
			return BaselineEntry{}, false, err
		}
		if ok {
			hd.e = e
			heap.Push(&u.h, hd)
		}
	}
	return winner, true, nil
}

// splitByDisk writes the baseline's entries into one list per disk in dir and
// returns each disk's list file. A disk's entries are contiguous in the
// baseline; a disk that appears twice is a baseline that is not whole.
func (b *BaselineReader) splitByDisk(ctx context.Context, dir string) (map[string]string, error) {
	lists := map[string]string{}
	var cur string
	var w *entryWriter
	closeCur := func() error {
		if w == nil {
			return nil
		}
		err := w.close()
		w = nil
		return err
	}
	var n int64
	err := readRecords(b.path, func(r record) error {
		switch r.K {
		case KindFile, KindSymlink, KindSpecial:
		default:
			return nil
		}
		if n++; n%50000 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if w == nil || r.D != cur {
			if err := closeCur(); err != nil {
				return err
			}
			if _, seen := lists[r.D]; seen {
				return fmt.Errorf("the entries of %q are not together in the baseline", r.D)
			}
			path := filepath.Join(dir, fmt.Sprintf("base-%d.list", len(lists)))
			nw, err := newEntryWriter(path)
			if err != nil {
				return err
			}
			w, cur, lists[r.D] = nw, r.D, path
		}
		return w.write(r)
	})
	if cerr := closeCur(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	return lists, nil
}
