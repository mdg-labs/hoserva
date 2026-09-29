package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/container"
)

// recordingContainers wraps the real Lifecycle over a container.FakeProvider
// and records every stop and start, with hooks a test uses to observe or
// interfere at exactly that moment.
type recordingContainers struct {
	inner AppdataContainers

	mu      sync.Mutex
	events  []string
	onStop  func(name string)
	onStart func(name string)
}

func (r *recordingContainers) record(ev string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recordingContainers) Events() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *recordingContainers) List(ctx context.Context) ([]container.Container, error) {
	return r.inner.List(ctx)
}

func (r *recordingContainers) RequireArrayRunning() error { return r.inner.RequireArrayRunning() }

func (r *recordingContainers) Stop(ctx context.Context, id string) (container.Container, error) {
	r.record("stop " + id)
	c, err := r.inner.Stop(ctx, id)
	if r.onStop != nil {
		r.onStop(id)
	}
	return c, err
}

func (r *recordingContainers) Start(ctx context.Context, id string) (container.Container, error) {
	r.record("start " + id)
	if r.onStart != nil {
		r.onStart(id)
	}
	return r.inner.Start(ctx, id)
}

type appdataRig struct {
	*remoteRig
	svc        *AppdataService
	engine     *container.FakeProvider
	containers *recordingContainers
	policies   *FakeAppdataPolicyStore
	cache      string
	appdata    string
	poolDir    string
	bootDir    string
	halted     bool
	out        *bytes.Buffer
}

func newAppdataRig(t *testing.T) *appdataRig {
	t.Helper()
	remote := newRemoteRig(t)
	cache := filepath.Join(remote.root, "cache")
	rig := &appdataRig{
		remoteRig: remote,
		engine:    container.NewFakeProvider(),
		policies:  &FakeAppdataPolicyStore{},
		cache:     cache,
		appdata:   filepath.Join(cache, "appdata"),
		poolDir:   filepath.Join(remote.root, "pool-backups"),
		bootDir:   filepath.Join(remote.root, "boot-backups"),
		out:       &bytes.Buffer{},
	}
	if err := os.MkdirAll(rig.appdata, 0o755); err != nil {
		t.Fatal(err)
	}
	retention := Retention{Daily: 7, Weekly: 4, Monthly: 6}
	for _, d := range []Destination{
		{ID: DefaultBootID, Name: "Boot device", Type: TypeLocal, Path: rig.bootDir, Enabled: true, Retention: retention},
		{ID: DefaultPoolID, Name: "Pool", Type: TypeLocal, Path: rig.poolDir, Enabled: true, Retention: retention},
	} {
		if err := remote.store.CreateDestination(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	lifecycle := &container.Lifecycle{
		Provider:     rig.engine,
		Halted:       func() bool { return rig.halted },
		StorageReady: func() bool { return true },
	}
	rig.containers = &recordingContainers{inner: LifecycleContainers{lifecycle}}
	rig.svc = &AppdataService{
		Backup:      remote.svc,
		Containers:  rig.containers,
		Roots:       func(context.Context) ([]string, error) { return []string{rig.appdata}, nil },
		Policies:    rig.policies,
		JournalPath: filepath.Join(remote.root, "state", "appdata-stopped.json"),
	}
	return rig
}

// addApp creates a container whose appdata directory holds files, and
// returns that directory.
func (r *appdataRig) addApp(t *testing.T, name, image, state string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(r.appdata, name)
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r.engine.AddContainer(container.Container{
		ID: "id-" + name, Name: name, Image: image, State: state,
		Mounts: []container.Mount{{Source: dir, Destination: "/config", ReadWrite: true}},
	})
	return dir
}

func (r *appdataRig) run(t *testing.T, names ...string) error {
	t.Helper()
	r.out.Reset()
	return runNamed(context.Background(), r.svc, r.out, names...)
}

// runNamed resolves what a submit would (ScopeNames) and runs the backup on
// exactly that, the way the appdata_backup job does.
func runNamed(ctx context.Context, svc *AppdataService, out io.Writer, names ...string) error {
	resolved, err := svc.ScopeNames(ctx, names)
	if err != nil {
		return err
	}
	return svc.Run(ctx, AppdataRunRequest{Containers: names, Resolved: resolved}, out)
}

func (r *appdataRig) archives(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "hoserva-appdata-") {
			names = append(names, e.Name())
		}
	}
	return names
}

