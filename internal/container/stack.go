package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mdg-labs/hoserva/internal/store"
)

// Stack file names under <stacks root>/<name>/ (doc 04 §2).
const (
	stackComposeFile = "docker-compose.yml"
	stackEnvFile     = ".env"
	stackMetaFile    = "meta.json"
)

const (
	composeValidateTimeout = 30 * time.Second
	composeUpTimeout       = 15 * time.Minute
	composeDownTimeout     = 5 * time.Minute
)

var (
	// ErrInvalidStackName is returned for a name that is not a valid stack
	// directory and Compose project name. It is checked before anything on
	// disk or in the database is touched.
	ErrInvalidStackName = errors.New("container: invalid stack name")
	// ErrInvalidStack is returned by CreateStack when the Compose file is
	// empty or `docker compose config` rejects it.
	ErrInvalidStack = errors.New("container: invalid compose stack")
	// ErrReservedEnvName is returned by Create when the stack's .env defines
	// a variable Docker needs from the daemon's environment (PATH, HOME,
	// DOCKER_HOST, ...). Nothing is written.
	ErrReservedEnvName = errors.New("container: the .env defines a variable Docker reserves")
	// ErrStackNotFound is returned when no stack has the name.
	ErrStackNotFound = store.ErrStackNotFound
	// ErrStackExists is returned by CreateStack when a stack has the name.
	ErrStackExists = store.ErrStackExists
	// ErrStackDirExists is returned by CreateStack when a directory of the
	// stack's name is already under the stacks root: Create never writes
	// into, or over, a directory it did not make.
	ErrStackDirExists = errors.New("container: a directory of that name already exists under the stacks directory")
	// ErrStackDirUnsafe is returned when the stack's directory is a
	// symbolic link or not a directory, so it is neither written nor
	// deleted.
	ErrStackDirUnsafe = errors.New("container: the stack's directory is not a plain directory")
	// ErrStackProjectShared is returned by Remove when the stack has no .env,
	// so `compose down` would work from the project name alone, and a
	// container of that project is not the stack's own (a project of the
	// same name that something else runs). Nothing is removed.
	ErrStackProjectShared = errors.New("container: a Compose project of the stack's name that is not the stack's is running")
	// ErrComposeFailed wraps every failure of a `docker compose` run, so a
	// caller can tell Compose failing from a local failure of the daemon.
	ErrComposeFailed = errors.New("container: docker compose failed")
)

var stackNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ValidStackName reports whether name can be a stack: 1 to 63 lowercase
// letters, digits, '-' or '_', starting with a letter or digit. Every path
// the service builds is <root>/<name>, so a name with '/', '..' or a leading
// '-' never gets that far.
func ValidStackName(name string) bool {
	return stackNamePattern.MatchString(name)
}

// StackStore is the stacks table (store.StackStore).
type StackStore interface {
	Insert(ctx context.Context, st store.Stack) error
	Get(ctx context.Context, name string) (store.Stack, error)
	List(ctx context.Context) ([]store.Stack, error)
	Delete(ctx context.Context, name string) error
}

var _ StackStore = (*store.StackStore)(nil)

// SecretCipher seals a stack's .env under the machine key before it is
// stored (Q28); *auth.MachineKey satisfies it.
type SecretCipher interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}

// Stack is a stack as the API lists it: its row without the Compose text
// and without the .env, which holds generated secrets.
type Stack struct {
	Name             string
	TemplateSource   string
	TemplateID       string
	TemplateRevision string
	InstalledAt      time.Time
}

// NewStack is what CreateStack stores.
type NewStack struct {
	Name             string
	Compose          string
	Env              string
	TemplateSource   string
	TemplateID       string
	TemplateRevision string
}

// StackRemoveResult reports what Remove deleted besides the stack's
// containers, row and generated files.
type StackRemoveResult struct {
	// DeletedPaths are the appdata directories and the stack's directory
	// deleted when the caller asked for appdata deletion; empty otherwise.
	DeletedPaths []string
}

