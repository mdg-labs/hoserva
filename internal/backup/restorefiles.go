package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// The categories of files an in-place or bare-metal restore puts back
// besides the database (doc 10 §1). Each is restored so that it matches the
// archive afterwards. The archive's generated/ files are not among them:
// every managed config file is regenerated from the restored database (D4),
// so restoring generated/ would only let a stale file win.
const (
	FilesCustomConfig = "custom_config"
	FilesTemplates    = "templates"
	FilesStacks       = "stacks"
)

var filesCategories = []string{FilesCustomConfig, FilesTemplates, FilesStacks}

var filesCategoryLabels = map[string]string{
	FilesCustomConfig: "custom config files",
	FilesTemplates:    "app templates",
	FilesStacks:       "app stacks",
}

// stackFileNames are the files of a stack the archive carries and a restore
// manages; every other file in a stack's directory is never touched. Its
// .env is written only when the caller passes WithStackEnvs.
var stackFileNames = []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml", "meta.json"}

// FileChanges is what restoring one category replaces (in the archive and on
// disk, with different content), adds (in the archive only) and removes (on
// disk only). Names are slash-separated paths relative to the category's
// directory, sorted.
type FileChanges struct {
	Category string
	Replaced []string
	Added    []string
	Removed  []string
}

func (c FileChanges) empty() bool {
	return len(c.Replaced)+len(c.Added)+len(c.Removed) == 0
}

// UnsafeRestorePathError is a refusal to restore files: a destination that
// is a symbolic link or otherwise not what it must be, or an archive path
// that is not one of the files its category holds. Nothing has been written
// when it is returned.
type UnsafeRestorePathError struct {
	Path   string
	Reason string
}

func (e *UnsafeRestorePathError) Error() string {
	return fmt.Sprintf("restoring files: %s %s", e.Path, e.Reason)
}

// ApplyError is StagedFiles.Apply's failure. Every category it names is
// restored, left as it was, or, only when putting it back failed too,
// partly restored, in which case the files as they were are under SavedAt.
type ApplyError struct {
	Restored      []string
	Unchanged     []string
	Indeterminate []string
	SavedAt       []string
	Cause         error
}

func (e *ApplyError) Error() string {
	msg := fmt.Sprintf("restoring files failed: %v", e.Cause)
	if len(e.Restored) > 0 {
		msg += "; restored: " + labelList(e.Restored)
	}
	if len(e.Unchanged) > 0 {
		msg += "; left as they were: " + labelList(e.Unchanged)
	}
	if len(e.Indeterminate) > 0 {
		msg += fmt.Sprintf("; could not be put back and are partly restored: %s (the files as they were are in %s)",
			labelList(e.Indeterminate), strings.Join(e.SavedAt, ", "))
	}
	return msg
}

func (e *ApplyError) Unwrap() error { return e.Cause }

func labelList(categories []string) string {
	labels := make([]string, len(categories))
	for i, c := range categories {
		labels[i] = filesCategoryLabels[c]
	}
	return strings.Join(labels, ", ")
}

// FilesOption configures PlanFiles, StageFiles and RestoreFiles.
type FilesOption func(*filesConfig)

type filesConfig struct {
	stackEnvs   []StackEnv
	beforeStage func(dest string) error
	afterStep   func(category string, step int) error
}

// WithStackEnvs also restores each of envs into StacksDir/<stack>/.env, mode
// 0600, for the stacks the archive holds files of; an entry for any other
// stack is ignored, and every other .env is left as it is. The files are
// staged and swapped in with the stacks category, so a failure leaves every
// .env and every stack file wholly as it was.
func WithStackEnvs(envs []StackEnv) FilesOption {
	return func(c *filesConfig) { c.stackEnvs = envs }
}

