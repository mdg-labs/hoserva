package backup

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"
)

// AppdataPreviewSampleLimit is how many paths one group of a preview lists.
const AppdataPreviewSampleLimit = 20

// AppdataPreviewGroup is the files of one kind in one directory: how many,
// their total size, and the first Sample paths in path order, relative to
// the directory. Symbolic links and other non-directory entries count as
// files.
type AppdataPreviewGroup struct {
	Files  int64
	Bytes  int64
	Sample []string
}

func (g *AppdataPreviewGroup) add(rel string, size int64) {
	g.Files++
	g.Bytes += size
	if len(g.Sample) < AppdataPreviewSampleLimit {
		g.Sample = append(g.Sample, rel)
	}
}

// AppdataDirPreview is what restoring would do to one archived directory.
// The restore replaces the directory as a whole, so a file in both is
// replaced, one only in the archive is added, and one only in the live
// directory is removed. Replaced and Removed count the live files that
// would be lost; Added counts the archive's files.
type AppdataDirPreview struct {
	// Directory is relative to the appdata location.
	Directory string
	Replaced  AppdataPreviewGroup
	Added     AppdataPreviewGroup
	Removed   AppdataPreviewGroup
}

// AppdataRestorePreview is what AppdataService.PreviewRestore reports.
type AppdataRestorePreview struct {
	Container     string
	Archive       string
	DestinationID string
	CreatedAt     time.Time
	Directories   []AppdataDirPreview
}

// appdataPreviewStagingDir is where a preview fetches its archive, next to
// the appdata location like a restore's staging directory, but apart from
// it: a backup or restore of another container clears its own directory
// when it starts, and a preview must not lose its archive to that, nor
// clear theirs.
const appdataPreviewStagingDir = ".hoserva-preview-staging"

// AppdataPreviewKeep is how many finished previews are held for their
// caller to fetch; the oldest is dropped past it.
const AppdataPreviewKeep = 16

// appdataPreviews is the state the restore previews share: which staging
// directories are in use, and the results of finished previews by job id.
// A result is held in memory only: it is cheap to compute again, and holds
// nothing a restart needs.
type appdataPreviews struct {
	mu      sync.Mutex
	active  map[string]struct{}
	order   []string
	results map[string]AppdataRestorePreview
}

// stage returns a fresh staging directory for one preview and a function
// that removes it. Directories an earlier daemon left behind are removed
// first; those of previews running now are not.
func (p *appdataPreviews) stage(roots []string) (string, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	base := filepath.Join(filepath.Dir(roots[0]), appdataPreviewStagingDir)
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", nil, fmt.Errorf("creating %s: %w", base, err)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", nil, fmt.Errorf("reading %s: %w", base, err)
	}
	for _, e := range entries {
		if _, running := p.active[e.Name()]; !running {
			if err := os.RemoveAll(filepath.Join(base, e.Name())); err != nil {
				return "", nil, fmt.Errorf("clearing %s: %w", filepath.Join(base, e.Name()), err)
			}
		}
	}
	dir, err := os.MkdirTemp(base, "preview-")
	if err != nil {
		return "", nil, fmt.Errorf("creating a staging directory in %s: %w", base, err)
	}
	if p.active == nil {
		p.active = map[string]struct{}{}
	}
	p.active[filepath.Base(dir)] = struct{}{}
	return dir, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		_ = os.RemoveAll(dir)
		delete(p.active, filepath.Base(dir))
		if len(p.active) == 0 {
			_ = os.Remove(base)
		}
	}, nil
}

func (p *appdataPreviews) put(id string, r AppdataRestorePreview) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.results == nil {
		p.results = map[string]AppdataRestorePreview{}
	}
	if _, held := p.results[id]; !held {
		p.order = append(p.order, id)
	}
	p.results[id] = r
	for len(p.order) > AppdataPreviewKeep {
		delete(p.results, p.order[0])
		p.order = p.order[1:]
	}
}

// Preview returns the finished preview a RunPreview stored under a job id.
func (a *AppdataService) Preview(id string) (AppdataRestorePreview, bool) {
	a.previews.mu.Lock()
	defer a.previews.mu.Unlock()
	r, ok := a.previews.results[id]
	return r, ok
}

// RunPreview is the body of an appdata_restore_preview job: it previews the
// restore for req and holds the result under the job's id for Preview.
func (a *AppdataService) RunPreview(ctx context.Context, id string, req AppdataRestoreRequest, out io.Writer) error {
	_, _ = fmt.Fprintf(out, "reading %s from the destination\n", req.Archive)
	r, err := a.PreviewRestore(ctx, req)
	if err != nil {
		return err
	}
	a.previews.put(id, r)
	return nil
}