// StackService is the Compose stack model (doc 04 §2, D4): the stacks
// table is the state, and <Root>/<name>/docker-compose.yml, .env and
// meta.json are generated from it. `docker compose` always runs as an argv
// through Runner, never a shell.
type StackService struct {
	Store  StackStore
	Cipher SecretCipher
	Runner Runner
	// Root is the stacks directory: the state directory's stacks/
	// subdirectory, /var/lib/hoserva/stacks on an installed system.
	Root string
	// Now reports the install time of a new stack; nil means time.Now.
	Now func() time.Time
	// RequireArrayRunning and Admit gate Up, and Remove with deleteAppdata,
	// the way Lifecycle gates a container start and an appdata deletion: a
	// stack's bind mounts are never resolved against an unmounted pool. A
	// nil RequireArrayRunning refuses (fail closed); a nil Admit means no
	// hold.
	RequireArrayRunning func() error
	Admit               func() (release func(), err error)
	// Provider finds the stack's containers (the Compose project label) and
	// the other containers whose mounts its appdata must not overlap, and
	// AppdataRoots is where appdata may be deleted, exactly as for
	// Lifecycle.Remove. Both are needed to delete appdata; without them that
	// is refused. Provider is also needed to remove a stack that has no
	// .env, which is refused without it.
	Provider     Provider
	AppdataRoots func(ctx context.Context) ([]string, error)

	mu sync.Mutex
}

type stackMeta struct {
	Name        string            `json:"name"`
	Template    stackMetaTemplate `json:"template"`
	InstalledAt string            `json:"installedAt"`
}

type stackMetaTemplate struct {
	Source   string `json:"source"`
	ID       string `json:"id"`
	Revision string `json:"revision"`
}

func stackFrom(st store.Stack) Stack {
	return Stack{
		Name:             st.Name,
		TemplateSource:   st.TemplateSource,
		TemplateID:       st.TemplateID,
		TemplateRevision: st.TemplateRevision,
		InstalledAt:      st.InstalledAt,
	}
}

func (s *StackService) dir(name string) string {
	return filepath.Join(s.Root, name)
}

// checkRoot refuses every operation that writes or deletes when Root is not
// an absolute path, so <Root>/<name> can never resolve against the daemon's
// working directory.
func (s *StackService) checkRoot() error {
	if !filepath.IsAbs(s.Root) {
		return fmt.Errorf("container: the stacks directory %q is not an absolute path", s.Root)
	}
	return nil
}

// render returns the bytes of the generated file name for st. The same row
// always renders the same bytes. The .env is opened only when it is
// rendered: a row whose sealed env was cleared (a bare-metal restore without
// the backup passphrase) fails here rather than producing an empty .env.
func (s *StackService) render(st store.Stack, name string) ([]byte, error) {
	switch name {
	case stackComposeFile:
		return []byte(st.Compose), nil
	case stackEnvFile:
		env, err := s.Cipher.Decrypt(st.SealedEnv)
		if err != nil {
			return nil, fmt.Errorf("opening the .env of stack %s: %w", st.Name, err)
		}
		return env, nil
	default:
		meta, err := json.MarshalIndent(stackMeta{
			Name:        st.Name,
			Template:    stackMetaTemplate{Source: st.TemplateSource, ID: st.TemplateID, Revision: st.TemplateRevision},
			InstalledAt: st.InstalledAt.UTC().Format(store.TimeFormat),
		}, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("encoding meta.json of stack %s: %w", st.Name, err)
		}
		return append(meta, '\n'), nil
	}
}

var stackFileNames = []string{stackComposeFile, stackEnvFile, stackMetaFile}

var stackFileModes = map[string]os.FileMode{
	stackComposeFile: 0o644,
	stackEnvFile:     0o600,
	stackMetaFile:    0o644,
}

// writeFiles writes the generated files of st into dir, each through a
// temporary file and a rename, so a file is never seen half-written. With
// onlyMissing it leaves a file that already exists as it is (a user may
// have edited it by hand). With skipUnopenableEnv a .env the row cannot open
// is left unwritten instead of failing the call.
func (s *StackService) writeFiles(st store.Stack, dir string, onlyMissing, skipUnopenableEnv bool) error {
	for _, name := range stackFileNames {
		path := filepath.Join(dir, name)
		if onlyMissing {
			if _, err := os.Lstat(path); err == nil {
				continue
			} else if !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("checking %s: %w", path, err)
			}
		}
		data, err := s.render(st, name)
		if err != nil {
			if skipUnopenableEnv && name == stackEnvFile {
				continue
			}
			return err
		}
		if err := writeFileAtomic(path, data, stackFileModes[name]); err != nil {
			return err
		}
	}
	return syncDir(dir)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return fail(err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// plainDir returns nil when path is an existing directory that is not a
// symbolic link, fs.ErrNotExist when it does not exist, and
// ErrStackDirUnsafe otherwise.
func plainDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s", ErrStackDirUnsafe, path)
	}
	return nil
}

