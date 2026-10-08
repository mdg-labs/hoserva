package migrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mdg-labs/hoserva/internal/container"
)

// Phase D, steps 19 and 20 (doc 05 §4): the user's templates and Compose
// Manager projects become Compose stacks, created stopped once the parity
// initialisation is confirmed, and started one at a time, each confirmed to
// see its data before the next. The stacks are created and started through the
// stack layer (internal/container) and nothing here runs Docker.

var (
	// ErrParityNotInitialized is returned for every Phase D operation that
	// creates or starts something while the migration is not past its point of
	// no return.
	ErrParityNotInitialized = errors.New("the parity initialisation has not been confirmed: no container is created or started until the migration is past its point of no return")
	// ErrContainersNotConfigured is returned when this daemon cannot create or
	// start migrated containers.
	ErrContainersNotConfigured = errors.New("this daemon cannot create or start migrated containers")
	// ErrInvalidSelection is returned for a selection that is empty, names one
	// template twice, or would create two stacks of one name.
	ErrInvalidSelection = errors.New("invalid container selection")
	// ErrTemplateUnconvertible is returned for a template or project that could
	// not be read, so there is nothing to create a stack from.
	ErrTemplateUnconvertible = errors.New("the template could not be read, so no stack can be created from it")
	// ErrWarningsNotAcknowledged is returned for a template whose conversion has
	// warnings that need manual action (Q36) and that the request does not
	// acknowledge.
	ErrWarningsNotAcknowledged = errors.New("the conversion has warnings that need manual action: acknowledge them for this template to create its stack")
	// ErrStackNotMigrated is returned for a name no stack of this migration has.
	ErrStackNotMigrated = errors.New("no stack was created from the migration under this name")
	// ErrContainerUnconfirmed is returned for a start while another migrated
	// stack that was started is neither confirmed nor stopped.
	ErrContainerUnconfirmed = errors.New("another migrated container was started and is not confirmed yet: confirm that it sees its data, or stop it, before starting the next")
	// ErrContainerConfirmed is returned for a start of a stack already confirmed.
	ErrContainerConfirmed = errors.New("this migrated container is already confirmed")
	// ErrContainerNotStarted is returned for a data check or a confirmation of a
	// stack that has not been started.
	ErrContainerNotStarted = errors.New("this migrated container has not been started")
	// ErrNoContainer is returned for a data check when the stack has no
	// container, so there is nothing to read the mounts of.
	ErrNoContainer = errors.New("the stack has no container: its start has not created one")
	// ErrDataCheckRequired is returned for a confirmation before a data check
	// has been made since the start.
	ErrDataCheckRequired = errors.New("the data check has not been run since this container was started: run it, read what it found, then confirm")
	// ErrDataCheckFailed is returned for a confirmation of a stack whose data
	// check found a path that is missing, empty or unreadable, when the request
	// does not accept that.
	ErrDataCheckFailed = errors.New("the data check found a path that is missing, empty or unreadable: stop the container, or confirm it accepting that")
	// ErrContainerNotRunning is returned for a confirmation of a stack none of
	// whose containers is running.
	ErrContainerNotRunning = errors.New("none of the stack's containers is running")
)

// migrationSourcePrefix starts the template source of every stack this flow
// creates; the selection's name follows. The stack layer stores it in the same
// row as the stack, so a stack whose record in the session was never written
// still says which selection made it. A catalog template's source is a catalog
// source id and is never this.
const migrationSourcePrefix = "unraid-migration:"

func migrationSource(selection string) string { return migrationSourcePrefix + selection }

// StackLayer is the part of the Compose stack layer (*container.StackService)
// the migrated containers use.
type StackLayer interface {
	Create(ctx context.Context, n container.NewStack) (container.Stack, error)
	Get(ctx context.Context, name string) (container.Stack, error)
	Remove(ctx context.Context, name string, deleteAppdata bool) (container.StackRemoveResult, error)
	RequireRunning() error
	Containers(ctx context.Context, name string) ([]container.Container, error)
}

var _ StackLayer = (*container.StackService)(nil)

// StackState is how far a migrated stack has come.
type StackState string

const (
	// StackCreated is a stack that exists and has not been started by the flow.
	StackCreated StackState = "created"
	// StackStarted is a stack whose start was queued and that the user has not
	// confirmed.
	StackStarted StackState = "started"
	// StackConfirmed is a stack the user confirmed sees its data.
	StackConfirmed StackState = "confirmed"
)