// PreviewRestore reports what Restore would overwrite for the same request
// without changing anything: it stops no container and writes nothing to
// the appdata location. The archive is fetched, decrypted and verified by
// the code the restore uses, into a staging directory of its own that is
// removed before it returns, and refused for the same reasons. The live
// appdata is read only here, on request.
//
// It does not take the lock a backup or restore holds: it changes nothing
// they use, and the job scheduler already runs it behind any backup or
// restore of the same container. A cancelled ctx stops the fetch, the
// reading of the archive and the walk of the live directories, but not the
// decryption or the verification read between them, which take no context.
func (a *AppdataService) PreviewRestore(ctx context.Context, req AppdataRestoreRequest) (AppdataRestorePreview, error) {
	var out AppdataRestorePreview
	if err := a.Containers.RequireArrayRunning(); err != nil {
		return out, err
	}
	source, err := a.checkRestoreRequest(ctx, req)
	if err != nil {
		return out, err
	}
	roots, err := a.appdataRoots(ctx)
	if err != nil {
		return out, err
	}
	if len(roots) == 0 {
		return out, invalidArchivef("the array has no cache disk, so there is no appdata location to restore into")
	}
	key, err := a.archiveKey()
	if err != nil {
		return out, err
	}
	staging, release, err := a.previews.stage(roots)
	if err != nil {
		return out, err
	}
	defer release()

	plain, hdr, err := a.fetchVerifiedAppdata(ctx, req, source, roots, staging, key)
	if err != nil {
		return out, err
	}
	archived, err := readArchivedFiles(ctx, plain, len(hdr.Dirs))
	if err != nil {
		return out, err
	}
	resolved := resolveRoots(roots)
	out = AppdataRestorePreview{
		Container: hdr.Container, Archive: req.Archive, DestinationID: req.DestinationID, CreatedAt: hdr.CreatedAt,
		Directories: make([]AppdataDirPreview, 0, len(hdr.Dirs)),
	}
	for i, dir := range hdr.Dirs {
		d, err := previewDirectory(ctx, dir, resolved, archived[i])
		if err != nil {
			return AppdataRestorePreview{}, err
		}
		out.Directories = append(out.Directories, d)
	}
	return out, nil
}

// archiveNode is one entry of the tree extraction would build.
type archiveNode struct {
	kind byte
	link string
	size int64
}

// archiveTree is the tree extractAppdata would build for one directory,
// keyed by slash-separated path below it, with every path already resolved
// through the symbolic links the archive created. It exists so the preview
// can refuse an entry layout extraction refuses without unpacking anything.
type archiveTree map[string]archiveNode

// maxArchiveLinks is how many symbolic links resolving one path may follow,
// as many as the operating system follows.
const maxArchiveLinks = 255

// resolveDir returns the path of the directory rel names once every
// symbolic link on the way is followed, the way extraction's own check
// resolves the directory an entry is written into. A link that leaves the
// tree (an absolute target or one climbing above it), a path that is not
// there yet and one that is not a directory are refused, all of which make
// extraction fail.
func (t archiveTree) resolveDir(rel string) (string, error) {
	todo := strings.Split(rel, "/")
	var cur []string
	links := 0
	for len(todo) > 0 {
		c := todo[0]
		todo = todo[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			if len(cur) == 0 {
				return "", errors.New("resolves outside its directory")
			}
			cur = cur[:len(cur)-1]
			continue
		}
		p := strings.Join(append(cur[:len(cur):len(cur)], c), "/")
		n, ok := t[p]
		switch {
		case !ok:
			return "", fmt.Errorf("%s is not there", p)
		case n.kind == tar.TypeSymlink:
			if links++; links > maxArchiveLinks {
				return "", fmt.Errorf("%s is part of a loop of links", p)
			}
			if strings.HasPrefix(n.link, "/") {
				return "", fmt.Errorf("%s resolves outside its directory", p)
			}
			todo = append(strings.Split(n.link, "/"), todo...)
		case n.kind == tar.TypeDir:
			cur = append(cur, c)
		default:
			return "", fmt.Errorf("%s is not a directory", p)
		}
	}
	return strings.Join(cur, "/"), nil
}

