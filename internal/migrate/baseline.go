package migrate

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"container/heap"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// The verify baseline (doc 05 §3): what the data disks held before the import,
// recorded by a metadata walk of each disk plus content hashes of a sample, for
// the verify phase to compare against. It is a file beside the session's row,
// not a table: it has a row per file, which on a real array is millions, and the
// database's config archive must not carry it.
//
// The file is gzip-compressed JSON lines: a header, then every disk's entries in
// walk order, then every path that exists on more than one disk, then a trailer
// with the entry count. A file with no trailer was cut short and is never read.

const (
	baselineFormat = 1
	// baselinePrefix names a baseline file in the session's directory.
	baselinePrefix = "baseline-"
	baselineSuffix = ".jsonl.gz"
	// tmpPrefix names the directory a scan keeps its per-disk entry lists in
	// until it ends.
	tmpPrefix = "tmp-"

	// DefaultSmallFileBytes is the size up to which every file is hashed.
	DefaultSmallFileBytes = 1 << 20
	// DefaultLargeFraction is the share of larger files hashed, as 1 in N.
	defaultLargeEvery = 100
	// DefaultMinLarge is the least number of larger files hashed per disk, or
	// all of them when the disk has fewer.
	DefaultMinLarge = 200

	maxListedDuplicates = 20
)

// Entry kinds.
const (
	KindFile    = "file"
	KindSymlink = "symlink"
	KindSpecial = "special"
)

// SampleRule is how the files whose content is hashed were chosen. It is stored
// with the baseline so verify and a reader know what the sample is.
type SampleRule struct {
	// Full is true when every file was hashed.
	Full bool `json:"full"`
	// SmallFileBytes: every regular file of at most this many bytes is hashed.
	SmallFileBytes int64 `json:"smallFileBytes"`
	// LargeEvery and MinLarge: of the larger files on a disk, those whose
	// path hash falls in the lowest 1 in LargeEvery, and at least MinLarge of
	// them (all, when the disk has fewer), are hashed.
	LargeEvery int `json:"largeEvery"`
	MinLarge   int `json:"minLarge"`
	// Selection says how a larger file is chosen, in words.
	Selection string `json:"selection"`
}

func sampleRule(full bool) SampleRule {
	return SampleRule{
		Full: full, SmallFileBytes: DefaultSmallFileBytes, LargeEvery: defaultLargeEvery, MinLarge: DefaultMinLarge,
		Selection: "a larger file is hashed when the first 8 bytes of the sha256 of its path from the disk's root, read big-endian, are among the lowest 1 in LargeEvery of the larger files on its disk, or among the lowest MinLarge of them",
	}
}

// Describe is the rule as the report says it.
func (r SampleRule) Describe() string {
	if r.Full {
		return "every file is hashed (a full-checksum scan)"
	}
	return fmt.Sprintf("every file of %s or less is hashed, plus a deterministic 1 in %d (at least %d) of the larger files on each disk, chosen by a stable hash of the path", formatBytes(r.SmallFileBytes), r.LargeEvery, r.MinLarge)
}

// ShareTotals counts what a disk holds under one top-level directory.
type ShareTotals struct {
	Share    string `json:"share"`
	Files    int64  `json:"files"`
	Symlinks int64  `json:"symlinks"`
	Special  int64  `json:"special"`
	// Bytes is the sum of the sizes of the regular files.
	Bytes int64 `json:"bytes"`
}

func (t *ShareTotals) add(e *BaselineEntry) {
	switch e.Kind {
	case KindFile:
		t.Files++
		t.Bytes += e.Size
	case KindSymlink:
		t.Symlinks++
	default:
		t.Special++
	}
}

// DiskBaseline is one disk's totals.
type DiskBaseline struct {
	Slot       string `json:"slot"`
	Filesystem string `json:"filesystem"`
	Files      int64  `json:"files"`
	Symlinks   int64  `json:"symlinks"`
	Special    int64  `json:"special"`
	Bytes      int64  `json:"bytes"`
	// Hashed and HashedBytes count the files whose content was hashed.
	Hashed      int64 `json:"hashed"`
	HashedBytes int64 `json:"hashedBytes"`
	// RootEntries counts what sits directly in the disk's root outside any
	// top-level directory: recorded, but belonging to no share.
	RootEntries int64         `json:"rootEntries"`
	Shares      []ShareTotals `json:"shares"`
	// HiddenSkipped names the hidden top-level directories left out
	// (.Trash-* and the like): Unraid does not treat them as shares.
	HiddenSkipped []string `json:"hiddenSkipped,omitempty"`
}