// MigratedStack is a stack created from the report and how far it has come.
type MigratedStack struct {
	// Name is the stack's name and Source the template file or project it was
	// created from.
	Name   string       `json:"name"`
	Source string       `json:"source"`
	Kind   TemplateKind `json:"kind"`
	// Position and WaitSeconds are the stack's place on Unraid's autostart list
	// and the wait the list gives after it; both are 0 off the list.
	Position    int        `json:"position,omitempty"`
	WaitSeconds int        `json:"waitSeconds,omitempty"`
	State       StackState `json:"state"`
	// StartJob is the job the latest start queued.
	StartJob string `json:"startJob,omitempty"`
	// Checked is true once a data check has run since the latest start, and
	// CheckFailed when that check found a path that is not whole.
	Checked     bool `json:"checked,omitempty"`
	CheckFailed bool `json:"checkFailed,omitempty"`
}

// ContainerFlow is the record of the stacks created from a report.
type ContainerFlow struct {
	Stacks []MigratedStack `json:"stacks"`
}

// ordered returns the stacks in the order they are offered: Unraid's autostart
// list first, in its order, then the others as they were created.
func (f *ContainerFlow) ordered() []MigratedStack {
	out := append([]MigratedStack(nil), f.Stacks...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Position, out[j].Position
		switch {
		case a > 0 && b > 0:
			return a < b
		default:
			return a > 0 && b == 0
		}
	})
	return out
}

func (f *ContainerFlow) index(name string) int {
	for i := range f.Stacks {
		if f.Stacks[i].Name == name {
			return i
		}
	}
	return -1
}

func (f *ContainerFlow) bySource(source string) int {
	for i := range f.Stacks {
		if f.Stacks[i].Source == source {
			return i
		}
	}
	return -1
}

// StackNameFor turns the name a template or project carries into a stack name:
// lowercase letters, digits, "-" and "_", starting with a letter or digit, at
// most 63 characters. Unraid allows capitals and spaces in a name; a stack
// does not.
func StackNameFor(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	n := strings.TrimLeft(b.String(), "-_")
	if len(n) > 63 {
		n = n[:63]
	}
	return n
}

func (s *Service) requireInitialized(ctx context.Context) error {
	if s.Initialized == nil || s.Stacks == nil {
		return ErrContainersNotConfigured
	}
	ok, err := s.Initialized(ctx)
	if err != nil {
		return fmt.Errorf("reading whether the parity initialisation has finished: %w", err)
	}
	if !ok {
		return ErrParityNotInitialized
	}
	return nil
}

// flowOf reads the record of the created stacks; a report with none has an
// empty one.
func (s *Service) flowOf(ctx context.Context) (*ContainerFlow, error) {
	st, err := s.State(ctx)
	if err != nil {
		return nil, err
	}
	if st.Report == nil {
		return nil, ErrNoReport
	}
	if st.Report.Containers == nil {
		return &ContainerFlow{}, nil
	}
	return st.Report.Containers, nil
}