func (r *appdataRig) archiveFor(t *testing.T, dir, container string) string {
	t.Helper()
	for _, n := range r.archives(t, dir) {
		if _, c, reason, _, ok := parseAppdataName(n); ok && c == container && reason == ReasonNone {
			return filepath.Join(dir, n)
		}
	}
	t.Fatalf("no archive of %s in %s (have %v)", container, dir, r.archives(t, dir))
	return ""
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAppdataRun_StopsArchivesRestartsInReverseThenUploads(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "lscr.io/linuxserver/sonarr", "running", map[string]string{"config.xml": "alpha-config"})
	rig.addApp(t, "beta", "lscr.io/linuxserver/radarr", "running", map[string]string{"nested/db.txt": "beta-data"})

	var atFirstStart []string
	rig.containers.onStart = func(string) {
		if atFirstStart == nil {
			atFirstStart = rig.archives(t, rig.poolDir)
			if atFirstStart == nil {
				atFirstStart = []string{}
			}
		}
	}
	if err := rig.run(t); err != nil {
		t.Fatalf("Run: %v\n%s", err, rig.out)
	}

	want := []string{"stop alpha", "stop beta", "start beta", "start alpha"}
	if got := rig.containers.Events(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("stop/start order = %v, want %v", got, want)
	}
	if len(atFirstStart) != 0 {
		t.Fatalf("archives already uploaded when the first container restarted: %v", atFirstStart)
	}
	for _, name := range []string{"alpha", "beta"} {
		path := rig.archiveFor(t, rig.poolDir, name)
		hdr, trailer, err := verifyAppdata(path)
		if err != nil {
			t.Fatalf("verifying %s: %v", path, err)
		}
		if hdr.Container != name || !hdr.Stopped || trailer.Files != 1 {
			t.Fatalf("%s: header %+v trailer %+v", name, hdr, trailer)
		}
	}
	if _, err := os.Stat(rig.svc.JournalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stopped-container journal survived a clean run: %v", err)
	}
	stagingRoot := filepath.Join(rig.cache, appdataStagingDir)
	if _, err := os.Stat(stagingRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging directory %s was left behind: %v", stagingRoot, err)
	}
}

func TestAppdataRun_NeverWritesToTheBootDeviceDestination(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"a": "1"})
	if err := rig.run(t); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := rig.archives(t, rig.bootDir); len(got) != 0 {
		t.Fatalf("appdata archives on the boot device destination: %v", got)
	}
	if got := rig.archives(t, rig.poolDir); len(got) != 1 {
		t.Fatalf("pool destination archives = %v, want one", got)
	}
}

func TestAppdataRun_NoDestinationFailsBeforeStoppingAnything(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"a": "1"})
	if err := rig.store.DeleteDestination(context.Background(), DefaultPoolID); err != nil {
		t.Fatal(err)
	}
	if err := rig.run(t); !errors.Is(err, ErrAppdataNoDestination) {
		t.Fatalf("Run = %v, want ErrAppdataNoDestination", err)
	}
	if ev := rig.containers.Events(); len(ev) != 0 {
		t.Fatalf("containers were touched: %v", ev)
	}
}

func TestAppdataRun_RefusesWhileTheArrayIsStopped(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"a": "1"})
	rig.halted = true
	if err := rig.run(t); !errors.Is(err, container.ErrArrayStopped) {
		t.Fatalf("Run = %v, want ErrArrayStopped", err)
	}
	if ev := rig.containers.Events(); len(ev) != 0 {
		t.Fatalf("containers were touched: %v", ev)
	}
	if got := rig.archives(t, rig.poolDir); len(got) != 0 {
		t.Fatalf("archives written with the array stopped: %v", got)
	}
}

