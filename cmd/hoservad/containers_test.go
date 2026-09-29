package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/api/gen/go/events"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// newContainerArrayFixture is the daemon's array sequence built the way
// main.go builds it, with the container service in it, over a fake
// Docker: jellyfin running, portainer already stopped.
func newContainerArrayFixture(t *testing.T) (context.Context, *api.Handler, *container.FakeProvider, *disk.FakeRunner) {
	t.Helper()
	ctx, h, arrays, shares, disks, runner := newArrayTestEnv(t)
	assigned := persistSampleArray(t, arrays)
	presentMatchingDisks(disks, assigned)

	fake := container.NewFakeProvider()
	fake.AddContainer(container.Container{ID: "a", Name: "jellyfin", State: "running"})
	fake.AddContainer(container.Container{ID: "b", Name: "portainer", State: "exited"})
	apps := newContainers(fake, t.TempDir(), arrays, nil)

	seq, err := newArraySequence(ctx, h.Scheduler, arrays, shares, disks, runner, apps.arrayService())
	if err != nil {
		t.Fatalf("newArraySequence: %v", err)
	}
	realCatchAll, ok := seq.CatchAll.(pool.MountController)
	if !ok {
		t.Fatalf("CatchAll is %T, want pool.MountController", seq.CatchAll)
	}
	seq.CatchAll = arrayTestCatchAll{where: pool.CatchAllPath, argv: realCatchAll.Mnt.Argv(), runner: runner}
	isolateDiskCheck(t, seq)
	h.Array = seq
	return ctx, h, fake, runner
}

