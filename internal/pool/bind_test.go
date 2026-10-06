package pool

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// bindTable is a fake mount table for the branch binds: `mount --bind`
// mounts its target with bindOpts, `umount` removes it, and every call is
// recorded in order. Every other call succeeds without effect. The source
// of each bind passed to newBindTable reads as a mounted disk.
type bindTable struct {
	mu       sync.Mutex
	mounted  map[string][]string
	bindOpts []string
	calls    []string
}

func newBindTable(mountedSources ...disk.BranchBind) *bindTable {
	t := &bindTable{mounted: map[string][]string{}, bindOpts: []string{"rw", "nosymfollow"}}
	for _, b := range mountedSources {
		t.mounted[b.Source] = []string{"rw"}
	}
	return t
}

func (b *bindTable) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, strings.Join(append([]string{name}, args...), " "))
	switch {
	case name == "mount" && len(args) == 5 && args[0] == "--bind":
		b.mounted[args[4]] = b.bindOpts
	case name == "umount" && len(args) == 1:
		delete(b.mounted, args[0])
	}
	return nil, nil
}

func (b *bindTable) target(path string) ([]string, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	opts, ok := b.mounted[path]
	return opts, ok, nil
}

func (b *bindTable) Calls() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

// testBind is a branch bind of a real temporary source directory, so the
// existence check sees it, at a temporary bind point.
func testBind(t *testing.T, name string) disk.BranchBind {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join(root, "mnt", name)
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	return disk.BranchBind{Source: src, Where: filepath.Join(root, "branches", name)}
}

func bindMount(t *testing.T, binds ...disk.BranchBind) Mount {
	t.Helper()
	what := make([]string, len(binds))
	for i, b := range binds {
		what[i] = b.Where + "/movies=RW"
	}
	return Mount{
		Where: testWhere(t), What: strings.Join(what, ":"), FSName: "hoserva-movies",
		CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions(), Binds: binds,
	}
}

func notMounted(string) (bool, error) { return false, nil }

func TestMounter_Mount_BindsEveryBranchNosymfollowBeforeMergerfs(t *testing.T) {
	b1, b2 := testBind(t, "disk1"), testBind(t, "disk2")
	mnt := bindMount(t, b1, b2)
	table := newBindTable(b1, b2)
	m := Mounter{Runner: table, IsMountpoint: notMounted, MountTarget: table.target}

	if err := m.Mount(context.Background(), mnt); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	argv := mnt.Argv()
	want := []string{
		"mount --bind -o nosymfollow " + b1.Source + " " + b1.Where,
		"mount --bind -o nosymfollow " + b2.Source + " " + b2.Where,
		strings.Join(argv, " "),
	}
	if got := table.Calls(); !slices.Equal(got, want) {
		t.Fatalf("calls:\n got %q\nwant %q", got, want)
	}
}

func TestMounter_Mount_KeepsABindStillOnItsSourceWithNosymfollow(t *testing.T) {
	b := testBind(t, "disk1")
	table := newBindTable(b)
	table.mounted[b.Where] = []string{"rw", "nosymfollow"}
	m := Mounter{Runner: table, IsMountpoint: notMounted, MountTarget: table.target, SameFile: func(string, string) (bool, error) { return true, nil }}

	if err := m.Mount(context.Background(), bindMount(t, b)); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	for _, c := range table.Calls() {
		if strings.HasPrefix(c, "mount") || strings.HasPrefix(c, "umount") {
			t.Fatalf("calls %q: a live bind on its own source was bound again", table.Calls())
		}
	}
}

// A bind left over from a disk that was since remounted (another
// filesystem at its source), or one without nosymfollow in force, is
// unmounted and bound again: it must never keep serving the old
// filesystem or follow a symlink.
func TestMounter_Mount_RebindsAStaleOrSymfollowingBind(t *testing.T) {
	for name, tc := range map[string]struct {
		same bool
		opts []string
	}{
		"other source": {same: false, opts: []string{"rw", "nosymfollow"}},
		"symfollowing": {same: true, opts: []string{"rw"}},
	} {
		t.Run(name, func(t *testing.T) {
			b := testBind(t, "disk1")
			table := newBindTable(b)
			table.mounted[b.Where] = tc.opts
			m := Mounter{Runner: table, IsMountpoint: notMounted, MountTarget: table.target, SameFile: func(string, string) (bool, error) { return tc.same, nil }}

			if err := m.Mount(context.Background(), bindMount(t, b)); err != nil {
				t.Fatalf("Mount: %v", err)
			}
			calls := table.Calls()
			want := []string{"umount " + b.Where, "mount --bind -o nosymfollow " + b.Source + " " + b.Where}
			if len(calls) < 2 || !slices.Equal(calls[:2], want) {
				t.Fatalf("calls %q, want %q first", calls, want)
			}
		})
	}
}