func TestAppdataRun_DatabaseImageOptedOutIsNotStoppedAndIsFlagged(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "db", "postgres", "running", map[string]string{"PG_VERSION": "16"})
	rig.addApp(t, "web", "nginx", "running", map[string]string{"index.html": "hi"})
	if _, err := rig.svc.SetPolicy(context.Background(), "db", false, true); err != nil {
		t.Fatal(err)
	}
	if err := rig.run(t); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, ev := range rig.containers.Events() {
		if strings.Contains(ev, "db") {
			t.Fatalf("the opted-out database was touched: %v", rig.containers.Events())
		}
	}
	if !strings.Contains(rig.out.String(), "warning: db runs a database image and is not stopped") {
		t.Fatalf("no database warning in the job output:\n%s", rig.out)
	}
	hdr, _, err := verifyAppdata(rig.archiveFor(t, rig.poolDir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Stopped || !hdr.DatabaseImage {
		t.Fatalf("header = %+v, want a live database archive", hdr)
	}
}

func TestAppdataConfig_FlagsDatabaseImagesThatAreNotStopped(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "db", "docker.io/library/mariadb:11", "running", map[string]string{"x": "1"})
	rig.addApp(t, "web", "nginx", "running", map[string]string{"x": "1"})
	ctx := context.Background()

	before, err := rig.svc.Config(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range before {
		if !c.Stop || !c.Included || c.Warning() != "" {
			t.Fatalf("default policy of %s = %+v", c.Name, c)
		}
	}
	got, err := rig.svc.SetPolicy(ctx, "db", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !got.DatabaseImage || got.Warning() == "" {
		t.Fatalf("opted-out database = %+v, want a warning", got)
	}
	web, err := rig.svc.SetPolicy(ctx, "web", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if web.DatabaseImage || web.Warning() != "" {
		t.Fatalf("web = %+v, want no warning", web)
	}
	if _, err := rig.svc.SetPolicy(ctx, "ghost", false, true); !errors.Is(err, ErrAppdataContainerNotFound) {
		t.Fatalf("SetPolicy(ghost) = %v, want ErrAppdataContainerNotFound", err)
	}
	stored, _ := rig.policies.ListAppdataPolicies(ctx)
	if len(stored) != 2 {
		t.Fatalf("stored policies = %v, want db and web only", stored)
	}
}

func TestAppdataRun_ExcludedContainerIsNotArchivedUnlessNamed(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"a": "1"})
	rig.addApp(t, "beta", "radarr", "running", map[string]string{"b": "1"})
	if _, err := rig.svc.SetPolicy(context.Background(), "beta", true, false); err != nil {
		t.Fatal(err)
	}
	if err := rig.run(t); err != nil {
		t.Fatal(err)
	}
	if got := rig.archives(t, rig.poolDir); len(got) != 1 || !strings.Contains(got[0], "-alpha-") {
		t.Fatalf("archives = %v, want alpha only", got)
	}
	if _, err := rig.svc.ScopeNames(context.Background(), []string{"ghost"}); !errors.Is(err, ErrAppdataContainerNotFound) {
		t.Fatalf("ScopeNames(ghost) = %v", err)
	}
}

func TestAppdataRun_ContainerThatWillNotStopIsNotArchivedButEveryoneRestarts(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"a": "1"})
	rig.addApp(t, "beta", "radarr", "running", map[string]string{"b": "1"})
	rig.engine.FailOn("stop", "beta", errors.New("stuck"))

	err := rig.run(t)
	if err == nil || !strings.Contains(err.Error(), "beta") || !strings.Contains(err.Error(), "stopping the container") {
		t.Fatalf("Run = %v, want a failure naming beta", err)
	}
	if got := rig.archives(t, rig.poolDir); len(got) != 1 || !strings.Contains(got[0], "-alpha-") {
		t.Fatalf("archives = %v, want alpha only", got)
	}
	events := strings.Join(rig.containers.Events(), ",")
	if !strings.Contains(events, "start beta") || !strings.Contains(events, "start alpha") {
		t.Fatalf("not every attempted container was started again: %s", events)
	}
	if _, err := os.Stat(rig.svc.JournalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal left behind although every start succeeded: %v", err)
	}
}

func TestAppdataRun_CancelledRunStillStartsEveryContainerItStopped(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"a": "1"})
	rig.addApp(t, "beta", "radarr", "running", map[string]string{"b": "1"})
	ctx, cancel := context.WithCancel(context.Background())
	rig.containers.onStop = func(name string) {
		if name == "beta" {
			cancel()
		}
	}
	err := runNamed(ctx, rig.svc, rig.out)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if got, want := strings.Join(rig.containers.Events(), ","), "stop alpha,stop beta,start beta,start alpha"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
	if got := rig.archives(t, rig.poolDir); len(got) != 0 {
		t.Fatalf("a cancelled run uploaded %v", got)
	}
	for _, name := range []string{"alpha", "beta"} {
		c, _ := rig.engine.Inspect(ctx, name)
		if c.State != "running" {
			t.Fatalf("%s is %s after the cancelled run, want running", name, c.State)
		}
	}
}