func containerState(t *testing.T, f *container.FakeProvider, name string) string {
	t.Helper()
	c, err := f.Inspect(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return c.State
}

func TestNewArraySequence_ContainersStopBeforeSambaAndNFS(t *testing.T) {
	_, h, _, _ := newContainerArrayFixture(t)

	var names []string
	for _, s := range h.Array.Services {
		names = append(names, s.Name())
	}
	if got := strings.Join(names, ","); got != "Docker containers,Samba,NFS" {
		t.Fatalf("Services = %s, want containers first (doc 02 §4: containers stop before Samba and NFS)", got)
	}
}

// Stop and start through the real handler and sequence: the running
// container is stopped before any unmount, and the one that was already
// stopped is not started again.
func TestArrayStopAndStart_StopContainersBeforeUnmountAndRestartTheRunningOnes(t *testing.T) {
	ctx, h, fake, runner := newContainerArrayFixture(t)

	if _, err := h.StopArray(ctx, confirmStop()); err != nil {
		t.Fatalf("StopArray: %v", err)
	}
	if s := containerState(t, fake, "jellyfin"); s != "exited" {
		t.Fatalf("jellyfin is %s after array stop, want exited", s)
	}
	var sawUnmount bool
	for _, c := range runner.Calls() {
		if c.Name == "fusermount" {
			sawUnmount = true
		}
	}
	if !sawUnmount {
		t.Fatalf("StopArray never unmounted: %+v", runner.Calls())
	}

	if _, err := h.StartArray(ctx); err != nil {
		t.Fatalf("StartArray: %v", err)
	}
	if s := containerState(t, fake, "jellyfin"); s != "running" {
		t.Fatalf("jellyfin is %s after array start, want running", s)
	}
	if s := containerState(t, fake, "portainer"); s != "exited" {
		t.Fatalf("portainer was stopped before the array stop and is %s after array start, want exited", s)
	}
}

// The data-loss scenario: a container that will not stop must abort the
// array stop before Samba, NFS or any unmount runs — the same contract
// #309 set for Samba and NFS.
func TestArrayStop_AbortsBeforeAnyUnmountWhenAContainerWillNotStop(t *testing.T) {
	ctx, h, fake, runner := newContainerArrayFixture(t)
	fake.FailOn("stop", "jellyfin", errors.New("container is holding a file open"))

	if _, err := h.StopArray(ctx, confirmStop()); err == nil {
		t.Fatal("StopArray succeeded although a container would not stop")
	}
	if calls := runner.Calls(); len(calls) != 0 {
		t.Fatalf("runner calls = %+v; a failed container stop must run nothing else — no Samba stop, no unmount", calls)
	}
	status, err := h.GetStatus(ctx)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if !status.MaintenanceMode.Or(false) {
		t.Fatal("maintenanceMode must stay true after a failed stop")
	}
}

// A rebuild of the array sequence (a share change, a disk-topology change,
// a SIGHUP) must keep the container service: newRebuildArraySequence
// threads it through, and it is the same instance, so the list of
// containers array start still owes a start survives the rebuild.
func TestRebuildArraySequence_KeepsTheContainerService(t *testing.T) {
	ctx, h, arrays, shares, disks, runner := newArrayTestEnv(t)
	assigned := persistSampleArray(t, arrays)
	presentMatchingDisks(disks, assigned)
	fake := container.NewFakeProvider()
	apps := newContainers(fake, t.TempDir(), arrays, nil)

	s := newTestStorageTargetSync(t)
	s.PoolMounted = func(string) (bool, error) { return true, nil }
	rebuild := newRebuildArraySequence(h.Scheduler, arrays, shares, disks, runner, s, nil, h, &acknowledgedDegraded{}, apps.arrayService())
	if err := rebuild(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	seq := h.CurrentArray()
	if seq == nil || len(seq.Services) != 3 || seq.Services[0] != job.ArrayService(apps.Array) {
		t.Fatalf("rebuilt Services = %v, want the shared container service first, then Samba and NFS", seq)
	}
}

// containersWiringHarness is the daemon's real API server on a real Unix
// socket over a fake Docker, with the container wiring main.go performs.
type containersWiringHarness struct {
	client    *http.Client
	fake      *container.FakeProvider
	scheduler *job.Scheduler
	// storageReady is the storage-target readiness the daemon wiring reads.
	storageReady *atomic.Bool
	appdata      string
	stopWatch    context.CancelFunc
	apps         *appServices
	ctx          context.Context
}

// startReconcile starts the start-up reconciliation of interrupted
// recreates the way main.go does, and returns the channel that closes
// when it has finished. The harness does not start it on its own, so a
// test that changes the array state is not raced by it.
func (w *containersWiringHarness) startReconcile() <-chan struct{} {
	return reconcileContainersAtStart(w.ctx, w.apps, w.scheduler.InMaintenance, w.storageReady.Load, 10*time.Millisecond)
}

func newContainersWiringHarness(t *testing.T) *containersWiringHarness {
	t.Helper()
	return newContainersWiringHarnessWith(t, nil)
}

// newContainersWiringHarnessWith scripts the fake Docker through prepare
// before the daemon wiring starts using it.
func newContainersWiringHarnessWith(t *testing.T, prepare func(*container.FakeProvider)) *containersWiringHarness {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	db, err := sql.Open("sqlite", store.DSN(filepath.Join(root, "hoservad.db")))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, _, err := (&store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}).Apply(ctx); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	authStore := api.NewAuthStore(db)
	machineKey, err := auth.LoadOrGenerateMachineKey(ctx, filepath.Join(root, "secret.key"), authStore)
	if err != nil {
		t.Fatalf("machine key: %v", err)
	}
	if _, _, err := api.NewAuthService(authStore, machineKey).CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	// A cache disk is where appdata lives: its appdata directory is the one
	// place Remove may delete under.
	cacheMount := filepath.Join(root, "cache")
	arrayStore := store.NewArrayStore(db)
	if err := arrayStore.PutArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now().UTC()}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", WWN: "wwn-p", Serial: "PARITY1", ByIDName: "wwn-wwn-p", Mountpoint: filepath.Join(root, "parity1")},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d", WWN: "wwn-d", Serial: "DATA1", ByIDName: "wwn-wwn-d", Mountpoint: filepath.Join(root, "disk1")},
		{Role: store.ArrayRoleCache, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-c", WWN: "wwn-c", Serial: "CACHE1", ByIDName: "wwn-wwn-c", Mountpoint: cacheMount},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	appdata := filepath.Join(cacheMount, "appdata", "jellyfin")
	if err := os.MkdirAll(appdata, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appdata, "library.db"), []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}

	fake := container.NewFakeProvider()
	fake.AddContainer(container.Container{
		ID: "a", Name: "jellyfin", Image: "jf", Tag: "10", State: "exited",
		Mounts: []container.Mount{{Source: appdata, Destination: "/config", ReadWrite: true}},
	})
	if prepare != nil {
		prepare(fake)
	}

	registry := job.NewRegistry()
	jobStore := job.NewStore(db)
	scheduler := job.NewScheduler(jobStore, job.NewLogStore(t.TempDir()), job.NewHub(), registry)
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = scheduler.Drain(dctx)
	})

	handler := &api.Handler{Scheduler: scheduler, Store: jobStore, Container: fake}
	apps := newContainers(fake, root, arrayStore, nil)
	storageReady := &atomic.Bool{}
	storageReady.Store(true)
	wireContainers(handler, registry, apps, scheduler.InMaintenance, storageReady.Load)
	wctx, stopWatch := context.WithCancel(ctx)
	t.Cleanup(stopWatch)
	go apps.Watcher.Run(wctx)

	unixServer, err := buildUnixServer(handler, authStore, job.NewHub(), notify.NewHub())
	if err != nil {
		t.Fatalf("buildUnixServer: %v", err)
	}
	sockPath := filepath.Join(root, "hoserva.sock")
	ln, err := setupUnixListener(sockPath)
	if err != nil {
		t.Fatalf("setupUnixListener: %v", err)
	}
	go func() { _ = unixServer.Serve(ln) }()
	t.Cleanup(func() { _ = unixServer.Close() })

	return &containersWiringHarness{
		client: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		}},
		fake:         fake,
		scheduler:    scheduler,
		storageReady: storageReady,
		appdata:      appdata,
		stopWatch:    stopWatch,
		apps:         apps,
		ctx:          wctx,
	}
}