// updateFlow reads the session, lets fn change the record and writes the
// session back, all under the session lock. It is used after a stack was
// created or a start queued, so it does not stop at the request's end.
func (s *Service) updateFlow(ctx context.Context, fn func(f *ContainerFlow) error) error {
	ctx = context.WithoutCancel(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.load(ctx)
	if err != nil {
		return err
	}
	if sess.Report == nil {
		return ErrNoReport
	}
	if sess.Report.Containers == nil {
		sess.Report.Containers = &ContainerFlow{}
	}
	if err := fn(sess.Report.Containers); err != nil {
		return err
	}
	return s.save(ctx, sess)
}

func (s *Service) migratedStack(ctx context.Context, name string) (*ContainerFlow, MigratedStack, error) {
	flow, err := s.flowOf(ctx)
	if err != nil {
		return nil, MigratedStack{}, err
	}
	i := flow.index(name)
	if i < 0 {
		return nil, MigratedStack{}, fmt.Errorf("%w: %q", ErrStackNotMigrated, name)
	}
	return flow, flow.Stacks[i], nil
}

// OfferedTemplate is a template of the report as Phase D offers it.
type OfferedTemplate struct {
	Entry TemplateEntry
	// Stack is the name its stack would have; empty when the template's name
	// cannot make a valid stack name.
	Stack string
	// Created is true when the stack exists from this migration.
	Created bool
	// Creatable is false for a template that could not be read.
	Creatable bool
	// Preselected is true for a creatable template on Unraid's autostart list
	// whose stack is not created yet.
	Preselected bool
}

// OfferedProject is a Compose Manager project as Phase D offers it.
type OfferedProject struct {
	Project   ComposeProject
	Stack     string
	Created   bool
	Creatable bool
}

// StackStatus is a created stack with whether it is awaiting confirmation.
type StackStatus struct {
	MigratedStack
	// Awaiting is true for a started stack that is neither confirmed nor known
	// to be stopped; it is also true when that cannot be read.
	Awaiting bool
}

// ContainerOffer is Phase D's screen as data.
type ContainerOffer struct {
	// ParityInitialized is Initialized's answer; no stack is created or started
	// while it is false.
	ParityInitialized bool
	// Templates are in the order the groups are shown: autostart (in Unraid's
	// order), running, stopped, template only, unknown, each by file name.
	Templates []OfferedTemplate
	Projects  []OfferedProject
	ByHand    []ByHandContainer
	// Stacks are the created stacks in the order they are offered.
	Stacks []StackStatus
	// Awaiting is the stack that must be confirmed or stopped before another is
	// started, and Next the stack to start now; at most one is set. Next is the
	// first stack not started yet; a stack that was started, stopped and never
	// confirmed is offered only when none of those is left.
	Awaiting string
	Next     string
}

var classOrder = map[TemplateClass]int{ClassAutostart: 0, ClassRunning: 1, ClassStopped: 2, ClassTemplateOnly: 3, ClassUnknown: 4}

// Offer lists what Phase D can create from the report and what it has created.
// It reads no disk: the stack layer's containers are read only for a stack that
// was started and is not confirmed.
func (s *Service) Offer(ctx context.Context) (*ContainerOffer, error) {
	if s.Initialized == nil {
		return nil, ErrContainersNotConfigured
	}
	imp, err := s.previewImport(ctx)
	if err != nil {
		return nil, err
	}
	initialized, err := s.Initialized(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading whether the parity initialisation has finished: %w", err)
	}
	flow, err := s.flowOf(ctx)
	if err != nil {
		return nil, err
	}
	out := &ContainerOffer{ParityInitialized: initialized, ByHand: imp.ByHand}
	for _, e := range imp.Templates {
		t := OfferedTemplate{Entry: e, Stack: StackNameFor(e.Name), Creatable: e.Outcome != nil && e.Outcome.Status != PreviewFailed}
		if !validStackName(t.Stack) {
			t.Stack, t.Creatable = "", false
		}
		t.Created = flow.bySource(templateKey(e)) >= 0
		t.Preselected = t.Creatable && !t.Created && e.Class == ClassAutostart && e.AutostartPosition > 0
		out.Templates = append(out.Templates, t)
	}
	sort.SliceStable(out.Templates, func(i, j int) bool {
		a, b := out.Templates[i], out.Templates[j]
		if ca, cb := classOrder[a.Entry.Class], classOrder[b.Entry.Class]; ca != cb {
			return ca < cb
		}
		if a.Entry.Class == ClassAutostart && a.Entry.AutostartPosition != b.Entry.AutostartPosition {
			return a.Entry.AutostartPosition < b.Entry.AutostartPosition
		}
		return templateKey(a.Entry) < templateKey(b.Entry)
	})
	for _, p := range imp.ComposeProjects {
		pr := OfferedProject{Project: p, Stack: StackNameFor(p.Name), Creatable: p.Outcome != nil && p.Outcome.Status != PreviewFailed}
		if !validStackName(pr.Stack) {
			pr.Stack, pr.Creatable = "", false
		}
		pr.Created = flow.bySource(p.Name) >= 0
		out.Projects = append(out.Projects, pr)
	}
	for _, st := range flow.ordered() {
		status := StackStatus{MigratedStack: st}
		if st.State == StackStarted && s.Stacks != nil {
			awaiting, err := s.awaiting(ctx, st)
			status.Awaiting = err != nil || awaiting
		} else if st.State == StackStarted {
			status.Awaiting = true
		}
		if status.Awaiting && out.Awaiting == "" {
			out.Awaiting = st.Name
		}
		out.Stacks = append(out.Stacks, status)
	}
	if out.Awaiting == "" {
		for _, st := range out.Stacks {
			if st.State == StackCreated {
				out.Next = st.Name
				break
			}
		}
	}
	if out.Awaiting == "" && out.Next == "" {
		for _, st := range out.Stacks {
			if st.State == StackStarted {
				out.Next = st.Name
				break
			}
		}
	}
	return out, nil
}

func validStackName(name string) bool { return container.ValidStackName(name) }

// StackSelection is one template or project the user chose, by the file name or
// project name the template listing gives, and whether the user acknowledges
// its warnings.
type StackSelection struct {
	Name         string
	Acknowledged bool
}

// StackResult is what creating one selected stack came to. Err is why it was
// not created.
type StackResult struct {
	Name           string
	Stack          string
	AlreadyCreated bool
	Err            error
}

type plannedStack struct {
	sel     StackSelection
	stack   string
	compose string
	env     string
	kind    TemplateKind
	pos     int
	wait    int
}

// CreateStacks creates a stopped Compose stack for each selected template or
// project, from the generated Compose the preview showed (the project's own
// compose.yaml for a project). Everything that can refuse the request as a
// whole is checked before the first stack is made: the parity initialisation, a
// selection that is empty or repeats itself, a template that cannot be read, a
// conversion with warnings the request does not acknowledge, a stack name two
// selections share. After that each stack is created on its own and the answer
// holds one result per selection, so a failure of stack k leaves the stacks
// before it created and recorded, and says so. A selection whose stack already
// exists from this migration is reported as already created, and made again if
// the stack was removed since. So is a stack this flow made whose record was
// never written (the daemon stopped between the two): it is recorded now and
// never made again or deleted.
func (s *Service) CreateStacks(ctx context.Context, selection []StackSelection) ([]StackResult, error) {
	if err := s.requireInitialized(ctx); err != nil {
		return nil, err
	}
	if len(selection) == 0 {
		return nil, fmt.Errorf("%w: select at least one template or Compose Manager project", ErrInvalidSelection)
	}
	s.flowMu.Lock()
	defer s.flowMu.Unlock()

	seen := map[string]bool{}
	for _, sel := range selection {
		if seen[sel.Name] {
			return nil, fmt.Errorf("%w: %q is selected twice", ErrInvalidSelection, sel.Name)
		}
		seen[sel.Name] = true
	}
	stacks := map[string]string{}
	var plan []plannedStack
	for _, sel := range selection {
		view, err := s.Template(ctx, sel.Name)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", sel.Name, err)
		}
		project := view.Kind == KindComposeProject
		outcome := view.Preview.Outcome(project)
		if view.Preview.Failure != "" {
			return nil, fmt.Errorf("%q: %w: %s", sel.Name, ErrTemplateUnconvertible, outcome.FailureText())
		}
		if !project && outcome.ActionWarnings() > 0 && !sel.Acknowledged {
			return nil, fmt.Errorf("%q: %w", sel.Name, ErrWarningsNotAcknowledged)
		}
		p := plannedStack{sel: sel, compose: view.Preview.Compose, env: view.Preview.Env, kind: view.Kind}
		if project {
			p.stack = StackNameFor(view.Project.Name)
		} else {
			p.stack = StackNameFor(view.Entry.Name)
			p.pos, p.wait = view.Entry.AutostartPosition, view.Entry.AutostartWaitSeconds
		}
		if !validStackName(p.stack) {
			return nil, fmt.Errorf("%w: %q has no name a stack can have", ErrInvalidSelection, sel.Name)
		}
		if other, dup := stacks[p.stack]; dup {
			return nil, fmt.Errorf("%w: %q and %q would both create the stack %q", ErrInvalidSelection, other, sel.Name, p.stack)
		}
		stacks[p.stack] = sel.Name
		plan = append(plan, p)
	}

	flow, err := s.flowOf(ctx)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(plan, func(i, j int) bool {
		a, b := plan[i].pos, plan[j].pos
		return a > 0 && (b == 0 || a < b)
	})
	results := make([]StackResult, 0, len(plan))
	for _, p := range plan {
		res := StackResult{Name: p.sel.Name, Stack: p.stack}
		if i := flow.bySource(p.sel.Name); i >= 0 {
			_, err := s.Stacks.Get(ctx, flow.Stacks[i].Name)
			if err == nil {
				res.AlreadyCreated = true
				results = append(results, res)
				continue
			}
			if !errors.Is(err, container.ErrStackNotFound) {
				res.Err = fmt.Errorf("checking the stack created earlier: %w", err)
				results = append(results, res)
				continue
			}
		}
		recovered, err := s.createStack(ctx, p)
		res.Err, res.AlreadyCreated = err, recovered && err == nil
		results = append(results, res)
	}
	return results, nil
}

