package job

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
)

func validFormatParams(parity, data string, sizes map[string]int64) DiskFormatParams {
	plan := disk.TopologyPlan{
		Parity: []disk.AssignedDisk{{Device: parity, Filesystem: disk.XFS}},
		Data:   []disk.AssignedDisk{{Device: data, Filesystem: disk.XFS}},
	}
	return DiskFormatParams{
		Confirmation: plan.Confirmation(),
		Parity:       plan.Parity,
		Data:         plan.Data,
		Sizes:        sizes,
	}
}

func TestRunDiskFormat_WrongConfirmationFormatsNothing(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	s.registry.Register(TypeDiskFormat, false, RunDiskFormat(p, disk.NewFakeRunner()))

	params := validFormatParams("/dev/sda", "/dev/sdb", map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdb": 4 * disk.TB})
	params.Confirmation = "erase /dev/sda, /dev/sdb"

	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "typed confirmation") {
		t.Fatalf("ErrorMessage = %q, want confirmation mismatch", finished.ErrorMessage)
	}
	if _, ok := p.FormattedAs("/dev/sda"); ok {
		t.Fatal("wrong confirmation formatted /dev/sda")
	}
	if _, ok := p.FormattedAs("/dev/sdb"); ok {
		t.Fatal("wrong confirmation formatted /dev/sdb")
	}
}

func TestSubmit_RejectsDiskFormatWithoutConfirmation(t *testing.T) {
	s := newTestScheduler(t)
	s.registry.Register(TypeDiskFormat, false, RunDiskFormat(disk.NewFakeProvider(), disk.NewFakeRunner()))
	for _, params := range [][]byte{
		nil,
		[]byte("null"),
		[]byte(""),
		mustJSON(t, DiskFormatParams{Parity: []disk.AssignedDisk{{Device: "/dev/sda", Filesystem: disk.XFS}}}),
	} {
		_, err := s.Submit(context.Background(), TypeDiskFormat, nil, params)
		if err == nil {
			t.Fatalf("Submit(disk_format, params=%q) = nil error, want rejection", params)
		}
	}
}

func TestRunDiskFormat_UnmanagedPathFormatsNothing(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	s.registry.Register(TypeDiskFormat, false, RunDiskFormat(p, disk.NewFakeRunner()))

	params := validFormatParams("/dev/sda", "/dev/sda1", map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sda1": 4 * disk.TB})
	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if !errors.Is(errors.Unwrap(errors.New(finished.ErrorMessage)), disk.ErrUnmanagedDevice) &&
		!strings.Contains(finished.ErrorMessage, "not a loop device") {
		t.Fatalf("ErrorMessage = %q, want unmanaged-device refusal", finished.ErrorMessage)
	}
	if _, ok := p.FormattedAs("/dev/sda"); ok {
		t.Fatal("unmanaged path in the plan formatted /dev/sda")
	}
	if _, ok := p.FormattedAs("/dev/sda1"); ok {
		t.Fatal("unmanaged path formatted /dev/sda1")
	}
}

func TestRunDiskFormat_ValidateFailuresFormatNothing(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		params DiskFormatParams
		want   string
	}{
		{
			name: "no parity",
			params: DiskFormatParams{
				Confirmation: "ERASE /dev/sdb",
				Data:         []disk.AssignedDisk{{Device: "/dev/sdb", Filesystem: disk.XFS}},
				Sizes:        map[string]int64{"/dev/sdb": 4 * disk.TB},
			},
			want: "at least one parity",
		},
		{
			name: "three parity",
			params: DiskFormatParams{
				Confirmation: "ERASE /dev/sda, /dev/sdb, /dev/sdc, /dev/sdd",
				Parity: []disk.AssignedDisk{
					{Device: "/dev/sda", Filesystem: disk.XFS},
					{Device: "/dev/sdb", Filesystem: disk.XFS},
					{Device: "/dev/sdc", Filesystem: disk.XFS},
				},
				Data:  []disk.AssignedDisk{{Device: "/dev/sdd", Filesystem: disk.XFS}},
				Sizes: map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdb": 8 * disk.TB, "/dev/sdc": 8 * disk.TB, "/dev/sdd": 4 * disk.TB},
			},
			want: "at most two parity",
		},
		{
			name: "parity not xfs",
			params: DiskFormatParams{
				Confirmation: "ERASE /dev/sda, /dev/sdb",
				Parity:       []disk.AssignedDisk{{Device: "/dev/sda", Filesystem: disk.EXT4}},
				Data:         []disk.AssignedDisk{{Device: "/dev/sdb", Filesystem: disk.XFS}},
				Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdb": 4 * disk.TB},
			},
			want: "always formatted XFS",
		},
		{
			name: "weak identity parity",
			params: DiskFormatParams{
				Confirmation: "ERASE /dev/sda, /dev/sdb",
				Parity:       []disk.AssignedDisk{{Device: "/dev/sda", Filesystem: disk.XFS, WeakIdentity: true}},
				Data:         []disk.AssignedDisk{{Device: "/dev/sdb", Filesystem: disk.XFS}},
				Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdb": 4 * disk.TB},
			},
			want: "weak-identity",
		},
		{
			name: "parity smaller than data",
			params: DiskFormatParams{
				Confirmation: "ERASE /dev/sda, /dev/sdb",
				Parity:       []disk.AssignedDisk{{Device: "/dev/sda", Filesystem: disk.XFS}},
				Data:         []disk.AssignedDisk{{Device: "/dev/sdb", Filesystem: disk.XFS}},
				Sizes:        map[string]int64{"/dev/sda": 4 * disk.TB, "/dev/sdb": 8 * disk.TB},
			},
			want: "at least as large",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestScheduler(t)
			p := disk.NewFakeProvider()
			p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
			p.AddDisk("/dev/sdb", disk.Disk{Size: 8 * disk.TB})
			p.AddDisk("/dev/sdc", disk.Disk{Size: 8 * disk.TB})
			p.AddDisk("/dev/sdd", disk.Disk{Size: 8 * disk.TB})
			s.registry.Register(TypeDiskFormat, false, RunDiskFormat(p, disk.NewFakeRunner()))

			j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, tc.params))
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			finished := await(t, s, j.ID)
			if finished.Status != StatusFailed {
				t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
			}
			if !strings.Contains(finished.ErrorMessage, tc.want) {
				t.Fatalf("ErrorMessage = %q, want substring %q", finished.ErrorMessage, tc.want)
			}
			for _, dev := range []string{"/dev/sda", "/dev/sdb", "/dev/sdc", "/dev/sdd"} {
				if _, ok := p.FormattedAs(dev); ok {
					t.Fatalf("formatted %s despite Validate failure %q", dev, tc.name)
				}
			}
		})
	}
}

func TestRunDiskFormat_SuccessFormatsAssignedDisks(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	s.registry.Register(TypeDiskFormat, false, RunDiskFormat(p, disk.NewFakeRunner()))

	params := validFormatParams("/dev/sda", "/dev/sdb", map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdb": 4 * disk.TB})
	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	if fs, ok := p.FormattedAs("/dev/sda"); !ok || fs != disk.XFS {
		t.Fatalf("FormattedAs(/dev/sda) = (%v, %v), want xfs", fs, ok)
	}
	if fs, ok := p.FormattedAs("/dev/sdb"); !ok || fs != disk.XFS {
		t.Fatalf("FormattedAs(/dev/sdb) = (%v, %v), want xfs", fs, ok)
	}
}