// removeGeneratedFiles deletes the three generated files from dir and then
// dir itself if nothing else is left in it, so a file a hand-edited compose
// keeps in the stack directory (a bind-mounted ./data) stays. A directory
// that does not exist is nothing to do; one that is not a plain directory is
// refused.
func removeGeneratedFiles(dir string) error {
	if err := plainDir(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	for _, name := range stackFileNames {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("deleting %s: %w", filepath.Join(dir, name), err)
		}
	}
	if err := os.Remove(dir); err != nil &&
		!errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("deleting %s: %w", dir, err)
	}
	return nil
}

func (s *StackService) composeArgs(name string, sub ...string) []string {
	return s.composeArgsWithEnv(name, filepath.Join(s.dir(name), stackEnvFile), sub...)
}

func (s *StackService) composeArgsWithEnv(name, envFile string, sub ...string) []string {
	dir := s.dir(name)
	args := []string{
		"compose",
		"--project-name", name,
		"--project-directory", dir,
		"--file", filepath.Join(dir, stackComposeFile),
		"--env-file", envFile,
	}
	return append(args, sub...)
}

func (s *StackService) compose(ctx context.Context, timeout time.Duration, name string, sub ...string) error {
	return s.runCompose(ctx, timeout, s.composeArgs(name, sub...), filepath.Join(s.dir(name), stackEnvFile))
}

// composeEnvNames are the only variables of the daemon's environment a
// Compose run gets: what the Docker CLI needs to find its plugins, its
// credential helpers and the Engine. They are reserved: a stack's .env may
// not define them (ErrReservedEnvName), so the daemon's values always reach
// Docker.
var composeEnvNames = []string{
	"PATH", "HOME", "XDG_RUNTIME_DIR",
	"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY",
}

// ReservedEnvNames returns the variable names a stack's .env, and so a
// template input, may not use.
func ReservedEnvNames() []string {
	return append([]string(nil), composeEnvNames...)
}

// ReservedEnvDefined returns the reserved names the .env text defines, in
// ReservedEnvNames order.
func ReservedEnvDefined(env string) []string {
	var out []string
	for _, name := range composeEnvNames {
		if envFileDefines([]byte(env), name) {
			out = append(out, name)
		}
	}
	return out
}

// composeEnv is the environment of a Compose run. Compose lets a variable of
// its own environment override the same name in --env-file, and reads its own
// settings (COMPOSE_PROJECT_NAME, COMPOSE_FILE, ...) from there, so the
// daemon's environment is never passed on whole: the stack's .env, whose
// values are recorded in the database, is what Compose interpolates. The
// variables Docker needs are the daemon's own, and a .env cannot define them
// since a stack is created. A stack stored before that rule whose .env still
// defines one has the daemon's value left out of the run, so the stack's
// value wins and Docker falls back to its default for it, and a warning is
// logged. envFile is the file --env-file names, or empty for a run without
// one.
func composeEnv(envFile string) ([]string, error) {
	var defined []byte
	if envFile != "" {
		b, err := os.ReadFile(envFile)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("reading %s: %w", envFile, err)
		}
		defined = b
	}
	env := make([]string, 0, len(composeEnvNames))
	for _, name := range composeEnvNames {
		v, ok := os.LookupEnv(name)
		if envFileDefines(defined, name) {
			log.Printf("container: %s defines %s, which Docker needs from the daemon's environment; the daemon's value is left out of this run. Create the stack again without it", envFile, name)
			continue
		}
		if !ok {
			continue
		}
		env = append(env, name+"="+v)
	}
	return env, nil
}

// envFileDefines reports whether a line of the .env file starts with name as
// a key, with or without `export` and with or without a value.
func envFileDefines(envFile []byte, name string) bool {
	return regexp.MustCompile(`(?m)^[ \t]*(?:export[ \t]+)?` + regexp.QuoteMeta(name) + `[ \t]*(?:[=:]|\r?$)`).Match(envFile)
}