// DuplicatePath is a path that exists on more than one data disk.
type DuplicatePath struct {
	Path  string   `json:"path"`
	Disks []string `json:"disks"`
}

// BaselineSummary is what the report keeps of the baseline: the file it is in,
// the sample rule, and every disk's and share's totals. The entries are in the
// file.
type BaselineSummary struct {
	// File is the baseline file's name in the session's directory.
	File  string         `json:"file"`
	Rule  SampleRule     `json:"rule"`
	Disks []DiskBaseline `json:"disks"`
	// Duplicates is how many paths exist on more than one disk, and
	// DuplicateSample the first of them; the file lists every one.
	Duplicates      int64           `json:"duplicates"`
	DuplicateSample []DuplicatePath `json:"duplicateSample,omitempty"`
}

// ShareBytes returns what the baseline counted under the share on every disk,
// and the disks that hold it.
func (b *BaselineSummary) ShareBytes(share string) (bytes int64, disks map[string]int64) {
	disks = map[string]int64{}
	for _, d := range b.Disks {
		for _, s := range d.Shares {
			if s.Share == share {
				bytes += s.Bytes
				disks[d.Slot] += s.Bytes
			}
		}
	}
	return bytes, disks
}

// BaselineEntry is one recorded file, symlink or special file.
type BaselineEntry struct {
	Disk string
	// Path is the slash-separated path from the disk's root.
	Path string
	// RawPath is set instead of Path when the name is not valid UTF-8.
	RawPath []byte
	Kind    string
	Size    int64
	// SHA256 is set for a file whose content was hashed.
	SHA256 string
	// Target is a symlink's target, read and never followed. RawTarget is set
	// instead when it is not valid UTF-8.
	Target    string
	RawTarget []byte
	// Special is the type of a special file: fifo, socket, chardev, blockdev.
	Special string
}

// name is the path as a Go string, whatever its encoding.
func (e *BaselineEntry) name() string {
	if e.RawPath != nil {
		return string(e.RawPath)
	}
	return e.Path
}

// Share is the top-level directory the entry is under, "" for an entry directly
// in the disk's root.
func (e *BaselineEntry) Share() string {
	n := e.name()
	if i := strings.IndexByte(n, '/'); i >= 0 {
		return n[:i]
	}
	return ""
}

// record is the JSON line an entry is written as. A name that is not valid UTF-8
// is written base64-encoded, because JSON would replace its bytes.
type record struct {
	K  string   `json:"k"`
	D  string   `json:"d,omitempty"`
	P  string   `json:"p,omitempty"`
	PB string   `json:"pb,omitempty"`
	S  int64    `json:"s,omitempty"`
	H  string   `json:"h,omitempty"`
	T  string   `json:"t,omitempty"`
	TB string   `json:"tb,omitempty"`
	X  string   `json:"x,omitempty"`
	Ds []string `json:"ds,omitempty"`

	Format int         `json:"format,omitempty"`
	Rule   *SampleRule `json:"rule,omitempty"`
	N      int64       `json:"n,omitempty"`
}

func encodeName(s string) (plain, raw string) {
	if utf8.ValidString(s) {
		return s, ""
	}
	return "", base64.StdEncoding.EncodeToString([]byte(s))
}

func decodeName(plain, raw string) (string, []byte, error) {
	if raw == "" {
		return plain, nil, nil
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", nil, err
	}
	return string(b), b, nil
}

func (e *BaselineEntry) record() record {
	r := record{K: e.Kind, D: e.Disk, S: e.Size, H: e.SHA256, X: e.Special}
	r.P, r.PB = encodeName(e.name())
	if e.RawTarget != nil {
		r.TB = base64.StdEncoding.EncodeToString(e.RawTarget)
	} else {
		r.T, r.TB = encodeName(e.Target)
	}
	return r
}

func (r record) entry() (BaselineEntry, error) {
	name, rawName, err := decodeName(r.P, r.PB)
	if err != nil {
		return BaselineEntry{}, err
	}
	target, rawTarget, err := decodeName(r.T, r.TB)
	if err != nil {
		return BaselineEntry{}, err
	}
	e := BaselineEntry{Disk: r.D, Kind: r.K, Size: r.S, SHA256: r.H, Special: r.X, Target: target, RawTarget: rawTarget}
	if rawName != nil {
		e.RawPath = rawName
	} else {
		e.Path = name
	}
	return e, nil
}

// pathKey is the number a larger file's sampling is decided by.
func pathKey(path string) uint64 {
	sum := sha256.Sum256([]byte(path))
	return binary.BigEndian.Uint64(sum[:8])
}