// createStack makes one stack and records it. The stack is created with a
// template source that names this selection (migrationSource), so a stack of
// that name that already exists and carries it was made by this flow for this
// selection and only its record is missing; it is recorded and left as it is,
// and recovered is true. A stack of that name that does not carry it is not the
// migration's and is refused as it was. A record that cannot be written takes a
// stack just created away again, so a stack the migration does not know is not
// left behind; a stack found already existing is never removed.
func (s *Service) createStack(ctx context.Context, p plannedStack) (recovered bool, err error) {
	marker := migrationSource(p.sel.Name)
	_, err = s.Stacks.Create(ctx, container.NewStack{Name: p.stack, Compose: p.compose, Env: p.env, TemplateSource: marker})
	if errors.Is(err, container.ErrStackExists) {
		existing, gerr := s.Stacks.Get(ctx, p.stack)
		if gerr != nil {
			return false, errors.Join(err, fmt.Errorf("checking whether the migration made it: %w", gerr))
		}
		if existing.TemplateSource != marker {
			return false, err
		}
		recovered, err = true, nil
	}
	if err != nil {
		return false, err
	}
	entry := MigratedStack{Name: p.stack, Source: p.sel.Name, Kind: p.kind, Position: p.pos, WaitSeconds: p.wait, State: StackCreated}
	err = s.updateFlow(ctx, func(f *ContainerFlow) error {
		for i := range f.Stacks {
			if f.Stacks[i].Source == entry.Source {
				f.Stacks[i] = entry
				return nil
			}
		}
		f.Stacks = append(f.Stacks, entry)
		return nil
	})
	if err == nil {
		return recovered, nil
	}
	err = fmt.Errorf("recording stack %s in the migration: %w", p.stack, err)
	if recovered {
		return false, err
	}
	if _, rerr := s.Stacks.Remove(context.WithoutCancel(ctx), p.stack, false); rerr != nil {
		err = errors.Join(err, fmt.Errorf("removing the stack again: %w", rerr))
	}
	return false, err
}

