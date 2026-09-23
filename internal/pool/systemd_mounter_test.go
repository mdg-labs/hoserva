package pool

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
)

func TestUnitFileName_EscapesLiteralHyphens(t *testing.T) {
	got := UnitFileName("/mnt/user/tv-shows")
	want := `mnt-user-tv\x2dshows.mount`
	if got != want {
		t.Fatalf("UnitFileName(/mnt/user/tv-shows): got %q, want %q", got, want)
	}
	if got := UnitFileName("/mnt/user"); got != "mnt-user.mount" {
		t.Fatalf("UnitFileName(/mnt/user): got %q, want mnt-user.mount", got)
	}
}

func TestSystemdMounter_MountAndUnmount(t *testing.T) {
	where := testWhere(t)
	m := Mount{Where: where, What: "/mnt/disk1=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}
	r := disk.NewFakeRunner()
	unit := UnitFileName(where)
	r.Script("systemctl", []string{"daemon-reload"}, nil, nil)
	r.Script("systemctl", []string{"start", unit}, nil, nil)
	r.Script("systemctl", []string{"stop", unit}, nil, nil)

	mounter := SystemdMounter{Runner: r, IsMountpoint: func(string) (bool, error) { return false, nil }}
	if err := mounter.Mount(context.Background(), m); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if err := mounter.Unmount(context.Background(), where); err != nil {
		t.Fatalf("Unmount: %v", err)
	}

	calls := r.Calls()
	if len(calls) != 3 {
		t.Fatalf("got calls %+v, want daemon-reload, start, stop", calls)
	}
	if calls[0].Name != "systemctl" || strings.Join(calls[0].Args, " ") != "daemon-reload" {
		t.Fatalf("first call = %+v, want systemctl daemon-reload", calls[0])
	}
	if calls[1].Name != "systemctl" || strings.Join(calls[1].Args, " ") != "start "+unit {
		t.Fatalf("second call = %+v, want systemctl start %s", calls[1], unit)
	}
	if calls[2].Name != "systemctl" || strings.Join(calls[2].Args, " ") != "stop "+unit {
		t.Fatalf("third call = %+v, want systemctl stop %s", calls[2], unit)
	}
}

// TestSystemdMounter_Mount_AdoptsAlreadyOwnMount is #335's adopt path:
// after hoservad restarts, the pool .mount unit (and its mergerfs) is
// still up — Mount must not stack a second start, only applyRuntime.
func TestSystemdMounter_Mount_AdoptsAlreadyOwnMount(t *testing.T) {
	where := testWhere(t)
	m := Mount{
		Where: where, What: "/mnt/disk1=RW:/mnt/disk2=RW", FSName: "hoserva-pool",
		CreatePolicy: FillDisksInOrder, Options: Options{MinFreeSpace: "20G"},
	}
	r := disk.NewFakeRunner()
	r.Script("findmnt", []string{"-n", "-o", "SOURCE", where}, []byte(m.FSName+"\n"), nil)

	set := map[string]string{}
	mounter := SystemdMounter{
		Runner:       r,
		IsMountpoint: func(string) (bool, error) { return true, nil },
		SetXattr: func(path, attr string, value []byte) error {
			set[attr] = string(value)
			return nil
		},
	}
	if err := mounter.Mount(context.Background(), m); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	calls := r.Calls()
	for _, c := range calls {
		if c.Name == "systemctl" {
			t.Fatalf("Mount: got systemctl call %+v, want only findmnt then applyRuntime", calls)
		}
	}
	if set["user.mergerfs.branches"] != m.What {
		t.Fatalf("applyRuntime branches = %q, want %q", set["user.mergerfs.branches"], m.What)
	}
}

func TestSystemdMounter_Mount_PropagatesStartError(t *testing.T) {
	where := testWhere(t)
	m := Mount{Where: where, What: "/mnt/disk1=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}
	wantErr := errors.New("unit not found")
	r := disk.NewFakeRunner()
	r.Script("systemctl", []string{"daemon-reload"}, nil, nil)
	r.Script("systemctl", []string{"start", UnitFileName(where)}, nil, wantErr)

	mounter := SystemdMounter{Runner: r, IsMountpoint: func(string) (bool, error) { return false, nil }}
	if err := mounter.Mount(context.Background(), m); !errors.Is(err, wantErr) {
		t.Fatalf("Mount: got %v, want it to wrap %v", err, wantErr)
	}
}