func TestAppdataRun_FailedRestartKeepsTheJournalAndRecoveryStartsIt(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"a": "1"})
	rig.addApp(t, "beta", "radarr", "running", map[string]string{"b": "1"})
	rig.engine.FailOn("start", "alpha", errors.New("engine hiccup"))

	err := rig.run(t)
	if err == nil || !strings.Contains(err.Error(), "starting alpha again") {
		t.Fatalf("Run = %v, want the failed restart reported", err)
	}
	beta, _ := rig.engine.Inspect(context.Background(), "beta")
	if beta.State != "running" {
		t.Fatalf("beta = %s; a failed start of alpha must not stop beta's restart", beta.State)
	}
	raw := readFile(t, rig.svc.JournalPath)
	if !strings.Contains(raw, "alpha") || strings.Contains(raw, "beta") {
		t.Fatalf("journal = %s, want alpha only", raw)
	}

	rig.engine.FailOn("start", "alpha", nil)
	if err := rig.svc.RecoverStopped(context.Background()); err != nil {
		t.Fatalf("RecoverStopped: %v", err)
	}
	alpha, _ := rig.engine.Inspect(context.Background(), "alpha")
	if alpha.State != "running" {
		t.Fatalf("alpha = %s after recovery, want running", alpha.State)
	}
	if _, err := os.Stat(rig.svc.JournalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal survived a completed recovery: %v", err)
	}
}

func TestAppdataRecoverStopped_WaitsForTheArrayAndForARunningBackup(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "sonarr", "exited", map[string]string{"a": "1"})
	if err := rig.svc.writeJournal([]string{"alpha", "gone"}); err != nil {
		t.Fatal(err)
	}

	rig.halted = true
	if err := rig.svc.RecoverStopped(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ev := rig.containers.Events(); len(ev) != 0 {
		t.Fatalf("containers started while the array was stopped: %v", ev)
	}

	rig.halted = false
	rig.svc.runMu.Lock()
	if err := rig.svc.RecoverStopped(context.Background()); err != nil {
		t.Fatal(err)
	}
	rig.svc.runMu.Unlock()
	if ev := rig.containers.Events(); len(ev) != 0 {
		t.Fatalf("recovery started containers under a running backup: %v", ev)
	}

	if err := rig.svc.RecoverStopped(context.Background()); err != nil {
		t.Fatalf("RecoverStopped: %v", err)
	}
	alpha, _ := rig.engine.Inspect(context.Background(), "alpha")
	if alpha.State != "running" {
		t.Fatalf("alpha = %s, want running", alpha.State)
	}
	if _, err := os.Stat(rig.svc.JournalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal survived: %v", err)
	}
}

// A second run that reaches the service while another is stopping and
// copying waits its turn: it never fails as busy, and it takes nothing
// (not the staging directory, not the journal) until the first is done.
func TestAppdataRun_ASecondRunWaitsForTheFirst(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"a": "1"})
	rig.addApp(t, "beta", "radarr", "running", map[string]string{"b": "1"})
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	rig.containers.onStop = func(string) {
		once.Do(func() { close(entered) })
		<-release
	}
	first := make(chan error, 1)
	go func() {
		first <- runNamed(context.Background(), rig.svc, &bytes.Buffer{}, "alpha")
	}()
	<-entered
	second := make(chan error, 1)
	go func() {
		second <- runNamed(context.Background(), rig.svc, &bytes.Buffer{}, "beta")
	}()

	time.Sleep(200 * time.Millisecond)
	if got := strings.Join(rig.containers.Events(), ","); got != "stop alpha" {
		t.Fatalf("events while the first run holds the service = %s, want the second to have done nothing", got)
	}
	select {
	case err := <-second:
		t.Fatalf("the second run returned %v while the first was still running", err)
	default:
	}
	if _, err := os.Stat(filepath.Join(rig.cache, appdataStagingDir)); err != nil {
		t.Fatalf("the first run's staging directory is gone: %v", err)
	}

	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second Run: %v", err)
	}
	rig.archiveFor(t, rig.poolDir, "alpha")
	rig.archiveFor(t, rig.poolDir, "beta")
	if _, err := os.Stat(rig.svc.JournalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal left behind: %v", err)
	}
}