// StartContainer queues the start of a migrated stack through submit, which
// returns the id of the job it queued. It is refused before the parity
// initialisation is confirmed, for a stack this migration did not create or one
// already confirmed, while the array is stopped, and while another migrated
// stack that was started is neither confirmed nor stopped (a stack whose start
// job is still running counts as started, and so does one whose containers are
// running, and so does one recorded as started with no job). A stack that
// cannot be read is not known to be stopped, so it refuses. The start is
// recorded before it is queued and the job's id after it; the record is put
// back when submit fails.
func (s *Service) StartContainer(ctx context.Context, name string, submit func(ctx context.Context, stack string) (jobID string, err error)) error {
	if err := s.requireInitialized(ctx); err != nil {
		return err
	}
	s.flowMu.Lock()
	defer s.flowMu.Unlock()
	flow, target, err := s.migratedStack(ctx, name)
	if err != nil {
		return err
	}
	if target.State == StackConfirmed {
		return fmt.Errorf("%w: %s", ErrContainerConfirmed, name)
	}
	if _, err := s.Stacks.Get(ctx, name); err != nil {
		return err
	}
	if err := s.Stacks.RequireRunning(); err != nil {
		return err
	}
	for _, other := range flow.Stacks {
		if other.Name == name || other.State != StackStarted {
			continue
		}
		awaiting, err := s.awaiting(ctx, other)
		if err != nil {
			return err
		}
		if awaiting {
			return fmt.Errorf("%w: %s", ErrContainerUnconfirmed, other.Name)
		}
	}
	// The start is recorded before it is queued, with no job yet: a record that
	// cannot be written then queues nothing, and a start that is queued and
	// then not recorded (a write failure, a daemon that stops) leaves the stack
	// recorded as started, which the gate above counts as unconfirmed.
	if err := s.updateFlow(ctx, func(f *ContainerFlow) error {
		i := f.index(name)
		if i < 0 {
			return fmt.Errorf("%w: %q", ErrStackNotMigrated, name)
		}
		f.Stacks[i].State, f.Stacks[i].StartJob = StackStarted, ""
		f.Stacks[i].Checked, f.Stacks[i].CheckFailed = false, false
		return nil
	}); err != nil {
		return fmt.Errorf("recording the start in the migration: %w", err)
	}
	id, err := submit(ctx, name)
	if err != nil {
		if rerr := s.updateFlow(ctx, func(f *ContainerFlow) error {
			i := f.index(name)
			if i < 0 {
				return fmt.Errorf("%w: %q", ErrStackNotMigrated, name)
			}
			f.Stacks[i] = target
			return nil
		}); rerr != nil {
			err = errors.Join(err, fmt.Errorf("the migration still records %s as started, and no other container is started until it is confirmed or started again: %w", name, rerr))
		}
		return err
	}
	if err := s.updateFlow(ctx, func(f *ContainerFlow) error {
		i := f.index(name)
		if i < 0 {
			return fmt.Errorf("%w: %q", ErrStackNotMigrated, name)
		}
		f.Stacks[i].StartJob = id
		f.Stacks[i].Checked, f.Stacks[i].CheckFailed = false, false
		return nil
	}); err != nil {
		return fmt.Errorf("the start was queued as job %s, but recording the job in the migration failed: the stack stays recorded as started, and no other container is started until it is confirmed or started again: %w", id, err)
	}
	return nil
}