func newFilesConfig(opts []FilesOption) filesConfig {
	var cfg filesConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// archivedFile is a file to restore: one extracted from the archive tree at
// src, or, for a stack .env, the body decrypted from secrets.age.
type archivedFile struct {
	src  string
	body []byte
	sum  string
}

type liveFile struct {
	sum  string
	mode fs.FileMode
}

type categoryPlan struct {
	category string
	root     string
	archive  map[string]archivedFile
	live     map[string]liveFile
	changes  FileChanges
	// envs are the paths, in changes.Added or changes.Replaced, that are a
	// stack's .env; Changes reports them apart from the stack's files.
	envs []string
}

// PlanFiles reports what RestoreFiles would do with the verified archive
// tree and paths, changing nothing. It is the check a preview and an import
// share: an *UnsafeRestorePathError is a restore that would be refused. The
// result always has the three categories, in a fixed order.
func PlanFiles(tree string, paths Paths, opts ...FilesOption) ([]FileChanges, error) {
	plans, err := buildPlans(tree, paths, newFilesConfig(opts).stackEnvs)
	if err != nil {
		return nil, err
	}
	return plansChanges(plans), nil
}

func plansChanges(plans []categoryPlan) []FileChanges {
	out := make([]FileChanges, len(plans))
	for i, p := range plans {
		out[i] = p.changes
		if len(p.envs) > 0 {
			out[i].Added = withoutAny(p.changes.Added, p.envs)
			out[i].Replaced = withoutAny(p.changes.Replaced, p.envs)
		}
	}
	return out
}

func withoutAny(names, drop []string) []string {
	out := []string{}
	for _, n := range names {
		if !slices.Contains(drop, n) {
			out = append(out, n)
		}
	}
	return out
}

// RestoreFiles puts the archive's custom/ files back into paths.ConfigRoot
// (only *.custom.conf files), its templates/ into paths.TemplatesDir and its
// stacks/<name>/ compose and meta.json files into paths.StacksDir, from
// tree, an archive already extracted and verified by ExtractVerifiedArchive.
// Afterwards each category matches the archive: files the archive lacks are
// removed. It never touches a stack's .env unless WithStackEnvs is given,
// and then only the .env of a stack it names, any other file of StacksDir or
// ConfigRoot, appdata, or a path outside those three directories. It returns
// what it replaced, added and removed per category, a stack's .env not
// among them.
//
// It is StageFiles followed by StagedFiles.Apply. A caller that must write
// nothing until something else has succeeded stages first, does that other
// work, and applies last.
func RestoreFiles(ctx context.Context, tree string, paths Paths, opts ...FilesOption) ([]FileChanges, error) {
	staged, err := StageFiles(ctx, tree, paths, opts...)
	if err != nil {
		return nil, err
	}
	defer staged.Discard()
	if err := staged.Apply(); err != nil {
		return nil, err
	}
	return staged.Changes(), nil
}

// StagedFiles is every file RestoreFiles will write, already copied beside
// its runtime directory and checked against the archive manifest.
type StagedFiles struct {
	plans []categoryPlan
	cats  []*stagedCategory
	cfg   filesConfig
}

type stagedCategory struct {
	plan     categoryPlan
	stageDir string
	keep     bool
}

// StageFiles does everything RestoreFiles does except change a runtime
// directory. Each file the restore will write is copied from tree into a
// directory beside its category's own, on the same filesystem, and its
// checksum is compared with the manifest's as it is copied, so a file
// changed after verification is never written. On an error nothing has
// changed and the staging directories are removed.
func StageFiles(ctx context.Context, tree string, paths Paths, opts ...FilesOption) (*StagedFiles, error) {
	cfg := newFilesConfig(opts)
	plans, err := buildPlans(tree, paths, cfg.stackEnvs)
	if err != nil {
		return nil, err
	}
	s := &StagedFiles{plans: plans, cfg: cfg}
	for _, p := range plans {
		if p.changes.empty() {
			continue
		}
		sc := &stagedCategory{plan: p}
		s.cats = append(s.cats, sc)
		if err := sc.stage(ctx, cfg); err != nil {
			s.Discard()
			return nil, fmt.Errorf("staging %s: %w", filesCategoryLabels[p.category], err)
		}
	}
	return s, nil
}

// Changes is what Apply replaces, adds and removes, per category. A stack's
// .env is not among the files; StackEnvChanges has those.
func (s *StagedFiles) Changes() []FileChanges { return plansChanges(s.plans) }

// EnvChanges names the stacks whose .env Apply adds and replaces. A stack
// whose .env already holds what the archive does is in neither.
type EnvChanges struct {
	Added    []string
	Replaced []string
}

// StackEnvChanges is what Apply does to stack .env files.
func (s *StagedFiles) StackEnvChanges() EnvChanges {
	out := EnvChanges{Added: []string{}, Replaced: []string{}}
	for _, p := range s.plans {
		for _, rel := range p.envs {
			stack := strings.TrimSuffix(rel, "/.env")
			if slices.Contains(p.changes.Added, rel) {
				out.Added = append(out.Added, stack)
			} else {
				out.Replaced = append(out.Replaced, stack)
			}
		}
	}
	return out
}

// Discard removes the staging directories, and with them every file Apply
// moved out of the way: after a successful Apply those are the replaced and
// removed files, gone for good. A category whose own rollback failed keeps
// its directory, since the files as they were are in it.
func (s *StagedFiles) Discard() {
	for _, sc := range s.cats {
		if sc.stageDir != "" && !sc.keep {
			_ = os.RemoveAll(sc.stageDir)
			sc.stageDir = ""
		}
	}
}

// Apply puts the staged files in place, category by category (custom
// config, templates, stacks). It stops at the first category that fails and
// puts that category back exactly as it was, so every category ends wholly
// as it was or wholly as in the archive; the *ApplyError names which. It
// never observes cancellation: once the database has been replaced, half a
// restore is worse than a finished one.
func (s *StagedFiles) Apply() error {
	for i, sc := range s.cats {
		rollbackErr, err := sc.apply(&s.cfg)
		if err == nil {
			continue
		}
		ae := &ApplyError{Cause: err}
		for _, done := range s.cats[:i] {
			ae.Restored = append(ae.Restored, done.plan.category)
		}
		if rollbackErr != nil {
			sc.keep = true
			ae.Cause = errors.Join(err, fmt.Errorf("putting %s back: %w", filesCategoryLabels[sc.plan.category], rollbackErr))
			ae.Indeterminate = append(ae.Indeterminate, sc.plan.category)
			ae.SavedAt = append(ae.SavedAt, sc.stageDir)
		} else {
			ae.Unchanged = append(ae.Unchanged, sc.plan.category)
		}
		for _, rest := range s.cats[i+1:] {
			ae.Unchanged = append(ae.Unchanged, rest.plan.category)
		}
		return ae
	}
	return nil
}

func (sc *stagedCategory) categoryMode() (fileMode, dirMode fs.FileMode) {
	if sc.plan.category == FilesStacks {
		return 0o600, 0o700
	}
	return 0o644, 0o755
}

func (sc *stagedCategory) stage(ctx context.Context, cfg filesConfig) error {
	p := sc.plan
	dir, err := os.MkdirTemp(filepath.Dir(p.root), ".hoserva-restore-*")
	if err != nil {
		return err
	}
	sc.stageDir = dir
	if fi, err := os.Stat(p.root); err == nil {
		if err := sameFilesystem(dir, fi); err != nil {
			return err
		}
	}

	names := slices.Concat(p.changes.Added, p.changes.Replaced)
	if p.category == FilesTemplates {
		names = names[:0]
		for rel := range p.archive {
			names = append(names, rel)
		}
		slices.Sort(names)
	}
	fileMode, dirMode := sc.categoryMode()
	newDir := filepath.Join(dir, "new")
	if err := os.Mkdir(newDir, dirMode); err != nil {
		return err
	}
	for _, rel := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		dest := filepath.Join(p.root, filepath.FromSlash(rel))
		if cfg.beforeStage != nil {
			if err := cfg.beforeStage(dest); err != nil {
				return err
			}
		}
		mode := fileMode
		if l, ok := p.live[rel]; ok && l.mode != 0 {
			mode = l.mode
		}
		if err := copyVerified(p.archive[rel], filepath.Join(newDir, filepath.FromSlash(rel)), mode, dirMode); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
	}
	return syncDirTree(newDir)
}

