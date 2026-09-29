package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/mdg-labs/hoserva/internal/container"
)

var (
	// ErrAppdataBusy is returned when an appdata backup or restore is
	// already running: two would stop the same containers and copy the same
	// directories at once.
	ErrAppdataBusy = errors.New("backup: an appdata backup or restore is already running")
	// ErrAppdataContainerNotFound is a container name with no appdata
	// directory in scope.
	ErrAppdataContainerNotFound = errors.New("backup: no such container in the appdata backup")
	// ErrAppdataNoDestination is returned when no enabled destination can
	// take appdata archives.
	ErrAppdataNoDestination = errors.New("backup: no destination can take appdata archives — add one, or enable the pool destination")
	// ErrAppdataArchiveNotFound is an archive the named destination does
	// not hold.
	ErrAppdataArchiveNotFound = errors.New("backup: no such appdata archive")
	// ErrAppdataArchiveInvalid wraps every reason an archive or its request
	// is refused before anything is changed.
	ErrAppdataArchiveInvalid = errors.New("backup: invalid appdata archive")
	// ErrPreRestoreSnapshot is returned when the snapshot of the current
	// appdata could not be written anywhere; the restore then changes
	// nothing.
	ErrPreRestoreSnapshot = errors.New("backup: could not write the snapshot of the current appdata")
)

// appdataStagingDir is where archives are built and fetched, next to the
// appdata location so they use the cache disk's space, not the boot
// device's.
const appdataStagingDir = ".hoserva-backup-staging"

const defaultAppdataStartTimeout = 2 * time.Minute

// AppdataContainers is what appdata backup needs of the Docker Engine:
// listing, and stopping and starting a container by name. Start and Stop go
// through container.Lifecycle, so a start is refused while the array is
// stopped, and RequireArrayRunning is checked before anything is stopped.
type AppdataContainers interface {
	List(ctx context.Context) ([]container.Container, error)
	Stop(ctx context.Context, id string) (container.Container, error)
	Start(ctx context.Context, id string) (container.Container, error)
	RequireArrayRunning() error
}

// LifecycleContainers adapts a container.Lifecycle to AppdataContainers.
type LifecycleContainers struct {
	*container.Lifecycle
}

func (l LifecycleContainers) List(ctx context.Context) ([]container.Container, error) {
	return l.Provider.List(ctx)
}

// AppdataPolicy is one container's backup policy. A container with no
// stored policy is stopped and included.
type AppdataPolicy struct {
	Container string
	Stop      bool
	Included  bool
}

// AppdataPolicyStore persists policies (D4).
type AppdataPolicyStore interface {
	ListAppdataPolicies(ctx context.Context) ([]AppdataPolicy, error)
	SetAppdataPolicy(ctx context.Context, p AppdataPolicy, at time.Time) error
}

// AppdataContainer is one container in the appdata backup's scope: it has
// at least one bind-mounted directory inside the appdata location.
type AppdataContainer struct {
	Name          string
	Image         string
	Running       bool
	Stop          bool
	Included      bool
	DatabaseImage bool
	// Dirs are the container's appdata directories, symbolic links
	// resolved, none inside another.
	Dirs []string
}

// Warning says why a known database image that is included but not stopped
// is a risk, or is empty for any other container.
func (c AppdataContainer) Warning() string {
	if !c.DatabaseImage || !c.Included || c.Stop {
		return ""
	}
	return fmt.Sprintf("%s runs a database image and is not stopped for the backup: its files are copied while it may be writing them, so the archive may not restore", c.Name)
}

// AppdataService is doc 10 §2's appdata backup: per-container archives of
// the appdata directories, taken while the containers that hold databases
// or other live state are stopped, and per-container restore.
type AppdataService struct {
	// Backup supplies the destinations, encryption and pool-write gate
	// the config backup already uses.
	Backup     *Service
	Containers AppdataContainers
	// Roots returns the appdata locations (the cache disk's appdata
	// directory); none means the array has no cache disk.
	Roots    func(ctx context.Context) ([]string, error)
	Policies AppdataPolicyStore
	// JournalPath is a file in Hoserva's own state directory naming the
	// containers a running backup or restore has stopped and not yet
	// started, so a daemon that dies in between still starts them.
	JournalPath string
	// StartTimeout bounds each container start after the copy; zero uses
	// two minutes.
	StartTimeout time.Duration

	runMu sync.Mutex
}

func (a *AppdataService) startTimeout() time.Duration {
	if a.StartTimeout > 0 {
		return a.StartTimeout
	}
	return defaultAppdataStartTimeout
}