// Several runs of different containers arriving together all succeed: each
// keeps its own archive, every container it stopped is started again, and
// no journal entry is lost or left behind.
func TestAppdataRun_ConcurrentRunsOfDifferentContainersAllSucceed(t *testing.T) {
	rig := newAppdataRig(t)
	names := []string{"alpha", "beta", "gamma", "delta"}
	for _, n := range names {
		rig.addApp(t, n, n+"-image", "running", map[string]string{"data": n})
	}
	errs := make(chan error, len(names))
	for _, n := range names {
		go func() {
			errs <- runNamed(context.Background(), rig.svc, &bytes.Buffer{}, n)
		}()
	}
	for range names {
		if err := <-errs; err != nil {
			t.Fatalf("a concurrent Run failed: %v", err)
		}
	}
	for _, n := range names {
		rig.archiveFor(t, rig.poolDir, n)
		if c, _ := rig.engine.Inspect(context.Background(), n); c.State != "running" {
			t.Fatalf("%s = %s after the runs, want running", n, c.State)
		}
	}
	if _, err := os.Stat(rig.svc.JournalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal left behind: %v", err)
	}
}

// RecoverStopped starts containers while it holds the service, and is not a
// job the scheduler could queue: a run that arrives meanwhile waits for it
// instead of failing.
func TestAppdataRun_WaitsForARecoveryInsteadOfFailing(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "sonarr", "exited", map[string]string{"a": "1"})
	rig.addApp(t, "beta", "radarr", "running", map[string]string{"b": "1"})
	if err := rig.svc.writeJournal([]string{"alpha"}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	rig.containers.onStart = func(string) {
		close(entered)
		<-release
	}
	recovered := make(chan error, 1)
	go func() { recovered <- rig.svc.RecoverStopped(context.Background()) }()
	<-entered
	rig.containers.mu.Lock()
	rig.containers.onStart = nil
	rig.containers.mu.Unlock()

	ran := make(chan error, 1)
	go func() {
		ran <- runNamed(context.Background(), rig.svc, &bytes.Buffer{}, "beta")
	}()
	time.Sleep(200 * time.Millisecond)
	select {
	case err := <-ran:
		t.Fatalf("Run returned %v while the recovery was still starting containers", err)
	default:
	}
	close(release)
	if err := <-recovered; err != nil {
		t.Fatalf("RecoverStopped: %v", err)
	}
	if err := <-ran; err != nil {
		t.Fatalf("Run after the recovery: %v", err)
	}
	rig.archiveFor(t, rig.poolDir, "beta")
	if _, err := os.Stat(rig.svc.JournalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal left behind: %v", err)
	}
}

// A run cancelled while it waits stops nothing once its turn comes.
func TestAppdataRun_ACancelledWaitingRunDoesNothing(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"a": "1"})
	rig.addApp(t, "beta", "radarr", "running", map[string]string{"b": "1"})
	entered := make(chan struct{})
	release := make(chan struct{})
	rig.containers.onStop = func(string) {
		close(entered)
		<-release
	}
	first := make(chan error, 1)
	go func() {
		first <- runNamed(context.Background(), rig.svc, &bytes.Buffer{}, "alpha")
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	second := make(chan error, 1)
	go func() { second <- runNamed(ctx, rig.svc, &bytes.Buffer{}, "beta") }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := <-second; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiting Run = %v, want context.Canceled", err)
	}
	if got := strings.Join(rig.containers.Events(), ","); got != "stop alpha,start alpha" {
		t.Fatalf("events = %s, want only the first run's", got)
	}
}

