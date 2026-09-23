package disk

import (
	"context"
	"errors"
	"testing"
)

func TestMountUnitController_MountAndUnmount(t *testing.T) {
	unit := MountUnit{Where: "/mnt/disk1", UUID: "1234", Filesystem: XFS}
	r := NewFakeRunner()
	r.Script("systemctl", []string{"start", "mnt-disk1.mount"}, nil, nil)
	r.Script("systemctl", []string{"stop", "mnt-disk1.mount"}, nil, nil)

	c := MountUnitController{Unit: unit, Runner: r}
	if got := c.Where(); got != "/mnt/disk1" {
		t.Fatalf("Where() = %q, want /mnt/disk1", got)
	}
	if err := c.Mount(context.Background()); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if err := c.Unmount(context.Background()); err != nil {
		t.Fatalf("Unmount: %v", err)
	}

	calls := r.Calls()
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2: %+v", len(calls), calls)
	}
	if calls[0].Name != "systemctl" || calls[0].Args[0] != "start" || calls[0].Args[1] != "mnt-disk1.mount" {
		t.Fatalf("first call = %+v, want systemctl start mnt-disk1.mount", calls[0])
	}
	if calls[1].Name != "systemctl" || calls[1].Args[0] != "stop" || calls[1].Args[1] != "mnt-disk1.mount" {
		t.Fatalf("second call = %+v, want systemctl stop mnt-disk1.mount", calls[1])
	}
}

func TestMountUnitController_PropagatesErrors(t *testing.T) {
	unit := MountUnit{Where: "/mnt/disk1"}
	r := NewFakeRunner()
	wantErr := errors.New("unit not found")
	r.Script("systemctl", []string{"start", "mnt-disk1.mount"}, nil, wantErr)
	r.Script("systemctl", []string{"stop", "mnt-disk1.mount"}, nil, wantErr)

	c := MountUnitController{Unit: unit, Runner: r}
	if err := c.Mount(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Mount: got %v, want it to wrap %v", err, wantErr)
	}
	if err := c.Unmount(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Unmount: got %v, want it to wrap %v", err, wantErr)
	}
}

func TestServiceUnitController_StopAndStart(t *testing.T) {
	r := NewFakeRunner()
	r.Script("systemctl", []string{"show", "--property=LoadState", "--value", "smbd.service"}, []byte("loaded\n"), nil)
	r.Script("systemctl", []string{"show", "--property=UnitFileState", "--value", "smbd.service"}, []byte("enabled\n"), nil)
	r.Script("systemctl", []string{"stop", "smbd.service"}, nil, nil)
	r.Script("systemctl", []string{"start", "smbd.service"}, nil, nil)

	c := ServiceUnitController{ServiceName: "Samba", Unit: "smbd.service", Runner: r}
	if got := c.Name(); got != "Samba" {
		t.Fatalf("Name() = %q, want Samba", got)
	}
	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	calls := r.Calls()
	if len(calls) != 5 {
		t.Fatalf("got %d calls, want 5: %+v", len(calls), calls)
	}
	if calls[0].Name != "systemctl" || calls[0].Args[0] != "show" || calls[0].Args[3] != "smbd.service" {
		t.Fatalf("first call = %+v, want systemctl show --property=LoadState --value smbd.service", calls[0])
	}
	if calls[1].Name != "systemctl" || calls[1].Args[0] != "stop" || calls[1].Args[1] != "smbd.service" {
		t.Fatalf("second call = %+v, want systemctl stop smbd.service", calls[1])
	}
	if calls[2].Name != "systemctl" || calls[2].Args[0] != "show" || calls[2].Args[3] != "smbd.service" {
		t.Fatalf("third call = %+v, want systemctl show --property=LoadState --value smbd.service", calls[2])
	}
	if calls[3].Name != "systemctl" || calls[3].Args[1] != "--property=UnitFileState" {
		t.Fatalf("fourth call = %+v, want systemctl show --property=UnitFileState --value smbd.service", calls[3])
	}
	if calls[4].Name != "systemctl" || calls[4].Args[0] != "start" || calls[4].Args[1] != "smbd.service" {
		t.Fatalf("fifth call = %+v, want systemctl start smbd.service", calls[4])
	}
}