// awaiting reports whether a started stack is still in the way of the next:
// its start job has not ended, or one of its containers is not stopped. A
// stack recorded as started with no job (the start was queued and its job never
// recorded, or never queued) is not known to be over, so it is awaited.
func (s *Service) awaiting(ctx context.Context, st MigratedStack) (bool, error) {
	if st.StartJob == "" || s.JobEnded == nil {
		return true, nil
	}
	_, ended, err := s.JobEnded(ctx, st.StartJob)
	if err != nil {
		return false, fmt.Errorf("reading the start job of stack %s: %w", st.Name, err)
	}
	if !ended {
		return true, nil
	}
	cs, err := s.Stacks.Containers(ctx, st.Name)
	if err != nil {
		return false, fmt.Errorf("reading the containers of stack %s: %w", st.Name, err)
	}
	return anyActive(cs), nil
}

// anyActive reports whether a container is in any state but one that holds no
// process: an unknown state counts as active.
func anyActive(cs []container.Container) bool {
	for _, c := range cs {
		switch c.State {
		case "exited", "dead", "created":
		default:
			return true
		}
	}
	return false
}

// Path states of a data check.
const (
	PathOK         = "ok"
	PathEmpty      = "empty"
	PathMissing    = "missing"
	PathUnreadable = "unreadable"
)

// PathCheck is the data check of one bind mount.
type PathCheck struct {
	Container   string
	Path        string
	Destination string
	// Status is PathOK for a path that exists and is not empty, and PathEmpty,
	// PathMissing or PathUnreadable otherwise. Unreadable is never fine.
	Status string
	Error  string
}

// DataCheck is what a stack's containers see of their data.
type DataCheck struct {
	Stack string
	// Running is true when a container of the stack is running.
	Running bool
	// Paths are the bind mounts under the data roots, one per mount; a mount
	// elsewhere is not read.
	Paths []PathCheck
	// AllOK is true when every path is ok. A stack with no path under the data
	// roots has nothing to check and is all ok.
	AllOK bool
}

func (s *Service) dataRoots() []string {
	if s.DataRoots != nil {
		return s.DataRoots
	}
	return []string{"/mnt/user", "/mnt/cache"}
}