func TestMounter_Mount_LeavesOutTheBindOfAMissingSource(t *testing.T) {
	present := testBind(t, "disk1")
	missing := disk.BranchBind{Source: filepath.Join(t.TempDir(), "gone"), Where: filepath.Join(t.TempDir(), "branches", "gone")}
	table := newBindTable(present)
	m := Mounter{Runner: table, IsMountpoint: notMounted, MountTarget: table.target}

	if err := m.Mount(context.Background(), bindMount(t, present, missing)); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	for _, c := range table.Calls() {
		if strings.HasPrefix(c, "mount ") && strings.Contains(c, missing.Where) {
			t.Fatalf("calls %q: a source that does not exist was bound", table.Calls())
		}
	}
	if _, ok := table.mounted[present.Where]; !ok {
		t.Fatalf("the present disk's bind is not mounted: %v", table.mounted)
	}
}

func TestMounter_Mount_UnmountsTheBindOfASourceThatIsGone(t *testing.T) {
	missing := disk.BranchBind{Source: filepath.Join(t.TempDir(), "gone"), Where: filepath.Join(t.TempDir(), "branches", "gone")}
	table := newBindTable()
	table.mounted[missing.Where] = []string{"rw", "nosymfollow"}
	m := Mounter{Runner: table, IsMountpoint: notMounted, MountTarget: table.target}

	if err := m.Mount(context.Background(), bindMount(t, missing)); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if _, ok := table.mounted[missing.Where]; ok {
		t.Fatalf("calls %q: the bind of a source that is gone was kept", table.Calls())
	}
}

// A data disk that is not mounted still leaves its mountpoint directory on
// the root filesystem. That directory is never bound, and a bind left over
// from when the disk was mounted is unmounted, so the mover never places a
// file on the root filesystem through it.
func TestMounter_Mount_LeavesOutTheBindOfASourceThatIsNotMounted(t *testing.T) {
	for name, leftover := range map[string]bool{"no bind": false, "leftover bind": true} {
		t.Run(name, func(t *testing.T) {
			present, unmounted := testBind(t, "disk1"), testBind(t, "disk2")
			table := newBindTable(present)
			if leftover {
				table.mounted[unmounted.Where] = []string{"rw", "nosymfollow"}
			}
			m := Mounter{Runner: table, IsMountpoint: notMounted, MountTarget: table.target, SameFile: func(string, string) (bool, error) { return true, nil }}

			if err := m.Mount(context.Background(), bindMount(t, present, unmounted)); err != nil {
				t.Fatalf("Mount: %v", err)
			}
			for _, c := range table.Calls() {
				if strings.HasPrefix(c, "mount ") && strings.Contains(c, unmounted.Where) {
					t.Fatalf("calls %q: the bare mountpoint of a disk that is not mounted was bound", table.Calls())
				}
			}
			if _, ok := table.mounted[unmounted.Where]; ok {
				t.Fatalf("calls %q: a bind of a disk that is not mounted was kept", table.Calls())
			}
			if _, ok := table.mounted[present.Where]; !ok {
				t.Fatalf("the mounted disk's bind is not mounted: %v", table.mounted)
			}
		})
	}
}

// A bind the kernel did not give nosymfollow (a mount(8) that ignores the
// flag on a bind) is never used: it is unmounted, and the mount refused
// before mergerfs runs.
func TestMounter_Mount_RefusesABindWithoutNosymfollow(t *testing.T) {
	b := testBind(t, "disk1")
	mnt := bindMount(t, b)
	table := newBindTable(b)
	table.bindOpts = []string{"rw"}
	m := Mounter{Runner: table, IsMountpoint: notMounted, MountTarget: table.target}

	err := m.Mount(context.Background(), mnt)
	if err == nil || !strings.Contains(err.Error(), "nosymfollow") {
		t.Fatalf("Mount = %v, want a refusal naming nosymfollow", err)
	}
	if _, ok := table.mounted[b.Where]; ok {
		t.Fatal("the bind without nosymfollow was left mounted")
	}
	for _, c := range table.Calls() {
		if strings.HasPrefix(c, "mergerfs") {
			t.Fatalf("calls %q: mergerfs ran over a bind that follows symlinks", table.Calls())
		}
	}
}