func TestAppdataRun_NothingToDoWithoutAppdataOrCache(t *testing.T) {
	rig := newAppdataRig(t)
	rig.svc.Roots = func(context.Context) ([]string, error) { return nil, nil }
	if err := rig.run(t); err != nil {
		t.Fatalf("Run without a cache disk: %v", err)
	}
	if !strings.Contains(rig.out.String(), "no cache disk") {
		t.Fatalf("output = %q", rig.out)
	}

	rig2 := newAppdataRig(t)
	rig2.engine.AddContainer(container.Container{ID: "m", Name: "media", Image: "plex", State: "running",
		Mounts: []container.Mount{{Source: t.TempDir(), Destination: "/media"}}})
	if err := rig2.run(t); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rig2.out.String(), "no container with appdata") {
		t.Fatalf("output = %q", rig2.out)
	}
	if ev := rig2.containers.Events(); len(ev) != 0 {
		t.Fatalf("a container with no appdata was stopped: %v", ev)
	}
}

func TestAppdataRun_StoppedContainerIsArchivedWithoutBeingStartedByTheRun(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "sonarr", "exited", map[string]string{"a": "1"})
	if err := rig.run(t); err != nil {
		t.Fatal(err)
	}
	if ev := rig.containers.Events(); len(ev) != 0 {
		t.Fatalf("a container that was already stopped was touched: %v", ev)
	}
	hdr, _, err := verifyAppdata(rig.archiveFor(t, rig.poolDir, "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if !hdr.Stopped {
		t.Fatalf("header = %+v, want Stopped", hdr)
	}
}

func TestAppdataRun_EncryptedDestinationGetsAnAgeArchiveOpenedWithThePassphrase(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"secret.txt": "top secret"})
	enc := filepath.Join(rig.root, "encrypted")
	if err := rig.store.CreateDestination(context.Background(), Destination{
		ID: "enc", Name: "Encrypted", Type: TypeLocal, Path: enc, Enabled: true, Encrypt: true,
		Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6},
	}); err != nil {
		t.Fatal(err)
	}
	if err := rig.store.DeleteDestination(context.Background(), DefaultPoolID); err != nil {
		t.Fatal(err)
	}
	if err := rig.run(t); err != nil {
		t.Fatalf("Run: %v", err)
	}
	names := rig.archives(t, enc)
	if len(names) != 2 {
		t.Fatalf("encrypted destination holds %v, want the archive and its sidecar", names)
	}
	var archive, sidecar string
	for _, n := range names {
		if strings.HasSuffix(n, identitySidecarSuffix) {
			sidecar = filepath.Join(enc, n)
		} else {
			archive = filepath.Join(enc, n)
		}
	}
	if !strings.HasSuffix(archive, ".tar.zst.age") {
		t.Fatalf("archive = %s", archive)
	}
	plain, err := decryptArchiveWithPassphrase(archive, sidecar, "backup-pass")
	if err != nil {
		t.Fatalf("decrypting with the passphrase: %v", err)
	}
	if bytes.Contains(readBytes(t, archive), []byte("top secret")) {
		t.Fatal("plaintext found in the encrypted archive")
	}
	path := filepath.Join(t.TempDir(), "plain.tar.zst")
	if err := os.WriteFile(path, plain, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, trailer, err := verifyAppdata(path); err != nil || trailer.Files != 1 {
		t.Fatalf("decrypted archive: trailer %+v, err %v", trailer, err)
	}
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAppdataRun_RemoteDestinationIsEncryptedAndFailureFailsTheRun(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"a": "1"})
	ctx := context.Background()
	if _, err := rig.remoteRig.svc.AddDestination(ctx, s3Request()); err != nil {
		t.Fatal(err)
	}
	if err := rig.run(t); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var remoteNames []string
	for key := range rig.rclone.Files() {
		remoteNames = append(remoteNames, key)
		if strings.HasSuffix(key, ".tar.zst") {
			t.Fatalf("a plaintext appdata archive reached the remote: %s", key)
		}
	}
	if len(remoteNames) != 2 {
		t.Fatalf("remote holds %v, want the encrypted archive and its sidecar", remoteNames)
	}

	firstWeek := rig.archives(t, rig.poolDir)
	for _, n := range firstWeek {
		if err := os.Chtimes(filepath.Join(rig.poolDir, n), rig.now, rig.now); err != nil {
			t.Fatal(err)
		}
	}
	rig.rclone.Fail = map[string]error{"copy": errors.New("remote down")}
	rig.now = rig.now.Add(7 * 24 * time.Hour)
	err := rig.run(t)
	if err == nil || !strings.Contains(err.Error(), "remote down") {
		t.Fatalf("Run with a failing remote = %v, want the failure reported", err)
	}
	if got := rig.archives(t, rig.poolDir); len(got) != 2 {
		t.Fatalf("pool archives = %v, want both weeks' archives despite the remote failing", got)
	}
}