// comparePaths orders two slash-separated paths the way a walk that reads every
// directory in name order visits them: component by component, a path before
// what is under it. The walk of every disk is in this order, so the disks'
// entries merge in it.
func comparePaths(a, b string) int {
	for {
		ca, ra, moreA := strings.Cut(a, "/")
		cb, rb, moreB := strings.Cut(b, "/")
		if c := strings.Compare(ca, cb); c != 0 {
			return c
		}
		switch {
		case !moreA && !moreB:
			return 0
		case !moreA:
			return -1
		case !moreB:
			return 1
		}
		a, b = ra, rb
	}
}

// ctxReader fails a read once ctx is done, so hashing a large file stops when a
// scan is cancelled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// hashFile returns the sha256 of the file at path.
func hashFile(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, ctxReader{ctx: ctx, r: f}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// entryWriter writes a compressed list of records: a scan's per-disk list of
// entries, which the hash pass and the duplicate merge read back, or the
// baseline file itself.
type entryWriter struct {
	f  *os.File
	gz *gzip.Writer
	bw *bufio.Writer
	n  int64
}

func newEntryWriter(path string) (*entryWriter, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	gz := gzip.NewWriter(f)
	return &entryWriter{f: f, gz: gz, bw: bufio.NewWriterSize(gz, 1<<20)}, nil
}

func (w *entryWriter) write(r record) error {
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if _, err := w.bw.Write(line); err != nil {
		return err
	}
	w.n++
	return nil
}

func (w *entryWriter) close() error {
	err := w.bw.Flush()
	if cerr := w.gz.Close(); err == nil {
		err = cerr
	}
	if cerr := w.f.Sync(); err == nil {
		err = cerr
	}
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// recordReader reads a gzip JSON-lines file one record at a time.
type recordReader struct {
	f  *os.File
	gz *gzip.Reader
	br *bufio.Reader
}

func openRecords(path string) (*recordReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &recordReader{f: f, gz: gz, br: bufio.NewReaderSize(gz, 1<<20)}, nil
}

func (rr *recordReader) close() {
	_ = rr.gz.Close()
	_ = rr.f.Close()
}

// next returns the next record, and false at the end of the file. A file that
// ends inside a record, or inside the compressed stream, is an error.
func (rr *recordReader) next() (record, bool, error) {
	line, err := rr.br.ReadBytes('\n')
	if errors.Is(err, io.EOF) && len(line) == 0 {
		return record{}, false, nil
	}
	if err != nil {
		return record{}, false, err
	}
	var r record
	if uerr := json.Unmarshal(bytes.TrimSuffix(line, []byte("\n")), &r); uerr != nil {
		return record{}, false, fmt.Errorf("a line is not a record: %w", uerr)
	}
	return r, true, nil
}

// readRecords calls fn for each line of the gzip JSON-lines file at path, in
// order. fn returning an error stops the read.
func readRecords(path string, fn func(record) error) error {
	rr, err := openRecords(path)
	if err != nil {
		return err
	}
	defer rr.close()
	for {
		r, ok, err := rr.next()
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if err := fn(r); err != nil {
			return err
		}
	}
}

// lowestKeys is a max-heap of the k lowest keys seen.
type lowestKeys []uint64

func (h lowestKeys) Len() int           { return len(h) }
func (h lowestKeys) Less(i, j int) bool { return h[i] > h[j] }
func (h lowestKeys) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *lowestKeys) Push(x any)        { *h = append(*h, x.(uint64)) }
func (h *lowestKeys) Pop() any {
	old := *h
	v := old[len(old)-1]
	*h = old[:len(old)-1]
	return v
}

// sampleCount is how many of nLarge larger files are hashed: 1 in every, and at
// least minLarge, or all of them when there are fewer.
func sampleCount(nLarge int64, every, minLarge int) int64 {
	k := (nLarge + int64(every) - 1) / int64(every)
	if k < int64(minLarge) {
		k = int64(minLarge)
	}
	return min(k, nLarge)
}

// sampleThreshold returns the key at or below which a larger file is hashed:
// the k-th lowest key of those in list, k being sampleCount of them. A disk
// with no larger file has no threshold.
func sampleThreshold(list string, rule SampleRule) (threshold uint64, found bool, err error) {
	var nLarge int64
	err = readRecords(list, func(r record) error {
		if r.K == KindFile && r.S > rule.SmallFileBytes {
			nLarge++
		}
		return nil
	})
	if err != nil || nLarge == 0 {
		return 0, false, err
	}
	k := sampleCount(nLarge, rule.LargeEvery, rule.MinLarge)
	h := make(lowestKeys, 0, int(k))
	err = readRecords(list, func(r record) error {
		if r.K != KindFile || r.S <= rule.SmallFileBytes {
			return nil
		}
		name, _, derr := decodeName(r.P, r.PB)
		if derr != nil {
			return derr
		}
		key := pathKey(name)
		switch {
		case int64(len(h)) < k:
			heap.Push(&h, key)
		case key < h[0]:
			h[0] = key
			heap.Fix(&h, 0)
		}
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	return h[0], true, nil
}

// baselineFile writes the baseline file of one scan. It is written under a name
// no reader opens and renamed to its own only once complete, so a scan that
// fails or is cancelled leaves nothing a reader could take for a baseline.
type baselineFile struct {
	dir     string
	name    string
	partial string
	w       *entryWriter
	rule    SampleRule
}

func newBaselineFile(dir, token string, rule SampleRule) (*baselineFile, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	b := &baselineFile{dir: dir, name: baselinePrefix + token + baselineSuffix, rule: rule}
	b.partial = filepath.Join(dir, tmpPrefix+b.name)
	w, err := newEntryWriter(b.partial)
	if err != nil {
		return nil, err
	}
	b.w = w
	if err := w.write(record{K: "header", Format: baselineFormat, Rule: &rule}); err != nil {
		b.abort()
		return nil, err
	}
	return b, nil
}

func (b *baselineFile) duplicate(d DuplicatePath) error {
	p, pb := encodeName(d.Path)
	return b.w.write(record{K: "dup", P: p, PB: pb, Ds: d.Disks})
}

// finish writes the trailer and publishes the file under its own name.
func (b *baselineFile) finish() error {
	if err := b.w.write(record{K: "end", N: b.w.n}); err != nil {
		b.abort()
		return err
	}
	if err := b.w.close(); err != nil {
		_ = os.Remove(b.partial)
		return err
	}
	if err := os.Rename(b.partial, filepath.Join(b.dir, b.name)); err != nil {
		_ = os.Remove(b.partial)
		return err
	}
	return syncDir(b.dir)
}

// abort drops the file of a scan that did not finish.
func (b *baselineFile) abort() {
	_ = b.w.close()
	_ = os.Remove(b.partial)
}

// BaselineReader reads a baseline file.
type BaselineReader struct {
	path string
	Rule SampleRule
}

// ErrNoBaseline is returned when the session has no baseline to read: no scan
// recorded one, or its file is not on this machine.
var ErrNoBaseline = errors.New("there is no migration baseline")

// OpenBaseline opens the baseline file at path, refusing one that is not whole.
func OpenBaseline(path string) (*BaselineReader, error) {
	br := &BaselineReader{path: path}
	var header, end bool
	var n, seen int64
	err := readRecords(path, func(r record) error {
		switch r.K {
		case "header":
			if r.Format != baselineFormat || r.Rule == nil {
				return fmt.Errorf("the baseline is in format %d, not %d", r.Format, baselineFormat)
			}
			header = true
			br.Rule = *r.Rule
		case "end":
			end, n = true, r.N
		default:
			if end {
				return errors.New("a record follows the trailer")
			}
			seen++
		}
		return nil
	})
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("%w: %w", ErrNoBaseline, err)
	case err != nil:
		return nil, fmt.Errorf("reading the baseline: %w", err)
	case !header || !end:
		return nil, errors.New("the baseline is cut short: it has no header or no trailer")
	case n != seen+1:
		return nil, fmt.Errorf("the baseline's trailer counts %d records and the file holds %d", n, seen)
	}
	return br, nil
}

// Each calls fn for every entry of the baseline, in the order they were
// recorded: each disk's, in walk order.
func (b *BaselineReader) Each(fn func(BaselineEntry) error) error {
	return readRecords(b.path, func(r record) error {
		switch r.K {
		case KindFile, KindSymlink, KindSpecial:
			e, err := r.entry()
			if err != nil {
				return err
			}
			return fn(e)
		}
		return nil
	})
}

// Duplicates calls fn for every path the baseline records on more than one
// disk.
func (b *BaselineReader) Duplicates(fn func(DuplicatePath) error) error {
	return readRecords(b.path, func(r record) error {
		if r.K != "dup" {
			return nil
		}
		name, _, err := decodeName(r.P, r.PB)
		if err != nil {
			return err
		}
		return fn(DuplicatePath{Path: name, Disks: r.Ds})
	})
}