func (s *StackService) runCompose(ctx context.Context, timeout time.Duration, args []string, envFile string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	env, err := composeEnv(envFile)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrComposeFailed, err)
	}
	if _, err := s.Runner.Run(ctx, env, "docker", args...); err != nil {
		return fmt.Errorf("%w: %w", ErrComposeFailed, err)
	}
	return nil
}

func (s *StackService) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Create stores the stack's row (its .env sealed), generates its three
// files into <Root>/<name> and checks them with `docker compose config`.
// Nothing is started. A .env that defines a reserved variable (see
// ReservedEnvNames) is refused as ErrReservedEnvName before anything is
// stored or written.
//
// An existing plain directory of that name is adopted, so a stack removed
// without its appdata can be installed again, but one that holds a
// docker-compose.yml no stack owns is refused and left as it is. Only the
// three generated files are ever written, and a failure deletes only those
// and the row (and the directory, if that leaves it empty).
//
// The row is stored before the files, so a daemon that dies half way leaves
// a stack that Remove can take away, never a directory the name is stuck
// behind. A .env or meta.json left in the directory with no compose file
// (an in-place restore of an archive without the stack keeps the .env) is
// replaced.
func (s *StackService) Create(ctx context.Context, n NewStack) (Stack, error) {
	if !ValidStackName(n.Name) {
		return Stack{}, fmt.Errorf("%w: %q", ErrInvalidStackName, n.Name)
	}
	if strings.TrimSpace(n.Compose) == "" {
		return Stack{}, fmt.Errorf("%w: the compose file is empty", ErrInvalidStack)
	}
	if names := ReservedEnvDefined(n.Env); len(names) > 0 {
		return Stack{}, fmt.Errorf("%w: %s; Docker takes these from the daemon's environment", ErrReservedEnvName, strings.Join(names, ", "))
	}
	if err := s.checkRoot(); err != nil {
		return Stack{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.Store.Get(ctx, n.Name); err == nil {
		return Stack{}, fmt.Errorf("%w: %s", ErrStackExists, n.Name)
	} else if !errors.Is(err, store.ErrStackNotFound) {
		return Stack{}, err
	}
	sealed, err := s.Cipher.Encrypt([]byte(n.Env))
	if err != nil {
		return Stack{}, fmt.Errorf("sealing the .env of stack %s: %w", n.Name, err)
	}
	st := store.Stack{
		Name:             n.Name,
		TemplateSource:   n.TemplateSource,
		TemplateID:       n.TemplateID,
		TemplateRevision: n.TemplateRevision,
		Compose:          n.Compose,
		SealedEnv:        sealed,
		InstalledAt:      s.now().UTC().Truncate(time.Second),
	}

	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return Stack{}, fmt.Errorf("creating the stacks directory: %w", err)
	}
	dir := s.dir(n.Name)
	switch err := plainDir(dir); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return Stack{}, err
	default:
		if _, err := os.Lstat(filepath.Join(dir, stackComposeFile)); err == nil {
			return Stack{}, fmt.Errorf("%w: %s", ErrStackDirExists, n.Name)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return Stack{}, fmt.Errorf("checking %s: %w", dir, err)
		}
	}

	if err := s.Store.Insert(ctx, st); err != nil {
		return Stack{}, err
	}
	if err := s.createFiles(ctx, st, dir); err != nil {
		// The compensation must not be cancelled with the request, or a
		// disconnect would leave the row and files of a stack that was
		// never accepted.
		cleanup := context.WithoutCancel(ctx)
		err = errors.Join(err, removeGeneratedFiles(dir))
		if derr := s.Store.Delete(cleanup, st.Name); derr != nil {
			err = errors.Join(err, fmt.Errorf("deleting the row of stack %s: %w", st.Name, derr))
		}
		return Stack{}, err
	}
	return stackFrom(st), nil
}

func (s *StackService) createFiles(ctx context.Context, st store.Stack, dir string) error {
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	if err := s.writeFiles(st, dir, false, false); err != nil {
		return err
	}
	if err := s.compose(ctx, composeValidateTimeout, st.Name, "config", "--quiet"); err != nil {
		var exit *exec.ExitError
		switch {
		case composePluginMissing(err):
			return fmt.Errorf("%w: %v", ErrComposeUnavailable, err)
		case errors.As(err, &exit):
			return fmt.Errorf("%w: %v", ErrInvalidStack, err)
		}
		return fmt.Errorf("checking the compose file of stack %s: %w", st.Name, err)
	}
	return nil
}