func TestPruneAppdata_KeepsSnapshotsApartFromTheTiers(t *testing.T) {
	dir := t.TempDir()
	target := localTarget{dest: Destination{Path: dir}}
	inst := "aaaaaaaaaaaa"
	now := time.Date(2026, 9, 20, 4, 0, 0, 0, time.UTC)
	write := func(name string, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	day := 24 * time.Hour
	for i := 0; i < 4; i++ {
		write(appdataArchiveName(inst, "alpha", now.Add(-time.Duration(i)*7*day), ReasonNone, 0), time.Duration(i)*7*day)
	}
	for i := 0; i < 7; i++ {
		write(appdataArchiveName(inst, "alpha", now.Add(-time.Duration(i)*time.Hour), ReasonPreRestore, 0), time.Duration(i)*time.Hour)
	}
	write(appdataArchiveName(inst, "beta", now.Add(-40*day), ReasonNone, 0), 40*day)
	write(appdataArchiveName("bbbbbbbbbbbb", "alpha", now.Add(-90*day), ReasonNone, 0), 90*day)
	write("unrelated.txt", 100*day)

	newest := appdataArchiveName(inst, "alpha", now, ReasonNone, 0)
	write(newest, 0)
	if err := pruneAppdata(context.Background(), target, inst, Retention{Daily: 2, Weekly: 1, Monthly: 1}, now, "alpha", newest); err != nil {
		t.Fatal(err)
	}

	var ordinary, snaps int
	files, _ := os.ReadDir(dir)
	present := map[string]bool{}
	for _, f := range files {
		present[f.Name()] = true
		if i, c, reason, _, ok := parseAppdataName(f.Name()); ok && i == inst && c == "alpha" {
			if reason == ReasonNone {
				ordinary++
			} else {
				snaps++
			}
		}
	}
	if snaps != preChangeKeepCount {
		t.Fatalf("kept %d pre-restore snapshots, want %d", snaps, preChangeKeepCount)
	}
	if ordinary != 2 {
		t.Fatalf("kept %d ordinary archives, want the 2 newest days", ordinary)
	}
	for _, keep := range []string{"unrelated.txt", appdataArchiveName("bbbbbbbbbbbb", "alpha", now.Add(-90*day), ReasonNone, 0), appdataArchiveName(inst, "beta", now.Add(-40*day), ReasonNone, 0), newest} {
		if !present[keep] {
			t.Fatalf("%s was pruned; retention must only touch this installation's archives of this container", keep)
		}
	}
}

func TestAppdataRun_APausedContainerIsStoppedLikeARunningOne(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "sonarr", "paused", map[string]string{"a": "1"})
	if err := rig.run(t); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := strings.Join(rig.containers.Events(), ","), "stop alpha,start alpha"; got != want {
		t.Fatalf("events = %s, want %s: a paused container's processes are still there", got, want)
	}
}

func TestAppdataRun_KeepsAnEarlierRunsStoppedContainerInTheJournal(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "sonarr", "exited", map[string]string{"a": "1"})
	rig.addApp(t, "beta", "radarr", "running", map[string]string{"b": "1"})
	if err := rig.svc.writeJournal([]string{"alpha"}); err != nil {
		t.Fatal(err)
	}

	if err := rig.run(t); err != nil {
		t.Fatalf("Run: %v\n%s", err, rig.out)
	}
	if got := readFile(t, rig.svc.JournalPath); !strings.Contains(got, "alpha") || strings.Contains(got, "beta") {
		t.Fatalf("journal = %s, want alpha (an earlier run's) and not beta (this run started it)", got)
	}
	if err := rig.svc.RecoverStopped(context.Background()); err != nil {
		t.Fatalf("RecoverStopped: %v", err)
	}
	alpha, _ := rig.engine.Inspect(context.Background(), "alpha")
	if alpha.State != "running" {
		t.Fatalf("alpha = %s after recovery, want running", alpha.State)
	}
	if _, err := os.Stat(rig.svc.JournalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal survived a completed recovery: %v", err)
	}
}