// The adopt path (#268) brings a new disk's bind up before applyRuntime
// adds its branch to the live mount.
func TestMounter_Mount_BindsBeforeUpdatingALiveMount(t *testing.T) {
	b := testBind(t, "disk1")
	mnt := bindMount(t, b)
	table := newBindTable(b)
	var order []string
	m := Mounter{
		Runner: runnerFunc(func(name string, args ...string) ([]byte, error) {
			if name == "findmnt" {
				return []byte(mnt.FSName + "\n"), nil
			}
			order = append(order, name)
			return table.Run(context.Background(), name, args...)
		}),
		IsMountpoint: func(string) (bool, error) { return true, nil },
		MountTarget:  table.target,
		GetXattr:     func(string, string) ([]byte, error) { return nil, os.ErrNotExist },
		SetXattr: func(_, attr string, _ []byte) error {
			order = append(order, "setxattr "+attr)
			return nil
		},
	}

	if err := m.Mount(context.Background(), mnt); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if len(order) < 2 || order[0] != "mount" || !slices.Contains(order, "setxattr user.mergerfs.branches") {
		t.Fatalf("order %q, want the bind mounted before the live branches change", order)
	}
}

type runnerFunc func(name string, args ...string) ([]byte, error)

func (f runnerFunc) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	return f(name, args...)
}

func TestSystemdMounter_Mount_StartsTheBindUnitsBeforeTheMount(t *testing.T) {
	b1, b2 := testBind(t, "disk1"), testBind(t, "disk2")
	mnt := bindMount(t, b1, b2)
	table := newBindTable()
	table.mounted[b2.Where] = []string{"rw", "nosymfollow"}
	m := SystemdMounter{Runner: table, IsMountpoint: notMounted, MountTarget: table.target, SameFile: sameFile}

	if err := m.Mount(context.Background(), mnt); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	want := []string{
		"systemctl daemon-reload",
		"systemctl start " + UnitFileName(b1.Where),
		"systemctl start " + UnitFileName(mnt.Where),
	}
	if got := table.Calls(); !slices.Equal(got, want) {
		t.Fatalf("calls:\n got %q\nwant %q", got, want)
	}
}

// A disk added to a live mover target (#268's adopt path): its bind unit,
// just written, is started — after a reload — before the new branch list
// is applied; binds already up cost no systemctl call.
func TestSystemdMounter_Mount_StartsANewBindBeforeUpdatingALiveMount(t *testing.T) {
	b1, b2 := testBind(t, "disk1"), testBind(t, "disk2")
	mnt := bindMount(t, b1, b2)
	table := newBindTable()
	table.mounted[b1.Where] = []string{"rw", "nosymfollow"}
	var order []string
	m := SystemdMounter{
		Runner: runnerFunc(func(name string, args ...string) ([]byte, error) {
			if name == "findmnt" {
				return []byte(mnt.FSName + "\n"), nil
			}
			order = append(order, strings.Join(append([]string{name}, args...), " "))
			return nil, nil
		}),
		IsMountpoint: func(string) (bool, error) { return true, nil },
		MountTarget:  table.target,
		SameFile:     sameFile,
		GetXattr:     func(string, string) ([]byte, error) { return nil, os.ErrNotExist },
		SetXattr: func(_, attr string, _ []byte) error {
			order = append(order, "setxattr "+attr)
			return nil
		},
	}

	if err := m.Mount(context.Background(), mnt); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	want := []string{"systemctl daemon-reload", "systemctl start " + UnitFileName(b2.Where), "setxattr user.mergerfs.branches"}
	if len(order) < 3 || !slices.Equal(order[:3], want) {
		t.Fatalf("order %q, want %q first", order, want)
	}

	order = nil
	table.mounted[b2.Where] = []string{"rw", "nosymfollow"}
	if err := m.Mount(context.Background(), mnt); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	for _, c := range order {
		if strings.HasPrefix(c, "systemctl") {
			t.Fatalf("order %q: systemd was asked to start binds that are already up", order)
		}
	}
}

// A bind mounted without nosymfollow — by hand, or by a mount(8) that
// dropped the flag — is never used as a branch, already up or once
// started.
func TestSystemdMounter_Mount_RefusesABindWithoutNosymfollow(t *testing.T) {
	for name, alreadyUp := range map[string]bool{"already up": true, "once started": false} {
		t.Run(name, func(t *testing.T) {
			b := testBind(t, "disk1")
			mnt := bindMount(t, b)
			table := newBindTable()
			if alreadyUp {
				table.mounted[b.Where] = []string{"rw"}
			}
			runner := runnerFunc(func(name string, args ...string) ([]byte, error) {
				if name == "systemctl" && len(args) == 2 && args[0] == "start" && args[1] == UnitFileName(b.Where) {
					table.mu.Lock()
					table.mounted[b.Where] = []string{"rw"}
					table.mu.Unlock()
				}
				return table.Run(context.Background(), name, args...)
			})
			m := SystemdMounter{Runner: runner, IsMountpoint: notMounted, MountTarget: table.target, SameFile: sameFile}

			err := m.Mount(context.Background(), mnt)
			if err == nil || !strings.Contains(err.Error(), "nosymfollow") {
				t.Fatalf("Mount = %v, want a refusal naming nosymfollow", err)
			}
			for _, c := range table.Calls() {
				if c == "systemctl start "+UnitFileName(mnt.Where) {
					t.Fatalf("calls %q: the mover target was started over a bind that follows symlinks", table.Calls())
				}
			}
		})
	}
}