// Get returns the stack named name.
func (s *StackService) Get(ctx context.Context, name string) (Stack, error) {
	if !ValidStackName(name) {
		return Stack{}, fmt.Errorf("%w: %q", ErrInvalidStackName, name)
	}
	st, err := s.Store.Get(ctx, name)
	if err != nil {
		return Stack{}, err
	}
	return stackFrom(st), nil
}

// List returns every stack, sorted by name.
func (s *StackService) List(ctx context.Context) ([]Stack, error) {
	rows, err := s.Store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Stack, len(rows))
	for i, r := range rows {
		out[i] = stackFrom(r)
	}
	return out, nil
}

// ensureFiles writes whichever of the stack's three files is missing from
// its row, so a stack whose directory was deleted by hand can still be
// started or removed. A file that exists is never overwritten. With
// skipUnopenableEnv a missing .env the row cannot open is not an error: the
// caller does not need its values.
func (s *StackService) ensureFiles(st store.Stack, skipUnopenableEnv bool) error {
	if err := s.checkRoot(); err != nil {
		return err
	}
	dir := s.dir(st.Name)
	switch err := plainDir(dir); {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	case err != nil:
		return err
	}
	return s.writeFiles(st, dir, true, skipUnopenableEnv)
}

// Up starts the stack with `docker compose up -d`. Like a container start
// it is refused while the array is stopped or its storage is not ready.
func (s *StackService) Up(ctx context.Context, name string) error {
	if !ValidStackName(name) {
		return fmt.Errorf("%w: %q", ErrInvalidStackName, name)
	}
	if s.Admit != nil {
		release, err := s.Admit()
		if err != nil {
			return err
		}
		defer release()
	}
	if s.RequireArrayRunning == nil {
		return ErrArrayStateUnknown
	}
	if err := s.RequireArrayRunning(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.Store.Get(ctx, name)
	if err != nil {
		return err
	}
	if err := s.ensureFiles(st, false); err != nil {
		return err
	}
	if err := s.compose(ctx, composeUpTimeout, name, "up", "--detach"); err != nil {
		return fmt.Errorf("starting stack %s: %w", name, err)
	}
	return nil
}

// down runs `docker compose down`: the stack's containers and networks are
// removed, and with volumes its named volumes too. It needs no .env values,
// so a stack whose .env is missing and cannot be regenerated from its row (a
// bare-metal restore without the backup passphrase, a machine key that
// changed) is still taken down. Compose interpolates the file with the .env,
// and a template's file does not parse with its variables empty, so then the
// file is not loaded at all: `down` with only the project name works from the
// containers' project label.
//
// Either way `compose down` removes every container and network of the project
// name, and with volumes its `<project>_<volume>` volumes, whichever directory
// they were started from: the file and the working directory only say which
// services and volumes are declared, never whose they are. So a `down` of any
// kind is refused unless every container of that project is the stack's own
// (checkProjectOwned).
func (s *StackService) down(ctx context.Context, st store.Stack, volumes bool) error {
	if err := s.checkProjectOwned(ctx, st.Name); err != nil {
		return err
	}
	if err := s.ensureFiles(st, true); err != nil {
		return err
	}
	envFile := filepath.Join(s.dir(st.Name), stackEnvFile)
	_, err := os.Lstat(envFile)
	noEnv := errors.Is(err, fs.ErrNotExist)
	if err != nil && !noEnv {
		return fmt.Errorf("checking %s: %w", envFile, err)
	}
	sub := []string{"down"}
	if volumes {
		sub = append(sub, "--volumes")
	}
	args := s.composeArgs(st.Name, sub...)
	if noEnv {
		args = append([]string{"compose", "--project-name", st.Name}, sub...)
		envFile = ""
	}
	if err := s.runCompose(ctx, composeDownTimeout, args, envFile); err != nil {
		return fmt.Errorf("stopping stack %s: %w", st.Name, err)
	}
	return nil
}

// samePath reports whether a and b name the same place, following symbolic
// links where both resolve.
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// ownsContainer reports whether c was started by this stack: it carries the
// stack's Compose project name and was started from the stack's directory or
// its compose file. A project of the same name that something else runs (a
// hand-run ~/immich/compose.yml is project "immich") is not the stack's, yet
// `compose down` for the stack's name removes its containers and, with
// --volumes, its named volumes, whichever file it is run with.
func (s *StackService) ownsContainer(name string, c Container) bool {
	if c.Labels[composeProjectLabel] != name {
		return false
	}
	dir := s.dir(name)
	if wd := c.Labels[composeWorkingDirLabel]; wd != "" && samePath(wd, dir) {
		return true
	}
	file := filepath.Join(dir, stackComposeFile)
	for _, f := range strings.Split(c.Labels[composeConfigFilesLabel], ",") {
		if f != "" && samePath(f, file) {
			return true
		}
	}
	return false
}

// checkProjectOwned refuses unless every container carrying the stack's
// Compose project name is the stack's own (ownsContainer). It fails closed: a
// missing Provider or a failed listing means the project cannot be known to
// be the stack's alone.
func (s *StackService) checkProjectOwned(ctx context.Context, name string) error {
	if s.Provider == nil {
		return fmt.Errorf("%w: the containers of project %s cannot be listed to check they are all stack %s's own", ErrUnavailable, name, name)
	}
	all, err := s.Provider.List(ctx)
	if err != nil {
		return fmt.Errorf("listing containers to check that project %s is the stack's own: %w", name, err)
	}
	var foreign []string
	for _, c := range all {
		if c.Labels[composeProjectLabel] == name && !s.ownsContainer(name, c) {
			foreign = append(foreign, c.Name)
		}
	}
	if len(foreign) > 0 {
		sort.Strings(foreign)
		return fmt.Errorf("%w: `docker compose down` for stack %s would remove every container of the project %q, including %s, which Compose did not start from the stack's directory", ErrStackProjectShared, name, name, strings.Join(foreign, ", "))
	}
	return nil
}

// stackPlan is what deleteAppdata may delete and what must be gone first.
type stackPlan struct {
	// paths are the bind-mount directories to delete.
	paths []string
	// containers are the IDs and names of the stack's own containers, which
	// `compose down` must have removed before anything is deleted.
	containers map[string]string
}

// planAppdata returns what deleteAppdata may delete for the stack:
// what planAppdataDeletion plans for all of the stack's own containers taken
// together (ownsContainer), refused if any other container uses any of them
// or binds a directory inside the stack's directory, which goes with it. It
// must run before `compose down`, while the containers and their mounts can
// still be read.
func (s *StackService) planAppdata(ctx context.Context, name string) (stackPlan, error) {
	if s.AppdataRoots == nil || s.Provider == nil {
		return stackPlan{}, ErrAppdataUnavailable
	}
	roots, err := s.AppdataRoots(ctx)
	if err != nil {
		return stackPlan{}, fmt.Errorf("finding the appdata location: %w", err)
	}
	if len(roots) == 0 {
		return stackPlan{}, ErrAppdataUnavailable
	}
	all, err := s.Provider.List(ctx)
	if err != nil {
		return stackPlan{}, fmt.Errorf("listing containers to find the stack's appdata: %w", err)
	}
	stack := Container{ID: "stack:" + name, Name: name}
	plan := stackPlan{containers: map[string]string{}}
	var others []Container
	for _, c := range all {
		if s.ownsContainer(name, c) {
			stack.Mounts = append(stack.Mounts, c.Mounts...)
			plan.containers[c.ID] = c.Name
			continue
		}
		others = append(others, c)
	}
	if err := s.checkStackDirUnshared(name, others); err != nil {
		return stackPlan{}, err
	}
	plan.paths, err = planAppdataDeletion(stack, others, roots)
	return plan, err
}

// checkStackDirUnshared refuses when another container mounts the stack's
// directory or a place inside it, since deleting the directory takes that
// with it.
func (s *StackService) checkStackDirUnshared(name string, others []Container) error {
	dir := s.dir(name)
	if root, err := filepath.EvalSymlinks(s.Root); err == nil {
		dir = filepath.Join(root, name)
	}
	for _, o := range others {
		for _, m := range o.Mounts {
			if m.Source == "" || !filepath.IsAbs(m.Source) {
				continue
			}
			src := filepath.Clean(m.Source)
			if real, err := filepath.EvalSymlinks(src); err == nil {
				src = real
			}
			if holds(dir, src) {
				return fmt.Errorf("%w: %s is used by %s", ErrAppdataShared, dir, o.Name)
			}
		}
	}
	return nil
}

// stillRunning returns the names of the stack's own containers that still
// exist, sorted; an error listing them is treated as unknown, not as none.
func (s *StackService) stillRunning(ctx context.Context, plan stackPlan) ([]string, error) {
	all, err := s.Provider.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing containers to check the stack is down: %w", err)
	}
	var left []string
	for _, c := range all {
		if _, ok := plan.containers[c.ID]; ok {
			left = append(left, c.Name)
		}
	}
	sort.Strings(left)
	return left, nil
}