func underRoots(p string, roots []string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	p = filepath.Clean(p)
	for _, r := range roots {
		r = filepath.Clean(r)
		if p == r || strings.HasPrefix(p, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// CheckContainer reads, for each bind mount of the stack's containers whose host
// path is under /mnt/user or /mnt/cache, whether it exists and is not empty, and
// records that the check ran. It reads one directory entry of each path and is
// asked for by the user, never repeated on a timer (doc 02 §1). A path that
// cannot be read is reported unreadable, never ok.
func (s *Service) CheckContainer(ctx context.Context, name string) (*DataCheck, error) {
	if err := s.requireInitialized(ctx); err != nil {
		return nil, err
	}
	_, st, err := s.migratedStack(ctx, name)
	if err != nil {
		return nil, err
	}
	if st.State == StackCreated {
		return nil, fmt.Errorf("%w: %s", ErrContainerNotStarted, name)
	}
	cs, err := s.Stacks.Containers(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("reading the containers of stack %s: %w", name, err)
	}
	if len(cs) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoContainer, name)
	}
	check := &DataCheck{Stack: name, Running: anyActive(cs), AllOK: true}
	roots := s.dataRoots()
	for _, c := range cs {
		for _, m := range c.Mounts {
			if !underRoots(m.Source, roots) {
				continue
			}
			pc := PathCheck{Container: c.Name, Path: filepath.Clean(m.Source), Destination: m.Destination}
			pc.Status, pc.Error = readDataPath(pc.Path)
			if pc.Status != PathOK {
				check.AllOK = false
			}
			check.Paths = append(check.Paths, pc)
		}
	}
	if st.State != StackStarted {
		return check, nil
	}
	if err := s.updateFlow(ctx, func(f *ContainerFlow) error {
		i := f.index(name)
		if i < 0 || f.Stacks[i].State != StackStarted || f.Stacks[i].StartJob != st.StartJob {
			return fmt.Errorf("%w: %s was started again while its data was read; check it again", ErrContainerNotStarted, name)
		}
		f.Stacks[i].Checked, f.Stacks[i].CheckFailed = true, !check.AllOK
		return nil
	}); err != nil {
		return nil, err
	}
	return check, nil
}

// readDataPath reports whether p exists and holds something: a directory with
// at least one entry, or a file with at least one byte.
func readDataPath(p string) (status, errText string) {
	fi, err := os.Stat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return PathMissing, ""
	case err != nil:
		return PathUnreadable, err.Error()
	case !fi.IsDir():
		if fi.Size() > 0 {
			return PathOK, ""
		}
		return PathEmpty, ""
	}
	f, err := os.Open(p)
	if err != nil {
		return PathUnreadable, err.Error()
	}
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(1)
	switch {
	case errors.Is(err, io.EOF), err == nil && len(names) == 0:
		return PathEmpty, ""
	case err != nil:
		return PathUnreadable, err.Error()
	}
	return PathOK, ""
}

// ConfirmContainer records that the user confirmed a started stack sees its
// data, which offers the next, and returns the stack as it is then. It needs a
// data check since the start and one of the stack's containers running; a check
// that found a path that is not whole needs acceptFailedCheck. Confirming a
// confirmed stack changes nothing.
func (s *Service) ConfirmContainer(ctx context.Context, name string, acceptFailedCheck bool) (MigratedStack, error) {
	if err := s.requireInitialized(ctx); err != nil {
		return MigratedStack{}, err
	}
	s.flowMu.Lock()
	defer s.flowMu.Unlock()
	_, st, err := s.migratedStack(ctx, name)
	if err != nil {
		return MigratedStack{}, err
	}
	switch {
	case st.State == StackConfirmed:
		return st, nil
	case st.State != StackStarted:
		return MigratedStack{}, fmt.Errorf("%w: %s", ErrContainerNotStarted, name)
	case !st.Checked:
		return MigratedStack{}, fmt.Errorf("%w: %s", ErrDataCheckRequired, name)
	case st.CheckFailed && !acceptFailedCheck:
		return MigratedStack{}, fmt.Errorf("%w: %s", ErrDataCheckFailed, name)
	}
	cs, err := s.Stacks.Containers(ctx, name)
	if err != nil {
		return MigratedStack{}, fmt.Errorf("reading the containers of stack %s: %w", name, err)
	}
	if !anyActive(cs) {
		return MigratedStack{}, fmt.Errorf("%w: %s", ErrContainerNotRunning, name)
	}
	st.State = StackConfirmed
	err = s.updateFlow(ctx, func(f *ContainerFlow) error {
		i := f.index(name)
		if i < 0 {
			return fmt.Errorf("%w: %q", ErrStackNotMigrated, name)
		}
		f.Stacks[i].State = StackConfirmed
		st = f.Stacks[i]
		return nil
	})
	return st, err
}