func sameFilesystem(dir string, other fs.FileInfo) error {
	di, err := os.Stat(dir)
	if err != nil {
		return err
	}
	ds, ok1 := di.Sys().(*syscall.Stat_t)
	os_, ok2 := other.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 {
		return errors.New("cannot tell which filesystem the directory is on")
	}
	if ds.Dev != os_.Dev {
		return errors.New("the directory is on a different filesystem from the one beside it, so a file cannot be moved into it atomically")
	}
	return nil
}

func copyVerified(src archivedFile, dst string, mode, dirMode fs.FileMode) (err error) {
	var in io.Reader = bytes.NewReader(src.body)
	if src.src != "" {
		f, err := os.OpenFile(src.src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return fmt.Errorf("opening the extracted archive's file: %w", err)
		}
		defer func() { _ = f.Close() }()
		in = f
	}
	if err := os.MkdirAll(filepath.Dir(dst), dirMode); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), in); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != src.sum {
		return errors.New("the file in the extracted archive no longer matches the archive's manifest")
	}
	if err := out.Chmod(mode.Perm()); err != nil {
		return err
	}
	return out.Sync()
}

func syncDirTree(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return fsyncDir(p)
		}
		return nil
	})
}

type stepKind int

const (
	stepSaved stepKind = iota
	stepPlaced
	stepMkdir
	stepPlacedTree
)