// Remove takes the stack down and deletes its row and its three generated
// files, and its directory if nothing else is left in it: a file a
// hand-edited compose keeps there stays, and the name can be used again.
//
// Appdata is deleted only when deleteAppdata is true, never as a side effect
// of anything else. Then the stack's named volumes go with `compose down
// --volumes`, each bind-mount directory of its containers strictly inside an
// appdata root that no other container uses is deleted (planAppdataDeletion,
// the rule Lifecycle.Remove applies), and so is the whole stack directory.
// Like Lifecycle.Remove it needs the array running, checked first, and keeps
// the array-action hold until the last deletion, so array stop cannot
// unmount the cache in between. Everything that can refuse the request is
// checked before `compose down`, including another container's mount inside
// the stack's directory; a failed down removes nothing, and nothing is
// deleted while one of the stack's containers is still there after it.
//
// A plain remove deletes only the generated files and an empty directory, so
// it needs no appdata check. Every remove does need the Provider, to see that
// the project's containers are all the stack's own before `compose down` runs
// (down); with Docker unreachable that is refused as ErrUnavailable, as
// compose could not run either.
//
// The name is validated before anything is read or written. Files and
// appdata go before the row, so a failed delete leaves a row the remove can
// be retried from.
func (s *StackService) Remove(ctx context.Context, name string, deleteAppdata bool) (StackRemoveResult, error) {
	if !ValidStackName(name) {
		return StackRemoveResult{}, fmt.Errorf("%w: %q", ErrInvalidStackName, name)
	}
	if deleteAppdata {
		if s.Admit != nil {
			release, err := s.Admit()
			if err != nil {
				return StackRemoveResult{}, err
			}
			defer release()
		}
		if s.RequireArrayRunning == nil {
			return StackRemoveResult{}, ErrArrayStateUnknown
		}
		if err := s.RequireArrayRunning(); err != nil {
			return StackRemoveResult{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.Store.Get(ctx, name)
	if err != nil {
		return StackRemoveResult{}, err
	}
	var plan stackPlan
	if deleteAppdata {
		if plan, err = s.planAppdata(ctx, name); err != nil {
			return StackRemoveResult{}, err
		}
	}
	if err := s.down(ctx, st, deleteAppdata); err != nil {
		return StackRemoveResult{}, err
	}
	if deleteAppdata {
		left, err := s.stillRunning(ctx, plan)
		if err != nil {
			return StackRemoveResult{}, err
		}
		if len(left) > 0 {
			return StackRemoveResult{}, fmt.Errorf("stack %s was taken down but its containers %s still exist, so nothing of its appdata was deleted", name, strings.Join(left, ", "))
		}
	}

	res := StackRemoveResult{DeletedPaths: []string{}}
	// The containers are gone by now, so a client that disconnects must not
	// leave the appdata or the files half deleted.
	ctx = context.WithoutCancel(ctx)
	dir := s.dir(name)
	if !deleteAppdata {
		if err := removeGeneratedFiles(dir); err != nil {
			return res, err
		}
	} else {
		deleted, err := removeAppdataDirs(ctx, plan.paths)
		res.DeletedPaths = append(res.DeletedPaths, deleted...)
		if err != nil {
			return res, fmt.Errorf("the containers of stack %s were removed but its appdata was not fully deleted: %w", name, err)
		}
		if err := plainDir(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return res, err
		} else if err == nil {
			if err := os.RemoveAll(dir); err != nil {
				return res, fmt.Errorf("deleting %s: %w", dir, err)
			}
			res.DeletedPaths = append(res.DeletedPaths, dir)
		}
	}
	if err := s.Store.Delete(ctx, name); err != nil {
		return res, err
	}
	return res, nil
}