func (w *containersWiringHarness) do(t *testing.T, method, path string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, "http://unix"+apiPathPrefix+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// Every /apps lifecycle operation reaches the container service through
// the real daemon server, never a 501 from a nil Handler field.
func TestContainersWiring_LifecycleOperationsAreReachableOverHTTP(t *testing.T) {
	w := newContainersWiringHarness(t)

	status, body := w.do(t, http.MethodPost, "/apps/jellyfin/start")
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"running"`)) {
		t.Fatalf("POST /apps/jellyfin/start = %d %s, want 200 with the running container", status, body)
	}
	status, body = w.do(t, http.MethodPost, "/apps/jellyfin/restart")
	if status != http.StatusOK {
		t.Fatalf("POST restart = %d %s", status, body)
	}
	status, body = w.do(t, http.MethodGet, "/apps/jellyfin/stats")
	if status != http.StatusOK {
		t.Fatalf("GET stats = %d %s, want 200 for a running container", status, body)
	}
	w.fake.SetLogs("a", "hello from jellyfin\n")
	status, body = w.do(t, http.MethodGet, "/apps/jellyfin/logs?tail=5")
	if status != http.StatusOK || string(body) != "hello from jellyfin\n" {
		t.Fatalf("GET logs = %d %q", status, body)
	}
	status, body = w.do(t, http.MethodPost, "/apps/jellyfin/stop")
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"exited"`)) {
		t.Fatalf("POST stop = %d %s", status, body)
	}
	status, body = w.do(t, http.MethodGet, "/apps/jellyfin/stats")
	if status != http.StatusConflict {
		t.Fatalf("GET stats of a stopped container = %d %s, want 409", status, body)
	}
}