type journalStep struct {
	kind stepKind
	a, b string
}

type journal []journalStep

func (j *journal) push(k stepKind, a, b string) { *j = append(*j, journalStep{k, a, b}) }

func (j journal) rollback() error {
	var errs []error
	for i := len(j) - 1; i >= 0; i-- {
		st := j[i]
		var err error
		switch st.kind {
		case stepPlaced:
			err = os.Remove(st.a)
			if errors.Is(err, fs.ErrNotExist) {
				err = nil
			}
		case stepSaved:
			err = os.Rename(st.b, st.a)
		case stepPlacedTree:
			err = os.Rename(st.a, st.b)
		case stepMkdir:
			err = os.Remove(st.a)
			if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, fs.ErrNotExist) {
				err = nil
			}
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// apply returns the error that stopped the category and, separately, the
// error from putting it back, nil when it was put back.
func (sc *stagedCategory) apply(cfg *filesConfig) (rollbackErr, err error) {
	var j journal
	if sc.plan.category == FilesTemplates {
		err = sc.applyTree(cfg, &j)
	} else {
		err = sc.applyFiles(cfg, &j)
	}
	if err == nil {
		return nil, nil
	}
	return j.rollback(), err
}

func (sc *stagedCategory) step(cfg *filesConfig, n *int) error {
	i := *n
	*n++
	if cfg.afterStep != nil {
		return cfg.afterStep(sc.plan.category, i)
	}
	return nil
}

func (sc *stagedCategory) applyTree(cfg *filesConfig, j *journal) error {
	root := sc.plan.root
	old := filepath.Join(sc.stageDir, "old")
	step := 0
	if _, err := os.Lstat(root); err == nil {
		if err := os.Rename(root, old); err != nil {
			return err
		}
		j.push(stepSaved, root, old)
		if err := sc.step(cfg, &step); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if len(sc.plan.archive) > 0 {
		newDir := filepath.Join(sc.stageDir, "new")
		_, dirMode := sc.categoryMode()
		if fi, err := os.Stat(old); err == nil {
			dirMode = fi.Mode().Perm()
		}
		if err := os.Chmod(newDir, dirMode); err != nil {
			return err
		}
		if err := os.Rename(newDir, root); err != nil {
			return err
		}
		j.push(stepPlacedTree, root, filepath.Join(sc.stageDir, "discarded"))
		if err := sc.step(cfg, &step); err != nil {
			return err
		}
	}
	return fsyncDir(filepath.Dir(root))
}

func (sc *stagedCategory) applyFiles(cfg *filesConfig, j *journal) error {
	p := sc.plan
	_, dirMode := sc.categoryMode()
	newDir := filepath.Join(sc.stageDir, "new")
	saved := 0
	step := 0
	syncDirs := map[string]struct{}{}

	type op struct {
		rel   string
		away  bool
		place bool
	}
	var ops []op
	for _, rel := range p.changes.Removed {
		ops = append(ops, op{rel: rel, away: true})
	}
	for _, rel := range p.changes.Replaced {
		ops = append(ops, op{rel: rel, away: true, place: true})
	}
	for _, rel := range p.changes.Added {
		ops = append(ops, op{rel: rel, place: true})
	}
	for _, o := range ops {
		dest := filepath.Join(p.root, filepath.FromSlash(o.rel))
		if err := checkDestination(p.root, o.rel); err != nil {
			return err
		}
		if o.away {
			to := filepath.Join(sc.stageDir, "old", strconv.Itoa(saved))
			saved++
			if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
				return err
			}
			if err := os.Rename(dest, to); err != nil {
				return err
			}
			j.push(stepSaved, dest, to)
			syncDirs[filepath.Dir(dest)] = struct{}{}
		}
		if o.place {
			if err := mkdirParents(p.root, o.rel, dirMode, j, syncDirs); err != nil {
				return err
			}
			if err := os.Rename(filepath.Join(newDir, filepath.FromSlash(o.rel)), dest); err != nil {
				return err
			}
			j.push(stepPlaced, dest, "")
			syncDirs[filepath.Dir(dest)] = struct{}{}
		}
		if err := sc.step(cfg, &step); err != nil {
			return err
		}
	}
	for dir := range syncDirs {
		if err := fsyncDir(dir); err != nil {
			return err
		}
	}
	return nil
}

func mkdirParents(root, rel string, mode fs.FileMode, j *journal, syncDirs map[string]struct{}) error {
	dirs := []string{root}
	cur := root
	if d := path.Dir(rel); d != "." {
		for _, c := range strings.Split(d, "/") {
			cur = filepath.Join(cur, c)
			dirs = append(dirs, cur)
		}
	}
	for _, d := range dirs {
		err := os.Mkdir(d, mode)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		j.push(stepMkdir, d, "")
		syncDirs[filepath.Dir(d)] = struct{}{}
		if err := os.Chmod(d, mode); err != nil {
			return err
		}
	}
	return nil
}

func unsafePath(p, reason string) error {
	return &UnsafeRestorePathError{Path: p, Reason: reason}
}

// checkRoot refuses a category directory that is a symbolic link or not a
// directory. A directory that does not exist is fine: restoring creates it.
func checkRoot(root string) (exists bool, err error) {
	fi, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return true, unsafePath(root, "is a symbolic link, which is never followed")
	}
	if !fi.IsDir() {
		return true, unsafePath(root, "is not a directory")
	}
	return true, nil
}

// checkDestination refuses a write to root/rel that would land anywhere but
// inside root: a directory on the way, or the file itself, that is a
// symbolic link or is not what it must be. A path that does not exist yet
// is fine, from the first missing directory down.
func checkDestination(root, rel string) error {
	if _, err := checkRoot(root); err != nil {
		return err
	}
	cur := root
	if d := path.Dir(rel); d != "." {
		for _, c := range strings.Split(d, "/") {
			cur = filepath.Join(cur, c)
			fi, err := os.Lstat(cur)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if fi.Mode()&fs.ModeSymlink != 0 {
				return unsafePath(cur, "is a symbolic link, which is never followed")
			}
			if !fi.IsDir() {
				return unsafePath(cur, "is not a directory")
			}
		}
	}
	fi, err := os.Lstat(filepath.Join(cur, path.Base(rel)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return unsafePath(filepath.Join(cur, path.Base(rel)), "is a symbolic link, which is never followed")
	}
	if !fi.Mode().IsRegular() {
		return unsafePath(filepath.Join(cur, path.Base(rel)), "is not a regular file")
	}
	return nil
}

func buildPlans(tree string, paths Paths, stackEnvs []StackEnv) ([]categoryPlan, error) {
	archived, err := archivedFiles(tree)
	if err != nil {
		return nil, err
	}
	roots := map[string]string{
		FilesCustomConfig: paths.ConfigRoot,
		FilesTemplates:    paths.TemplatesDir,
		FilesStacks:       paths.StacksDir,
	}
	plans := make([]categoryPlan, 0, len(filesCategories))
	for _, cat := range filesCategories {
		p := categoryPlan{category: cat, archive: archived[cat], live: map[string]liveFile{}}
		p.changes = FileChanges{Category: cat, Replaced: []string{}, Added: []string{}, Removed: []string{}}
		if roots[cat] == "" {
			p.archive = nil
			plans = append(plans, p)
			continue
		}
		p.root, err = filepath.Abs(roots[cat])
		if err != nil {
			return nil, fmt.Errorf("resolving %s: %w", roots[cat], err)
		}
		exists, err := checkRoot(p.root)
		if err != nil {
			return nil, err
		}
		if exists {
			switch cat {
			case FilesCustomConfig:
				err = liveCustomConfig(p.root, p.live)
			case FilesTemplates:
				err = liveTemplates(p.root, p.live)
			case FilesStacks:
				err = liveStacks(p.root, p.live)
			}
			if err != nil {
				return nil, err
			}
		}
		for rel, a := range p.archive {
			l, ok := p.live[rel]
			switch {
			case !ok:
				p.changes.Added = append(p.changes.Added, rel)
			case l.sum != a.sum:
				p.changes.Replaced = append(p.changes.Replaced, rel)
			}
			if cat != FilesTemplates && (!ok || l.sum != a.sum) {
				if err := checkDestination(p.root, rel); err != nil {
					return nil, err
				}
			}
		}
		for rel := range p.live {
			if _, ok := p.archive[rel]; !ok {
				p.changes.Removed = append(p.changes.Removed, rel)
			}
		}
		if cat == FilesStacks {
			if err := p.planStackEnvs(stackEnvs); err != nil {
				return nil, err
			}
		}
		slices.Sort(p.changes.Added)
		slices.Sort(p.changes.Replaced)
		slices.Sort(p.changes.Removed)
		plans = append(plans, p)
	}
	return plans, nil
}

// planStackEnvs adds the .env of each stack envs names that the archive holds
// files of. It runs after the plan's other changes are decided, and .env is
// never among a stack's live files, so a stack's .env is never removed.
func (p *categoryPlan) planStackEnvs(envs []StackEnv) error {
	archived := map[string]bool{}
	for rel := range p.archive {
		stack, _, _ := strings.Cut(rel, "/")
		archived[stack] = true
	}
	seen := map[string]bool{}
	for _, e := range envs {
		if !archived[e.Stack] || len(e.Body) == 0 || seen[e.Stack] {
			continue
		}
		seen[e.Stack] = true
		rel := e.Stack + "/.env"
		sum := sha256.Sum256(e.Body)
		want := hex.EncodeToString(sum[:])
		if err := checkDestination(p.root, rel); err != nil {
			return err
		}
		live := filepath.Join(p.root, e.Stack, ".env")
		if _, err := os.Lstat(live); errors.Is(err, fs.ErrNotExist) {
			p.changes.Added = append(p.changes.Added, rel)
		} else if err != nil {
			return err
		} else {
			have, err := hashFile(live)
			if err != nil {
				return fmt.Errorf("reading %s: %w", live, err)
			}
			if have == want {
				continue
			}
			p.changes.Replaced = append(p.changes.Replaced, rel)
		}
		p.archive[rel] = archivedFile{body: e.Body, sum: want}
		p.envs = append(p.envs, rel)
	}
	slices.Sort(p.envs)
	return nil
}

// archivedFiles lists, by category, the files the manifest lists under
// custom/, templates/ and stacks/, each with its manifest checksum. Only a
// file the manifest lists is ever restored, and each must be a regular file
// in the extracted tree.
func archivedFiles(tree string) (map[string]map[string]archivedFile, error) {
	manifest, err := readManifest(filepath.Join(tree, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	keys := make([]string, 0, len(manifest.Checksums))
	for k := range manifest.Checksums {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	prefixes := map[string]string{"custom/": FilesCustomConfig, "templates/": FilesTemplates, "stacks/": FilesStacks}
	out := map[string]map[string]archivedFile{}
	for _, key := range keys {
		var cat, rel string
		for prefix, c := range prefixes {
			if r, ok := strings.CutPrefix(key, prefix); ok {
				cat, rel = c, r
			}
		}
		if cat == "" {
			continue
		}
		if !fs.ValidPath(key) {
			return nil, unsafePath(key, "is not a path inside the archive")
		}
		switch cat {
		case FilesCustomConfig:
			if !isCustomFile(rel) {
				return nil, unsafePath(key, "is not a *.custom.conf file")
			}
		case FilesStacks:
			parts := strings.Split(rel, "/")
			if len(parts) != 2 || !slices.Contains(stackFileNames, parts[1]) {
				return nil, unsafePath(key, "is not a stack's compose or meta.json file")
			}
		}
		src := filepath.Join(tree, filepath.FromSlash(key))
		fi, err := os.Lstat(src)
		if err != nil {
			return nil, fmt.Errorf("the archive lists %s: %w", key, err)
		}
		if !fi.Mode().IsRegular() {
			return nil, unsafePath(key, "is not a regular file in the extracted archive")
		}
		if out[cat] == nil {
			out[cat] = map[string]archivedFile{}
		}
		out[cat][rel] = archivedFile{src: src, sum: manifest.Checksums[key]}
	}
	return out, nil
}

func liveCustomConfig(root string, live map[string]liveFile) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !isCustomFile(d.Name()) {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		return addLive(live, filepath.ToSlash(rel), p, d.Type())
	})
}

func liveStacks(root string, live map[string]liveFile) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("listing %s: %w", root, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		for _, name := range stackFileNames {
			p := filepath.Join(root, e.Name(), name)
			fi, err := os.Lstat(p)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if err := addLive(live, e.Name()+"/"+name, p, fi.Mode().Type()); err != nil {
				return err
			}
		}
	}
	return nil
}

func liveTemplates(root string, live map[string]liveFile) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			// Never followed and never compared: the whole directory is
			// moved aside and replaced, and this entry goes with it.
			live[filepath.ToSlash(rel)] = liveFile{}
			return nil
		}
		return addLive(live, filepath.ToSlash(rel), p, d.Type())
	})
}

func addLive(live map[string]liveFile, rel, p string, typ fs.FileMode) error {
	if typ&fs.ModeSymlink != 0 {
		return unsafePath(p, "is a symbolic link, which is never followed")
	}
	if !typ.IsRegular() {
		return unsafePath(p, "is not a regular file")
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	sum, err := hashFile(p)
	if err != nil {
		return fmt.Errorf("reading %s: %w", p, err)
	}
	live[rel] = liveFile{sum: sum, mode: fi.Mode().Perm()}
	return nil
}