func sameFile(string, string) (bool, error) { return true, nil }

// systemdBinds is systemd for binds' units over table: stopping a bind's
// unit unmounts it, starting it mounts it with table's bindOpts, and
// current reports afterwards whether that bind is its disk's current
// filesystem.
func systemdBinds(table *bindTable, current func(where string) bool, binds ...disk.BranchBind) (disk.Runner, func(string, string) (bool, error)) {
	units := map[string]string{}
	for _, b := range binds {
		units[UnitFileName(b.Where)] = b.Where
	}
	runner := runnerFunc(func(name string, args ...string) ([]byte, error) {
		if name == "systemctl" && len(args) == 2 {
			if where, ok := units[args[1]]; ok {
				table.mu.Lock()
				switch args[0] {
				case "stop":
					delete(table.mounted, where)
				case "start":
					table.mounted[where] = table.bindOpts
				}
				table.mu.Unlock()
			}
		}
		return table.Run(context.Background(), name, args...)
	})
	same := func(_, where string) (bool, error) { return current(where), nil }
	return runner, same
}

// A bind still mounted from a disk that has since left its slot — the old
// disk still attached, a new one now at /mnt/diskN — is not its disk's
// current filesystem, however it is mounted: the mover would write onto
// the old disk, outside the array. Its unit is stopped and started again
// before the mover target uses it, on a fresh start and on a live update.
func TestSystemdMounter_Mount_RestartsABindThatIsNotItsDisksCurrentFilesystem(t *testing.T) {
	for name, live := range map[string]bool{"fresh start": false, "live update": true} {
		t.Run(name, func(t *testing.T) {
			b := testBind(t, "disk1")
			mnt := bindMount(t, b)
			table := newBindTable()
			table.mounted[b.Where] = []string{"rw", "nosymfollow"}
			restarted := false
			runner, same := systemdBinds(table, func(string) bool { return restarted }, b)
			var calls []string
			m := SystemdMounter{
				Runner: runnerFunc(func(name string, args ...string) ([]byte, error) {
					if name == "findmnt" {
						return []byte(mnt.FSName + "\n"), nil
					}
					call := strings.Join(append([]string{name}, args...), " ")
					calls = append(calls, call)
					if call == "systemctl start "+UnitFileName(b.Where) {
						restarted = true
					}
					return runner.Run(context.Background(), name, args...)
				}),
				IsMountpoint: func(string) (bool, error) { return live, nil },
				MountTarget:  table.target,
				SameFile:     same,
				GetXattr:     func(string, string) ([]byte, error) { return nil, os.ErrNotExist },
				SetXattr:     func(string, string, []byte) error { return nil },
			}

			if err := m.Mount(context.Background(), mnt); err != nil {
				t.Fatalf("Mount: %v", err)
			}
			want := []string{"systemctl daemon-reload", "systemctl stop " + UnitFileName(b.Where), "systemctl start " + UnitFileName(b.Where)}
			if len(calls) < 3 || !slices.Equal(calls[:3], want) {
				t.Fatalf("calls %q, want %q first", calls, want)
			}
		})
	}
}

// A bind that is still not its disk's current filesystem once its unit is
// started again is refused, and the mover target never started over it.
func TestSystemdMounter_Mount_RefusesABindThatStaysOffItsDisk(t *testing.T) {
	b := testBind(t, "disk1")
	mnt := bindMount(t, b)
	table := newBindTable()
	table.mounted[b.Where] = []string{"rw", "nosymfollow"}
	runner, same := systemdBinds(table, func(string) bool { return false }, b)
	m := SystemdMounter{Runner: runner, IsMountpoint: notMounted, MountTarget: table.target, SameFile: same}

	err := m.Mount(context.Background(), mnt)
	if err == nil || !strings.Contains(err.Error(), b.Source) {
		t.Fatalf("Mount = %v, want a refusal naming %s", err, b.Source)
	}
	for _, c := range table.Calls() {
		if c == "systemctl start "+UnitFileName(mnt.Where) {
			t.Fatalf("calls %q: the mover target was started over a bind of another filesystem", table.Calls())
		}
	}
}