// The data-loss scenario: dockerd is reachable again while the array is
// stopped (docker.socket activation, a manual systemctl start docker), so
// a started container's bind mounts under /mnt/user would resolve to empty
// directories on the boot device. With the array in maintenance mode the
// real daemon server refuses start, restart and recreate, and the Engine
// sees no such call; stop, logs, stats and a plain remove stay allowed.
func TestContainersWiring_StartRestartRecreateRefusedWhileTheArrayIsStopped(t *testing.T) {
	w := newContainersWiringHarness(t)
	if err := w.scheduler.EnterMaintenance(context.Background()); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}

	for _, action := range []string{"start", "restart", "recreate"} {
		status, body := w.do(t, http.MethodPost, "/apps/jellyfin/"+action)
		if status != http.StatusConflict || !bytes.Contains(body, []byte(`"array_stopped"`)) {
			t.Fatalf("POST /apps/jellyfin/%s on a stopped array = %d %s, want 409 array_stopped", action, status, body)
		}
	}
	for _, c := range w.fake.Calls() {
		if c.Op == "start" || c.Op == "restart" || c.Op == "recreate" {
			t.Fatalf("the Engine saw %+v although the array is stopped; calls = %v", c, w.fake.Calls())
		}
	}
	if s := containerState(t, w.fake, "jellyfin"); s != "exited" {
		t.Fatalf("jellyfin is %s after refused starts, want exited", s)
	}

	if status, body := w.do(t, http.MethodGet, "/apps/jellyfin/logs?tail=5"); status != http.StatusOK {
		t.Fatalf("GET logs on a stopped array = %d %s, want 200", status, body)
	}
	if status, body := w.do(t, http.MethodPost, "/apps/jellyfin/stop"); status != http.StatusOK {
		t.Fatalf("POST stop on a stopped array = %d %s, want 200", status, body)
	}
	if status, body := w.do(t, http.MethodDelete, "/apps/jellyfin"); status != http.StatusOK {
		t.Fatalf("DELETE without appdata on a stopped array = %d %s, want 200", status, body)
	}
}

// Maintenance mode is not the only way the array is unusable: storage that
// is not ready (no array, a degraded array not yet acknowledged, a pool
// that has not mounted) leaves /mnt/user just as empty.
func TestContainersWiring_StartRefusedWhileStorageIsNotReady(t *testing.T) {
	w := newContainersWiringHarness(t)
	w.storageReady.Store(false)

	for _, action := range []string{"start", "restart", "recreate"} {
		status, body := w.do(t, http.MethodPost, "/apps/jellyfin/"+action)
		if status != http.StatusConflict || !bytes.Contains(body, []byte(`"array_stopped"`)) {
			t.Fatalf("POST %s with storage not ready = %d %s, want 409 array_stopped", action, status, body)
		}
	}
	if calls := w.fake.Calls(); len(calls) != 0 {
		t.Fatalf("the Engine saw %v although storage is not ready", calls)
	}

	w.storageReady.Store(true)
	if status, body := w.do(t, http.MethodPost, "/apps/jellyfin/start"); status != http.StatusOK {
		t.Fatalf("POST start once storage is ready = %d %s, want 200", status, body)
	}
}