// readArchivedFiles lists the non-directory entries of each of the
// archive's n trees with their sizes, keyed by the slash-separated path
// they are unpacked to, relative to the tree. It refuses what extraction
// refuses: a path that escapes its tree, an unsupported entry type, an
// entry whose place is already taken, one written below anything but a
// directory the archive listed earlier, or through a link that leaves the
// tree. A restore the preview lets through is then not one that fails at
// unpacking, after it stopped the container and took a snapshot.
func readArchivedFiles(ctx context.Context, archivePath string, n int) ([]map[string]int64, error) {
	in, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("opening archive: %w", err)
	}
	defer func() { _ = in.Close() }()
	zr, err := zstd.NewReader(in)
	if err != nil {
		return nil, fmt.Errorf("creating zstd reader: %w", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)

	trees := make([]archiveTree, n)
	for i := range trees {
		trees[i] = archiveTree{}
	}
	for {
		e, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading archive: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e.Name == appdataHeaderName || e.Name == appdataTrailerName {
			continue
		}
		i, rel, ok := appdataEntryDir(e.Name, n)
		if !ok {
			return nil, invalidArchivef("archive entry %q is outside its directories", e.Name)
		}
		rel = path.Clean(strings.TrimSuffix(rel, "/"))
		if rel == "." || rel == "" {
			if e.Typeflag != tar.TypeDir {
				return nil, invalidArchivef("archive entry %q is not a directory", e.Name)
			}
			continue
		}
		if !filepath.IsLocal(filepath.FromSlash(rel)) {
			return nil, invalidArchivef("archive entry %q escapes its directory", e.Name)
		}
		parent, err := trees[i].resolveDir(path.Dir(rel))
		if err != nil {
			return nil, invalidArchivef("archive entry %q: %v", e.Name, err)
		}
		key := path.Join(parent, path.Base(rel))
		if _, taken := trees[i][key]; taken {
			return nil, invalidArchivef("archive entry %q is unpacked to %s, which an earlier entry already made", e.Name, key)
		}
		switch e.Typeflag {
		case tar.TypeDir:
			trees[i][key] = archiveNode{kind: tar.TypeDir}
		case tar.TypeReg:
			trees[i][key] = archiveNode{kind: tar.TypeReg, size: e.Size}
		case tar.TypeSymlink:
			if e.Linkname == "" {
				return nil, invalidArchivef("archive entry %q is a link to nothing", e.Name)
			}
			trees[i][key] = archiveNode{kind: tar.TypeSymlink, link: e.Linkname}
		default:
			return nil, invalidArchivef("archive entry %q has an unsupported type", e.Name)
		}
	}
	files := make([]map[string]int64, n)
	for i, t := range trees {
		files[i] = map[string]int64{}
		for key, node := range t {
			if node.kind != tar.TypeDir {
				files[i][key] = node.size
			}
		}
	}
	return files, nil
}

// previewDirectory compares one live directory with the archived files
// that would replace it. archived is consumed.
func previewDirectory(ctx context.Context, dir string, roots []string, archived map[string]int64) (AppdataDirPreview, error) {
	var out AppdataDirPreview
	shown, err := relativeToRoot(dir, roots)
	if err != nil {
		return out, err
	}
	out.Directory = shown

	w := appdataWalk{
		ctx: ctx,
		dir: func(string, int) error { return nil },
		entry: func(_ int, _, rel string, st *unix.Stat_t) error {
			var size int64
			if st.Mode&unix.S_IFMT == unix.S_IFREG {
				size = st.Size
			}
			if _, ok := archived[rel]; ok {
				delete(archived, rel)
				out.Replaced.add(rel, size)
			} else {
				out.Removed.add(rel, size)
			}
			return nil
		},
		// A file that goes away while walking is not a file the restore
		// would replace.
		gone: func() {},
	}
	// A directory that is not there yet has nothing to lose; anything else
	// that stops the walk means the comparison is unknown.
	if err := w.walk(dir); err != nil && !errors.Is(err, errWalkRootMissing) {
		return AppdataDirPreview{}, fmt.Errorf("reading %s: %w", dir, err)
	}

	added := make([]string, 0, len(archived))
	for rel := range archived {
		added = append(added, rel)
	}
	sort.Strings(added)
	for _, rel := range added {
		out.Added.add(rel, archived[rel])
	}
	return out, nil
}

// relativeToRoot names dir by its path below the appdata location that
// holds it, so a preview does not disclose where that location is mounted.
func relativeToRoot(dir string, roots []string) (string, error) {
	for _, r := range roots {
		if withinDir(dir, r) && dir != r {
			rel, err := filepath.Rel(r, dir)
			if err != nil {
				return "", err
			}
			return filepath.ToSlash(rel), nil
		}
	}
	return "", invalidArchivef("%s is not inside the appdata location", dir)
}
