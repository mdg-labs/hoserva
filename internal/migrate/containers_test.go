package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/store"
)

type nopCipher struct{}

func (nopCipher) Encrypt(p []byte) ([]byte, error) { return append([]byte{}, p...), nil }
func (nopCipher) Decrypt(c []byte) ([]byte, error) { return append([]byte{}, c...), nil }

// flowRig is a scanned session over the inventory fixture with the real stack
// layer (container.StackService) over #67's fakes: a scripted `docker compose`
// and a scripted Engine.
type flowRig struct {
	s         *Service
	db        *sql.DB
	jobs      fakeJobs
	fake      *container.FakeProvider
	runner    *container.FakeRunner
	stacks    *container.StackService
	stackRoot string
	dataRoot  string
	running   bool
	ready     bool
	submits   []string
}

func newFlowRig(t *testing.T, mutate func(map[string][]byte)) *flowRig {
	t.Helper()
	files, spec := flashTree(t, inventoryVariant)
	if mutate != nil {
		mutate(files)
	}
	sessions, db := newSessionsDB(t)
	r := &flowRig{db: db, jobs: fakeJobs{}, fake: container.NewFakeProvider(), runner: container.NewFakeRunner(), stackRoot: filepath.Join(t.TempDir(), "stacks"), dataRoot: t.TempDir(), running: true, ready: true}
	r.s = &Service{Dir: filepath.Join(t.TempDir(), "migrate"), Scanner: scanner(fixtureDisks(spec)), Sessions: sessions, JobEnded: r.jobs.ended}
	if err := scanNow(r.s, zipOf(t, files, false), ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	r.stacks = &container.StackService{
		Store:  store.NewStackStore(db),
		Cipher: nopCipher{},
		Runner: r.runner,
		Root:   r.stackRoot,
		RequireArrayRunning: func() error {
			if !r.running {
				return container.ErrArrayStopped
			}
			return nil
		},
		Provider: r.fake,
	}
	r.s.Stacks = r.stacks
	r.s.DataRoots = []string{r.dataRoot}
	r.s.Initialized = func(context.Context) (bool, error) { return r.ready, nil }
	t.Cleanup(func() { _ = db.Close() })
	return r
}

func (r *flowRig) submit(_ context.Context, stack string) (string, error) {
	r.submits = append(r.submits, stack)
	return fmt.Sprintf("job-%d", len(r.submits)), nil
}

func (r *flowRig) confirm(stack string, accept bool) error {
	_, err := r.s.ConfirmContainer(ctx0, stack, accept)
	return err
}

func (r *flowRig) create(t *testing.T, names ...string) []StackResult {
	t.Helper()
	var sel []StackSelection
	for _, n := range names {
		sel = append(sel, StackSelection{Name: n, Acknowledged: true})
	}
	res, err := r.s.CreateStacks(ctx0, sel)
	if err != nil {
		t.Fatalf("CreateStacks(%v) = %v", names, err)
	}
	for _, x := range res {
		if x.Err != nil {
			t.Fatalf("creating %s: %v", x.Name, x.Err)
		}
	}
	return res
}

func (r *flowRig) addContainer(stack, state string, sources ...string) string {
	id := "id-" + stack
	c := container.Container{ID: id, Name: stack, State: state, Labels: map[string]string{
		"com.docker.compose.project":             stack,
		"com.docker.compose.project.working_dir": filepath.Join(r.stackRoot, stack),
	}}
	for _, src := range sources {
		c.Mounts = append(c.Mounts, container.Mount{Source: src, Destination: "/data", ReadWrite: true})
	}
	r.fake.AddContainer(c)
	return id
}

func (r *flowRig) stackNames(t *testing.T) []string {
	t.Helper()
	list, err := r.stacks.List(ctx0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, st := range list {
		out = append(out, st.Name)
	}
	return out
}

func (r *flowRig) mkData(t *testing.T, rel string, files ...string) string {
	t.Helper()
	dir := filepath.Join(r.dataRoot, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func (r *flowRig) composeUps() int {
	n := 0
	for _, c := range r.runner.Calls() {
		for _, a := range c.Args {
			if a == "up" {
				n++
			}
		}
	}
	return n
}

func stateOf(t *testing.T, r *flowRig, stack string) MigratedStack {
	t.Helper()
	flow, err := r.s.flowOf(ctx0)
	if err != nil {
		t.Fatal(err)
	}
	i := flow.index(stack)
	if i < 0 {
		t.Fatalf("stack %s is not in the migration's record: %+v", stack, flow.Stacks)
	}
	return flow.Stacks[i]
}

// The pre-selection is exactly Unraid's autostart list, in its order, and
// nothing else: the other installed and the template-only templates are listed
// and not selected, a Compose Manager project is its own group offered with its
// own compose.yaml, and a container made by hand is listed with its image and
// nothing is generated for it.
func TestContainers_OfferPreselectsTheAutostartListInItsOrderAndGroupsTheRest(t *testing.T) {
	r := newFlowRig(t, nil)
	r.ready = false
	o, err := r.s.Offer(ctx0)
	if err != nil {
		t.Fatal(err)
	}
	if o.ParityInitialized {
		t.Error("the offer says the parity initialisation was confirmed")
	}
	var pre, order []string
	for _, tm := range o.Templates {
		order = append(order, fmt.Sprintf("%s:%s", tm.Entry.Class, templateKey(tm.Entry)))
		if tm.Preselected {
			pre = append(pre, templateKey(tm.Entry))
		}
	}
	if want := []string{"my-notes.xml", "my-Photos.xml", "my-mediaserver.xml"}; strings.Join(pre, ",") != strings.Join(want, ",") {
		t.Errorf("pre-selected = %v, want the autostart list in its order %v", pre, want)
	}
	wantOrder := "autostart:my-notes.xml autostart:my-Photos.xml autostart:my-mediaserver.xml running:my-gateway.xml stopped:my-syncer.xml template_only:my-old-thing.xml template_only:my-unused-tool.xml"
	if got := strings.Join(order, " "); got != wantOrder {
		t.Errorf("groups = %q, want %q", got, wantOrder)
	}
	if len(o.Projects) != 1 || o.Projects[0].Project.Name != "stack" || !o.Projects[0].Creatable || o.Projects[0].Stack != "stack" {
		t.Errorf("projects = %+v, want the Compose Manager project offered with its own compose.yaml", o.Projects)
	}
	if len(o.ByHand) != 1 || o.ByHand[0].Name != "handmade" || o.ByHand[0].Image != "fixture/handmade:latest" {
		t.Errorf("by hand = %+v, want handmade with its image", o.ByHand)
	}
	if len(o.Stacks) != 0 || o.Next != "" || o.Awaiting != "" {
		t.Errorf("stacks = %+v next %q awaiting %q before anything is created", o.Stacks, o.Next, o.Awaiting)
	}
	if got := r.stackNames(t); len(got) != 0 {
		t.Errorf("listing the offer created %v", got)
	}
}

func TestContainers_NothingIsPreselectedWithoutACapture(t *testing.T) {
	r := newFlowRig(t, func(f map[string][]byte) {
		delete(f, "config/hoserva/containers.json")
		delete(f, "config/hoserva/capture.json")
	})
	o, err := r.s.Offer(ctx0)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Templates) == 0 {
		t.Fatal("no template offered")
	}
	for _, tm := range o.Templates {
		if tm.Preselected || tm.Entry.Class != ClassUnknown {
			t.Errorf("%s: class %s pre-selected %v, want unknown and not pre-selected without the capture", templateKey(tm.Entry), tm.Entry.Class, tm.Preselected)
		}
	}
	if len(o.ByHand) != 0 {
		t.Errorf("by hand = %+v without a container list", o.ByHand)
	}
}

func TestContainers_ATemplateTheConverterCannotReadIsNeitherPreselectedNorCreatable(t *testing.T) {
	r := newFlowRig(t, func(f map[string][]byte) {
		f[tmplDir+"my-dbtool.xml"] = []byte(`<?xml version="1.0"?><Container version="2"><Name>dbtool</Name><Repository>fixture/dbtool:1</Repository><Overview>` + strings.Repeat("x", 60*1024) + `</Overview></Container>`)
	})
	o, err := r.s.Offer(ctx0)
	if err != nil {
		t.Fatal(err)
	}
	for _, tm := range o.Templates {
		if templateKey(tm.Entry) == "my-dbtool.xml" && (tm.Creatable || tm.Preselected) {
			t.Errorf("my-dbtool.xml = %+v, want it listed and neither creatable nor pre-selected", tm)
		}
	}
	_, err = r.s.CreateStacks(ctx0, []StackSelection{{Name: "my-dbtool.xml", Acknowledged: true}})
	if !errors.Is(err, ErrTemplateUnconvertible) {
		t.Errorf("CreateStacks(my-dbtool.xml) = %v, want ErrTemplateUnconvertible", err)
	}
	if got := r.stackNames(t); len(got) != 0 {
		t.Errorf("created %v", got)
	}
}

// Before the parity initialisation is confirmed nothing is created, started,
// checked or confirmed: the refusal comes first, ahead of everything else, and a
// failure to read the state refuses as well.
func TestContainers_NothingHappensBeforeTheParityInitialisationIsConfirmed(t *testing.T) {
	r := newFlowRig(t, nil)
	r.create(t, "my-Photos.xml")
	r.ready = false

	check := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, ErrParityNotInitialized) {
			t.Errorf("%s = %v, want ErrParityNotInitialized", name, err)
		}
	}
	_, err := r.s.CreateStacks(ctx0, []StackSelection{{Name: "my-mediaserver.xml"}})
	check("CreateStacks", err)
	_, err = r.s.CreateStacks(ctx0, nil)
	check("CreateStacks with no selection", err)
	check("StartContainer", r.s.StartContainer(ctx0, "photos", r.submit))
	check("StartContainer of an unknown stack", r.s.StartContainer(ctx0, "nothing", r.submit))
	_, err = r.s.CheckContainer(ctx0, "photos")
	check("CheckContainer", err)
	check("ConfirmContainer", r.confirm("photos", false))
	if len(r.submits) != 0 || r.composeUps() != 0 {
		t.Errorf("a start was queued (%v) or compose up ran before the point of no return", r.submits)
	}
	if got := strings.Join(r.stackNames(t), ","); got != "photos" {
		t.Errorf("stacks = %q, want only the one created beforehand", got)
	}

	r.s.Initialized = func(context.Context) (bool, error) { return false, errors.New("database is locked") }
	if _, err = r.s.CreateStacks(ctx0, []StackSelection{{Name: "my-mediaserver.xml"}}); err == nil || errors.Is(err, ErrParityNotInitialized) || !strings.Contains(err.Error(), "database is locked") {
		t.Errorf("CreateStacks with the state unreadable = %v, want the read failure", err)
	}
	if err := r.s.StartContainer(ctx0, "photos", r.submit); err == nil || len(r.submits) != 0 {
		t.Errorf("StartContainer with the state unreadable = %v (submits %v), want a refusal", err, r.submits)
	}

	r.s.Initialized = nil
	if _, err := r.s.CreateStacks(ctx0, []StackSelection{{Name: "my-mediaserver.xml"}}); !errors.Is(err, ErrContainersNotConfigured) {
		t.Errorf("CreateStacks with no way to tell = %v, want ErrContainersNotConfigured", err)
	}
}

func TestContainers_NoReportIsRefusedAfterTheParityCheck(t *testing.T) {
	sessions := newSessions(t)
	s := &Service{Dir: filepath.Join(t.TempDir(), "migrate"), Sessions: sessions, Stacks: &container.StackService{}, Initialized: func(context.Context) (bool, error) { return true, nil }}
	if _, err := s.CreateStacks(ctx0, []StackSelection{{Name: "my-Photos.xml"}}); !errors.Is(err, ErrNoReport) {
		t.Errorf("CreateStacks without a scan = %v, want ErrNoReport", err)
	}
	if _, err := s.Offer(ctx0); !errors.Is(err, ErrNoReport) {
		t.Errorf("Offer without a scan = %v, want ErrNoReport", err)
	}
}

// Everything that can refuse the request as a whole is checked before the first
// stack is made: a template with warnings the request does not acknowledge
// stops the clean one beside it too.
func TestContainers_WarningsNeedAnAcknowledgementPerTemplateAndNothingIsCreatedFirst(t *testing.T) {
	r := newFlowRig(t, nil)
	_, err := r.s.CreateStacks(ctx0, []StackSelection{{Name: "my-Photos.xml"}, {Name: "my-gateway.xml"}})
	if !errors.Is(err, ErrWarningsNotAcknowledged) || !strings.Contains(err.Error(), "my-gateway.xml") {
		t.Fatalf("CreateStacks = %v, want ErrWarningsNotAcknowledged naming my-gateway.xml", err)
	}
	if got := r.stackNames(t); len(got) != 0 {
		t.Fatalf("created %v although the request was refused as a whole", got)
	}

	res, err := r.s.CreateStacks(ctx0, []StackSelection{{Name: "my-Photos.xml"}, {Name: "my-gateway.xml", Acknowledged: true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range res {
		if x.Err != nil || x.AlreadyCreated {
			t.Errorf("result %+v, want created", x)
		}
	}
	if got := strings.Join(r.stackNames(t), ","); got != "gateway,photos" {
		t.Errorf("stacks = %q, want gateway and photos", got)
	}
	st, err := r.stacks.Get(ctx0, "gateway")
	if err != nil || !strings.Contains(st.Compose, "fixture/gateway") {
		t.Errorf("gateway = %+v, %v, want the generated Compose stored", st, err)
	}
	if r.composeUps() != 0 || len(r.submits) != 0 {
		t.Error("creating a stack started it: stacks are created stopped")
	}
	if s := stateOf(t, r, "gateway"); s.State != StackCreated || s.Source != "my-gateway.xml" {
		t.Errorf("gateway's record = %+v", s)
	}
}

func TestContainers_AComposeManagerProjectIsCreatedFromItsOwnComposeYAML(t *testing.T) {
	r := newFlowRig(t, func(f map[string][]byte) {
		f[composeFile] = []byte("services:\n  web:\n    image: fixture/web:1.0\n    privileged: true\n    volumes:\n      - /mnt/user/appdata/stack:/data\n")
	})
	res, err := r.s.CreateStacks(ctx0, []StackSelection{{Name: "stack"}})
	if err != nil || len(res) != 1 || res[0].Err != nil {
		t.Fatalf("CreateStacks(stack) = %+v, %v, want the project created without an acknowledgement: its privileges are shown, it has no conversion warnings", res, err)
	}
	st, err := r.stacks.Get(ctx0, "stack")
	if err != nil || st.Compose != "services:\n  web:\n    image: fixture/web:1.0\n    privileged: true\n    volumes:\n      - /mnt/user/appdata/stack:/data\n" {
		t.Errorf("stack = %+v, %v, want the project's compose.yaml byte for byte", st, err)
	}
	if s := stateOf(t, r, "stack"); s.Kind != KindComposeProject || s.Position != 0 {
		t.Errorf("record = %+v", s)
	}
}

func TestContainers_AProjectWithAVersionKeyIsCreatedFromItsOwnComposeYAML(t *testing.T) {
	body := "version: '3'\nservices:\n  web:\n    image: fixture/web:1.0\n"
	r := newFlowRig(t, func(f map[string][]byte) { f[composeFile] = []byte(body) })
	res, err := r.s.CreateStacks(ctx0, []StackSelection{{Name: "stack"}})
	if err != nil || len(res) != 1 || res[0].Err != nil {
		t.Fatalf("CreateStacks(stack) = %+v, %v, want the project created", res, err)
	}
	if st, err := r.stacks.Get(ctx0, "stack"); err != nil || st.Compose != body {
		t.Errorf("stack = %+v, %v, want the project's compose.yaml byte for byte, version included", st, err)
	}
}

// createCounts stands in for the stack layer and counts the stacks asked for.
type createCounts struct {
	StackLayer
	creates int
}

func (l *createCounts) Create(ctx context.Context, n container.NewStack) (container.Stack, error) {
	l.creates++
	return l.StackLayer.Create(ctx, n)
}

func TestContainers_AProjectTheAllowListRefusesIsNeitherCreatableNorCreated(t *testing.T) {
	cases := map[string]string{
		"secrets":        "services:\n  web:\n    image: x\n    secrets: [token]\nsecrets:\n  token:\n    file: /etc/hoserva/key\n",
		"configs":        "services:\n  web:\n    image: x\n    configs: [conf]\nconfigs:\n  conf:\n    file: /var/lib/hoserva/hoserva.db\n",
		"build":          "services:\n  web:\n    build: /root\n",
		"include":        "include:\n  - other.yaml\nservices:\n  web:\n    image: x\n",
		"volumes_from":   "services:\n  web:\n    image: x\n    volumes_from: [\"container:other\"]\n",
		"env_file":       "services:\n  web:\n    image: x\n    env_file: /etc/hoserva/key\n",
		"ipc host":       "services:\n  web:\n    image: x\n    ipc: host\n",
		"network_mode":   "services:\n  web:\n    image: x\n    network_mode: \"container:other\"\n",
		"gpus":           "services:\n  web:\n    image: x\n    gpus: all\n",
		"extends a file": "services:\n  web:\n    extends:\n      file: other.yaml\n      service: web\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			r := newFlowRig(t, func(f map[string][]byte) { f[composeFile] = []byte(body) })
			layer := &createCounts{StackLayer: r.stacks}
			r.s.Stacks = layer

			o, err := r.s.Offer(ctx0)
			if err != nil || len(o.Projects) != 1 || o.Projects[0].Creatable {
				t.Fatalf("Offer = %+v, %v, want the project listed and not creatable", o.Projects, err)
			}
			for _, sel := range []StackSelection{{Name: "stack"}, {Name: "stack", Acknowledged: true}} {
				if _, err := r.s.CreateStacks(ctx0, []StackSelection{sel}); !errors.Is(err, ErrTemplateUnconvertible) {
					t.Errorf("CreateStacks(%+v) = %v, want ErrTemplateUnconvertible", sel, err)
				}
			}
			_, err = r.s.CreateStacks(ctx0, []StackSelection{{Name: "my-Photos.xml", Acknowledged: true}, {Name: "stack"}})
			if !errors.Is(err, ErrTemplateUnconvertible) {
				t.Errorf("CreateStacks(a template and the project) = %v, want ErrTemplateUnconvertible", err)
			}
			if layer.creates != 0 || len(r.stackNames(t)) != 0 {
				t.Errorf("Create called %d times, stacks %v, want none: the whole request is refused first", layer.creates, r.stackNames(t))
			}
		})
	}
}

func TestContainers_AnInvalidSelectionIsRefusedBeforeAnythingIsCreated(t *testing.T) {
	r := newFlowRig(t, func(f map[string][]byte) {
		f[tmplDir+"my-notes-b.xml"] = []byte(`<Container version="2"><Name>Notes</Name><Repository>fixture/notes:2</Repository></Container>`)
	})
	cases := map[string][]StackSelection{
		"empty":                    nil,
		"the same template twice":  {{Name: "my-Photos.xml"}, {Name: "my-Photos.xml"}},
		"two names for one stack":  {{Name: "my-Photos.xml"}, {Name: "my-notes.xml", Acknowledged: true}, {Name: "my-notes-b.xml"}},
		"a name the report lacks":  {{Name: "my-Photos.xml"}, {Name: "nothing.xml"}},
		"a name the report lacks2": {{Name: "my-Photos.xml"}, {Name: "my-ghost.xml"}},
	}
	for name, sel := range cases {
		_, err := r.s.CreateStacks(ctx0, sel)
		switch name {
		case "a name the report lacks", "a name the report lacks2":
			if !errors.Is(err, ErrTemplateNotFound) {
				t.Errorf("%s: err = %v, want ErrTemplateNotFound", name, err)
			}
		default:
			if !errors.Is(err, ErrInvalidSelection) {
				t.Errorf("%s: err = %v, want ErrInvalidSelection", name, err)
			}
		}
	}
	if got := r.stackNames(t); len(got) != 0 {
		t.Errorf("created %v from selections that were refused", got)
	}
}

// A stack that cannot be created leaves the stacks before it created and
// recorded, says which failed, and a second request makes only what is missing.
func TestContainers_AFailureForOneStackLeavesTheOthersCreatedAndTheAnswerSaysSo(t *testing.T) {
	r := newFlowRig(t, nil)
	if _, err := r.stacks.Create(ctx0, container.NewStack{Name: "mediaserver", Compose: "services: {}\n"}); err != nil {
		t.Fatal(err)
	}
	res, err := r.s.CreateStacks(ctx0, []StackSelection{{Name: "my-mediaserver.xml"}, {Name: "my-Photos.xml"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Name != "my-Photos.xml" || res[0].Err != nil || res[1].Name != "my-mediaserver.xml" || !errors.Is(res[1].Err, container.ErrStackExists) {
		t.Fatalf("results = %+v, want photos created first (autostart order) and mediaserver refused as existing", res)
	}
	flow, err := r.s.flowOf(ctx0)
	if err != nil || len(flow.Stacks) != 1 || flow.Stacks[0].Name != "photos" {
		t.Fatalf("record = %+v, %v, want only photos: a stack nobody created through the migration is not recorded", flow, err)
	}

	if _, err := r.stacks.Remove(ctx0, "mediaserver", false); err != nil {
		t.Fatal(err)
	}
	res, err = r.s.CreateStacks(ctx0, []StackSelection{{Name: "my-mediaserver.xml"}, {Name: "my-Photos.xml"}})
	if err != nil || len(res) != 2 || !res[0].AlreadyCreated || res[0].Err != nil || res[1].AlreadyCreated || res[1].Err != nil {
		t.Fatalf("retry = %+v, %v, want photos already created and mediaserver created now", res, err)
	}

	if _, err := r.stacks.Remove(ctx0, "photos", false); err != nil {
		t.Fatal(err)
	}
	res, err = r.s.CreateStacks(ctx0, []StackSelection{{Name: "my-Photos.xml"}})
	if err != nil || len(res) != 1 || res[0].AlreadyCreated || res[0].Err != nil {
		t.Fatalf("after photos was removed = %+v, %v, want it created again", res, err)
	}
	if flow, _ := r.s.flowOf(ctx0); len(flow.Stacks) != 2 {
		t.Errorf("record = %+v, want one entry per stack and none doubled", flow.Stacks)
	}
}

func TestContainers_AStackTheRecordCannotBeWrittenForIsTakenAwayAgain(t *testing.T) {
	files, spec := flashTree(t, inventoryVariant)
	sessions, db := newSessionsDB(t)
	r := &flowRig{fake: container.NewFakeProvider(), runner: container.NewFakeRunner(), stackRoot: filepath.Join(t.TempDir(), "stacks")}
	s := &Service{Dir: filepath.Join(t.TempDir(), "migrate"), Scanner: scanner(fixtureDisks(spec)), Sessions: sessions}
	if err := scanNow(s, zipOf(t, files, false), ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	stacks := &container.StackService{Store: store.NewStackStore(db), Cipher: nopCipher{}, Runner: r.runner, Root: r.stackRoot, Provider: r.fake}
	s.Stacks = stacks
	s.Initialized = func(context.Context) (bool, error) { return true, nil }
	failWrites(t, db)

	res, err := s.CreateStacks(ctx0, []StackSelection{{Name: "my-Photos.xml"}})
	if err != nil || len(res) != 1 || res[0].Err == nil || !strings.Contains(res[0].Err.Error(), "disk full") {
		t.Fatalf("CreateStacks = %+v, %v, want the record's write failure", res, err)
	}
	if list, err := stacks.List(ctx0); err != nil || len(list) != 0 {
		t.Errorf("stacks = %+v, %v, want none: a stack the migration does not know is taken away", list, err)
	}
}

// The order a start is refused in: the parity initialisation, then a stack the
// migration did not create, then one already confirmed, then the array, then
// another stack that is started and not confirmed. A start that was refused
// queued nothing.
func TestContainers_StartsOneAtATimeAndRefusesBeforeQueueing(t *testing.T) {
	r := newFlowRig(t, nil)
	r.create(t, "my-notes.xml", "my-Photos.xml", "my-mediaserver.xml")

	if err := r.s.StartContainer(ctx0, "nothing", r.submit); !errors.Is(err, ErrStackNotMigrated) {
		t.Errorf("start of an unknown stack = %v, want ErrStackNotMigrated", err)
	}
	r.running = false
	if err := r.s.StartContainer(ctx0, "notes", r.submit); !errors.Is(err, container.ErrArrayStopped) {
		t.Errorf("start with the array stopped = %v, want container.ErrArrayStopped", err)
	}
	r.running = true
	if len(r.submits) != 0 {
		t.Fatalf("a refused start queued %v", r.submits)
	}

	if err := r.s.StartContainer(ctx0, "notes", r.submit); err != nil {
		t.Fatalf("start of notes = %v", err)
	}
	if got := stateOf(t, r, "notes"); got.State != StackStarted || got.StartJob != "job-1" || got.Checked {
		t.Errorf("notes = %+v, want started by job-1", got)
	}
	err := r.s.StartContainer(ctx0, "photos", r.submit)
	if !errors.Is(err, ErrContainerUnconfirmed) || !strings.Contains(err.Error(), "notes") || len(r.submits) != 1 {
		t.Fatalf("start of photos while notes' job runs = %v (submits %v), want ErrContainerUnconfirmed", err, r.submits)
	}

	r.jobs["job-1"] = "succeeded"
	id := r.addContainer("notes", "running")
	if err := r.s.StartContainer(ctx0, "photos", r.submit); !errors.Is(err, ErrContainerUnconfirmed) {
		t.Fatalf("start of photos while notes runs unconfirmed = %v, want ErrContainerUnconfirmed", err)
	}
	if o, err := r.s.Offer(ctx0); err != nil || o.Awaiting != "notes" || o.Next != "" {
		t.Errorf("offer = %+v, %v, want notes awaiting and nothing offered next", o, err)
	}

	if err := r.fake.Stop(ctx0, id); err != nil {
		t.Fatal(err)
	}
	if o, err := r.s.Offer(ctx0); err != nil || o.Awaiting != "" || o.Next != "photos" {
		t.Errorf("offer after notes was stopped = %+v, %v, want photos offered, not the stopped and unconfirmed notes", o, err)
	}
	if err := r.s.StartContainer(ctx0, "photos", r.submit); err != nil {
		t.Fatalf("start of photos after notes was stopped = %v", err)
	}
	if len(r.submits) != 2 || r.submits[1] != "photos" {
		t.Errorf("submits = %v", r.submits)
	}
}

func TestContainers_AStartThatCannotBeReadIsNotKnownToBeStoppedAndRefuses(t *testing.T) {
	r := newFlowRig(t, nil)
	r.create(t, "my-notes.xml", "my-Photos.xml")
	if err := r.s.StartContainer(ctx0, "notes", r.submit); err != nil {
		t.Fatal(err)
	}
	r.jobs["job-1"] = "failed"
	r.fake.SetUnavailable(nil)
	err := r.s.StartContainer(ctx0, "photos", r.submit)
	if err == nil || errors.Is(err, ErrContainerUnconfirmed) || !errors.Is(err, container.ErrUnavailable) || len(r.submits) != 1 {
		t.Fatalf("start with the Engine unreachable = %v (submits %v), want the read failure and nothing queued", err, r.submits)
	}
	if o, err := r.s.Offer(ctx0); err != nil || o.Awaiting != "notes" {
		t.Errorf("offer = %+v, %v, want notes awaiting while its state cannot be read", o, err)
	}

	r.fake = container.NewFakeProvider()
	r.stacks.Provider = r.fake
	r.jobs["job-1"] = "failed"
	if err := r.s.StartContainer(ctx0, "photos", r.submit); err != nil {
		t.Errorf("start after a start that failed and left no container = %v, want it offered: nothing of notes is running", err)
	}
}

// The offer's Next is the first stack not started yet, in the offered order. A
// stack that was started, stopped and never confirmed comes after those, and is
// offered only when none is left.
func TestContainers_NextNeverOffersAStoppedUnconfirmedStackAheadOfAnUnstartedOne(t *testing.T) {
	r := newFlowRig(t, nil)
	r.create(t, "my-notes.xml", "my-Photos.xml")
	if err := r.s.StartContainer(ctx0, "notes", r.submit); err != nil {
		t.Fatal(err)
	}
	r.jobs["job-1"] = "succeeded"
	if o, err := r.s.Offer(ctx0); err != nil || o.Awaiting != "" || o.Next != "photos" {
		t.Fatalf("offer with notes started and its job ended = %+v, %v, want photos next", o, err)
	}
	if err := r.s.StartContainer(ctx0, "photos", r.submit); err != nil {
		t.Fatal(err)
	}
	r.jobs["job-2"] = "failed"
	if o, err := r.s.Offer(ctx0); err != nil || o.Awaiting != "" || o.Next != "notes" {
		t.Fatalf("offer with both started and stopped = %+v, %v, want notes offered again for want of an unstarted stack", o, err)
	}
}

// A stack the daemon created and never recorded (it stopped between the two, or
// the record could not be written and the stack could not be removed again) is
// recognised by the selection its template source names: the retry records it,
// answers already created, and neither creates it again nor deletes it.
func TestContainers_AStackCreatedAndNeverRecordedIsRecordedByTheRetry(t *testing.T) {
	r := newFlowRig(t, nil)
	r.s.Stacks = &removeFails{StackLayer: r.stacks}
	failWrites(t, r.db)
	res, err := r.s.CreateStacks(ctx0, []StackSelection{{Name: "my-Photos.xml"}})
	if err != nil || len(res) != 1 || res[0].Err == nil || !strings.Contains(res[0].Err.Error(), "disk full") {
		t.Fatalf("CreateStacks = %+v, %v, want the record's write failure", res, err)
	}
	if flow, _ := r.s.flowOf(ctx0); len(flow.Stacks) != 0 {
		t.Fatalf("record = %+v, want none: the crash left no record", flow.Stacks)
	}
	before, err := r.stacks.Get(ctx0, "photos")
	if err != nil {
		t.Fatalf("the stack is gone: %v", err)
	}
	dropWriteFailures(t, r.db)
	layer := &removeFails{StackLayer: r.stacks}
	r.s.Stacks = layer
	composeCalls := len(r.runner.Calls())

	res, err = r.s.CreateStacks(ctx0, []StackSelection{{Name: "my-Photos.xml"}})
	if err != nil || len(res) != 1 || res[0].Err != nil || !res[0].AlreadyCreated {
		t.Fatalf("retry = %+v, %v, want photos recorded and answered already created", res, err)
	}
	if layer.removes != 0 {
		t.Errorf("the retry removed the stack %d times", layer.removes)
	}
	if got := len(r.runner.Calls()); got != composeCalls {
		t.Errorf("the retry ran %d more docker compose calls: the stack was made again", got-composeCalls)
	}
	after, err := r.stacks.Get(ctx0, "photos")
	if err != nil || !after.InstalledAt.Equal(before.InstalledAt) {
		t.Errorf("stack after the retry = %+v, %v, want the one that was there", after, err)
	}
	if got := stateOf(t, r, "photos"); got.Source != "my-Photos.xml" || got.State != StackCreated || got.Position == 0 {
		t.Errorf("record = %+v, want photos recorded as created, with its autostart place, as the first request would have", got)
	}
	r.s.Stacks = r.stacks
	if err := r.s.StartContainer(ctx0, "photos", r.submit); err != nil {
		t.Errorf("start of the recovered stack = %v", err)
	}
}

// The same retry never takes a stack the migration did not make: one with no
// template source, or one made for another selection, is refused as it exists.
func TestContainers_AStackOfTheSameNameTheMigrationDidNotMakeStaysRefused(t *testing.T) {
	r := newFlowRig(t, nil)
	for name, source := range map[string]string{"photos": "", "mediaserver": migrationSource("other.xml")} {
		if _, err := r.stacks.Create(ctx0, container.NewStack{Name: name, Compose: "services: {}\n", TemplateSource: source}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := r.s.CreateStacks(ctx0, []StackSelection{{Name: "my-Photos.xml"}, {Name: "my-mediaserver.xml"}})
	if err != nil || len(res) != 2 {
		t.Fatalf("CreateStacks = %+v, %v", res, err)
	}
	for _, x := range res {
		if !errors.Is(x.Err, container.ErrStackExists) || x.AlreadyCreated {
			t.Errorf("%s = %+v, want it refused as existing", x.Name, x)
		}
	}
	if flow, _ := r.s.flowOf(ctx0); len(flow.Stacks) != 0 {
		t.Errorf("record = %+v, want none", flow.Stacks)
	}
	if got := r.stackNames(t); strings.Join(got, ",") != "mediaserver,photos" {
		t.Errorf("stacks = %v, want both left as they were", got)
	}
}

// A recovered stack whose record cannot be written is left alone: the next
// retry still recognises it.
func TestContainers_ARecoveredStackIsNeverRemovedWhenItsRecordFails(t *testing.T) {
	r := newFlowRig(t, nil)
	layer := &removeFails{StackLayer: r.stacks}
	r.s.Stacks = layer
	failWrites(t, r.db)
	if _, err := r.s.CreateStacks(ctx0, []StackSelection{{Name: "my-Photos.xml"}}); err != nil {
		t.Fatal(err)
	}
	removesBefore := layer.removes
	res, err := r.s.CreateStacks(ctx0, []StackSelection{{Name: "my-Photos.xml"}})
	if err != nil || len(res) != 1 || res[0].Err == nil || res[0].AlreadyCreated {
		t.Fatalf("retry with the record still failing = %+v, %v, want the write failure", res, err)
	}
	if got := r.stackNames(t); strings.Join(got, ",") != "photos" || layer.removes != removesBefore {
		t.Fatalf("stacks = %v after %d more removals, want photos left alone", got, layer.removes-removesBefore)
	}
	dropWriteFailures(t, r.db)
	res, err = r.s.CreateStacks(ctx0, []StackSelection{{Name: "my-Photos.xml"}})
	if err != nil || len(res) != 1 || res[0].Err != nil || !res[0].AlreadyCreated {
		t.Errorf("retry = %+v, %v, want photos recorded", res, err)
	}
}

// A start whose job was queued and whose record then failed is still a started
// stack: no other container is started until it is confirmed or started again,
// and starting it again records it.
func TestContainers_AStartWhoseRecordFailedStillHoldsTheNextStart(t *testing.T) {
	r := newFlowRig(t, nil)
	r.create(t, "my-notes.xml", "my-Photos.xml")
	err := r.s.StartContainer(ctx0, "notes", func(ctx context.Context, stack string) (string, error) {
		id, err := r.submit(ctx, stack)
		failWrites(t, r.db)
		return id, err
	})
	if err == nil || !strings.Contains(err.Error(), "job-1") || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("start of notes = %v, want the job's id and the write failure", err)
	}
	dropWriteFailures(t, r.db)
	err = r.s.StartContainer(ctx0, "photos", r.submit)
	if !errors.Is(err, ErrContainerUnconfirmed) || !strings.Contains(err.Error(), "notes") || len(r.submits) != 1 {
		t.Fatalf("start of photos = %v (submits %v), want ErrContainerUnconfirmed and nothing queued", err, r.submits)
	}
	r.jobs["job-1"] = "succeeded"
	if err := r.s.StartContainer(ctx0, "photos", r.submit); !errors.Is(err, ErrContainerUnconfirmed) {
		t.Errorf("start of photos once notes' job ended = %v, want it still refused: the job is not on record", err)
	}
	if o, err := r.s.Offer(ctx0); err != nil || o.Awaiting != "notes" || o.Next != "" {
		t.Errorf("offer = %+v, %v, want notes awaiting and nothing offered", o, err)
	}

	if err := r.s.StartContainer(ctx0, "notes", r.submit); err != nil {
		t.Fatalf("start of notes again = %v", err)
	}
	if got := stateOf(t, r, "notes"); got.State != StackStarted || got.StartJob != "job-2" {
		t.Errorf("notes = %+v, want started by job-2", got)
	}
}

// A start whose record cannot be written is not queued, so nothing is started
// and the next start is not held.
func TestContainers_AStartThatCannotBeRecordedQueuesNothing(t *testing.T) {
	r := newFlowRig(t, nil)
	r.create(t, "my-notes.xml", "my-Photos.xml")
	failWrites(t, r.db)
	if err := r.s.StartContainer(ctx0, "notes", r.submit); err == nil || !strings.Contains(err.Error(), "disk full") || len(r.submits) != 0 {
		t.Fatalf("start = %v (submits %v), want the write failure and nothing queued", err, r.submits)
	}
	dropWriteFailures(t, r.db)
	if err := r.s.StartContainer(ctx0, "photos", r.submit); err != nil {
		t.Errorf("start of photos after notes was not started = %v", err)
	}
}

// A submit that fails after the start was recorded puts the record back; when
// that fails as well the stack stays recorded as started and holds the next.
func TestContainers_AFailedSubmitWhoseRecordCannotBePutBackHoldsTheNextStart(t *testing.T) {
	r := newFlowRig(t, nil)
	r.create(t, "my-notes.xml", "my-Photos.xml")
	boom := errors.New("scheduler refused")
	err := r.s.StartContainer(ctx0, "notes", func(context.Context, string) (string, error) {
		failWrites(t, r.db)
		return "", boom
	})
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("start = %v, want the submit's error and the write failure", err)
	}
	dropWriteFailures(t, r.db)
	if err := r.s.StartContainer(ctx0, "photos", r.submit); !errors.Is(err, ErrContainerUnconfirmed) {
		t.Errorf("start of photos = %v, want ErrContainerUnconfirmed: notes is still recorded as started", err)
	}
}

// removeFails stands in for the stack layer with a Remove that fails, so a
// stack whose record could not be written stays behind, as a daemon that stops
// between creating a stack and recording it leaves it.
type removeFails struct {
	StackLayer
	removes int
}

func (l *removeFails) Remove(context.Context, string, bool) (container.StackRemoveResult, error) {
	l.removes++
	return container.StackRemoveResult{}, errors.New("daemon stopped")
}

func dropWriteFailures(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, trigger := range []string{"no_insert", "no_update"} {
		if _, err := db.Exec("DROP TRIGGER " + trigger); err != nil {
			t.Fatal(err)
		}
	}
}

func TestContainers_AFailedSubmitLeavesTheStackNotStarted(t *testing.T) {
	r := newFlowRig(t, nil)
	r.create(t, "my-notes.xml")
	boom := errors.New("scheduler refused")
	if err := r.s.StartContainer(ctx0, "notes", func(context.Context, string) (string, error) { return "", boom }); !errors.Is(err, boom) {
		t.Fatalf("start = %v, want the submit's error", err)
	}
	if got := stateOf(t, r, "notes"); got.State != StackCreated || got.StartJob != "" {
		t.Errorf("notes = %+v, want it still created", got)
	}
}

func TestContainers_DataCheckOutcomes(t *testing.T) {
	r := newFlowRig(t, nil)
	r.create(t, "my-notes.xml")

	if _, err := r.s.CheckContainer(ctx0, "notes"); !errors.Is(err, ErrContainerNotStarted) {
		t.Errorf("check before the start = %v, want ErrContainerNotStarted", err)
	}
	if err := r.s.StartContainer(ctx0, "notes", r.submit); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.CheckContainer(ctx0, "notes"); !errors.Is(err, ErrNoContainer) {
		t.Errorf("check with no container yet = %v, want ErrNoContainer: nothing is known about its mounts", err)
	}

	full := r.mkData(t, "appdata/full", "config.xml")
	empty := r.mkData(t, "appdata/empty")
	missing := filepath.Join(r.dataRoot, "appdata", "missing")
	file := filepath.Join(r.dataRoot, "appdata", "one.conf")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(file, "child")
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.addContainer("notes", "running", full, empty, missing, file, blocked, outside, "/var/lib/docker/volumes/notes_data/_data", "relative/path")

	c, err := r.s.CheckContainer(ctx0, "notes")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range c.Paths {
		got[p.Path] = p.Status
	}
	want := map[string]string{full: PathOK, empty: PathEmpty, missing: PathMissing, file: PathOK, blocked: PathUnreadable}
	if len(got) != len(want) {
		t.Fatalf("paths = %v, want only the mounts under the data roots: %v", got, want)
	}
	for p, st := range want {
		if got[p] != st {
			t.Errorf("%s = %q, want %q", p, got[p], st)
		}
	}
	if c.AllOK || !c.Running {
		t.Errorf("check = %+v, want not all ok and running", c)
	}
	for _, p := range c.Paths {
		if p.Path == blocked && p.Error == "" {
			t.Errorf("an unreadable path says nothing of why: %+v", p)
		}
	}
	if st := stateOf(t, r, "notes"); !st.Checked || !st.CheckFailed {
		t.Errorf("record = %+v, want a failed check recorded", st)
	}

	r.fake.RemoveContainer("id-notes")
	r.addContainer("notes", "running", full, file)
	c, err = r.s.CheckContainer(ctx0, "notes")
	if err != nil || !c.AllOK || len(c.Paths) != 2 {
		t.Fatalf("check = %+v, %v, want every path ok", c, err)
	}
	if st := stateOf(t, r, "notes"); !st.Checked || st.CheckFailed {
		t.Errorf("record = %+v, want the newer passing check", st)
	}

	r.fake.RemoveContainer("id-notes")
	r.addContainer("notes", "running", "/var/lib/docker/volumes/notes_data/_data")
	if c, err = r.s.CheckContainer(ctx0, "notes"); err != nil || !c.AllOK || len(c.Paths) != 0 {
		t.Errorf("check of a container with no host data = %+v, %v, want nothing to check", c, err)
	}
}

func TestContainers_ConfirmNeedsACheckAndARunningContainerAndOffersTheNext(t *testing.T) {
	r := newFlowRig(t, nil)
	r.create(t, "my-notes.xml", "my-Photos.xml")
	if err := r.confirm("notes", false); !errors.Is(err, ErrContainerNotStarted) {
		t.Errorf("confirm before the start = %v, want ErrContainerNotStarted", err)
	}
	if err := r.s.StartContainer(ctx0, "notes", r.submit); err != nil {
		t.Fatal(err)
	}
	full := r.mkData(t, "appdata/notes", "config.xml")
	empty := r.mkData(t, "appdata/empty")
	id := r.addContainer("notes", "running", full, empty)

	if err := r.confirm("notes", false); !errors.Is(err, ErrDataCheckRequired) {
		t.Errorf("confirm before a check = %v, want ErrDataCheckRequired", err)
	}
	if _, err := r.s.CheckContainer(ctx0, "notes"); err != nil {
		t.Fatal(err)
	}
	if err := r.confirm("notes", false); !errors.Is(err, ErrDataCheckFailed) {
		t.Errorf("confirm after a failing check = %v, want ErrDataCheckFailed", err)
	}
	if got := stateOf(t, r, "notes"); got.State != StackStarted {
		t.Errorf("notes = %+v after refused confirmations", got)
	}

	if err := r.fake.Stop(ctx0, id); err != nil {
		t.Fatal(err)
	}
	if err := r.confirm("notes", true); !errors.Is(err, ErrContainerNotRunning) {
		t.Errorf("confirm of a stopped container = %v, want ErrContainerNotRunning", err)
	}
	r.fake.RemoveContainer(id)
	r.addContainer("notes", "running", full, empty)
	if err := r.confirm("notes", true); err != nil {
		t.Fatalf("confirm accepting the check = %v", err)
	}
	if err := r.confirm("notes", false); err != nil {
		t.Errorf("confirming a confirmed stack = %v, want it to change nothing", err)
	}
	if err := r.s.StartContainer(ctx0, "notes", r.submit); !errors.Is(err, ErrContainerConfirmed) {
		t.Errorf("start of a confirmed stack = %v, want ErrContainerConfirmed", err)
	}

	o, err := r.s.Offer(ctx0)
	if err != nil || o.Awaiting != "" || o.Next != "photos" {
		t.Fatalf("offer = %+v, %v, want photos next", o, err)
	}
	if err := r.s.StartContainer(ctx0, "photos", r.submit); err != nil {
		t.Errorf("start of photos once notes is confirmed = %v", err)
	}
}

// A start resets what the check said: the data the new container sees is not
// the data the earlier one was checked against.
func TestContainers_AStartAgainClearsTheCheck(t *testing.T) {
	r := newFlowRig(t, nil)
	r.create(t, "my-notes.xml")
	if err := r.s.StartContainer(ctx0, "notes", r.submit); err != nil {
		t.Fatal(err)
	}
	r.addContainer("notes", "running", r.mkData(t, "appdata/notes", "a"))
	if _, err := r.s.CheckContainer(ctx0, "notes"); err != nil {
		t.Fatal(err)
	}
	r.jobs["job-1"] = "succeeded"
	if err := r.s.StartContainer(ctx0, "notes", r.submit); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, r, "notes"); st.Checked || st.StartJob != "job-2" {
		t.Errorf("record = %+v, want the check cleared by the second start", st)
	}
	if err := r.confirm("notes", false); !errors.Is(err, ErrDataCheckRequired) {
		t.Errorf("confirm after a restart without a check = %v, want ErrDataCheckRequired", err)
	}
}

// A check that runs while a start is being queued is not a check since that
// start: the job's id is recorded after it, and the record that results has no
// check on it.
func TestContainers_ACheckMadeWhileAStartIsQueuedDoesNotCountAsSinceIt(t *testing.T) {
	r := newFlowRig(t, nil)
	r.create(t, "my-notes.xml")
	if err := r.s.StartContainer(ctx0, "notes", r.submit); err != nil {
		t.Fatal(err)
	}
	r.addContainer("notes", "running", r.mkData(t, "appdata/notes", "a"))
	r.jobs["job-1"] = "succeeded"
	err := r.s.StartContainer(ctx0, "notes", func(ctx context.Context, stack string) (string, error) {
		if _, err := r.s.CheckContainer(ctx, stack); err != nil {
			t.Errorf("check while the start is queued = %v", err)
		}
		return r.submit(ctx, stack)
	})
	if err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, r, "notes"); st.Checked || st.CheckFailed || st.StartJob != "job-2" {
		t.Errorf("record = %+v, want started by job-2 with no check on it", st)
	}
	if err := r.confirm("notes", false); !errors.Is(err, ErrDataCheckRequired) {
		t.Errorf("confirm on a check made before the job was recorded = %v, want ErrDataCheckRequired", err)
	}
}

func TestContainers_TheRecordSurvivesANewScan(t *testing.T) {
	files, spec := flashTree(t, inventoryVariant)
	r := newFlowRig(t, nil)
	r.create(t, "my-Photos.xml")
	if err := scanNow(r.s, zipOf(t, files, false), ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = spec
	if got := stateOf(t, r, "photos"); got.State != StackCreated {
		t.Errorf("photos = %+v", got)
	}
	o, err := r.s.Offer(ctx0)
	if err != nil || len(o.Stacks) != 1 {
		t.Fatalf("offer = %+v, %v", o, err)
	}
	for _, tm := range o.Templates {
		if templateKey(tm.Entry) == "my-Photos.xml" && (!tm.Created || tm.Preselected) {
			t.Errorf("photos = %+v, want it shown as created and no longer pre-selected", tm)
		}
	}
}