// A recreate job can be queued before an array stop and run after it, or
// be submitted by something other than the API: the job's own runner
// re-checks, so the Engine still sees no recreate.
func TestContainersWiring_RecreateJobRefusesAtRunTimeWhileTheArrayIsStopped(t *testing.T) {
	w := newContainersWiringHarness(t)
	w.storageReady.Store(false)

	j, err := w.scheduler.Submit(context.Background(), job.TypeContainerRecreate, []string{"container:jellyfin"}, []byte(`{"id":"jellyfin"}`))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, body := w.do(t, http.MethodGet, "/jobs/"+j.ID)
		if bytes.Contains(body, []byte(`"failed"`)) {
			break
		}
		if bytes.Contains(body, []byte(`"succeeded"`)) || time.Now().After(deadline) {
			t.Fatalf("the recreate job should fail on a stopped array: %s", body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, c := range w.fake.Calls() {
		if c.Op == "recreate" {
			t.Fatalf("the recreate job reached the Engine on a stopped array: %v", w.fake.Calls())
		}
	}
}

// The daemon wiring fails closed: without the array-state signals nothing
// is started, rather than starting on a guess.
func TestWireContainers_WithoutArrayStateRefusesEveryStart(t *testing.T) {
	fake := container.NewFakeProvider()
	fake.AddContainer(container.Container{ID: "a", Name: "jellyfin", State: "exited"})
	apps := newContainers(fake, t.TempDir(), nil, nil)
	handler := &api.Handler{}
	wireContainers(handler, job.NewRegistry(), apps, nil, nil)

	_, err := handler.StartApp(context.Background(), apiv1.StartAppParams{ID: "jellyfin"})
	if err == nil {
		t.Fatal("StartApp succeeded although the array state is unknown")
	}
	if e := handler.NewError(context.Background(), err); e.StatusCode != http.StatusServiceUnavailable || e.Response.Code != "array_state_unknown" {
		t.Fatalf("StartApp error = %d %s, want 503 array_state_unknown", e.StatusCode, e.Response.Code)
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Fatalf("the Engine saw %v", calls)
	}
}

func TestContainersWiring_RecreateRunsAsAJobRegisteredByTheDaemonWiring(t *testing.T) {
	w := newContainersWiringHarness(t)
	w.startReconcile()

	status, body := w.do(t, http.MethodPost, "/apps/jellyfin/recreate")
	if status == http.StatusNotImplemented {
		t.Fatalf("POST recreate = 501 %s — the job type is not registered or the handler field is nil", body)
	}
	if status != http.StatusOK {
		t.Fatalf("POST recreate = %d %s, want 200 with the queued job", status, body)
	}
	var j struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		Class string `json:"class"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		t.Fatal(err)
	}
	if j.Type != "container_recreate" || j.Class != "service" {
		t.Fatalf("job = %+v, want a container_recreate job in the service class", j)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, body = w.do(t, http.MethodGet, "/jobs/"+j.ID)
		if bytes.Contains(body, []byte(`"succeeded"`)) {
			break
		}
		if bytes.Contains(body, []byte(`"failed"`)) || time.Now().After(deadline) {
			t.Fatalf("job did not succeed: %s", body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var recreated bool
	for _, c := range w.fake.Calls() {
		recreated = recreated || c.Op == "recreate"
	}
	if !recreated {
		t.Fatalf("the job never called the provider's Recreate: %v", w.fake.Calls())
	}
}

// Remove through the real daemon: appdata stays unless deleteAppdata=true,
// and then only what is inside the cache disk's appdata directory goes.
func TestContainersWiring_RemoveKeepsAppdataUnlessAskedTo(t *testing.T) {
	w := newContainersWiringHarness(t)
	library := filepath.Join(w.appdata, "library.db")

	status, body := w.do(t, http.MethodDelete, "/apps/jellyfin")
	if status != http.StatusOK {
		t.Fatalf("DELETE /apps/jellyfin = %d %s", status, body)
	}
	if _, err := os.Stat(library); err != nil {
		t.Fatalf("a remove that did not ask for it deleted the appdata: %v", err)
	}

	w.fake.AddContainer(container.Container{
		ID: "b", Name: "jellyfin", State: "exited",
		Mounts: []container.Mount{{Source: w.appdata, Destination: "/config", ReadWrite: true}},
	})
	status, body = w.do(t, http.MethodDelete, "/apps/jellyfin?deleteAppdata=true")
	if status != http.StatusOK || !bytes.Contains(body, []byte("jellyfin")) {
		t.Fatalf("DELETE ?deleteAppdata=true = %d %s, want 200 listing the deleted directory", status, body)
	}
	if _, err := os.Stat(w.appdata); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("appdata still exists after an explicit request to delete it: %v", err)
	}
}

// The data-loss scenario through the real daemon: with the array stopped,
// or its storage not ready, the cache disk is not mounted, so a remove that
// deletes appdata would remove the container, report success and leave the
// real appdata on the disk. DELETE ?deleteAppdata=true is refused with 409
// array_stopped; the Engine sees no remove and the appdata is untouched.
// The same DELETE without deleteAppdata is still allowed.
func TestContainersWiring_RemoveWithAppdataRefusedWhileTheArrayIsStopped(t *testing.T) {
	for _, tc := range []struct {
		name string
		down func(*containersWiringHarness)
	}{
		{"maintenance mode", func(w *containersWiringHarness) {
			if err := w.scheduler.EnterMaintenance(context.Background()); err != nil {
				t.Fatalf("EnterMaintenance: %v", err)
			}
		}},
		{"storage not ready", func(w *containersWiringHarness) { w.storageReady.Store(false) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newContainersWiringHarness(t)
			tc.down(w)

			status, body := w.do(t, http.MethodDelete, "/apps/jellyfin?deleteAppdata=true")
			if status != http.StatusConflict || !bytes.Contains(body, []byte(`"array_stopped"`)) {
				t.Fatalf("DELETE ?deleteAppdata=true on a stopped array = %d %s, want 409 array_stopped", status, body)
			}
			if calls := w.fake.Calls(); len(calls) != 0 {
				t.Fatalf("the Engine saw %v although the remove was refused", calls)
			}
			if _, err := os.Stat(filepath.Join(w.appdata, "library.db")); err != nil {
				t.Fatalf("appdata touched by a refused remove: %v", err)
			}

			if status, body := w.do(t, http.MethodDelete, "/apps/jellyfin"); status != http.StatusOK {
				t.Fatalf("DELETE without appdata on a stopped array = %d %s, want 200", status, body)
			}
			if _, err := os.Stat(filepath.Join(w.appdata, "library.db")); err != nil {
				t.Fatalf("appdata deleted by a remove that did not ask for it: %v", err)
			}
		})
	}
}

// A container killed from outside Hoserva arrives on the daemon's real
// /events stream as a container_state event, through the Watcher main.go
// starts — not by anything polling the container.
func TestContainersWiring_KilledContainerArrivesOnTheEventStream(t *testing.T) {
	w := newContainersWiringHarness(t)
	if err := w.fake.Start(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix"+apiPathPrefix+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	got := make(chan events.Event, 1)
	go func() {
		r := events.NewReader(resp.Body)
		for {
			ev, err := r.Next()
			if err != nil {
				return
			}
			if ev.IsContainerStateEvent() {
				got <- ev
				return
			}
		}
	}()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if err := w.fake.Kill("a"); err != nil {
			t.Fatal(err)
		}
		select {
		case ev := <-got:
			data := ev.ContainerStateEvent.Data
			if data.Name != "jellyfin" || data.State != events.AppStateExited {
				t.Fatalf("event = %+v, want jellyfin exited", data)
			}
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("no container_state event on /events for a killed container")
}

func TestNewContainers_UnhealthyTransitionPublishesTheNotification(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	migrations, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", store.DSN(filepath.Join(root, "n.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, _, err := (&store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}).Apply(ctx); err != nil {
		t.Fatal(err)
	}
	authStore := api.NewAuthStore(db)
	key, err := auth.LoadOrGenerateMachineKey(ctx, filepath.Join(root, "secret.key"), authStore)
	if err != nil {
		t.Fatal(err)
	}
	hub := notify.NewHub()
	svc := notify.NewService(notify.NewStore(db), key, nil)
	svc.Hub = hub
	alerts, unsub := hub.Subscribe()
	defer unsub()

	fake := container.NewFakeProvider()
	fake.AddContainer(container.Container{ID: "a", Name: "jellyfin", State: "running"})
	apps := newContainers(fake, root, store.NewArrayStore(db), svc)
	apps.Watcher.Retry = 10 * time.Millisecond
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go apps.Watcher.Run(wctx)

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if err := fake.SetHealth("a", container.HealthHealthy); err != nil {
			t.Fatal(err)
		}
		if err := fake.SetHealth("a", container.HealthUnhealthy); err != nil {
			t.Fatal(err)
		}
		select {
		case a := <-alerts:
			if a.EventType != notify.EventContainerUnhealthy || !strings.Contains(a.Title, "jellyfin") {
				t.Fatalf("alert = %+v, want a container_unhealthy alert naming jellyfin", a)
			}
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("no container_unhealthy notification for a container that became unhealthy")
}