func TestAppdataRun_ARestartThatKeepsFailingStaysJournalledAcrossLaterRuns(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"a": "1"})
	rig.engine.FailOn("start", "alpha", errors.New("engine hiccup"))
	if err := rig.run(t); err == nil {
		t.Fatal("Run succeeded although alpha did not start again")
	}
	if err := rig.run(t); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if got := readFile(t, rig.svc.JournalPath); !strings.Contains(got, "alpha") {
		t.Fatalf("journal = %s, want alpha", got)
	}
	rig.engine.FailOn("start", "alpha", nil)
	if err := rig.svc.RecoverStopped(context.Background()); err != nil {
		t.Fatalf("RecoverStopped: %v", err)
	}
	alpha, _ := rig.engine.Inspect(context.Background(), "alpha")
	if alpha.State != "running" {
		t.Fatalf("alpha = %s after recovery, want running", alpha.State)
	}
}

func TestAppdataRun_ResolvedNamesAreTheWholeScopeAndAContainerAddedLaterIsLeftAlone(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"a": "1"})
	rig.addApp(t, "late", "radarr", "running", map[string]string{"l": "1"})

	err := rig.svc.Run(context.Background(), AppdataRunRequest{Resolved: []string{"alpha"}}, rig.out)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := strings.Join(rig.containers.Events(), ","), "stop alpha,start alpha"; got != want {
		t.Fatalf("events = %s, want %s: a container outside the resolved scope was touched", got, want)
	}
	if got := rig.archives(t, rig.poolDir); len(got) != 1 || !strings.Contains(got[0], "-alpha-") {
		t.Fatalf("archives = %v, want alpha's only", got)
	}
}

func TestAppdataRun_ResolvedSkipsAContainerThatIsGoneOrNoLongerIncluded(t *testing.T) {
	rig := newAppdataRig(t)
	ctx := context.Background()
	rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"a": "1"})
	rig.addApp(t, "beta", "radarr", "running", map[string]string{"b": "1"})
	if _, err := rig.svc.SetPolicy(ctx, "beta", true, false); err != nil {
		t.Fatal(err)
	}

	err := rig.svc.Run(ctx, AppdataRunRequest{Resolved: []string{"alpha", "beta", "gone"}}, rig.out)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := strings.Join(rig.containers.Events(), ","), "stop alpha,start alpha"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
	out := rig.out.String()
	for _, want := range []string{"skipping beta: it is no longer included", "skipping gone: it no longer exists"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q lacks %q", out, want)
		}
	}
	if got := rig.archives(t, rig.poolDir); len(got) != 1 || !strings.Contains(got[0], "-alpha-") {
		t.Fatalf("archives = %v, want alpha's only", got)
	}

	rig.containers.events = nil
	rig.out.Reset()
	err = rig.svc.Run(ctx, AppdataRunRequest{Containers: []string{"beta"}, Resolved: []string{"beta"}}, rig.out)
	if err != nil {
		t.Fatalf("Run of an explicitly requested, excluded container: %v", err)
	}
	if got, want := strings.Join(rig.containers.Events(), ","), "stop beta,start beta"; got != want {
		t.Fatalf("events = %s, want %s: a container the operator asked for by name is still backed up", got, want)
	}
}

func TestAppdataRun_ARunWithoutAResolvedScopeIsRefusedAndTouchesNothing(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"a": "1"})

	if err := rig.svc.Run(context.Background(), AppdataRunRequest{Containers: []string{"alpha"}}, rig.out); err == nil {
		t.Fatal("Run with no resolved containers succeeded")
	}
	if got := rig.containers.Events(); len(got) != 0 {
		t.Fatalf("events = %v, want none", got)
	}
}

func TestAppdataRun_AnEmptyResolvedScopeDoesNothingEvenWhenContainersAreIncludedNow(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"a": "1"})

	if err := rig.svc.Run(context.Background(), AppdataRunRequest{Resolved: []string{}}, rig.out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := rig.containers.Events(); len(got) != 0 {
		t.Fatalf("events = %v, want none", got)
	}
	if got := rig.archives(t, rig.poolDir); len(got) != 0 {
		t.Fatalf("archives = %v, want none", got)
	}
}