func (a *AppdataService) now() time.Time {
	return a.Backup.now()
}

// appdataRoots returns the appdata locations, with symbolic links resolved
// where they exist.
func (a *AppdataService) appdataRoots(ctx context.Context) ([]string, error) {
	if a.Roots == nil {
		return nil, nil
	}
	roots, err := a.Roots(ctx)
	if err != nil {
		return nil, fmt.Errorf("finding the appdata location: %w", err)
	}
	return roots, nil
}

// Scope lists the containers whose appdata the backup covers, by name.
func (a *AppdataService) Scope(ctx context.Context) ([]AppdataContainer, error) {
	roots, err := a.appdataRoots(ctx)
	if err != nil {
		return nil, err
	}
	if len(roots) == 0 {
		return nil, nil
	}
	resolved := resolveRoots(roots)
	listed, err := a.Containers.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	policies := map[string]AppdataPolicy{}
	if a.Policies != nil {
		stored, err := a.Policies.ListAppdataPolicies(ctx)
		if err != nil {
			return nil, fmt.Errorf("reading appdata backup policies: %w", err)
		}
		for _, p := range stored {
			policies[p.Container] = p
		}
	}
	var out []AppdataContainer
	for _, c := range listed {
		dirs := appdataDirs(c, resolved)
		if len(dirs) == 0 {
			continue
		}
		ac := AppdataContainer{
			Name:          c.Name,
			Image:         c.Image,
			Running:       containerActive(c.State),
			Stop:          true,
			Included:      true,
			DatabaseImage: IsDatabaseImage(c.Image),
			Dirs:          dirs,
		}
		if p, ok := policies[c.Name]; ok {
			ac.Stop, ac.Included = p.Stop, p.Included
		}
		out = append(out, ac)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func resolveRoots(roots []string) []string {
	var out []string
	for _, r := range roots {
		if real, err := filepath.EvalSymlinks(r); err == nil {
			out = append(out, real)
		}
	}
	return out
}

// appdataDirs is each bind-mounted directory of c that resolves, through
// symbolic links, to a place strictly inside one of the resolved roots.
// A mount outside every root, or equal to one, is not appdata.
func appdataDirs(c container.Container, roots []string) []string {
	var dirs []string
	for _, m := range c.Mounts {
		if m.Source == "" || !filepath.IsAbs(m.Source) {
			continue
		}
		real, err := filepath.EvalSymlinks(m.Source)
		if err != nil {
			continue
		}
		info, err := os.Stat(real)
		if err != nil || !info.IsDir() {
			continue
		}
		for _, r := range roots {
			if withinDir(real, r) && real != r {
				dirs = append(dirs, real)
				break
			}
		}
	}
	return dropNestedDirs(dirs)
}

// dropNestedDirs removes duplicates and any directory inside another of the
// list, so each tree is archived once.
func dropNestedDirs(paths []string) []string {
	sort.Strings(paths)
	var out []string
	for _, p := range paths {
		if len(out) > 0 && withinDir(p, out[len(out)-1]) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Config is Scope with each container's policy, for the API.
func (a *AppdataService) Config(ctx context.Context) ([]AppdataContainer, error) {
	return a.Scope(ctx)
}

// SetPolicy replaces one container's policy. The container must be in
// scope, so a typo does not store a policy for nothing.
func (a *AppdataService) SetPolicy(ctx context.Context, name string, stop, included bool) (AppdataContainer, error) {
	if a.Policies == nil {
		return AppdataContainer{}, errors.New("backup: appdata backup policies are not configured")
	}
	scope, err := a.Scope(ctx)
	if err != nil {
		return AppdataContainer{}, err
	}
	for _, c := range scope {
		if c.Name != name {
			continue
		}
		if err := a.Policies.SetAppdataPolicy(ctx, AppdataPolicy{Container: name, Stop: stop, Included: included}, a.now()); err != nil {
			return AppdataContainer{}, fmt.Errorf("storing appdata backup policy of %s: %w", name, err)
		}
		c.Stop, c.Included = stop, included
		return c, nil
	}
	return AppdataContainer{}, fmt.Errorf("%w: %s", ErrAppdataContainerNotFound, name)
}

// ScopeNames resolves the containers a run would cover: those named, which
// must all be in scope, or every included one. It is what the job is
// submitted with, so the job's resources are known before it starts.
func (a *AppdataService) ScopeNames(ctx context.Context, requested []string) ([]string, error) {
	scope, err := a.Scope(ctx)
	if err != nil {
		return nil, err
	}
	selected, err := selectAppdata(scope, requested)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(selected))
	for i, c := range selected {
		names[i] = c.Name
	}
	return names, nil
}

func selectAppdata(scope []AppdataContainer, requested []string) ([]AppdataContainer, error) {
	if len(requested) == 0 {
		var out []AppdataContainer
		for _, c := range scope {
			if c.Included {
				out = append(out, c)
			}
		}
		return out, nil
	}
	byName := make(map[string]AppdataContainer, len(scope))
	for _, c := range scope {
		byName[c.Name] = c
	}
	seen := map[string]bool{}
	var out []AppdataContainer
	for _, name := range requested {
		c, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrAppdataContainerNotFound, name)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

type appdataJournal struct {
	Containers []string `json:"containers"`
}

func (a *AppdataService) writeJournal(names []string) error {
	if a.JournalPath == "" {
		return errors.New("backup: no journal path is configured for stopped containers")
	}
	raw, err := json.Marshal(appdataJournal{Containers: names})
	if err != nil {
		return fmt.Errorf("encoding the stopped-container journal: %w", err)
	}
	dir := filepath.Dir(a.JournalPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(a.JournalPath)+".*.tmp")
	if err != nil {
		return fmt.Errorf("writing the stopped-container journal: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing the stopped-container journal: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("syncing the stopped-container journal: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("closing the stopped-container journal: %w", err)
	}
	if err := os.Rename(tmp.Name(), a.JournalPath); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing the stopped-container journal: %w", err)
	}
	return fsyncDir(dir)
}

// journalAdd records names as stopped, keeping every name an earlier run
// left in the journal: a container that run stopped and never started is
// already stopped now, so this run would not name it.
func (a *AppdataService) journalAdd(names []string) error {
	have, err := a.readJournal()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	merged := make([]string, 0, len(have)+len(names))
	for _, n := range append(have, names...) {
		if !seen[n] {
			seen[n] = true
			merged = append(merged, n)
		}
	}
	return a.writeJournal(merged)
}

// journalRemove drops names from the journal and leaves every other name in
// it, clearing the file once none is left.
func (a *AppdataService) journalRemove(names []string) error {
	have, err := a.readJournal()
	if err != nil {
		return err
	}
	drop := map[string]bool{}
	for _, n := range names {
		drop[n] = true
	}
	var left []string
	for _, n := range have {
		if !drop[n] {
			left = append(left, n)
		}
	}
	if len(left) == 0 {
		return a.clearJournal()
	}
	return a.writeJournal(left)
}

func (a *AppdataService) clearJournal() error {
	if err := os.Remove(a.JournalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing the stopped-container journal: %w", err)
	}
	return nil
}

func (a *AppdataService) readJournal() ([]string, error) {
	raw, err := os.ReadFile(a.JournalPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the stopped-container journal: %w", err)
	}
	var j appdataJournal
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, fmt.Errorf("decoding the stopped-container journal: %w", err)
	}
	return j.Containers, nil
}

// RecoverStopped starts the containers a backup or restore stopped and
// never started again, because the daemon died first or the array was
// stopped while it ran. It does nothing while a backup or restore is
// running in this process, and while the array is not running, and reads
// one small file — never a data disk. A container that has since been
// removed is dropped from the journal.
func (a *AppdataService) RecoverStopped(ctx context.Context) error {
	if a.JournalPath == "" {
		return nil
	}
	if !a.runMu.TryLock() {
		return nil
	}
	defer a.runMu.Unlock()
	names, err := a.readJournal()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return a.clearJournal()
	}
	if err := a.Containers.RequireArrayRunning(); err != nil {
		return nil
	}
	var still []string
	var errs []error
	for _, name := range names {
		sctx, cancel := context.WithTimeout(ctx, a.startTimeout())
		_, err := a.Containers.Start(sctx, name)
		cancel()
		switch {
		case err == nil, errors.Is(err, container.ErrNotFound):
		default:
			still = append(still, name)
			errs = append(errs, fmt.Errorf("starting %s after an interrupted appdata backup: %w", name, err))
		}
	}
	if len(still) == 0 {
		return a.clearJournal()
	}
	if err := a.writeJournal(still); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// containerActive reports whether a container in this Engine state has
// live processes that could be writing its appdata: it has to be stopped
// for a consistent copy, or before its files are replaced. A paused one
// counts, since its processes are frozen, not gone.
func containerActive(state string) bool {
	switch state {
	case "running", "restarting", "paused":
		return true
	default:
		return false
	}
}