// TestServiceUnitController_StartSkipsMaskedAndDisabledUnits: a user who
// masked nfs-kernel-server (no NFS) must not have every array start fail
// at its last step with "Unit is masked", and one who disabled smbd must
// not have file sharing switched back on by an array start. Stop still
// stops either.
func TestServiceUnitController_StartSkipsMaskedAndDisabledUnits(t *testing.T) {
	for _, tc := range []struct{ loadState, fileState string }{
		{"masked", "masked"},
		{"loaded", "disabled"},
	} {
		r := NewFakeRunner()
		r.Script("systemctl", []string{"show", "--property=LoadState", "--value", "smbd.service"}, []byte(tc.loadState+"\n"), nil)
		r.Script("systemctl", []string{"show", "--property=UnitFileState", "--value", "smbd.service"}, []byte(tc.fileState+"\n"), nil)
		r.Script("systemctl", []string{"stop", "smbd.service"}, nil, nil)

		c := ServiceUnitController{ServiceName: "Samba", Unit: "smbd.service", Runner: r}
		if err := c.Start(context.Background()); err != nil {
			t.Fatalf("%s/%s: Start: %v", tc.loadState, tc.fileState, err)
		}
		for _, call := range r.Calls() {
			if call.Args[0] == "start" {
				t.Fatalf("%s/%s: Start ran %+v, want the unit left alone", tc.loadState, tc.fileState, call)
			}
		}
		if err := c.Stop(context.Background()); err != nil {
			t.Fatalf("%s/%s: Stop: %v", tc.loadState, tc.fileState, err)
		}
		stopped := false
		for _, call := range r.Calls() {
			if call.Args[0] == "stop" {
				stopped = true
			}
		}
		if !stopped {
			t.Fatalf("%s/%s: Stop did not stop the unit", tc.loadState, tc.fileState)
		}
	}
}

func TestServiceUnitController_PropagatesErrors(t *testing.T) {
	r := NewFakeRunner()
	wantErr := errors.New("Job failed, unit is holding open files")
	r.Script("systemctl", []string{"show", "--property=LoadState", "--value", "smbd.service"}, []byte("loaded\n"), nil)
	r.Script("systemctl", []string{"stop", "smbd.service"}, nil, wantErr)
	r.Script("systemctl", []string{"start", "smbd.service"}, nil, wantErr)

	c := ServiceUnitController{ServiceName: "Samba", Unit: "smbd.service", Runner: r}
	if err := c.Stop(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Stop: got %v, want it to wrap %v", err, wantErr)
	}
	if err := c.Start(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Start: got %v, want it to wrap %v", err, wantErr)
	}
}

// TestServiceUnitController_SkipsUnitNotInstalled is #309's fix for
// hosts with only one of Samba/NFS installed (#331 covers making both
// `.deb` Depends): a LoadState of "not-found" must be treated as
// nothing to stop or start, never as a failure that aborts the array
// sequence, and never by attempting a real systemctl stop/start on a
// unit that was never installed.
func TestServiceUnitController_SkipsUnitNotInstalled(t *testing.T) {
	r := NewFakeRunner()
	r.Script("systemctl", []string{"show", "--property=LoadState", "--value", "nfs-kernel-server.service"}, []byte("not-found\n"), nil)

	c := ServiceUnitController{ServiceName: "NFS", Unit: "nfs-kernel-server.service", Runner: r}
	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: got %v, want nil — a not-found unit has nothing to stop", err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: got %v, want nil — a not-found unit has nothing to start", err)
	}

	calls := r.Calls()
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want only the two LoadState queries, no stop/start: %+v", len(calls), calls)
	}
	for _, c := range calls {
		if c.Args[0] != "show" {
			t.Fatalf("call = %+v, want only LoadState queries — a not-found unit must never be stopped or started", c)
		}
	}
}

// TestServiceUnitController_FailsClosedWhenLoadStateQueryErrors is the
// other half of #309's fix: a systemctl show error must abort Stop/Start
// with an error, the same as a real stop/start failure would, rather
// than being read as "nothing to do" and letting the array sequence
// unmount storage a service might still be holding open.
func TestServiceUnitController_FailsClosedWhenLoadStateQueryErrors(t *testing.T) {
	r := NewFakeRunner()
	queryErr := errors.New("systemctl: failed to connect to bus")
	r.Script("systemctl", []string{"show", "--property=LoadState", "--value", "smbd.service"}, nil, queryErr)

	c := ServiceUnitController{ServiceName: "Samba", Unit: "smbd.service", Runner: r}
	if err := c.Stop(context.Background()); !errors.Is(err, queryErr) {
		t.Fatalf("Stop: got %v, want it to wrap %v", err, queryErr)
	}
	if err := c.Start(context.Background()); !errors.Is(err, queryErr) {
		t.Fatalf("Start: got %v, want it to wrap %v", err, queryErr)
	}

	calls := r.Calls()
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want only the two failed LoadState queries, no stop/start attempted: %+v", len(calls), calls)
	}
}
